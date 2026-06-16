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
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO subscriber_categories (chat_id, category) VALUES (?, ?)`,
		chatID, category,
	)
	return err
}

// RemoveSubscriberCategory removes a chat's interest in the given category.
func (s *Store) RemoveSubscriberCategory(ctx context.Context, chatID int64, category string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM subscriber_categories WHERE chat_id = ? AND category = ?`,
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

	for _, item := range items {
		ttlStr := fmt.Sprintf("+%d hours", int(ttl.Hours()))
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

// CleanupExpired removes expired fetched items.
func (s *Store) CleanupExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM fetched_items WHERE expires_at <= CURRENT_TIMESTAMP`)
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

// GetLastSuccessfulRun returns the run_at time of the last successful or fallback digest
// (any trigger). This is used to advance the RSS fetch window so that both manual /digest
// runs and scheduled cron runs are counted — preventing duplicate broadcasts.
// Returns nil if no digest has ever run successfully.
func (s *Store) GetLastSuccessfulRun(ctx context.Context) (*time.Time, error) {
	var t time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT run_at FROM digest_runs
		 WHERE status != 'failed'
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
