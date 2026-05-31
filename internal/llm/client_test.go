package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/models"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func TestRankWithLLM_Success(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := llamaChatResponse{
			Choices: []llamaChoice{
				{Message: llamaMsg{
					Content: `1. Первая новость
   Краткое описание первой новости. URL: https://example.com/1
2. Вторая новость
   Краткое описание второй новости. URL: https://example.com/2
3. Третья новость
   Описание. URL: https://example.com/3`,
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

	items := []models.NewsItem{
		{ID: "1", Title: "Title 1", Description: "Desc 1", Link: "https://example.com/1", PublishedAt: time.Now().Add(-1 * time.Hour)},
		{ID: "2", Title: "Title 2", Description: "Desc 2", Link: "https://example.com/2", PublishedAt: time.Now().Add(-2 * time.Hour)},
	}

	ranked, llmUsed, err := client.RankWithLLM(context.Background(), items)
	if err != nil {
		t.Fatalf("RankWithLLM error: %v", err)
	}
	if !llmUsed {
		t.Error("expected llmUsed=true")
	}
	if len(ranked) != 3 {
		t.Fatalf("expected 3 ranked items, got %d", len(ranked))
	}
	if ranked[0].Rank != 1 {
		t.Errorf("expected rank 1, got %d", ranked[0].Rank)
	}
	if ranked[0].Link != "https://example.com/1" {
		t.Errorf("expected link https://example.com/1, got %s", ranked[0].Link)
	}
}

func TestRankWithLLM_ServerError_Fallback(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	})
	defer server.Close()

	cfg := config.LLMConfig{
		Provider:      "llama-cpp",
		Endpoint:      server.URL + "/v1/chat/completions",
		Model:         "test-model",
		Temperature:   0.3,
		MaxTokens:     2000,
		ContextWindow: 8192,
		Timeout:       1 * time.Second,
	}
	client := New(cfg, slog.Default())

	items := []models.NewsItem{
		{ID: "1", Title: "Title 1", Description: "Desc 1", Link: "https://example.com/1", PublishedAt: time.Now().Add(-1 * time.Hour)},
		{ID: "2", Title: "Title 2", Description: "Desc 2", Link: "https://example.com/2", PublishedAt: time.Now().Add(-2 * time.Hour)},
	}

	ranked, llmUsed, err := client.RankWithLLM(context.Background(), items)
	if err != nil {
		t.Fatalf("RankWithLLM should not error on fallback: %v", err)
	}
	if llmUsed {
		t.Error("expected llmUsed=false on server error")
	}
	// Should return fallback items
	if len(ranked) != 2 {
		t.Fatalf("expected 2 fallback items, got %d", len(ranked))
	}
	// Fallback should be sorted by date (most recent first)
	if ranked[0].PublishedAt.Before(ranked[1].PublishedAt) {
		t.Error("fallback items should be sorted by date descending")
	}
}

func TestRankWithLLM_EmptyItems(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("LLM should not be called with empty items")
		w.WriteHeader(200)
	})
	defer server.Close()

	cfg := config.LLMConfig{
		Provider:      "llama-cpp",
		Endpoint:      server.URL + "/v1/chat/completions",
		Model:         "test-model",
		Temperature:   0.3,
		MaxTokens:     2000,
		ContextWindow: 8192,
		Timeout:       1 * time.Second,
	}
	client := New(cfg, slog.Default())

	ranked, llmUsed, err := client.RankWithLLM(context.Background(), nil)
	if err != nil {
		t.Fatalf("RankWithLLM error: %v", err)
	}
	if ranked != nil {
		t.Errorf("expected nil for empty input, got %d items", len(ranked))
	}
	if llmUsed {
		t.Error("expected llmUsed=false for empty input")
	}
}

func TestRankWithLLM_EmptyResponse_Fallback(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := llamaChatResponse{
			Choices: []llamaChoice{
				{Message: llamaMsg{Content: ""}},
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
		Timeout:       1 * time.Second,
	}
	client := New(cfg, slog.Default())

	items := []models.NewsItem{
		{ID: "1", Title: "Title 1", Description: "Desc 1", Link: "https://example.com/1", PublishedAt: time.Now()},
	}

	ranked, llmUsed, _ := client.RankWithLLM(context.Background(), items)
	if llmUsed {
		t.Error("expected llmUsed=false for empty response")
	}
	if len(ranked) != 1 {
		t.Errorf("expected 1 fallback item, got %d", len(ranked))
	}
}
