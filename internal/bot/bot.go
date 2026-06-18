package bot

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/formatter"
	"github.com/nyver/tg-news-digest/internal/models"
	"github.com/nyver/tg-news-digest/internal/storage"
)

// BroadcastFunc is called by the bot to run the full digest pipeline + broadcast.
type BroadcastFunc func(ctx context.Context) error

// ErrBroadcastNoDeliveries means a broadcast had active subscribers, but no
// messages were sent, usually because every subscriber's category filter
// matched zero items.
var ErrBroadcastNoDeliveries = errors.New("bot: broadcast sent no messages")

// TopicDigestFunc selects and summarizes news relevant to a free-text topic
// requested via "/digest <тема>". The returned slice may legitimately be
// empty (nothing relevant found) without that being an error.
type TopicDigestFunc func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error)

// TranslateDigestFunc translates a ranked digest's titles, summaries and
// header into targetLang. Implementations should return the original items
// and header unchanged (plus an error) if translation isn't possible.
type TranslateDigestFunc func(ctx context.Context, items []models.RankedNewsItem, header, targetLang string) ([]models.RankedNewsItem, string, error)

// TGAPI defines the minimal subset of telegram-bot-api needed by Bot.
// This interface allows mocking in tests.
type TGAPI interface {
	Send(config tgbotapi.Chattable) (tgbotapi.Message, error)
	GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel
}

// Bot handles Telegram Bot API interactions.
type Bot struct {
	api         TGAPI
	cfg         config.BotConfig
	formatter   *formatter.Formatter
	store       *storage.Store
	broadcastFn BroadcastFunc
	logger      *slog.Logger
	ownerID     int64
	categories  []string
	topicFn     TopicDigestFunc
	translateFn TranslateDigestFunc
}

// SetTopicDigestFunc wires the callback used to answer "/digest <тема>"
// requests. Must be called after construction since the callback typically
// closes over the bot itself (e.g. to access store/llm client built in main).
func (b *Bot) SetTopicDigestFunc(fn TopicDigestFunc) {
	b.topicFn = fn
}

// SetTranslateFunc wires the callback used to translate digest titles and
// summaries into a subscriber's chosen language. Must be called after
// construction for the same reason as SetTopicDigestFunc.
func (b *Bot) SetTranslateFunc(fn TranslateDigestFunc) {
	b.translateFn = fn
}

// New creates a new Bot.
func New(cfg config.BotConfig, fmttr *formatter.Formatter, store *storage.Store, broadcastFn BroadcastFunc, logger *slog.Logger) (*Bot, error) {
	api, err := newTelegramAPI(cfg, logger)
	if err != nil {
		return nil, err
	}

	return &Bot{
		api:         api,
		cfg:         cfg,
		formatter:   fmttr,
		store:       store,
		broadcastFn: broadcastFn,
		logger:      logger,
		ownerID:     cfg.OwnerChatID,
		categories:  cfg.Categories,
	}, nil
}

// newTelegramAPI creates a Telegram BotAPI, optionally using MTProxy.
func newTelegramAPI(cfg config.BotConfig, logger *slog.Logger) (*tgbotapi.BotAPI, error) {
	if !cfg.MTProxy.Enabled {
		api, err := tgbotapi.NewBotAPI(cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("bot: new api: %w", err)
		}
		return api, nil
	}

	proxyPort := cfg.MTProxy.Port
	if proxyPort == 0 {
		proxyPort = 443
	}

	proxyAddr := net.JoinHostPort(cfg.MTProxy.Host, fmt.Sprintf("%d", proxyPort))
	logger.Info("bot: using MTProxy",
		slog.String("addr", proxyAddr),
		slog.Bool("enabled", cfg.MTProxy.Enabled),
	)

	// telegram-bot-api/v5 не поддерживает MTProxy из коробки
	// (функции NewBotAPIWithProxy удалены). Используем прямое соединение.
	logger.Warn("bot: MTProxy not natively supported in telegram-bot-api/v5, using direct connection")

	api, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("bot: new api: %w", err)
	}

	return api, nil
}

// NewWithAPI creates a new Bot with a custom TGAPI implementation (for testing).
func NewWithAPI(api TGAPI, cfg config.BotConfig, fmttr *formatter.Formatter, store *storage.Store, broadcastFn BroadcastFunc, logger *slog.Logger) (*Bot, error) {
	return &Bot{
		api:         api,
		cfg:         cfg,
		formatter:   fmttr,
		store:       store,
		broadcastFn: broadcastFn,
		logger:      logger,
		ownerID:     cfg.OwnerChatID,
		categories:  cfg.Categories,
	}, nil
}

// Start begins long-polling for updates.
// Long-polling timeout is set to 60 seconds per update (Telegram API recommendation).
func (b *Bot) Start(ctx context.Context) error {
	config := tgbotapi.NewUpdate(0)
	config.Timeout = 60
	updates := b.api.GetUpdatesChan(config)

	b.logger.Info("bot: polling started")

	return b.poll(ctx, updates)
}

