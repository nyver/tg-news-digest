package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nyver/tg-news-digest/internal/models"
)

// Store handles all SQLite operations.
type Store struct {
	db *sql.DB
}

// New creates a new Store and runs migrations.
func New(ctx context.Context, dbPath string) (*Store, error) {
	// busy_timeout and loc=UTC are set via DSN so every connection in the pool
	// inherits them automatically (PRAGMA set on a single conn is not inherited).
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_loc=UTC", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open db: %w", err)
	}

	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: set WAL mode: %w", err)
	}

	// SQLite supports only one writer at a time; a single open connection
	// eliminates SQLITE_BUSY under concurrent goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // keep the single connection alive indefinitely

	store := &Store{db: db}

	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: migrate: %w", err)
	}

	return store, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// DB returns the underlying *sql.DB for health checks.
func (s *Store) DB() *sql.DB {
	return s.db
}

// migrate runs all schema migrations.
func (s *Store) migrate(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS subscribers (
			chat_id INTEGER PRIMARY KEY,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			active BOOLEAN NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS fetched_items (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			link TEXT NOT NULL,
			published_at DATETIME NOT NULL,
			feed_url TEXT NOT NULL,
			fetched_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_fetched_items_expires ON fetched_items(expires_at)`,
		`CREATE TABLE IF NOT EXISTS subscriber_categories (
			chat_id INTEGER NOT NULL,
			category TEXT NOT NULL,
			PRIMARY KEY (chat_id, category)
		)`,
		`CREATE TABLE IF NOT EXISTS digest_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			status TEXT NOT NULL,
			item_count INTEGER NOT NULL DEFAULT 0,
			llm_used BOOLEAN NOT NULL DEFAULT 0,
			error_msg TEXT
		)`,
	}

	for _, q := range queries {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("storage: execute migration: %w", err)
		}
	}

	// Add trigger column to digest_runs — idempotent: ignore only "duplicate column" errors.
	_, alterErr := s.db.ExecContext(ctx, `ALTER TABLE digest_runs ADD COLUMN trigger TEXT NOT NULL DEFAULT 'cron'`)
	if alterErr != nil && !strings.Contains(alterErr.Error(), "duplicate column name") {
		return fmt.Errorf("storage: alter digest_runs: %w", alterErr)
	}

	// Add language column to subscribers — idempotent: ignore only "duplicate column" errors.
	_, langErr := s.db.ExecContext(ctx, `ALTER TABLE subscribers ADD COLUMN language TEXT NOT NULL DEFAULT 'ru'`)
	if langErr != nil && !strings.Contains(langErr.Error(), "duplicate column name") {
		return fmt.Errorf("storage: alter subscribers: %w", langErr)
	}

	return nil
}

// --- Subscriber methods ---

func (s *Store) SaveSubscriber(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscribers (chat_id, created_at, active)
		 VALUES (?, CURRENT_TIMESTAMP, 1)
		 ON CONFLICT(chat_id) DO UPDATE SET active = 1`,
		chatID,
	)
	return err
}

func (s *Store) Unsubscribe(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET active = 0 WHERE chat_id = ?`,
		chatID,
	)
	return err
}

// SetSubscriberLanguage sets the digest language preference for a chat.
// If the chat has never subscribed, a row is created with active = 0 so the
// preference is remembered for when they do subscribe (SaveSubscriber's
// ON CONFLICT only flips active, it never touches language).
func (s *Store) SetSubscriberLanguage(ctx context.Context, chatID int64, language string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscribers (chat_id, created_at, active, language)
		 VALUES (?, CURRENT_TIMESTAMP, 0, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET language = excluded.language`,
		chatID, language,
	)
	return err
}

