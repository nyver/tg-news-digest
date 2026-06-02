package storage

import (
	"context"
	"database/sql"
	"fmt"
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
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("storage: open db: %w", err)
	}

	// WAL mode for better concurrency
	_, err = db.ExecContext(ctx, `PRAGMA journal_mode=WAL`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: set WAL mode: %w", err)
	}

	// Busy timeout
	_, err = db.ExecContext(ctx, `PRAGMA busy_timeout=5000`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: set busy timeout: %w", err)
	}

	// Configure connection pool for concurrent access
	// MaxOpenConns: limit total concurrent connections to SQLite (WAL allows ~5)
	// MaxIdleConns: keep connections ready for reuse
	// MaxLifetime: periodically refresh connections
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

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

	return nil
}

// --- Subscriber methods ---

// SaveSubscriber stores or updates a subscriber.
func (s *Store) SaveSubscriber(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscribers (chat_id, created_at, active)
		 VALUES (?, CURRENT_TIMESTAMP, 1)
		 ON CONFLICT(chat_id) DO UPDATE SET active = 1`,
		chatID,
	)
	return err
}

// Unsubscribe marks a subscriber as inactive.
func (s *Store) Unsubscribe(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET active = 0 WHERE chat_id = ?`,
		chatID,
	)
	return err
}

// IsActive checks if a subscriber exists and is active.
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

// GetActiveChats returns all active subscriber chat IDs.
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
			return err
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
	n, raErr := res.RowsAffected()
	if raErr != nil {
		return 0, raErr
	}
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

// SaveDigestRun records a digest generation run.
func (s *Store) SaveDigestRun(ctx context.Context, run models.DigestRun) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO digest_runs (run_at, status, item_count, llm_used, error_msg)
		 VALUES (?, ?, ?, ?, ?)`,
		run.RunAt.UTC().Format("2006-01-02 15:04:05"),
		run.Status,
		run.ItemCount,
		run.LLMUsed,
		run.ErrorMsg,
	)
	return err
}

// GetLastRun returns the most recent digest run.
func (s *Store) GetLastRun(ctx context.Context) (*models.DigestRun, error) {
	run := &models.DigestRun{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, run_at, status, item_count, llm_used, error_msg
		 FROM digest_runs ORDER BY id DESC LIMIT 1`,
	).Scan(
		&run.ID,
		&run.RunAt,
		&run.Status,
		&run.ItemCount,
		&run.LLMUsed,
		&run.ErrorMsg,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
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
	n, raErr := res.RowsAffected()
	if raErr != nil {
		return 0, raErr
	}
	return int(n), nil
}
