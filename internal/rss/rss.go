package rss

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
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

// FetchAll fetches all configured RSS feeds in parallel and returns news items published
// after since. Items are deduplicated within the run by URL+title hash.
func (f *Fetcher) FetchAll(ctx context.Context, since time.Time) (*FetchResult, error) {
	result := &FetchResult{}

	if len(f.cfg.Feeds) == 0 {
		return result, nil
	}

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
			items, err := f.fetchSingle(ctx, feedURL, since)
			ch <- feedResult{items: items, url: feedURL, err: err}
			return err
		})
	}

	if err := grp.Wait(); err != nil {
		return nil, fmt.Errorf("rss: fetch group: %w", err)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	// Merge results; deduplicate across feeds by hash.
	seen := make(map[string]bool)
	for fr := range ch {
		if fr.err != nil {
			result.FeedsErr++
			continue
		}
		result.FeedsOK++
		for _, item := range fr.items {
			if !seen[item.ID] {
				seen[item.ID] = true
				result.Items = append(result.Items, item)
			}
		}
	}

	return result, nil
}

// fetchSingle fetches a single RSS feed and returns items published after since.
func (f *Fetcher) fetchSingle(ctx context.Context, feedURL string, since time.Time) ([]models.NewsItem, error) {
	parser := gofeed.NewParser()

	var (
		feed *gofeed.Feed
		err  error
	)

	if strings.HasPrefix(feedURL, "http://") || strings.HasPrefix(feedURL, "https://") {
		httpClient := &http.Client{Timeout: f.cfg.FetchTimeout}
		parser.Client = httpClient
		feed, err = parser.ParseURLWithContext(feedURL, ctx)
	} else {
		data, err := os.ReadFile(feedURL)
		if err != nil {
			return nil, fmt.Errorf("rss: read local feed %s: %w", feedURL, err)
		}
		feed, err = parser.Parse(strings.NewReader(string(data)))
		if err != nil {
			return nil, fmt.Errorf("rss: parse local feed %s: %w", feedURL, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("rss: parse feed %s: %w", feedURL, err)
	}

	seen := make(map[string]bool)
	var items []models.NewsItem

	for _, entry := range feed.Items {
		if len(items) >= f.cfg.MaxItemsPerFeed {
			break
		}

		pubDate := entry.PublishedParsed
		if pubDate == nil {
			continue
		}
		// Skip items published before (or at) the since cutoff.
		if !pubDate.After(since) {
			continue
		}

		hash := itemHash(entry.Title, entry.Link)
		if seen[hash] {
			continue
		}
		seen[hash] = true

		desc := entry.Description
		if desc == "" && entry.Content != "" {
			desc = entry.Content
		}
		desc = truncate(desc, 300)

		items = append(items, models.NewsItem{
			ID:          hash,
			Title:       entry.Title,
			Description: desc,
			Link:        entry.Link,
			PublishedAt: *pubDate,
			FeedURL:     feedURL,
		})
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

// truncate limits a string to max runes, adding ellipsis if truncated.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
