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
	if update.Message == nil {
		return
	}

	msg := update.Message
	b.logger.Info("bot: incoming message",
		slog.Int64("chat_id", msg.Chat.ID),
		slog.Int64("from_id", msg.From.ID),
		slog.String("from_username", msg.From.UserName),
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
		if chatID != b.ownerID {
			b.reply(ctx, msg, "❌ Только для владельца бота.")
			return
		}
		b.cmdDigest(ctx, msg)
	case "status":
		b.cmdStatus(ctx, chatID, msg)
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

// cmdDigest triggers a manual digest run (owner only).
func (b *Bot) cmdDigest(ctx context.Context, msg *tgbotapi.Message) {
	b.reply(ctx, msg, "⏳ Генерация дайджеста...")

	err := b.broadcastFn(ctx)
	if err != nil {
		b.reply(ctx, msg, fmt.Sprintf("❌ Ошибка генерации: %v", err))
		return
	}
	b.reply(ctx, msg, "✅ Дайджест успешно отправлен подписчикам.")
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

	text := b.formatter.Digest(items, date)

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

			// Wait for a rate limiter token (globally serialises sends).
			select {
			case <-rateLimiter.C:
			case <-ctx.Done():
				return
			}

			if err := b.SendRaw(ctx, id, text); err != nil {
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
