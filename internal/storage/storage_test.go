package storage

import (
	"context"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := New(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndGetSubscriber(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Initially not active
	active, err := s.IsActive(ctx, 12345)
	require.NoError(t, err)
	assert.False(t, active)

	// Save
	err = s.SaveSubscriber(ctx, 12345)
	require.NoError(t, err)

	// Check
	active, err = s.IsActive(ctx, 12345)
	require.NoError(t, err)
	assert.True(t, active)

	// Unsubscribe
	err = s.Unsubscribe(ctx, 12345)
	require.NoError(t, err)

	active, err = s.IsActive(ctx, 12345)
	require.NoError(t, err)
	assert.False(t, active)
}

func TestGetActiveChats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.SaveSubscriber(ctx, 100)
	require.NoError(t, err)
	err = s.SaveSubscriber(ctx, 200)
	require.NoError(t, err)
	err = s.Unsubscribe(ctx, 200)
	require.NoError(t, err)

	chats, err := s.GetActiveChats(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{100}, chats)
}

func TestFetchedItems(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	item := models.NewsItem{
		ID:          "abc123",
		Title:       "Test News",
		Description: "Test description",
		Link:        "https://example.com/news",
		PublishedAt: time.Now(),
		FeedURL:     "https://example.com/feed.xml",
	}

	// Should not exist initially
	exists, err := s.ItemExists(ctx, "abc123")
	require.NoError(t, err)
	assert.False(t, exists)

	// Save items
	err = s.SaveItems(ctx, []models.NewsItem{item}, 24*time.Hour)
	require.NoError(t, err)

	// Should exist now
	exists, err = s.ItemExists(ctx, "abc123")
	require.NoError(t, err)
	assert.True(t, exists)

	// Count
	count, err := s.CountActiveItems(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestFetchedItems_SubHourTTLDoesNotExpireImmediately(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	item := models.NewsItem{
		ID:          "short-ttl",
		Title:       "Short TTL",
		Description: "D",
		Link:        "https://example.com/short",
		PublishedAt: time.Now(),
		FeedURL:     "https://example.com/feed.xml",
	}

	require.NoError(t, s.SaveItems(ctx, []models.NewsItem{item}, 30*time.Minute))

	exists, err := s.ItemExists(ctx, "short-ttl")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestFetchedItems_Dedup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	item := models.NewsItem{
		ID:          "dedup-test",
		Title:       "Test",
		Description: "Desc",
		Link:        "https://example.com",
		PublishedAt: time.Now(),
		FeedURL:     "https://example.com/feed.xml",
	}

	err := s.SaveItems(ctx, []models.NewsItem{item}, 24*time.Hour)
	require.NoError(t, err)
	err = s.SaveItems(ctx, []models.NewsItem{item}, 24*time.Hour)
	require.NoError(t, err)

	count, err := s.CountActiveItems(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count) // Only one inserted (INSERT OR IGNORE)
}

func TestDigestRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	run := models.DigestRun{
		RunAt:     time.Now(),
		Status:    "success",
		Trigger:   "cron",
		ItemCount: 10,
		LLMUsed:   true,
	}

	runID, err := s.SaveDigestRun(ctx, run)
	require.NoError(t, err)
	assert.Positive(t, runID)

	last, err := s.GetLastRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.Equal(t, "success", last.Status)
	assert.Equal(t, "cron", last.Trigger)
	assert.Equal(t, 10, last.ItemCount)
	assert.True(t, last.LLMUsed)
}

func TestGetLastSuccessfulCronRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// No runs yet
	last, err := s.GetLastSuccessfulCronRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last)

	// Failed run should not count
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: time.Now(), Status: "failed", Trigger: "cron"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulCronRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last, "failed run should not be returned")

	// A command run should not advance the scheduled digest window.
	cmdTime := time.Now().Truncate(time.Second)
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: cmdTime, Status: "success", Trigger: "command"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulCronRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last, "command run should not be returned")

	// A later cron run should be returned.
	cronTime := cmdTime.Add(time.Hour)
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: cronTime, Status: "success", Trigger: "cron"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulCronRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.WithinDuration(t, cronTime, *last, time.Second)
}

