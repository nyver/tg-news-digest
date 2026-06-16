package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"log/slog"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/models"
)

func TestTranslateDigest_Success(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := llamaChatResponse{
			Choices: []llamaChoice{
				{Message: llamaMsg{
					Content: `ЗАГОЛОВОК: Top-2 news for 16.06.2026
1. New AI model released
   Short description of the news.
2. Programming language update
   Another short description.`,
				}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer server.Close()

	cfg := config.LLMConfig{
		Provider:      "llama-cpp",
		Endpoint:      server.URL + "/v1/chat/completions",
		Model:         "test-model",
		Temperature:   0.3,
		MaxTokens:     2000,
		ContextWindow: 8192,
		Timeout:       5 * time.Second,
	}
	client := New(cfg, slog.Default())

	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Новая модель ИИ", Summary: "Короткое описание.", Link: "https://example.com/1", Category: "AI"},
		{Rank: 2, Title: "Обновление языка программирования", Summary: "Другое описание.", Link: "https://example.com/2", Category: "Программирование"},
	}

	translated, header, err := client.TranslateDigest(context.Background(), items, "Топ-2 новостей за 16.06.2026", "English")
	if err != nil {
		t.Fatalf("TranslateDigest error: %v", err)
	}
	if header != "Top-2 news for 16.06.2026" {
		t.Errorf("unexpected header: %q", header)
	}
	if translated[0].Title != "New AI model released" {
		t.Errorf("unexpected title[0]: %q", translated[0].Title)
	}
	if translated[1].Title != "Programming language update" {
		t.Errorf("unexpected title[1]: %q", translated[1].Title)
	}
	// Non-text fields must be preserved untouched.
	if translated[0].Link != "https://example.com/1" || translated[0].Category != "AI" {
		t.Errorf("expected link/category preserved, got %+v", translated[0])
	}
}

func TestTranslateDigest_RussianIsNoOp(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("LLM should not be called when target language is Russian")
	})
	defer server.Close()

	cfg := config.LLMConfig{Provider: "llama-cpp", Endpoint: server.URL}
	client := New(cfg, slog.Default())

	items := []models.RankedNewsItem{{Title: "Заголовок", Summary: "Описание"}}

	translated, header, err := client.TranslateDigest(context.Background(), items, "Топ-1", "русский")
	if err != nil {
		t.Fatalf("TranslateDigest error: %v", err)
	}
	if header != "Топ-1" {
		t.Errorf("expected header unchanged, got %q", header)
	}
	if translated[0].Title != "Заголовок" {
		t.Errorf("expected title unchanged, got %q", translated[0].Title)
	}
}

func TestTranslateDigest_RequestFailed_ReturnsOriginal(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	defer server.Close()

	cfg := config.LLMConfig{
		Provider:      "llama-cpp",
		Endpoint:      server.URL + "/v1/chat/completions",
		Model:         "test-model",
		ContextWindow: 8192,
		Timeout:       1 * time.Second,
	}
	client := New(cfg, slog.Default())

	items := []models.RankedNewsItem{{Title: "Заголовок", Summary: "Описание"}}

	translated, header, err := client.TranslateDigest(context.Background(), items, "Топ-1", "English")
	if err == nil {
		t.Error("expected error to be propagated on request failure")
	}
	if header != "Топ-1" {
		t.Errorf("expected original header on failure, got %q", header)
	}
	if translated[0].Title != "Заголовок" {
		t.Errorf("expected original title on failure, got %q", translated[0].Title)
	}
}

func TestParseTranslatedDigest_PartialMatch(t *testing.T) {
	response := `ЗАГОЛОВОК: Header
1. Only first title
   Only first summary`

	header, items, ok := parseTranslatedDigest(response, 2)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if header != "Header" {
		t.Errorf("unexpected header: %q", header)
	}
	if items[0].title != "Only first title" {
		t.Errorf("unexpected title[0]: %q", items[0].title)
	}
	if items[1].title != "" {
		t.Errorf("expected empty title[1] for unmatched rank, got %q", items[1].title)
	}
}

func TestParseTranslatedDigest_NothingRecognized(t *testing.T) {
	_, _, ok := parseTranslatedDigest("just some unrelated text", 2)
	if ok {
		t.Error("expected ok=false when nothing is recognized")
	}
}

func TestIsDefaultLanguage(t *testing.T) {
	cases := map[string]bool{
		"":          true,
		"ru":        true,
		"RU":        true,
		"русский":   true,
		"Russian":   true,
		"English":   false,
		"español":   false,
		"中文":        false,
	}
	for lang, want := range cases {
		if got := isDefaultLanguage(lang); got != want {
			t.Errorf("isDefaultLanguage(%q) = %v, want %v", lang, got, want)
		}
	}
}