// GetSubscriberLanguage returns the chat's digest language, defaulting to "ru"
// if the chat has no preference set (or hasn't subscribed yet).
func (s *Store) GetSubscriberLanguage(ctx context.Context, chatID int64) (string, error) {
	var language string
	err := s.db.QueryRowContext(ctx,
		`SELECT language FROM subscribers WHERE chat_id = ?`, chatID,
	).Scan(&language)
	if err == sql.ErrNoRows {
		return "ru", nil
	}
	if err != nil {
		return "", err
	}
	if language == "" {
		language = "ru"
	}
	return language, nil
}

// GetActiveChatsByLanguage groups active subscribers by their selected digest
// language, so a broadcast only needs to translate once per distinct language.
func (s *Store) GetActiveChatsByLanguage(ctx context.Context) (map[string][]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, language FROM subscribers WHERE active = 1 ORDER BY chat_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]int64)
	for rows.Next() {
		var chatID int64
		var language string
		if err := rows.Scan(&chatID, &language); err != nil {
			return nil, err
		}
		if language == "" {
			language = "ru"
		}
		result[language] = append(result[language], chatID)
	}
	return result, rows.Err()
}

func (s *Store) IsActive(ctx context.Context, chatID int64) (bool, error) {
	var active int
	err := s.db.QueryRowContext(ctx,
		`SELECT active FROM subscribers WHERE chat_id = ?`, chatID,
	).Scan(&active)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return active == 1, err
}

func (s *Store) GetActiveChats(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id FROM subscribers WHERE active = 1 ORDER BY chat_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []int64
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return nil, err
		}
		chats = append(chats, chatID)
	}
	return chats, rows.Err()
}

// --- Subscriber category preference methods ---

// AddSubscriberCategory marks a chat as interested in the given category.
func (s *Store) AddSubscriberCategory(ctx context.Context, chatID int64, category string) error {
	category = strings.TrimSpace(category)
	if category == "" {
		return nil
	}

	var existing string
	err := s.db.QueryRowContext(ctx,
		`SELECT category FROM subscriber_categories
		 WHERE chat_id = ? AND lower(category) = lower(?)
		 LIMIT 1`,
		chatID, category,
	).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		return nil
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO subscriber_categories (chat_id, category) VALUES (?, ?)`,
		chatID, category,
	)
	return err
}

// RemoveSubscriberCategory removes a chat's interest in the given category.
func (s *Store) RemoveSubscriberCategory(ctx context.Context, chatID int64, category string) error {
	category = strings.TrimSpace(category)
	if category == "" {
		return nil
	}

	_, err := s.db.ExecContext(ctx,
		`DELETE FROM subscriber_categories WHERE chat_id = ? AND lower(category) = lower(?)`,
		chatID, category,
	)
	return err
}

// GetSubscriberCategories returns the categories a chat has selected.
// An empty result means the subscriber receives the full, unfiltered digest.
func (s *Store) GetSubscriberCategories(ctx context.Context, chatID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT category FROM subscriber_categories WHERE chat_id = ? ORDER BY category`, chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []string
	for rows.Next() {
		var category string
		if err := rows.Scan(&category); err != nil {
			return nil, err
		}
		categories = append(categories, category)
	}
	return categories, rows.Err()
}

// --- FetchedItem methods ---

// ItemExists checks if a news item (by ID) is still valid (not expired).
func (s *Store) ItemExists(ctx context.Context, id string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fetched_items WHERE id = ? AND expires_at > CURRENT_TIMESTAMP`,
		id,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// SaveItems stores a batch of news items with TTL-based expiration.
func (s *Store) SaveItems(ctx context.Context, items []models.NewsItem, ttl time.Duration) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO fetched_items (id, title, description, link, published_at, feed_url, fetched_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, datetime('now', ?))`,
	)
	if err != nil {
		return fmt.Errorf("storage: prepare: %w", err)
	}
	defer stmt.Close()

	ttlSeconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		ttlSeconds++
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	ttlStr := fmt.Sprintf("+%d seconds", ttlSeconds)

	for _, item := range items {
		_, err := stmt.ExecContext(ctx,
			item.ID, item.Title, item.Description, item.Link,
			item.PublishedAt.UTC().Format("2006-01-02 15:04:05"),
			item.FeedURL, ttlStr,
		)
		if err != nil {
			return fmt.Errorf("storage: insert item %s: %w", item.ID, err)
		}
	}

	return tx.Commit()
}

