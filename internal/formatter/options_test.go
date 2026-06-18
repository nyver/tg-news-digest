package formatter

import (
	"strings"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

func TestDigestPartsWithOptions_TopNAndBriefFormat(t *testing.T) {
	f := New(HTML, 10)
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	items := []models.RankedNewsItem{
		{Title: "One", Summary: "Summary one", Link: "https://example.com/1", Source: "S"},
		{Title: "Two", Summary: "Summary two", Link: "https://example.com/2"},
	}

	parts := f.DigestPartsWithOptions(items, date, DigestOptions{TopN: 1, Format: "brief"})
	if len(parts) != 1 {
		t.Fatalf("expected one part, got %d", len(parts))
	}

	got := parts[0]
	if !strings.Contains(got, "Топ-1") {
		t.Errorf("expected personalized topN header in %q", got)
	}
	if !strings.Contains(got, "One") {
		t.Errorf("expected first item in %q", got)
	}
	if strings.Contains(got, "Two") {
		t.Errorf("expected second item to be omitted in %q", got)
	}
	if !strings.Contains(got, "Summary one") || strings.Contains(got, "<i>") {
		t.Errorf("expected brief format with summary and without meta in %q", got)
	}
}

func TestDigestPartsWithOptions_WhyItMattersFormat(t *testing.T) {
	f := New(HTML, 10)
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	items := []models.RankedNewsItem{{
		Title:    "Model release",
		Summary:  "A new model was released",
		Link:     "https://example.com/model",
		Source:   "Example",
		Category: "AI",
	}}

	got := f.DigestPartsWithOptions(items, date, DigestOptions{Format: "why_it_matters"})[0]
	for _, want := range []string{"Коротко:", "Почему важно:", "Источник:", "Категория:"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}
}

func TestDigestPartsWithOptions_AllModesHTML(t *testing.T) {
	f := New(HTML, 10)
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	item := models.RankedNewsItem{
		Title:       "Model release",
		Summary:     "A new model was released",
		Link:        "https://example.com/model",
		Source:      "Example",
		Category:    "AI",
		PublishedAt: date,
	}

	tests := map[string][]string{
		"detailed":       {"A new model was released", "AI • Example"},
		"brief":          {"A new model was released", "Подробнее"},
		"executive":      {"Вывод:", "AI • Example"},
		"links":          {"https://example.com/model", "AI • Example"},
		"why_it_matters": {"Коротко:", "Почему важно:"},
	}
	for mode, wants := range tests {
		got := f.DigestPartsWithOptions([]models.RankedNewsItem{item}, date, DigestOptions{Format: mode})[0]
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("mode %s: expected %q in %q", mode, want, got)
			}
		}
	}
}

func TestDigestPartsWithOptions_AllModesMarkdown(t *testing.T) {
	f := New(MarkdownV2, 10)
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	item := models.RankedNewsItem{
		Title:    "Model release",
		Summary:  "A new model was released",
		Link:     "https://example.com/model",
		Source:   "Example",
		Category: "AI",
	}

	for _, mode := range []string{"detailed", "brief", "executive", "links", "why_it_matters"} {
		got := f.DigestPartsWithOptions([]models.RankedNewsItem{item}, date, DigestOptions{Format: mode})[0]
		if !strings.Contains(got, "Model release") {
			t.Errorf("mode %s: expected title in %q", mode, got)
		}
	}
}

func TestTopicDigestPartsWithOptionsAndModeNormalization(t *testing.T) {
	f := New(HTML, 10)
	item := models.RankedNewsItem{Title: "One", Summary: "Summary", Link: "ftp://invalid"}

	got := f.TopicDigestPartsWithOptions("<b>Header</b>", []models.RankedNewsItem{item}, DigestOptions{TopN: 0, Format: "unknown"})
	if len(got) != 1 {
		t.Fatalf("expected one part, got %d", len(got))
	}
	if !strings.Contains(got[0], "Header") || strings.Contains(got[0], "ftp://invalid") {
		t.Fatalf("unexpected topic digest: %q", got[0])
	}
	if NormalizeDigestMode("short") != "brief" {
		t.Fatal("short should normalize to brief")
	}
	if NormalizeDigestMode("unknown") != "detailed" {
		t.Fatal("unknown mode should normalize to detailed")
	}
}

func TestFormatterSmallMessageHelpers(t *testing.T) {
	if SubscribedMessage(HTML) == "" {
		t.Fatal("expected subscribed message")
	}
	if UnsubscribedMessage(HTML) == "" {
		t.Fatal("expected unsubscribed message")
	}
	if UnknownCommandMessage(HTML) == "" {
		t.Fatal("expected unknown command message")
	}
	if TopN := New(HTML, 0).TopN(); TopN != 10 {
		t.Fatalf("default topN = %d, want 10", TopN)
	}
	if PlainDigestHeader(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC), 5) == "" {
		t.Fatal("expected plain digest header")
	}
}