func TestGetLastRun_NoRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	last, err := s.GetLastRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last)
}

func TestCleanupOldFetchedItems(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := models.NewsItem{
		ID: "old", Title: "Old item", Description: "D", Link: "https://example.com/old",
		PublishedAt: time.Now().Add(-5 * 24 * time.Hour), FeedURL: "https://example.com/feed.xml",
	}
	recent := models.NewsItem{
		ID: "recent", Title: "Recent item", Description: "D", Link: "https://example.com/recent",
		PublishedAt: time.Now().Add(-1 * time.Hour), FeedURL: "https://example.com/feed.xml",
	}
	// Long TTL so CleanupExpired (the existing expires_at-based cleanup) does
	// not also remove these — we want to isolate CleanupOldFetchedItems.
	require.NoError(t, s.SaveItems(ctx, []models.NewsItem{old, recent}, 30*24*time.Hour))

	n, err := s.CleanupOldFetchedItems(ctx, 3*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	exists, err := s.ItemExists(ctx, "old")
	require.NoError(t, err)
	assert.False(t, exists)

	exists, err = s.ItemExists(ctx, "recent")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestCleanupOldFetchedItems_NothingToRemove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	item := models.NewsItem{
		ID: "recent", Title: "Recent item", Description: "D", Link: "https://example.com/recent",
		PublishedAt: time.Now(), FeedURL: "https://example.com/feed.xml",
	}
	require.NoError(t, s.SaveItems(ctx, []models.NewsItem{item}, 24*time.Hour))

	n, err := s.CleanupOldFetchedItems(ctx, 3*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGetRecentItems(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	older := models.NewsItem{
		ID: "old", Title: "Old item", Description: "D", Link: "https://example.com/old",
		PublishedAt: time.Now().Add(-2 * time.Hour), FeedURL: "https://example.com/feed.xml",
	}
	newer := models.NewsItem{
		ID: "new", Title: "New item", Description: "D", Link: "https://example.com/new",
		PublishedAt: time.Now().Add(-1 * time.Minute), FeedURL: "https://example.com/feed.xml",
	}

	require.NoError(t, s.SaveItems(ctx, []models.NewsItem{older, newer}, 24*time.Hour))

	items, err := s.GetRecentItems(ctx, 0)
	require.NoError(t, err)
	require.Len(t, items, 2)
	// Most recent first.
	assert.Equal(t, "new", items[0].ID)
	assert.Equal(t, "old", items[1].ID)
}

func TestGetRecentItems_ExcludesExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	expired := models.NewsItem{
		ID: "expired", Title: "Expired", Description: "D", Link: "https://example.com/expired",
		PublishedAt: time.Now(), FeedURL: "https://example.com/feed.xml",
	}
	require.NoError(t, s.SaveItems(ctx, []models.NewsItem{expired}, time.Hour))

	// Force the item into the past so it is treated as expired.
	_, err := s.DB().ExecContext(ctx,
		`UPDATE fetched_items SET expires_at = datetime('now', '-1 hour') WHERE id = ?`, "expired")
	require.NoError(t, err)

	items, err := s.GetRecentItems(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestSubscriberCategories_DefaultEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cats, err := s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.Empty(t, cats)
}

func TestSubscriberCategories_AddRemove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.AddSubscriberCategory(ctx, 555, "AI"))
	require.NoError(t, s.AddSubscriberCategory(ctx, 555, "IT"))

	cats, err := s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"AI", "IT"}, cats)

	// Re-adding the same category is idempotent.
	require.NoError(t, s.AddSubscriberCategory(ctx, 555, "AI"))
	cats, err = s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"AI", "IT"}, cats)

	require.NoError(t, s.RemoveSubscriberCategory(ctx, 555, "AI"))
	cats, err = s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.Equal(t, []string{"IT"}, cats)
}

