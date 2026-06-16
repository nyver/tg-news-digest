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

func TestRankByTopic_Success(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := llamaChatResponse{
			Choices: []llamaChoice{
				{Message: llamaMsg{
					Content: `1. Новая модель ИИ от OpenAI
   Описание релевантной новости. URL: https://example.com/1`,
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
		{ID: "1", Title: "AI news", Description: "Desc", Link: "https://example.com/1", PublishedAt: time.Now()},
		{ID: "2", Title: "Sports news", Description: "Desc", Link: "https://example.com/2", PublishedAt: time.Now()},
	}

	ranked, llmUsed, err := client.RankByTopic(context.Background(), items, "искусственный интеллект", 10)
	if err != nil {
		t.Fatalf("RankByTopic error: %v", err)
	}
	if !llmUsed {
		t.Error("expected llmUsed=true")
	}
	if len(ranked) != 1 {
		t.Fatalf("expected 1 ranked item, got %d", len(ranked))
	}
	if ranked[0].Link != "https://example.com/1" {
		t.Errorf("expected link https://example.com/1, got %s", ranked[0].Link)
	}
}

func TestRankByTopic_NoRelevantSentinel(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := llamaChatResponse{
			Choices: []llamaChoice{
				{Message: llamaMsg{Content: noRelevantSentinel}},
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
		{ID: "1", Title: "Unrelated", Description: "Desc", Link: "https://example.com/1", PublishedAt: time.Now()},
	}

	ranked, llmUsed, err := client.RankByTopic(context.Background(), items, "космос", 10)
	if err != nil {
		t.Fatalf("RankByTopic error: %v", err)
	}
	if !llmUsed {
		t.Error("expected llmUsed=true even when nothing is relevant")
	}
	if ranked != nil {
		t.Errorf("expected nil ranked items, got %d", len(ranked))
	}
}

func TestRankByTopic_ServerError_KeywordFallback(t *testing.T) {
	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
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
		{ID: "1", Title: "Новый процессор для нейросети", Description: "Desc", Link: "https://example.com/1", PublishedAt: time.Now()},
		{ID: "2", Title: "Футбольный матч", Description: "Desc", Link: "https://example.com/2", PublishedAt: time.Now()},
	}

	ranked, llmUsed, err := client.RankByTopic(context.Background(), items, "нейросети", 10)
	if err != nil {
		t.Fatalf("RankByTopic should not error on fallback: %v", err)
	}
	if llmUsed {
		t.Error("expected llmUsed=false on server error")
	}
	if len(ranked) != 1 {
		t.Fatalf("expected 1 keyword-matched item, got %d", len(ranked))
	}
	if ranked[0].Link != "https://example.com/1" {
		t.Errorf("expected link https://example.com/1, got %s", ranked[0].Link)
	}
}

func TestRankByTopic_EmptyTopic(t *testing.T) {
	cfg := config.LLMConfig{Provider: "llama-cpp"}
	client := New(cfg, slog.Default())

	items := []models.NewsItem{{ID: "1", Title: "T", Link: "http://x", PublishedAt: time.Now()}}

	ranked, llmUsed, err := client.RankByTopic(context.Background(), items, "   ", 10)
	if err != nil {
		t.Fatalf("RankByTopic error: %v", err)
	}
	if llmUsed {
		t.Error("expected llmUsed=false for empty topic")
	}
	if ranked != nil {
		t.Errorf("expected nil ranked for empty topic, got %d items", len(ranked))
	}
}

func TestFallbackByTopic(t *testing.T) {
	items := []models.NewsItem{
		{Title: "Большая языковая модель Gemini обновилась", Description: "", Link: "http://a", PublishedAt: time.Now()},
		{Title: "Чемпионат по футболу", Description: "", Link: "http://b", PublishedAt: time.Now()},
	}

	got := fallbackByTopic(items, "языковая модель", 10)
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
	if got[0].Link != "http://a" {
		t.Errorf("expected http://a, got %s", got[0].Link)
	}
}

func TestTopicWords(t *testing.T) {
	words := topicWords("ИИ, в медицине!")
	found := false
	for _, w := range words {
		if w == "медицине" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'медицине' among topic words, got %v", words)
	}
}