// GetRecentItems returns up to limit non-expired fetched items, most recent first.
// Used for on-demand topic digests so they don't need to re-fetch RSS feeds.
func (s *Store) GetRecentItems(ctx context.Context, limit int) ([]models.NewsItem, error) {
	if limit <= 0 {
		limit = 500
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, description, link, published_at, feed_url
		 FROM fetched_items WHERE expires_at > CURRENT_TIMESTAMP
		 ORDER BY published_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.NewsItem
	for rows.Next() {
		var item models.NewsItem
		if err := rows.Scan(&item.ID, &item.Title, &item.Description, &item.Link, &item.PublishedAt, &item.FeedURL); err != nil {
			return nil, err
		}
		item.PublishedAt = item.PublishedAt.UTC()
		items = append(items, item)
	}
	return items, rows.Err()
}

// CleanupExpired removes expired fetched items.
func (s *Store) CleanupExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM fetched_items WHERE expires_at <= CURRENT_TIMESTAMP`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupOldFetchedItems removes fetched items whose original publication date
// is older than olderThan. This is a retention safety net independent of the
// per-item TTL (expires_at/cache_ttl) — it guards against fetched_items growing
// unbounded if the cache TTL is configured longer than the desired retention.
func (s *Store) CleanupOldFetchedItems(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).UTC().Format("2006-01-02 15:04:05")
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM fetched_items WHERE published_at < ?`, cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountActiveItems returns the number of non-expired items.
func (s *Store) CountActiveItems(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fetched_items WHERE expires_at > CURRENT_TIMESTAMP`,
	).Scan(&count)
	return count, err
}

// --- DigestRun methods ---

// SaveDigestRun records a digest generation run and returns the inserted row ID.
func (s *Store) SaveDigestRun(ctx context.Context, run models.DigestRun) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO digest_runs (run_at, status, item_count, llm_used, error_msg, trigger)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		run.RunAt.UTC().Format("2006-01-02 15:04:05"),
		run.Status,
		run.ItemCount,
		run.LLMUsed,
		run.ErrorMsg,
		run.Trigger,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetLastRun returns the most recent digest run of any type.
func (s *Store) GetLastRun(ctx context.Context) (*models.DigestRun, error) {
	run := &models.DigestRun{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, run_at, status, item_count, llm_used, COALESCE(error_msg, ''), COALESCE(trigger, 'cron')
		 FROM digest_runs ORDER BY id DESC LIMIT 1`,
	).Scan(
		&run.ID,
		&run.RunAt,
		&run.Status,
		&run.ItemCount,
		&run.LLMUsed,
		&run.ErrorMsg,
		&run.Trigger,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.RunAt = run.RunAt.UTC()
	return run, nil
}

// GetLastSuccessfulCronRun returns the run_at time of the last successful or fallback
// scheduled digest. Manual /digest runs are intentionally ignored so they don't
// advance the next scheduled fetch window.
// Returns nil if no scheduled digest has ever run successfully.
func (s *Store) GetLastSuccessfulCronRun(ctx context.Context) (*time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT run_at FROM digest_runs
		 WHERE status != 'failed' AND COALESCE(trigger, 'cron') = 'cron'
		 ORDER BY id DESC LIMIT 1`,
	).Scan(&t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	utc := t.UTC()
	return &utc, nil
}

// CleanupOldDigestRuns removes digest run records older than the given duration.
func (s *Store) CleanupOldDigestRuns(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).UTC().Format("2006-01-02 15:04:05")
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM digest_runs WHERE run_at < ?`, cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