// poll runs the main update processing loop.
func (b *Bot) poll(ctx context.Context, updates tgbotapi.UpdatesChannel) error {

	for {
		select {
		case <-ctx.Done():
			b.logger.Info("bot: context done, stopping poll")
			return nil
		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("bot: updates channel closed")
			}
			go b.handleUpdate(ctx, update)
		}
	}
}

// handleUpdate processes a single Telegram update.
func (b *Bot) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		b.handleCallback(ctx, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message

	// msg.From is nil for channel posts and anonymous group admins.
	var fromID int64
	var fromUsername string
	if msg.From != nil {
		fromID = msg.From.ID
		fromUsername = msg.From.UserName
	}

	b.logger.Info("bot: incoming message",
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int64("from_id", fromID),
		slog.String("from_username", fromUsername),
		slog.Int("message_id", msg.MessageID),
		slog.Int("date", msg.Date),
		slog.Bool("is_command", msg.IsCommand()),
		slog.Int("reply_to_id", func() int {
			if msg.ReplyToMessage != nil {
				return msg.ReplyToMessage.MessageID
			}
			return 0
		}()),
		slog.String("text", msg.Text),
		slog.String("chat_type", msg.Chat.Type),
	)

	if msg.IsCommand() {
		b.handleCommand(ctx, msg)
		return
	}

	// Handle /start as text too
	text := strings.TrimSpace(msg.Text)
	if text == "/start" {
		b.handleCommand(ctx, msg)
		return
	}
}

// handleCommand dispatches Telegram commands.
func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	cmd := msg.Command()
	chatID := msg.Chat.ID

	switch cmd {
	case "start":
		b.cmdStart(ctx, chatID, msg)
	case "subscribe":
		b.cmdSubscribe(ctx, chatID, msg)
	case "unsubscribe":
		b.cmdUnsubscribe(ctx, chatID, msg)
	case "digest":
		if topic := strings.TrimSpace(msg.CommandArguments()); topic != "" {
			b.cmdDigestTopic(ctx, msg, topic)
		} else {
			b.cmdDigest(ctx, msg)
		}
	case "status":
		b.cmdStatus(ctx, chatID, msg)
	case "categories":
		b.cmdCategories(ctx, chatID, msg)
	case "addcategory":
		b.cmdAddCategory(ctx, chatID, msg)
	case "removecategory":
		b.cmdRemoveCategory(ctx, chatID, msg)
	case "language":
		b.cmdLanguage(ctx, chatID, msg)
	case "mode":
		b.cmdMode(ctx, chatID, msg)
	case "settings":
		b.cmdSettings(ctx, chatID, msg)
	default:
		b.reply(ctx, msg, formatter.UnknownCommandMessage(formatter.ParseMode(b.cfg.ParseMode)))
	}
}

// cmdStart handles /start: subscribes the chat and sends the greeting with
// the full list of available commands (unlike /subscribe, which only
// confirms the subscription).
func (b *Bot) cmdStart(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	if err := b.store.SaveSubscriber(ctx, chatID); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка подписки: %v", err))
		return
	}
	b.reply(ctx, msg, formatter.StartMessage(formatter.ParseMode(b.cfg.ParseMode)))
}

// cmdSubscribe handles /subscribe.
func (b *Bot) cmdSubscribe(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	if err := b.store.SaveSubscriber(ctx, chatID); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка подписки: %v", err))
		return
	}
	b.reply(ctx, msg, formatter.SubscribedMessage(formatter.ParseMode(b.cfg.ParseMode)))
}

// cmdUnsubscribe handles /unsubscribe.
func (b *Bot) cmdUnsubscribe(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	if err := b.store.Unsubscribe(ctx, chatID); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка отписки: %v", err))
		return
	}
	b.reply(ctx, msg, formatter.UnsubscribedMessage(formatter.ParseMode(b.cfg.ParseMode)))
}

// cmdDigest subscribes the caller if not already subscribed, then triggers a
// full digest run if the caller is the owner.
func (b *Bot) cmdDigest(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	// Auto-subscribe the user if they are not yet subscribed.
	active, err := b.store.IsActive(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: digest: check subscriber failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}
	if !active {
		if err := b.store.SaveSubscriber(ctx, chatID); err != nil {
			b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка подписки: %v", err))
			return
		}
		b.reply(ctx, msg, formatter.SubscribedMessage(formatter.ParseMode(b.cfg.ParseMode)))
	}

	// Only the owner can trigger the pipeline.
	if chatID != b.ownerID {
		return
	}

	b.reply(ctx, msg, "⏳ Генерация дайджеста...")

	err = b.broadcastFn(ctx)
	if err != nil {
		if errors.Is(err, ErrBroadcastNoDeliveries) {
			b.reply(ctx, msg, "ℹ️ Дайджест собран, но не отправлен: нет новостей по выбранным категориям. Проверьте /categories или снимите фильтры.")
			return
		}
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка генерации: %v", err))
		return
	}
	b.reply(ctx, msg, "✅ Дайджест успешно отправлен подписчикам.")
}

