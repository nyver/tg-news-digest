package bot

import (
	"context"
	"errors"
	"sync"
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
	fmttr := formatter.New(formatter.HTML, 10)

	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, broadcastFn, slog.Default())
	require.NoError(t, err)
	return bot
}

func TestNew_RealAPI(t *testing.T) {
	cfg := config.BotConfig{
		Token: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
	}
	fmttr := formatter.New(formatter.HTML, 10)
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
	fmttr := formatter.New(formatter.HTML, 10)

	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)
	assert.NotNil(t, bot)
}

func TestCmdStart_SendsHelpWithCommandList(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdStart(ctx, 42, msg)

	// /start must show the command list, not just a bare subscription confirmation.
	assert.Contains(t, gotText, "/categories")
	assert.Contains(t, gotText, "/digest")
	assert.Contains(t, gotText, "/language")

	active, err := bot.store.IsActive(ctx, 42)
	require.NoError(t, err)
	assert.True(t, active)
}

func TestCmdSubscribe_SendsConfirmationOnly(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdSubscribe(ctx, 42, msg)

	assert.NotContains(t, gotText, "/categories")
	assert.NotContains(t, gotText, "/digest")

	active, err := bot.store.IsActive(ctx, 42)
	require.NoError(t, err)
	assert.True(t, active)
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

	fmttr := formatter.New(formatter.HTML, 10)
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

	fmttr := formatter.New(formatter.HTML, 10)
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

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{{Rank: 1, Title: "Test", Summary: "S", Link: "https://x.com"}}

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Times(6)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertExpectations(t)
}

func TestBroadcast_CategoryFiltering(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	// Chat 100 wants only AI news; chat 200 has no preference (gets everything).
	require.NoError(t, store.SaveSubscriber(ctx, 100))
	require.NoError(t, store.SaveSubscriber(ctx, 200))
	require.NoError(t, store.AddSubscriberCategory(ctx, 100, "AI"))

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "AI news", Summary: "S", Link: "https://x.com/1", Category: "AI"},
		{Rank: 2, Title: "Sports news", Summary: "S", Link: "https://x.com/2", Category: "Sports"},
	}

	var sentToChat100, sentToChat200 int
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		msg, ok := c.(tgbotapi.MessageConfig)
		if ok && msg.ChatID == 100 {
			sentToChat100++
		}
		if ok && msg.ChatID == 200 {
			sentToChat200++
		}
		return ok
	})).Return(tgbotapi.Message{}, nil)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)

	// Chat 100 (AI only) gets a single part with just the AI item.
	assert.Equal(t, 1, sentToChat100)
	// Chat 200 (no filter) gets the full digest in one part too.
	assert.Equal(t, 1, sentToChat200)
}

func TestBroadcast_CategoryFiltering_NoMatch_SkipsChat(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	require.NoError(t, store.SaveSubscriber(ctx, 100))
	require.NoError(t, store.AddSubscriberCategory(ctx, 100, "Sports"))

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "AI news", Summary: "S", Link: "https://x.com/1", Category: "AI"},
	}

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertNotCalled(t, "Send", mock.Anything)
}

func TestBroadcast_CustomCategoryFiltering_MatchesItemText(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	require.NoError(t, store.SaveSubscriber(ctx, 100))
	require.NoError(t, store.AddSubscriberCategory(ctx, 100, "robotics"))

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Robotics lab launches humanoid platform", Summary: "S", Link: "https://x.com/1", Category: "Other"},
		{Rank: 2, Title: "Language model update", Summary: "S", Link: "https://x.com/2", Category: "AI"},
	}

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		msg, ok := c.(tgbotapi.MessageConfig)
		if ok {
			gotText = msg.Text
		}
		return ok
	})).Return(tgbotapi.Message{}, nil).Once()

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)

	assert.Contains(t, gotText, "Robotics")
	assert.NotContains(t, gotText, "Language model")
}

func TestHandleCallback_TogglesCategory(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	cfg := config.BotConfig{Categories: []string{"AI", "LLM"}}
	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, cfg, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil)

	cq := &tgbotapi.CallbackQuery{
		ID:   "cbid",
		Data: "cat:AI",
		Message: &tgbotapi.Message{
			MessageID: 1,
			Chat:      &tgbotapi.Chat{ID: 42},
		},
	}

	bot.handleCallback(ctx, cq)

	cats, err := store.GetSubscriberCategories(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, []string{"AI"}, cats)

	// Pressing again toggles it off.
	bot.handleCallback(ctx, cq)
	cats, err = store.GetSubscriberCategories(ctx, 42)
	require.NoError(t, err)
	assert.Empty(t, cats)
}

func TestCmdAddAndRemoveCategory(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil)

	addMsg := &tgbotapi.Message{
		MessageID: 1,
		Chat:      &tgbotapi.Chat{ID: 42},
		Text:      "/addcategory AI agents",
		Entities:  []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 12}},
	}
	bot.cmdAddCategory(ctx, 42, addMsg)

	cats, err := bot.store.GetSubscriberCategories(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, []string{"AI agents"}, cats)

	removeMsg := &tgbotapi.Message{
		MessageID: 2,
		Chat:      &tgbotapi.Chat{ID: 42},
		Text:      "/removecategory ai AGENTS",
		Entities:  []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 15}},
	}
	bot.cmdRemoveCategory(ctx, 42, removeMsg)

	cats, err = bot.store.GetSubscriberCategories(ctx, 42)
	require.NoError(t, err)
	assert.Empty(t, cats)
}