func TestSubscriberCategories_NormalizesCaseInsensitiveDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.AddSubscriberCategory(ctx, 555, " AI agents "))
	require.NoError(t, s.AddSubscriberCategory(ctx, 555, "ai agents"))

	cats, err := s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.Equal(t, []string{"AI agents"}, cats)

	require.NoError(t, s.RemoveSubscriberCategory(ctx, 555, "AI AGENTS"))
	cats, err = s.GetSubscriberCategories(ctx, 555)
	require.NoError(t, err)
	assert.Empty(t, cats)
}

func TestSubscriberCategories_ScopedPerChat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.AddSubscriberCategory(ctx, 1, "AI"))
	require.NoError(t, s.AddSubscriberCategory(ctx, 2, "LLM"))

	cats1, err := s.GetSubscriberCategories(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"AI"}, cats1)

	cats2, err := s.GetSubscriberCategories(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"LLM"}, cats2)
}

func TestSubscriberLanguage_DefaultsToRussian(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Chat has never subscribed.
	lang, err := s.GetSubscriberLanguage(ctx, 999)
	require.NoError(t, err)
	assert.Equal(t, "ru", lang)

	// Subscribed but never set a language preference.
	require.NoError(t, s.SaveSubscriber(ctx, 100))
	lang, err = s.GetSubscriberLanguage(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, "ru", lang)
}

func TestSubscriberLanguage_SetAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 100))
	require.NoError(t, s.SetSubscriberLanguage(ctx, 100, "English"))

	lang, err := s.GetSubscriberLanguage(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, "English", lang)
}

func TestSubscriberLanguage_SurvivesResubscribe(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 100))
	require.NoError(t, s.SetSubscriberLanguage(ctx, 100, "English"))
	require.NoError(t, s.Unsubscribe(ctx, 100))
	require.NoError(t, s.SaveSubscriber(ctx, 100)) // re-subscribe

	lang, err := s.GetSubscriberLanguage(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, "English", lang)
}

func TestGetActiveChatsByLanguage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 1))
	require.NoError(t, s.SaveSubscriber(ctx, 2))
	require.NoError(t, s.SaveSubscriber(ctx, 3))
	require.NoError(t, s.SetSubscriberLanguage(ctx, 2, "English"))
	require.NoError(t, s.SetSubscriberLanguage(ctx, 3, "English"))
	// Inactive subscriber should not show up in any group.
	require.NoError(t, s.SaveSubscriber(ctx, 4))
	require.NoError(t, s.SetSubscriberLanguage(ctx, 4, "English"))
	require.NoError(t, s.Unsubscribe(ctx, 4))

	groups, err := s.GetActiveChatsByLanguage(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{1}, groups["ru"])
	assert.ElementsMatch(t, []int64{2, 3}, groups["English"])
	assert.NotContains(t, groups["English"], int64(4))
}

func TestDashboardStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 1))
	require.NoError(t, s.SaveSubscriber(ctx, 2))
	require.NoError(t, s.Unsubscribe(ctx, 2))
	require.NoError(t, s.AddSubscriberCategory(ctx, 1, "AI"))
	require.NoError(t, s.AddSubscriberCategory(ctx, 2, "AI"))
	require.NoError(t, s.AddSubscriberCategory(ctx, 1, "ML"))

	active, total, err := s.CountSubscribers(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, active)
	assert.Equal(t, 2, total)

	cats, err := s.GetPopularCategories(ctx, 10)
	require.NoError(t, err)
	require.Len(t, cats, 2)
	assert.Equal(t, "AI", cats[0].Category)
	assert.Equal(t, 2, cats[0].Count)

	require.NoError(t, s.SaveRSSError(ctx, "https://example.com/feed.xml", "timeout"))
	rssErrors, err := s.GetRecentRSSErrors(ctx, 10)
	require.NoError(t, err)
	require.Len(t, rssErrors, 1)
	assert.Equal(t, "timeout", rssErrors[0].Error)

	require.NoError(t, s.SaveBroadcastStats(ctx, models.BroadcastStats{
		RunAt:          time.Now(),
		Recipients:     2,
		SentMessages:   1,
		FailedMessages: 1,
		SkippedNoMatch: 0,
	}))
	broadcasts, err := s.GetRecentBroadcastStats(ctx, 10)
	require.NoError(t, err)
	require.Len(t, broadcasts, 1)
	assert.Equal(t, 1, broadcasts[0].SentMessages)
	assert.Equal(t, 1, broadcasts[0].FailedMessages)
}