// cmdDigestTopic handles "/digest <тема>": any user can request an on-demand
// digest scoped to a free-text topic. Unlike cmdDigest, this does not require
// the owner and does not broadcast — the result is sent only to the requester.
func (b *Bot) cmdDigestTopic(ctx context.Context, msg *tgbotapi.Message, topic string) {
	chatID := msg.Chat.ID

	if b.topicFn == nil {
		b.reply(ctx, msg, "❌ Тематический дайджест временно недоступен.")
		return
	}

	b.reply(ctx, msg, fmt.Sprintf("🔎 Ищу новости по теме «%s»...", topic))

	ranked, llmUsed, err := b.topicFn(ctx, topic)
	if err != nil {
		b.logger.Warn("bot: topic digest failed",
			slog.Int64("chat_id", chatID), slog.String("topic", topic), slog.String("error", err.Error()))
		b.reply(ctx, msg, fmt.Sprintf("❌ Не удалось собрать дайджест по теме «%s».", topic))
		return
	}

	if len(ranked) == 0 {
		b.reply(ctx, msg, fmt.Sprintf("ℹ️ Не нашёл новостей по теме «%s» за последние сутки.", topic))
		return
	}

	header := fmt.Sprintf("🔎 Новости по теме «%s»", topic)
	if !llmUsed {
		header += " (по ключевым словам)"
	}

	settings, settingsErr := b.store.GetSubscriberSettings(ctx, chatID)
	if settingsErr != nil {
		b.logger.Warn("bot: get subscriber settings failed",
			slog.Int64("chat_id", chatID), slog.String("error", settingsErr.Error()))
		settings = models.SubscriberSettings{DigestTopN: b.formatter.TopN(), DigestFormat: "detailed"}
	}

	if settingsErr == nil {
		lang := settings.Language
		ranked, header = b.translateForLanguage(ctx, ranked, header, lang)
	}
	if settingsErr != nil {
		if lang, err := b.store.GetSubscriberLanguage(ctx, chatID); err != nil {
			b.logger.Warn("bot: get subscriber language failed",
				slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
		} else {
			ranked, header = b.translateForLanguage(ctx, ranked, header, lang)
		}
	}

	opts := formatter.DigestOptions{TopN: settings.DigestTopN, Format: settings.DigestFormat}
	for _, part := range b.formatter.TopicDigestPartsWithOptions(header, ranked, opts) {
		if err := b.SendRaw(ctx, chatID, part); err != nil {
			b.logger.Warn("bot: send topic digest part failed",
				slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
			return
		}
	}
}

// cmdAddCategory adds a user-defined category/topic to the chat preferences.
func (b *Bot) cmdAddCategory(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	category, err := normalizeUserCategory(msg.CommandArguments())
	if err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ %v\nПример: /addcategory AI agents", err))
		return
	}

	if err := b.store.AddSubscriberCategory(ctx, chatID, category); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка добавления категории: %v", err))
		return
	}
	b.reply(ctx, msg, fmt.Sprintf("✅ Категория добавлена: %s", category))
}

// cmdRemoveCategory removes a category/topic from the chat preferences.
func (b *Bot) cmdRemoveCategory(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	category, err := normalizeUserCategory(msg.CommandArguments())
	if err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ %v\nПример: /removecategory AI agents", err))
		return
	}

	if err := b.store.RemoveSubscriberCategory(ctx, chatID, category); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка удаления категории: %v", err))
		return
	}
	b.reply(ctx, msg, fmt.Sprintf("✅ Категория удалена: %s", category))
}

func normalizeUserCategory(raw string) (string, error) {
	category := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if category == "" {
		return "", fmt.Errorf("укажите название категории")
	}
	if strings.HasPrefix(category, "/") {
		return "", fmt.Errorf("категория не должна быть командой")
	}
	if len([]rune(category)) > 50 {
		return "", fmt.Errorf("категория слишком длинная, максимум 50 символов")
	}
	return category, nil
}

// cmdLanguage shows or changes the chat's digest language preference.
// "/language" alone reports the current setting; "/language <язык>" sets it.
// Any free-text language name is accepted — translation is done by the LLM,
// not matched against a fixed list.
func (b *Bot) cmdLanguage(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	lang := strings.TrimSpace(msg.CommandArguments())

	if lang == "" {
		current, err := b.store.GetSubscriberLanguage(ctx, chatID)
		if err != nil {
			b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
			return
		}
		b.reply(ctx, msg, fmt.Sprintf(
			"🌐 Текущий язык дайджеста: %s\nЧтобы изменить, напишите, например: /language English\nЧтобы вернуться к русскому: /language русский",
			current))
		return
	}

	if err := b.store.SetSubscriberLanguage(ctx, chatID, lang); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
		return
	}
	b.reply(ctx, msg, fmt.Sprintf("✅ Язык дайджеста изменён на: %s", lang))
}