func TestCmdCategories_ShowsCustomCategories(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	require.NoError(t, bot.store.AddSubscriberCategory(ctx, 42, "AI agents"))

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdCategories(ctx, 42, msg)

	assert.Contains(t, gotText, "/addcategory")
	assert.Contains(t, gotText, "AI agents")
}

func TestCmdDigestTopic_Success(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	bot.SetTopicDigestFunc(func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error) {
		assert.Equal(t, "нейросети", topic)
		return []models.RankedNewsItem{
			{Rank: 1, Title: "Новость про нейросети", Summary: "S", Link: "https://x.com"},
		}, true, nil
	})

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdDigestTopic(ctx, msg, "нейросети")

	mockAPI.AssertCalled(t, "Send", mock.Anything)
}

func TestCmdDigestTopic_NothingFound(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	bot.SetTopicDigestFunc(func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error) {
		return nil, true, nil
	})

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdDigestTopic(ctx, msg, "космос")

	assert.Contains(t, gotText, "Не нашёл новостей")
}

func TestCmdDigestTopic_NoHandlerConfigured(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdDigestTopic(ctx, msg, "космос")

	assert.Contains(t, gotText, "временно недоступен")
}

func TestCmdLanguage_ShowsCurrent(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}, Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 9}}, Text: "/language"}
	bot.cmdLanguage(ctx, 42, msg)

	assert.Contains(t, gotText, "ru")
}

func TestCmdLanguage_SetsLanguage(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil)

	text := "/language English"
	msg := &tgbotapi.Message{
		MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}, Text: text,
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: 9}},
	}
	bot.cmdLanguage(ctx, 42, msg)

	lang, err := bot.store.GetSubscriberLanguage(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, "English", lang)
}

func TestCmdDigestTopic_TranslatesForChatLanguage(t *testing.T) {
	ctx := context.Background()
	bot := newTestBot(t, nil)
	mockAPI := bot.api.(*mockTGAPI)

	require.NoError(t, bot.store.SaveSubscriber(ctx, 42))
	require.NoError(t, bot.store.SetSubscriberLanguage(ctx, 42, "English"))

	bot.SetTopicDigestFunc(func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error) {
		return []models.RankedNewsItem{{Rank: 1, Title: "Заголовок", Summary: "Описание", Link: "https://x.com"}}, true, nil
	})
	bot.SetTranslateFunc(func(ctx context.Context, items []models.RankedNewsItem, header, lang string) ([]models.RankedNewsItem, string, error) {
		assert.Equal(t, "English", lang)
		translated := make([]models.RankedNewsItem, len(items))
		copy(translated, items)
		translated[0].Title = "Translated title"
		return translated, "Translated header", nil
	})

	var gotText string
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			gotText = m.Text
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	msg := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 42}}
	bot.cmdDigestTopic(ctx, msg, "ai")

	assert.Contains(t, gotText, "Translated title")
}

func TestBroadcast_TranslatesPerLanguageGroup(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	require.NoError(t, store.SaveSubscriber(ctx, 100)) // ru (default)
	require.NoError(t, store.SaveSubscriber(ctx, 200))
	require.NoError(t, store.SetSubscriberLanguage(ctx, 200, "English"))

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	bot.SetTranslateFunc(func(ctx context.Context, items []models.RankedNewsItem, header, lang string) ([]models.RankedNewsItem, string, error) {
		assert.Equal(t, "English", lang)
		translated := make([]models.RankedNewsItem, len(items))
		copy(translated, items)
		translated[0].Title = "Translated"
		return translated, "Translated header", nil
	})

	items := []models.RankedNewsItem{{Rank: 1, Title: "Оригинал", Summary: "S", Link: "https://x.com"}}

	var textByChat = make(map[int64]string)
	var mu sync.Mutex
	mockAPI.On("Send", mock.MatchedBy(func(c tgbotapi.Chattable) bool {
		if m, ok := c.(tgbotapi.MessageConfig); ok {
			mu.Lock()
			textByChat[m.ChatID] = m.Text
			mu.Unlock()
			return true
		}
		return false
	})).Return(tgbotapi.Message{}, nil)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)

	assert.Contains(t, textByChat[100], "Оригинал")
	assert.NotContains(t, textByChat[100], "Translated")
	assert.Contains(t, textByChat[200], "Translated")
}

func TestBroadcast_ConcurrentSafe(t *testing.T) {
	ctx := context.Background()
	mockAPI := new(mockTGAPI)
	store := newTestStore(t)

	for i := int64(100); i <= 109; i++ {
		err := store.SaveSubscriber(ctx, i)
		require.NoError(t, err)
	}

	fmttr := formatter.New(formatter.HTML, 10)
	bot, err := NewWithAPI(mockAPI, config.BotConfig{}, fmttr, store, nil, slog.Default())
	require.NoError(t, err)

	items := []models.RankedNewsItem{{Rank: 1, Title: "Test", Summary: "S", Link: "https://x.com"}}

	mockAPI.On("Send", mock.Anything).Return(tgbotapi.Message{}, nil).Times(10)

	err = bot.Broadcast(ctx, items, time.Now())
	require.NoError(t, err)
	mockAPI.AssertExpectations(t)
}
