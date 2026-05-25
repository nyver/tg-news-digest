package rss

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
	"golang.org/x/sync/errgroup"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/models"
	"github.com/nyver/tg-news-digest/internal/storage"
)

// Fetcher handles RSS feed collection and preprocessing.
type Fetcher struct {
	cfg    config.RSSConfig
	store  *storage.Store
	parser *gofeed.Parser
}

// New creates a new Fetcher.
func New(cfg config.RSSConfig, store *storage.Store) *Fetcher {
	return &Fetcher{
		cfg:    cfg,
		store:  store,
		parser: gofeed.NewParser(),
	}
}

// FetchResult contains the results of an RSS fetch cycle.
type FetchResult struct {
	Items    []models.NewsItem
	FeedsOK  int
	FeedsErr int
}

// FetchAll fetches all configured RSS feeds in parallel and returns deduplicated news items.
func (f *Fetcher) FetchAll(ctx context.Context) (*FetchResult, error) {
	result := &FetchResult{}

	if len(f.cfg.Feeds) == 0 {
		return result, nil
	}

	// Fetch feeds in parallel
	type feedResult struct {
		items []models.NewsItem
		url   string
		err   error
	}

	ch := make(chan feedResult, len(f.cfg.Feeds))
	grp, ctx := errgroup.WithContext(ctx)
	var wg sync.WaitGroup
	wg.Add(len(f.cfg.Feeds))

	for _, feedURL := range f.cfg.Feeds {
		feedURL := feedURL
		grp.Go(func() error {
			defer wg.Done()
			items, err := f.fetchSingle(ctx, feedURL)
			ch <- feedResult{items: items, url: feedURL, err: err}
			return err
		})
	}

	if err := grp.Wait(); err != nil {
		return nil, fmt.Errorf("rss: fetch group: %w", err)
	}

	// Close channel after all goroutines complete so range exits
	go func() {
		wg.Wait()
		close(ch)
	}()

	for fr := range ch {
		if fr.err != nil {
			result.FeedsErr++
			continue
		}
		result.FeedsOK++
		result.Items = append(result.Items, fr.items...)
	}

	return result, nil
}

// fetchSingle fetches a single RSS feed with retry logic.
func (f *Fetcher) fetchSingle(ctx context.Context, feedURL string) ([]models.NewsItem, error) {
	// Use http.Client with timeout for this feed
	httpClient := &http.Client{
		Timeout: f.cfg.FetchTimeout,
	}
	parser := gofeed.NewParser()
	parser.Client = httpClient

	feed, err := parser.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return nil, fmt.Errorf("rss: parse feed %s: %w", feedURL, err)
	}

	now := time.Now()
	seen := make(map[string]bool)
	var items []models.NewsItem

	for _, entry := range feed.Items {
		if len(items) >= f.cfg.MaxItemsPerFeed {
			break
		}

		// Skip items older than 24 hours
		pubDate := entry.PublishedParsed
		if pubDate == nil {
			continue
		}
		if now.Sub(*pubDate) > 24*time.Hour {
			continue
		}

		// Dedup by URL + title hash
		hash := itemHash(entry.Title, entry.Link)
		if seen[hash] {
			continue
		}
		seen[hash] = true

		// Check storage dedup
		exists, err := f.store.ItemExists(ctx, hash)
		if err != nil {
			continue
		}
		if exists {
			continue
		}

		// Truncate description for LLM context budget
		desc := entry.Description
		if desc == "" && entry.Content != "" {
			desc = entry.Content
		}
		desc = truncate(desc, 300)

		item := models.NewsItem{
			ID:          hash,
			Title:       entry.Title,
			Description: desc,
			Link:        entry.Link,
			PublishedAt: *pubDate,
			FeedURL:     feedURL,
		}
		items = append(items, item)
	}

	return items, nil
}

// SaveAndCleanup persists fetched items and removes expired ones.
func (f *Fetcher) SaveAndCleanup(ctx context.Context, items []models.NewsItem) error {
	if len(items) > 0 {
		if err := f.store.SaveItems(ctx, items, f.cfg.CacheTTL); err != nil {
			return fmt.Errorf("rss: save items: %w", err)
		}
	}

	_, err := f.store.CleanupExpired(ctx)
	return err
}

// itemHash generates a SHA256 hash for deduplication.
func itemHash(title, link string) string {
	h := sha256.New()
	h.Write([]byte(title + "|" + link))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// truncate limits a string to max bytes, adding ellipsis if truncated.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