func (b *Bot) cmdMode(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	raw := strings.TrimSpace(msg.CommandArguments())
	if raw == "" {
		st, err := b.store.GetSubscriberSettings(ctx, chatID)
		if err != nil {
			b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
			return
		}
		b.reply(ctx, msg, fmt.Sprintf(
			"Текущий режим дайджеста: %s\n\nДоступные режимы:\n/mode brief\n/mode detailed\n/mode executive\n/mode links\n/mode why_it_matters",
			st.DigestFormat,
		))
		return
	}

	if !isValidDigestMode(raw) {
		b.reply(ctx, msg, "Неизвестный режим. Используйте: brief, detailed, executive, links, why_it_matters")
		return
	}
	mode := storage.NormalizeDigestMode(raw)
	if err := b.store.SetSubscriberDigestFormat(ctx, chatID, mode); err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
		return
	}
	b.reply(ctx, msg, fmt.Sprintf("✅ Режим дайджеста изменён на: %s", mode))
}

func isValidDigestMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "brief", "detailed", "executive", "links", "why_it_matters", "short":
		return true
	default:
		return false
	}
}

func (b *Bot) cmdSettings(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	st, err := b.store.GetSubscriberSettings(ctx, chatID)
	if err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
		return
	}

	cfg := tgbotapi.NewMessage(chatID, settingsText(st))
	cfg.ReplyMarkup = b.buildSettingsKeyboard(st)
	cfg.ReplyToMessageID = msg.MessageID

	if _, err := b.api.Send(cfg); err != nil {
		b.logger.Error("bot: send settings keyboard failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}
}

func settingsText(st models.SubscriberSettings) string {
	format := "подробный"
	if st.DigestFormat == "short" {
		format = "краткий"
	}
	quiet := "выключен"
	format = digestModeLabel(st.DigestFormat)
	if st.QuietWeekends {
		quiet = "включен"
	}
	return fmt.Sprintf(
		"⚙️ Настройки дайджеста\n\nВремя: %s\nTimezone: %s\nНовостей: %d\nФормат: %s\nТихий режим по выходным: %s\nЯзык: %s",
		st.DeliveryTime, st.Timezone, st.DigestTopN, format, quiet, st.Language,
	)
}

func digestModeLabel(mode string) string {
	switch storage.NormalizeDigestMode(mode) {
	case "brief":
		return "brief"
	case "detailed":
		return "detailed"
	case "executive":
		return "executive"
	case "links":
		return "links"
	case "why_it_matters":
		return "why_it_matters"
	default:
		return "detailed"
	}
}

func (b *Bot) buildSettingsKeyboard(st models.SubscriberSettings) tgbotapi.InlineKeyboardMarkup {
	formatLabel := "Краткий формат"
	formatData := "set:format:brief"
	if st.DigestFormat == "brief" {
		formatLabel = "Подробный формат"
		formatData = "set:format:detailed"
	}
	quietLabel := "Тихо по выходным"
	quietData := "set:weekends:on"
	if st.QuietWeekends {
		quietLabel = "Будить по выходным"
		quietData = "set:weekends:off"
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Время", "set:menu:time"),
			tgbotapi.NewInlineKeyboardButtonData("Timezone", "set:menu:tz"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("5", "set:top:5"),
			tgbotapi.NewInlineKeyboardButtonData("10", "set:top:10"),
			tgbotapi.NewInlineKeyboardButtonData("20", "set:top:20"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(formatLabel, formatData),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Режим", "set:menu:modes"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(quietLabel, quietData),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Язык и темы", "set:menu:topics"),
		),
	)
}

func buildSettingsSubmenuKeyboard(kind string) tgbotapi.InlineKeyboardMarkup {
	switch kind {
	case "time":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("08:00", "set:time:08:00"),
				tgbotapi.NewInlineKeyboardButtonData("09:00", "set:time:09:00"),
				tgbotapi.NewInlineKeyboardButtonData("10:00", "set:time:10:00"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("18:00", "set:time:18:00"),
				tgbotapi.NewInlineKeyboardButtonData("20:00", "set:time:20:00"),
			),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "set:back")),
		)
	case "tz":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Moscow", "set:tz:Europe/Moscow"),
				tgbotapi.NewInlineKeyboardButtonData("UTC", "set:tz:UTC"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("London", "set:tz:Europe/London"),
				tgbotapi.NewInlineKeyboardButtonData("New York", "set:tz:America/New_York"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Dubai", "set:tz:Asia/Dubai"),
				tgbotapi.NewInlineKeyboardButtonData("Almaty", "set:tz:Asia/Almaty"),
			),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "set:back")),
		)
	case "modes":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Brief", "set:mode:brief"),
				tgbotapi.NewInlineKeyboardButtonData("Detailed", "set:mode:detailed"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Executive", "set:mode:executive"),
				tgbotapi.NewInlineKeyboardButtonData("Links", "set:mode:links"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Why it matters", "set:mode:why_it_matters"),
			),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "set:back")),
		)
	case "topics":
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Русский", "set:lang:ru"),
				tgbotapi.NewInlineKeyboardButtonData("English", "set:lang:English"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Категории", "set:menu:categories"),
			),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "set:back")),
		)
	default:
		return tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Назад", "set:back"),
		))
	}
}