func TestSubscriberSettings_DefaultsAndSetters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	st, err := s.GetSubscriberSettings(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, int64(42), st.ChatID)
	assert.Equal(t, "ru", st.Language)
	assert.Equal(t, "Europe/Moscow", st.Timezone)
	assert.Equal(t, "09:00", st.DeliveryTime)
	assert.Equal(t, 10, st.DigestTopN)
	assert.Equal(t, "detailed", st.DigestFormat)
	assert.False(t, st.QuietWeekends)

	require.NoError(t, s.SetSubscriberLanguage(ctx, 42, "English"))
	require.NoError(t, s.SetSubscriberTimezone(ctx, 42, "UTC"))
	require.NoError(t, s.SetSubscriberDeliveryTime(ctx, 42, "20:00"))
	require.NoError(t, s.SetSubscriberDigestTopN(ctx, 42, 20))
	require.NoError(t, s.SetSubscriberDigestFormat(ctx, 42, "executive"))
	require.NoError(t, s.SetSubscriberQuietWeekends(ctx, 42, true))

	st, err = s.GetSubscriberSettings(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, "English", st.Language)
	assert.Equal(t, "UTC", st.Timezone)
	assert.Equal(t, "20:00", st.DeliveryTime)
	assert.Equal(t, 20, st.DigestTopN)
	assert.Equal(t, "executive", st.DigestFormat)
	assert.True(t, st.QuietWeekends)

	require.NoError(t, s.SetSubscriberDigestFormat(ctx, 42, "short"))
	st, err = s.GetSubscriberSettings(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, "brief", st.DigestFormat)
}

func TestGetDueSubscriberSettings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 1))
	require.NoError(t, s.SetSubscriberTimezone(ctx, 1, "UTC"))
	require.NoError(t, s.SetSubscriberDeliveryTime(ctx, 1, "09:00"))

	require.NoError(t, s.SaveSubscriber(ctx, 2))
	require.NoError(t, s.SetSubscriberTimezone(ctx, 2, "UTC"))
	require.NoError(t, s.SetSubscriberDeliveryTime(ctx, 2, "10:00"))

	now := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	due, err := s.GetDueSubscriberSettings(ctx, now)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, int64(1), due[0].ChatID)

	later := time.Date(2026, 6, 18, 9, 30, 0, 0, time.UTC)
	due, err = s.GetDueSubscriberSettings(ctx, later)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, int64(1), due[0].ChatID)

	require.NoError(t, s.MarkSubscriberDigestSent(ctx, 1, "2026-06-18"))
	due, err = s.GetDueSubscriberSettings(ctx, now)
	require.NoError(t, err)
	assert.Empty(t, due)
}

func TestGetDueSubscriberSettings_QuietWeekends(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SaveSubscriber(ctx, 1))
	require.NoError(t, s.SetSubscriberTimezone(ctx, 1, "UTC"))
	require.NoError(t, s.SetSubscriberDeliveryTime(ctx, 1, "09:00"))
	require.NoError(t, s.SetSubscriberQuietWeekends(ctx, 1, true))

	saturday := time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC)
	due, err := s.GetDueSubscriberSettings(ctx, saturday)
	require.NoError(t, err)
	assert.Empty(t, due)
}

func TestUpsertSubscriberPreferenceRejectsUnknownColumn(t *testing.T) {
	store := newTestStore(t)
	err := store.upsertSubscriberPreference(context.Background(), 1, "language = 'x', active", "y")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported subscriber preference column")
}
