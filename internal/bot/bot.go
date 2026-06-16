package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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

// TopicDigestFunc selects and summarizes news relevant to a free-text topic
// requested via "/digest <тема>". The returned slice may legitimately be
// empty (nothing relevant found) without that being an error.
type TopicDigestFunc func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error)

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
}

// SetTopicDigestFunc wires the callback used to answer "/digest <тема>"
// requests. Must be called after construction since the callback typically
// closes over the bot itself (e.g. to access store/llm client built in main).
func (b *Bot) SetTopicDigestFunc(fn TopicDigestFunc) {
	b.topicFn = fn
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
	case "start", "subscribe":
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
	default:
		b.reply(ctx, msg, formatter.UnknownCommandMessage(formatter.ParseMode(b.cfg.ParseMode)))
	}
}

// cmdSubscribe handles /start and /subscribe.
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

	for _, part := range b.formatter.TopicDigestParts(header, ranked) {
		if err := b.SendRaw(ctx, chatID, part); err != nil {
			b.logger.Warn("bot: send topic digest part failed",
				slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
			return
		}
	}
}

// cmdCategories shows the category selection keyboard for the chat.
func (b *Bot) cmdCategories(ctx context.Context, chatID int64, msg *tgbotapi.Message) {
	selected, err := b.store.GetSubscriberCategories(ctx, chatID)
	if err != nil {
		b.logger.Warn("bot: get subscriber categories failed",
			slog.Int64("chat_id", chatID), slog.String("error", err.Error()))
	}

	cfg := tgbotapi.NewMessage(chatID,
		"📂 Выберите интересующие категории новостей (нажмите, чтобы включить/выключить).\nЕсли ничего не выбрано — вы получаете дайджест по всем темам.")
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
			tgbotapi.NewInlineKeyboardButtonData(label, "cat:"+cat),
		))
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// handleCallback processes inline keyboard button presses (currently only
// category toggles from /categories).
func (b *Bot) handleCallback(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil {
		return
	}

	category, ok := strings.CutPrefix(cq.Data, "cat:")
	if !ok {
		return
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
// (case-insensitive). Items with no category never match a non-empty filter.
func filterByCategories(items []models.RankedNewsItem, cats []string) []models.RankedNewsItem {
	wanted := make(map[string]bool, len(cats))
	for _, c := range cats {
		wanted[strings.ToLower(c)] = true
	}

	filtered := make([]models.RankedNewsItem, 0, len(items))
	for _, item := range items {
		if item.Category != "" && wanted[strings.ToLower(item.Category)] {
			filtered = append(filtered, item)
		}
	}
	return filtered
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

	b.logger.Info("bot: broadcast starting",
		slog.Int("items", len(items)),
		slog.Time("date", date),
	)

	chats, err := b.store.GetActiveChats(ctx)
	if err != nil {
		return fmt.Errorf("bot: get active chats: %w", err)
	}

	b.logger.Info("bot: broadcast subscribers", slog.Int("subscribers", len(chats)))

	if len(chats) == 0 {
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

	for _, chatID := range chats {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()

			// Acquire semaphore slot with ctx awareness.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			filtered := items
			if cats, err := b.store.GetSubscriberCategories(ctx, id); err != nil {
				b.logger.Warn("bot: get subscriber categories failed",
					slog.Int64("chat_id", id), slog.String("error", err.Error()))
			} else if len(cats) > 0 {
				filtered = filterByCategories(items, cats)
			}
			if len(filtered) == 0 {
				return
			}
			parts := b.formatter.DigestParts(filtered, date)

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
			}
		}(chatID)
	}

	wg.Wait()

	if failedCount > 0 {
		b.logger.Warn("bot: broadcast completed with failures",
			slog.Int("total", len(chats)),
			slog.Int("failed", failedCount),
			slog.Int("success", len(chats)-failedCount),
		)
	}

	return nil
}