func settingsSubmenuText(kind string) string {
	switch kind {
	case "time":
		return "Выберите время доставки дайджеста."
	case "tz":
		return "Выберите timezone для доставки."
	case "modes":
		return "Выберите режим дайджеста."
	case "topics":
		return "Язык, категории и кастомные темы.\n\nСвои темы: /addcategory <тема>\nУдалить тему: /removecategory <тема>"
	default:
		return "Настройки дайджеста."
	}
}

// translateForLanguage translates items+header into lang via the configured
// TranslateDigestFunc, falling back to the original (Russian) items/header on
// any failure or if no translator is wired up. A Russian/empty lang is a no-op.
func (b *Bot) translateForLanguage(ctx context.Context, items []models.RankedNewsItem, header, lang string) ([]models.RankedNewsItem, string) {
	if isDefaultLanguage(lang) || b.translateFn == nil {
		return items, header
	}

	translated, translatedHeader, err := b.translateFn(ctx, items, header, lang)
	if err != nil {
		b.logger.Warn("bot: translate digest failed, using Russian",
			slog.String("language", lang), slog.String("error", err.Error()))
		return items, header
	}
	if translatedHeader == "" {
		translatedHeader = header
	}
	return translated, translatedHeader
}

// isDefaultLanguage reports whether lang means "no translation needed" (Russian).
func isDefaultLanguage(lang string) bool {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "", "ru", "rus", "russian", "русский":
		return true
	default:
		return false
	}
}

// cmdCategories shows the category selection keyboard for the chat.
func (b *Bot) cmdCategories(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	selected, err := b.store.GetSubscriberCategories(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: get subscriber categories failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}

	custom := customCategories(selected, b.categories)
	text := "📂 Выберите интересующие категории новостей (нажмите, чтобы включить/выключить).\nЕсли ничего не выбрано — вы получаете дайджест по всем темам.\n\nСвои темы можно добавить командой /addcategory <тема> и удалить командой /removecategory <тема>."
	if len(custom) > 0 {
		text += "\n\nВаши категории:\n" + formatCategoryList(custom)
	}

	cfg := tgbotapi.NewMessage(chatID, text)
	cfg.ReplyMarkup = b.buildCategoriesKeyboard(selected)
	cfg.ReplyToMessageID = msg.MessageID

	if _, err := b.api.Send(cfg); err != nil {
		b.logger.Error("bot: send categories keyboard failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}
}

// buildCategoriesKeyboard renders one toggle button per configured category,
// marking the ones the chat currently has selected.
func (b *Bot) buildCategoriesKeyboard(selected []string) tgbotapi.InlineKeyboardMarkup {
	selectedSet := make(map[string]bool, len(selected))
	for _, c := range selected {
		selectedSet[strings.ToLower(c)] = true
	}

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(b.categories))
	for _, cat := range b.categories {
		label := "⬜ " + cat
		if selectedSet[strings.ToLower(cat)] {
			label = "✅ " + cat
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, "cat:"+categoryCallbackID(cat)),
		))
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func categoryCallbackID(category string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(category))))
	return hex.EncodeToString(sum[:])[:12]
}

func (b *Bot) categoryFromCallbackID(id string) (string, bool) {
	for _, cat := range b.categories {
		if categoryCallbackID(cat) == id {
			return cat, true
		}
	}
	return "", false
}

func customCategories(selected, configured []string) []string {
	configuredSet := make(map[string]bool, len(configured))
	for _, c := range configured {
		configuredSet[strings.ToLower(strings.TrimSpace(c))] = true
	}

	var custom []string
	for _, c := range selected {
		c = strings.TrimSpace(c)
		if c != "" && !configuredSet[strings.ToLower(c)] {
			custom = append(custom, c)
		}
	}
	sort.Slice(custom, func(i, j int) bool {
		return strings.ToLower(custom[i]) < strings.ToLower(custom[j])
	})
	return custom
}

func formatCategoryList(categories []string) string {
	var sb strings.Builder
	for i, c := range categories {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("• ")
		sb.WriteString(c)
	}
	return sb.String()
}

