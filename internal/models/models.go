package models

import "time"

// NewsItem represents a single RSS feed entry.
type NewsItem struct {
	ID          string    `json:"id"` // SHA256(title + link)
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
	ChatID    int64     `json:"chat_id"`
	CreatedAt time.Time `json:"created_at"`
	Active    bool      `json:"active"`
}

// SubscriberSettings stores per-chat digest preferences.
type SubscriberSettings struct {
	ChatID             int64  `json:"chat_id"`
	Language           string `json:"language"`
	Timezone           string `json:"timezone"`
	DeliveryTime       string `json:"delivery_time"`
	DigestTopN         int    `json:"digest_top_n"`
	DigestFormat       string `json:"digest_format"` // brief, detailed, executive, links, why_it_matters
	QuietWeekends      bool   `json:"quiet_weekends"`
	LastDigestSentDate string `json:"last_digest_sent_date,omitempty"`
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

// RSSError records a failed RSS fetch for dashboard diagnostics.
type RSSError struct {
	ID        int       `json:"id"`
	FeedURL   string    `json:"feed_url"`
	Error     string    `json:"error"`
	CreatedAt time.Time `json:"created_at"`
}

// BroadcastStats records aggregate delivery counters for one broadcast.
type BroadcastStats struct {
	ID             int       `json:"id"`
	RunAt          time.Time `json:"run_at"`
	Recipients     int       `json:"recipients"`
	SentMessages   int       `json:"sent_messages"`
	FailedMessages int       `json:"failed_messages"`
	SkippedNoMatch int       `json:"skipped_no_match"`
}

// CategoryStat is the number of subscribers that selected a category/topic.
type CategoryStat struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}
