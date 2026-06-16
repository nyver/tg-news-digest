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

func TestGetLastSuccessfulRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// No runs yet
	last, err := s.GetLastSuccessfulRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last)

	// Failed run should not count
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: time.Now(), Status: "failed", Trigger: "cron"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last, "failed run should not be returned")

	// A command run SHOULD count (advances the window to prevent duplicate broadcasts)
	cmdTime := time.Now().Truncate(time.Second)
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: cmdTime, Status: "success", Trigger: "command"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.WithinDuration(t, cmdTime, *last, time.Second)

	// A later cron run should supersede the command run
	cronTime := cmdTime.Add(time.Hour)
	_, err = s.SaveDigestRun(ctx, models.DigestRun{RunAt: cronTime, Status: "success", Trigger: "cron"})
	require.NoError(t, err)

	last, err = s.GetLastSuccessfulRun(ctx)
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
