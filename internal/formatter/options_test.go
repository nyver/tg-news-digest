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
