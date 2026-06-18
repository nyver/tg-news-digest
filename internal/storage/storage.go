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
		`CREATE TABLE IF NOT EXISTS rss_errors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			feed_url TEXT NOT NULL,
			error TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rss_errors_created_at ON rss_errors(created_at)`,
		`CREATE TABLE IF NOT EXISTS broadcast_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			recipients INTEGER NOT NULL DEFAULT 0,
			sent_messages INTEGER NOT NULL DEFAULT 0,
			failed_messages INTEGER NOT NULL DEFAULT 0,
			skipped_no_match INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_stats_run_at ON broadcast_stats(run_at)`,
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

	for _, alter := range []string{
		`ALTER TABLE subscribers ADD COLUMN timezone TEXT NOT NULL DEFAULT 'Europe/Moscow'`,
		`ALTER TABLE subscribers ADD COLUMN delivery_time TEXT NOT NULL DEFAULT '09:00'`,
		`ALTER TABLE subscribers ADD COLUMN digest_top_n INTEGER NOT NULL DEFAULT 10`,
		`ALTER TABLE subscribers ADD COLUMN digest_format TEXT NOT NULL DEFAULT 'detailed'`,
		`ALTER TABLE subscribers ADD COLUMN quiet_weekends BOOLEAN NOT NULL DEFAULT 0`,
		`ALTER TABLE subscribers ADD COLUMN last_digest_sent_date TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.ExecContext(ctx, alter); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("storage: alter subscribers settings: %w", err)
		}
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

func normalizeSettings(st models.SubscriberSettings) models.SubscriberSettings {
	if st.Language == "" {
		st.Language = "ru"
	}
	if st.Timezone == "" {
		st.Timezone = "Europe/Moscow"
	}
	if st.DeliveryTime == "" {
		st.DeliveryTime = "09:00"
	}
	if st.DigestTopN <= 0 {
		st.DigestTopN = 10
	}
	st.DigestFormat = NormalizeDigestMode(st.DigestFormat)
	return st
}

func NormalizeDigestMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "short", "brief":
		return "brief"
	case "detailed", "executive", "links", "why_it_matters":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return "detailed"
	}
}

func (s *Store) upsertSubscriberPreference(ctx context.Context, chatID int64, column string, value any) error {
	if !allowedSubscriberPreferenceColumn(column) {
		return fmt.Errorf("storage: unsupported subscriber preference column %q", column)
	}
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO subscribers (chat_id, created_at, active, %s)
		 VALUES (?, CURRENT_TIMESTAMP, 0, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET %s = excluded.%s`, column, column, column),
		chatID, value,
	)
	return err
}

func allowedSubscriberPreferenceColumn(column string) bool {
	switch column {
	case "timezone",
		"delivery_time",
		"digest_top_n",
		"digest_format",
		"quiet_weekends",
		"last_digest_sent_date",
		"language":
		return true
	default:
		return false
	}
}

// GetSubscriberSettings returns the chat's digest preferences, defaulting in
// memory when the chat has not interacted with the bot yet.
func (s *Store) GetSubscriberSettings(ctx context.Context, chatID int64) (models.SubscriberSettings, error) {
	var st models.SubscriberSettings
	var quiet int
	err := s.db.QueryRowContext(ctx,
		`SELECT chat_id, language, timezone, delivery_time, digest_top_n, digest_format,
		        quiet_weekends, COALESCE(last_digest_sent_date, '')
		 FROM subscribers WHERE chat_id = ?`, chatID,
	).Scan(
		&st.ChatID,
		&st.Language,
		&st.Timezone,
		&st.DeliveryTime,
		&st.DigestTopN,
		&st.DigestFormat,
		&quiet,
		&st.LastDigestSentDate,
	)
	if err == sql.ErrNoRows {
		st.ChatID = chatID
		return normalizeSettings(st), nil
	}
	if err != nil {
		return models.SubscriberSettings{}, err
	}
	st.QuietWeekends = quiet == 1
	return normalizeSettings(st), nil
}

func (s *Store) SetSubscriberTimezone(ctx context.Context, chatID int64, timezone string) error {
	return s.upsertSubscriberPreference(ctx, chatID, "timezone", timezone)
}

func (s *Store) SetSubscriberDeliveryTime(ctx context.Context, chatID int64, deliveryTime string) error {
	return s.upsertSubscriberPreference(ctx, chatID, "delivery_time", deliveryTime)
}

func (s *Store) SetSubscriberDigestTopN(ctx context.Context, chatID int64, topN int) error {
	return s.upsertSubscriberPreference(ctx, chatID, "digest_top_n", topN)
}

func (s *Store) SetSubscriberDigestFormat(ctx context.Context, chatID int64, format string) error {
	return s.upsertSubscriberPreference(ctx, chatID, "digest_format", NormalizeDigestMode(format))
}

func (s *Store) SetSubscriberQuietWeekends(ctx context.Context, chatID int64, quiet bool) error {
	value := 0
	if quiet {
		value = 1
	}
	return s.upsertSubscriberPreference(ctx, chatID, "quiet_weekends", value)
}

func (s *Store) MarkSubscriberDigestSent(ctx context.Context, chatID int64, localDate string) error {
	return s.upsertSubscriberPreference(ctx, chatID, "last_digest_sent_date", localDate)
}

