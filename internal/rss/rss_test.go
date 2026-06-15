package rss

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oldNewsXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <item>
      <title>Old News</title>
      <description>This is old</description>
      <link>https://example.com/old</link>
      <pubDate>Mon, 01 Jan 2025 10:00:00 +0000</pubDate>
    </item>
  </channel>
</rss>`

func testFeedXML() string {
	now := time.Now().UTC()
	recent1 := now.Add(-2 * time.Hour).Format(time.RFC1123Z)
	recent2 := now.Add(-4 * time.Hour).Format(time.RFC1123Z)
	old := "Mon, 01 Jan 2025 10:00:00 +0000"
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <item>
      <title>News One</title>
      <description>Description of news one</description>
      <link>https://example.com/news-one</link>
      <pubDate>%s</pubDate>
    </item>
    <item>
      <title>News Two</title>
      <description>Description of news two</description>
      <link>https://example.com/news-two</link>
      <pubDate>%s</pubDate>
    </item>
    <item>
      <title>Old News</title>
      <description>This is old</description>
      <link>https://example.com/old</link>
      <pubDate>%s</pubDate>
    </item>
  </channel>
</rss>`, recent1, recent2, old)
}

func testServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(testFeedXML()))
	}))
}

func newTestFetcherWithURL(t *testing.T, feedURL string) *Fetcher {
	t.Helper()
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	cfg := config.RSSConfig{
		Feeds:           []string{feedURL},
		MaxItemsPerFeed: 50,
		FetchTimeout:    10 * time.Second,
		CacheTTL:        24 * time.Hour,
	}
	return New(cfg, store)
}

func TestFetchSingle_Success(t *testing.T) {
	server := testServer()
	defer server.Close()

	f := newTestFetcherWithURL(t, server.URL)
	since := time.Now().Add(-24 * time.Hour)

	items, err := f.fetchSingle(context.Background(), server.URL, since)
	require.NoError(t, err)
	assert.Len(t, items, 2) // Old news should be filtered out
}

func TestFetchSingle_OldItemFiltered(t *testing.T) {
	server := testServer()
	defer server.Close()

	f := newTestFetcherWithURL(t, server.URL)
	since := time.Now().Add(-24 * time.Hour)

	items, err := f.fetchSingle(context.Background(), server.URL, since)
	require.NoError(t, err)

	for _, item := range items {
		assert.NotEqual(t, "Old News", item.Title)
	}
}

func TestFetchAll_MultipleFeeds(t *testing.T) {
	server := testServer()
	defer server.Close()

	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.RSSConfig{
		Feeds:           []string{server.URL, server.URL},
		MaxItemsPerFeed: 50,
		FetchTimeout:    10 * time.Second,
		CacheTTL:        24 * time.Hour,
	}
	f := New(cfg, store)

	since := time.Now().Add(-24 * time.Hour)
	result, err := f.FetchAll(ctx, since)
	require.NoError(t, err)
	// Both feeds return 2 items, but cross-feed dedup removes duplicates
	assert.GreaterOrEqual(t, result.FeedsOK, 1)
	assert.GreaterOrEqual(t, len(result.Items), 2)
}

func TestItemHash_Deterministic(t *testing.T) {
	h1 := itemHash("Title", "https://example.com")
	h2 := itemHash("Title", "https://example.com")
	assert.Equal(t, h1, h2)

	h3 := itemHash("Different Title", "https://example.com")
	assert.NotEqual(t, h1, h3)
}

func TestTruncate(t *testing.T) {
	short := "Hello"
	assert.Equal(t, short, truncate(short, 100))

	long := "This is a very long string that should be truncated"
	result := truncate(long, 20)
	// 20 runes + ellipsis = 21 runes
	assert.Len(t, []rune(result), 21)
}

// TestFetchAll_SinceFiltering verifies that the since parameter controls the fetch window.
func TestFetchAll_SinceFiltering(t *testing.T) {
	server := testServer()
	defer server.Close()

	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.RSSConfig{
		Feeds:           []string{server.URL},
		MaxItemsPerFeed: 50,
		FetchTimeout:    10 * time.Second,
		CacheTTL:        24 * time.Hour,
	}
	f := New(cfg, store)

	// since = 24h ago: should get both recent items
	result1, err := f.FetchAll(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, result1.Items, 2)

	// since = 3h ago: only item published 2h ago should match (not the 4h one)
	result2, err := f.FetchAll(ctx, time.Now().Add(-3*time.Hour))
	require.NoError(t, err)
	assert.Len(t, result2.Items, 1)
}

// TestFetchLocalFile verifies that a local RSS XML file can be loaded.
func TestFetchLocalFile(t *testing.T) {
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "feed.xml")
	err := os.WriteFile(localPath, []byte(testFeedXML()), 0644)
	require.NoError(t, err)

	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.RSSConfig{
		Feeds:           []string{localPath},
		MaxItemsPerFeed: 50,
		FetchTimeout:    10 * time.Second,
		CacheTTL:        24 * time.Hour,
	}
	f := New(cfg, store)

	items, err := f.fetchSingle(ctx, localPath, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, items, 2) // Old news filtered out

	// Check that FeedURL is set to the local path
	for _, item := range items {
		assert.Equal(t, localPath, item.FeedURL)
	}
}

// TestFetchLocalFile_NotFound verifies error handling for missing files.
func TestFetchLocalFile_NotFound(t *testing.T) {
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	require.NoError(t, err)
	defer store.Close()

	cfg := config.RSSConfig{
		Feeds:           []string{"/no/such/path/feed.xml"},
		MaxItemsPerFeed: 50,
		FetchTimeout:    10 * time.Second,
		CacheTTL:        24 * time.Hour,
	}
	f := New(cfg, store)

	_, err = f.fetchSingle(ctx, "/no/such/path/feed.xml", time.Now().Add(-24*time.Hour))
	assert.Error(t, err)
}