// handleCallback processes inline keyboard button presses (currently only
// category toggles from /categories).
func (b *Bot) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil {
		return
	}

	if strings.HasPrefix(cq.Data, "set:") {
		b.handleSettingsCallback(ctx, cq)
		return
	}

	categoryID, ok := strings.CutPrefix(cq.Data, "cat:")
	if !ok {
		return
	}
	category, ok := b.categoryFromCallbackID(categoryID)
	if !ok {
		category = categoryID
	}

	chatID := cq.Message.Chat.ID

	selected, err := b.store.GetSubscriberCategories(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: get subscriber categories failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}

	isSelected := false
	for _, c := range selected {
		if strings.EqualFold(c, category) {
			isSelected = true
			break
		}
	}

	if isSelected {
		err = b.store.RemoveSubscriberCategory(ctx, chatID, category)
	} else {
		err = b.store.AddSubscriberCategory(ctx, chatID, category)
	}
	if err != nil {
		b.logger.Warn("bot: toggle subscriber category failed",
			slog.Int64("chat_id", chatID), slog.String("category", category), slog.String("error", err.Error()))
	}

	updated, err := b.store.GetSubscriberCategories(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: get subscriber categories failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}

	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, cq.Message.MessageID, b.buildCategoriesKeyboard(updated))
	if _, err := b.api.Send(edit); err != nil {
		b.logger.Warn("bot: update categories keyboard failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}

	// Best-effort: acknowledge the tap so Telegram clears the loading spinner.
	// The library tries to decode the result as a Message, which fails for the
	// boolean answerCallbackQuery result — that decode error is expected and ignored.
	_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, ""))
}

func (b *Bot) handleSettingsCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	data := cq.Data

	if menu, ok := strings.CutPrefix(data, "set:menu:"); ok {
		if menu == "categories" {
			selected, err := b.store.GetSubscriberCategories(ctx, chatID)
			if err != nil {
				b.logger.Warn("bot: get subscriber categories failed",
					slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
			}
			edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, cq.Message.MessageID,
				"Выберите категории. Свои темы: /addcategory <тема>", b.buildCategoriesKeyboard(selected))
			_, _ = b.api.Send(edit)
			_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, ""))
			return
		}
		edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, cq.Message.MessageID,
			settingsSubmenuText(menu), buildSettingsSubmenuKeyboard(menu))
		_, _ = b.api.Send(edit)
		_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, ""))
		return
	}

	if data == "set:back" {
		b.redrawSettings(ctx, cq)
		return
	}

	var err error
	switch {
	case strings.HasPrefix(data, "set:time:"):
		value := strings.TrimPrefix(data, "set:time:")
		if _, parseErr := time.Parse("15:04", value); parseErr != nil {
			err = parseErr
		} else {
			err = b.store.SetSubscriberDeliveryTime(ctx, chatID, value)
		}
	case strings.HasPrefix(data, "set:tz:"):
		value := strings.TrimPrefix(data, "set:tz:")
		if _, parseErr := time.LoadLocation(value); parseErr != nil {
			err = parseErr
		} else {
			err = b.store.SetSubscriberTimezone(ctx, chatID, value)
		}
	case strings.HasPrefix(data, "set:top:"):
		value := strings.TrimPrefix(data, "set:top:")
		topN, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			err = parseErr
		} else {
			err = b.store.SetSubscriberDigestTopN(ctx, chatID, topN)
		}
	case strings.HasPrefix(data, "set:format:"):
		err = b.store.SetSubscriberDigestFormat(ctx, chatID, strings.TrimPrefix(data, "set:format:"))
	case strings.HasPrefix(data, "set:mode:"):
		err = b.store.SetSubscriberDigestFormat(ctx, chatID, strings.TrimPrefix(data, "set:mode:"))
	case strings.HasPrefix(data, "set:weekends:"):
		err = b.store.SetSubscriberQuietWeekends(ctx, chatID, strings.TrimPrefix(data, "set:weekends:") == "on")
	case strings.HasPrefix(data, "set:lang:"):
		err = b.store.SetSubscriberLanguage(ctx, chatID, strings.TrimPrefix(data, "set:lang:"))
	}
	if err != nil {
		b.logger.Warn("bot: update settings failed",
			slog.Int64("chat_id", chatID), slog.String("data", data), slog.String("error", err.Error()))
		_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, "Не удалось сохранить"))
		return
	}

	b.redrawSettings(ctx, cq)
}

