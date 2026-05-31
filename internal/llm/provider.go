package llm

import (
	"context"
	"log/slog"

	"github.com/nyver/tg-news-digest/internal/config"
)

// Provider defines the interface for LLM providers that support the chat completion API.
// Both llama.cpp (local) and OpenRouter implement this interface.
type Provider interface {
	// Chat sends a chat completion request and returns the assistant's response content.
	Chat(ctx context.Context, req *ChatRequest) (string, error)
}

// ChatRequest represents a unified chat completion request across providers.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int
}

// Message represents a single message in a conversation.
type Message struct {
	Role    string
	Content string
}

// NewProvider creates a provider instance based on the configuration.
func NewProvider(cfg config.LLMConfig, logger *slog.Logger) Provider {
	switch cfg.Provider {
	case "openrouter":
		return NewOpenRouter(cfg, logger)
	default:
		return NewLlamaCPP(cfg, logger)
	}
}
