package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"log/slog"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/formatter"
	"github.com/nyver/tg-news-digest/internal/models"
	"github.com/nyver/tg-news-digest/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// --- Mock TGAPI ---

// mockTGAPI is a mock implementation of the TGAPI interface.
type mockTGAPI struct {
	mock.Mock
}

func (m *mockTGAPI) Send(config tgbotapi.Chattable) (tgbotapi.Message, error) {
	args := m.Called(config)
	if args.Get(0) == nil {
		return tgbotapi.Message{}, args.Error(1)
	}
	return args.Get(0).(tgbotapi.Message), args.Error(1)
}

func (m *mockTGAPI) GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	ch := make(chan tgbotapi.Update, 1)
	go func() {
		// Block forever so the mock can be used for Send-only tests
		select {}
	}()
	return ch
}

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.New(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestBot(t *testing.T, broadcastFn BroadcastFunc) *Bot {
	t.Helper()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)
	fmttr := formatter.New(formatter.HTML)

	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, broadcastFn, slog.Default())
	require.NoError(t, err)
	return bot
}

func TestNew_RealAPI(t *testing.T) {
	cfg := config.BotConfig{
		Token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
	}
	fmttr := formatter.New(formatter.HTML)
	store := newTestStore(t)

	_, err := New(cfg, fmttr, store, nil, slog.Default())
	// We expect an error because the token is fake, but the constructor should
	// attempt to connect to the Telegram API. We don't care about the specific
	// error here, just that New() doesn't panic.
	_ = err
}

func TestNewWithAPI(t *testing.T) {
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)
	fmttr := formatter.New(formatter.HTML)

	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)
	assert.NotNil(t, bot)
}

func TestBroadcast_NoSubscribers(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)

	err := bot.Broadcast(ctx, nil, time.Now())
	assert.NoError(t, err)
}

func TestBroadcast_EmptyItems(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)

	err := bot.Broadcast(ctx, []models.RankedNewsItem{}, time.Now())
	assert.NoError(t, err)
}

func TestBroadcast_Success(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	err := store.SaveSubscriber(ctx, 100)
	require.NoError(t, err)
	err = store.SaveSubscriber(ctx, 200)
	require.NoError(t, err)

	fmttr := formatter.New(formatter.HTML)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Test", Summary: "Summary", Link: "https://example.com"},
	}

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Twice()

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertExpectations(t)
}

func TestBroadcast_FailedSend_AutoUnsubscribe(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	err := store.SaveSubscriber(ctx, 100)
	require.NoError(t, err)
	err = store.SaveSubscriber(ctx, 200)
	require.NoError(t, err)

	fmttr := formatter.New(formatter.HTML)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Test", Summary: "Summary", Link: "https://example.com"},
	}

	// Use MatchedBy to make the failure deterministic: chat 100 always fails,
	// regardless of goroutine execution order.
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if msg, ok := c.(tgbotapi.MessageConfig); ok {
			return msg.ChatID == 100
		}
		return false
	})).Return(tgbotapi.Message{}, errors.New("telegram: bot was blocked by the user")).Once()
	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Once()

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)

	// Verify chat 100 was unsubscribed (the one that got blocked)
	time.Sleep(50 * time.Millisecond)
	active, err := store.IsActive(ctx, 100)
	require.NoError(t, err)
	assert.False(t, active)

	mockAPI.AssertExpectations(t)
}

func TestBroadcast_RateLimit(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	for i := int64(100); i <= 105; i++ {
		err := store.SaveSubscriber(ctx, i)
		require.NoError(t, err)
	}

	fmttr := formatter.New(formatter.HTML)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{{Rank: 1, Title: "Test", Summary: "S", Link: "https://x.com"}}

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Times(6)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertExpectations(t)
}

func TestBroadcast_ConcurrentSafe(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	for i := int64(100); i <= 109; i++ {
		err := store.SaveSubscriber(ctx, i)
		require.NoError(t, err)
	}

	fmttr := formatter.New(formatter.HTML)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{{Rank: 1, Title: "Test", Summary: "S", Link: "https://x.com"}}

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Times(10)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertExpectations(t)
}