func (b *Bot) redrawSettings(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	st, err := b.store.GetSubscriberSettings(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: get settings failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
		_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, "Не удалось загрузить настройки"))
		return
	}
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, cq.Message.MessageID, settingsText(st), b.buildSettingsKeyboard(st))
	if _, err := b.api.Send(edit); err != nil {
		b.logger.Warn("bot: redraw settings failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}
	_, _ = b.api.Send(tgbotapi.NewCallback(cq.ID, ""))
}

// cmdStatus shows the last digest run status.
func (b *Bot) cmdStatus(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	run, err := b.store.GetLastRun(ctx)
	if err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка: %v", err))
		return
	}
	if run == nil {
		b.reply(ctx, msg, "ℹ️ Дайджестов ещё не было.")
		return
	}

	dateStr := run.RunAt.Format("02.01.2006")
	statusMsg := formatter.StatusMessage(run.Status, dateStr, run.ItemCount, formatter.ParseMode(b.cfg.ParseMode))
	b.reply(ctx, msg, statusMsg)
}

// reply sends a message as a reply to the given message.
func (b *Bot) reply(ctx context.Context, msg *tgbotapi.Message, text string) {
	if text == "" {
		return
	}

	cfg := tgbotapi.NewMessage(msg.Chat.ID, text)
	cfg.ReplyToMessageID = msg.MessageID
	cfg.ParseMode = b.cfg.ParseMode

	resp, err := b.api.Send(cfg)
	if err != nil {
		b.logger.Error("bot: send reply failed",
			slog.Int64("chat_id", msg.Chat.ID),
			slog.Int("message_id", msg.MessageID),
			slog.String("error", err.Error()),
		)
		return
	}

	b.logger.Info("bot: sent reply",
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int("reply_to_message_id", msg.MessageID),
		slog.Int("sent_message_id", resp.MessageID),
		slog.Int("sent_date", resp.Date),
		slog.String("parse_mode", cfg.ParseMode),
	)
}

// filterByCategories returns the items whose Category matches one of cats
// (case-insensitive) or whose text matches a user-defined category/topic.
func filterByCategories(items []models.RankedNewsItem, cats []string) []models.RankedNewsItem {
	filtered := make([]models.RankedNewsItem, 0, len(items))
	for _, item := range items {
		for _, cat := range cats {
			if itemMatchesCategory(item, cat) {
				filtered = append(filtered, item)
				break
			}
		}
	}
	return filtered
}

func filterTranslatedBySource(display, source []models.RankedNewsItem, cats []string) []models.RankedNewsItem {
	if len(display) != len(source) {
		return filterByCategories(display, cats)
	}

	filtered := make([]models.RankedNewsItem, 0, len(display))
	for i, item := range source {
		for _, cat := range cats {
			if itemMatchesCategory(item, cat) {
				filtered = append(filtered, display[i])
				break
			}
		}
	}
	return filtered
}

