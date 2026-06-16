package models

import "time"

// NewsItem represents a single RSS feed entry.
type NewsItem struct {
	ID          string    `json:"id"`           // SHA256(title + link)
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Link        string    `json:"link"`
	PublishedAt time.Time `json:"published_at"`
	FeedURL     string    `json:"feed_url"`
}

// RankedNewsItem is a news item ranked by LLM with a summary.
type RankedNewsItem struct {
	Rank        int       `json:"rank"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Link        string    `json:"link"`
	PublishedAt time.Time `json:"published_at"`
	Source      string    `json:"source"`
	Category    string    `json:"category,omitempty"`
}

// Subscriber represents a Telegram subscriber.
type Subscriber struct {
	ChatID   int64     `json:"chat_id"`
	CreatedAt time.Time `json:"created_at"`
	Active   bool      `json:"active"`
}

// DigestRun represents a single digest generation run.
type DigestRun struct {
	ID        int       `json:"id"`
	RunAt     time.Time `json:"run_at"`
	Status    string    `json:"status"`  // success, failed, fallback
	Trigger   string    `json:"trigger"` // cron, command
	ItemCount int       `json:"item_count"`
	LLMUsed   bool      `json:"llm_used"`
	ErrorMsg  string    `json:"error_msg,omitempty"`
}