// SetSubscriberLanguage sets the digest language preference for a chat.
// If the chat has never subscribed, a row is created with active = 0 so the
// preference is remembered for when they do subscribe (SaveSubscriber's
// ON CONFLICT only flips active, it never touches language).
func (s *Store) SetSubscriberLanguage(ctx context.Context, chatID int64, language string) error {
	return s.upsertSubscriberPreference(ctx, chatID, "language", language)
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

func (s *Store) GetActiveSubscriberSettings(ctx context.Context) ([]models.SubscriberSettings, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, language, timezone, delivery_time, digest_top_n, digest_format,
		        quiet_weekends, COALESCE(last_digest_sent_date, '')
		 FROM subscribers WHERE active = 1 ORDER BY chat_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.SubscriberSettings
	for rows.Next() {
		var st models.SubscriberSettings
		var quiet int
		if err := rows.Scan(
			&st.ChatID,
			&st.Language,
			&st.Timezone,
			&st.DeliveryTime,
			&st.DigestTopN,
			&st.DigestFormat,
			&quiet,
			&st.LastDigestSentDate,
		); err != nil {
			return nil, err
		}
		st.QuietWeekends = quiet == 1
		result = append(result, normalizeSettings(st))
	}
	return result, rows.Err()
}

// GetDueSubscriberSettings returns active subscribers whose local delivery
// time is due at nowUTC and who have not received today's scheduled digest.
func (s *Store) GetDueSubscriberSettings(ctx context.Context, nowUTC time.Time) ([]models.SubscriberSettings, error) {
	settings, err := s.GetActiveSubscriberSettings(ctx)
	if err != nil {
		return nil, err
	}

	var due []models.SubscriberSettings
	for _, st := range settings {
		loc, err := time.LoadLocation(st.Timezone)
		if err != nil {
			loc = time.UTC
		}
		localNow := nowUTC.In(loc)
		localDate := localNow.Format("2006-01-02")
		if st.LastDigestSentDate == localDate {
			continue
		}
		if st.QuietWeekends && (localNow.Weekday() == time.Saturday || localNow.Weekday() == time.Sunday) {
			continue
		}
		nowMinutes := localNow.Hour()*60 + localNow.Minute()
		deliveryMinutes, ok := parseDeliveryMinutes(st.DeliveryTime)
		if !ok {
			continue
		}
		if nowMinutes >= deliveryMinutes {
			due = append(due, st)
		}
	}
	return due, nil
}

func parseDeliveryMinutes(deliveryTime string) (int, bool) {
	t, err := time.Parse("15:04", deliveryTime)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
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

func (s *Store) CountSubscribers(ctx context.Context) (active, total int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CASE WHEN active = 1 THEN 1 ELSE 0 END), 0), COUNT(*) FROM subscribers`,
	).Scan(&active, &total)
	return active, total, err
}

func (s *Store) GetPopularCategories(ctx context.Context, limit int) ([]models.CategoryStat, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT category, COUNT(*) AS cnt
		 FROM subscriber_categories
		 GROUP BY lower(category)
		 ORDER BY cnt DESC, category ASC
		 LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []models.CategoryStat
	for rows.Next() {
		var st models.CategoryStat
		if err := rows.Scan(&st.Category, &st.Count); err != nil {
			return nil, err
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
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

func (s *Store) GetRecentDigestRuns(ctx context.Context, limit int) ([]models.DigestRun, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_at, status, item_count, llm_used, COALESCE(error_msg, ''), COALESCE(trigger, 'cron')
		 FROM digest_runs ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []models.DigestRun
	for rows.Next() {
		var run models.DigestRun
		if err := rows.Scan(
			&run.ID,
			&run.RunAt,
			&run.Status,
			&run.ItemCount,
			&run.LLMUsed,
			&run.ErrorMsg,
			&run.Trigger,
		); err != nil {
			return nil, err
		}
		run.RunAt = run.RunAt.UTC()
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) SaveRSSError(ctx context.Context, feedURL, errorMessage string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rss_errors (feed_url, error, created_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
		feedURL, errorMessage,
	)
	return err
}

func (s *Store) GetRecentRSSErrors(ctx context.Context, limit int) ([]models.RSSError, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, feed_url, error, created_at FROM rss_errors ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var errors []models.RSSError
	for rows.Next() {
		var rssErr models.RSSError
		if err := rows.Scan(&rssErr.ID, &rssErr.FeedURL, &rssErr.Error, &rssErr.CreatedAt); err != nil {
			return nil, err
		}
		rssErr.CreatedAt = rssErr.CreatedAt.UTC()
		errors = append(errors, rssErr)
	}
	return errors, rows.Err()
}

func (s *Store) SaveBroadcastStats(ctx context.Context, stats models.BroadcastStats) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO broadcast_stats (run_at, recipients, sent_messages, failed_messages, skipped_no_match)
		 VALUES (?, ?, ?, ?, ?)`,
		stats.RunAt.UTC().Format("2006-01-02 15:04:05"),
		stats.Recipients,
		stats.SentMessages,
		stats.FailedMessages,
		stats.SkippedNoMatch,
	)
	return err
}

func (s *Store) GetRecentBroadcastStats(ctx context.Context, limit int) ([]models.BroadcastStats, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_at, recipients, sent_messages, failed_messages, skipped_no_match
		 FROM broadcast_stats ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []models.BroadcastStats
	for rows.Next() {
		var stats models.BroadcastStats
		if err := rows.Scan(
			&stats.ID,
			&stats.RunAt,
			&stats.Recipients,
			&stats.SentMessages,
			&stats.FailedMessages,
			&stats.SkippedNoMatch,
		); err != nil {
			return nil, err
		}
		stats.RunAt = stats.RunAt.UTC()
		result = append(result, stats)
	}
	return result, rows.Err()
}
