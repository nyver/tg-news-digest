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
		ItemCount: 10,
		LLMUsed:   true,
	}

	err := s.SaveDigestRun(ctx, run)
	require.NoError(t, err)

	last, err := s.GetLastRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, last)
	assert.Equal(t, "success", last.Status)
	assert.Equal(t, 10, last.ItemCount)
	assert.True(t, last.LLMUsed)
}

func TestGetLastRun_NoRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	last, err := s.GetLastRun(ctx)
	require.NoError(t, err)
	assert.Nil(t, last)
}
