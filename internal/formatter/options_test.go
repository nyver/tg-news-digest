package formatter

import (
	"strings"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

func TestDigestPartsWithOptions_TopNAndShortFormat(t *testing.T) {
	f := New(HTML, 10)
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	items := []models.RankedNewsItem{
		{Title: "One", Summary: "Summary one", Link: "https://example.com/1", Source: "S"},
		{Title: "Two", Summary: "Summary two", Link: "https://example.com/2"},
	}

	parts := f.DigestPartsWithOptions(items, date, DigestOptions{TopN: 1, Format: "short"})
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
	if strings.Contains(got, "Summary one") || strings.Contains(got, "<i>") {
		t.Errorf("expected short format without summary/meta in %q", got)
	}
}