func itemMatchesCategory(item models.RankedNewsItem, category string) bool {
	category = strings.TrimSpace(category)
	if category == "" {
		return false
	}
	if item.Category != "" && strings.EqualFold(item.Category, category) {
		return true
	}

	words := categoryWords(category)
	if len(words) == 0 {
		return false
	}

	text := strings.ToLower(item.Title + " " + item.Summary)
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func categoryWords(category string) []string {
	var words []string
	for _, word := range strings.Fields(strings.ToLower(category)) {
		word = strings.Trim(word, ".,!?;:()\"'«»[]{}")
		if len([]rune(word)) >= 3 {
			words = append(words, word)
		}
	}
	return words
}

// SendRaw sends a message to a chat ID without reply.
func (b *Bot) SendRaw(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return nil
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = b.cfg.ParseMode

	resp, err := b.api.Send(msg)
	if err != nil {
		b.logger.Error("bot: send raw failed",
			slog.Int64("chat_id", chatID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("bot: send to %d: %w", chatID, err)
	}

	b.logger.Info("bot: sent raw message",
		slog.Int64("chat_id", chatID),
		slog.Int("message_id", resp.MessageID),
		slog.Int("date", resp.Date),
		slog.String("parse_mode", msg.ParseMode),
	)
	return nil
}

// Broadcast sends a digest to all active subscribers.
func (b *Bot) Broadcast(ctx context.Context, items []models.RankedNewsItem, date time.Time) error {
	if len(items) == 0 {
		return nil
	}

	recipients, err := b.store.GetActiveSubscriberSettings(ctx)
	if err != nil {
		return fmt.Errorf("bot: get active subscribers: %w", err)
	}
	return b.BroadcastTo(ctx, items, date, recipients)
}

// BroadcastTo sends a digest to the provided subscribers using each chat's
// personal language, category, top-N and format settings.
func (b *Bot) BroadcastTo(ctx context.Context, items []models.RankedNewsItem, date time.Time, recipients []models.SubscriberSettings) error {
	if len(items) == 0 || len(recipients) == 0 {
		return nil
	}

	b.logger.Info("bot: broadcast starting",
		slog.Int("items", len(items)),
		slog.Time("date", date),
	)

	settingsByLang := make(map[string][]models.SubscriberSettings)
	for _, st := range recipients {
		if st.Language == "" {
			st.Language = "ru"
		}
		settingsByLang[st.Language] = append(settingsByLang[st.Language], st)
	}

	totalChats := len(recipients)
	b.logger.Info("bot: broadcast subscribers",
		slog.Int("subscribers", totalChats), slog.Int("languages", len(settingsByLang)))

	if totalChats == 0 {
		return nil
	}

	// Global rate limiter: one send token per 50ms (~20 msg/s, within Telegram's limit).
	// Each goroutine waits for a tick, so sends are serialised through the ticker channel.
	rateLimiter := time.NewTicker(50 * time.Millisecond)
	defer rateLimiter.Stop()

	// Semaphore caps the number of goroutines waiting on rateLimiter to avoid
	// creating thousands of goroutines for very large subscriber lists.
	const maxConcurrentSends = 20
	sem := make(chan struct{}, maxConcurrentSends)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failedCount := 0
	sentCount := 0
	skippedNoMatchCount := 0

	for lang, settings := range settingsByLang {
		// Translate once per distinct language rather than per subscriber.
		langItems := items
		translatedHeader := ""
		if !isDefaultLanguage(lang) {
			translatedItems, header := b.translateForLanguage(
				ctx, items, formatter.PlainDigestHeader(date, b.formatter.TopN()), lang)
			langItems = translatedItems
			translatedHeader = header
		}

		for _, st := range settings {
			wg.Add(1)
			go func(st models.SubscriberSettings, langItems []models.RankedNewsItem, translatedHeader string) {
				defer wg.Done()
				id := st.ChatID

				// Acquire semaphore slot with ctx awareness.
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				filtered := langItems
				categoryCount := 0
				if cats, err := b.store.GetSubscriberCategories(ctx, id); err != nil {
					b.logger.Warn("bot: get subscriber categories failed",
						slog.Int64("chat_id", id), slog.String("error", err.Error()))
				} else if len(cats) > 0 {
					categoryCount = len(cats)
					filtered = filterTranslatedBySource(langItems, items, cats)
				}
				if len(filtered) == 0 {
					mu.Lock()
					skippedNoMatchCount++
					mu.Unlock()
					b.logger.Info("bot: broadcast skipped subscriber, no matching categories",
						slog.Int64("chat_id", id),
						slog.Int("categories", categoryCount),
					)
					return
				}
				opts := formatter.DigestOptions{TopN: st.DigestTopN, Format: st.DigestFormat}
				var parts []string
				if translatedHeader != "" {
					parts = b.formatter.TopicDigestPartsWithOptions(translatedHeader, filtered, opts)
				} else {
					parts = b.formatter.DigestPartsWithOptions(filtered, date, opts)
				}

				for _, part := range parts {
					// Wait for a rate limiter token (globally serialises sends).
					select {
					case <-rateLimiter.C:
					case <-ctx.Done():
						return
					}

					if err := b.SendRaw(ctx, id, part); err != nil {
						mu.Lock()
						failedCount++
						mu.Unlock()

						// Check if it's a permanent failure (bot blocked us)
						if strings.Contains(err.Error(), "chat not found") ||
							strings.Contains(err.Error(), "bot was blocked") ||
							strings.Contains(err.Error(), "user is deactivated") {
							b.logger.Warn("bot: marking subscriber as inactive", slog.Int64("chat_id", id))
							if unsubErr := b.store.Unsubscribe(ctx, id); unsubErr != nil {
								b.logger.Warn("bot: unsubscribe failed",
									slog.Int64("chat_id", id),
									slog.String("error", unsubErr.Error()),
								)
							}
						}
						return
					}
					mu.Lock()
					sentCount++
					mu.Unlock()
				}
			}(st, langItems, translatedHeader)
		}
	}

	wg.Wait()

	b.logger.Info("bot: broadcast completed",
		slog.Int("total", totalChats),
		slog.Int("sent_messages", sentCount),
		slog.Int("failed", failedCount),
		slog.Int("skipped_no_match", skippedNoMatchCount),
	)
	if err := b.store.SaveBroadcastStats(ctx, models.BroadcastStats{
		RunAt:          time.Now(),
		Recipients:     totalChats,
		SentMessages:   sentCount,
		FailedMessages: failedCount,
		SkippedNoMatch: skippedNoMatchCount,
	}); err != nil {
		b.logger.Warn("bot: save broadcast stats failed", slog.String("error", err.Error()))
	}

	if failedCount > 0 {
		b.logger.Warn("bot: broadcast completed with failures",
			slog.Int("total", totalChats),
			slog.Int("failed", failedCount),
			slog.Int("success", totalChats-failedCount-skippedNoMatchCount),
		)
	}
	if sentCount == 0 && failedCount == 0 && skippedNoMatchCount > 0 {
		return ErrBroadcastNoDeliveries
	}

	return nil
}
