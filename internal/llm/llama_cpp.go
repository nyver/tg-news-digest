package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/nyver/tg-news-digest/internal/config"
)

// LlamaCPP implements Provider for a local llama.cpp server (OpenAI-compatible API).
type LlamaCPP struct {
	httpClient *resty.Client
	cfg        config.LLMConfig
	logger     *slog.Logger
}

// NewLlamaCPP creates a new LlamaCPP provider.
func NewLlamaCPP(cfg config.LLMConfig, logger *slog.Logger) Provider {
	httpClient := resty.New().
		SetBaseURL(cfg.Endpoint).
		SetRetryCount(2).
		SetRetryWaitTime(1 * time.Second).
		SetRetryMaxWaitTime(4 * time.Second)

	if cfg.APIKey != "" {
		httpClient.SetAuthToken(cfg.APIKey)
	}

	return &LlamaCPP{
		httpClient: httpClient,
		cfg:        cfg,
		logger:     logger,
	}
}

type llamaChatRequest struct {
	Model       string     `json:"model"`
	Messages    []llamaMsg `json:"messages"`
	Temperature float64    `json:"temperature"`
	MaxTokens   int        `json:"max_tokens"`
	Stream      bool       `json:"stream"`
}

type llamaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llamaChatResponse struct {
	Choices []llamaChoice `json:"choices"`
}

type llamaChoice struct {
	Message      llamaMsg `json:"message"`
	FinishReason string   `json:"finish_reason"`
}

// Chat sends a chat completion request to the llama.cpp server.
func (p *LlamaCPP) Chat(ctx context.Context, req *ChatRequest) (string, error) {
	llamaReq := llamaChatRequest{
		Model:       req.Model,
		Messages:    make([]llamaMsg, len(req.Messages)),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
	for i, msg := range req.Messages {
		llamaReq.Messages[i] = llamaMsg{Role: msg.Role, Content: msg.Content}
	}

	resp, err := p.httpClient.R().
		SetContext(ctx).
		SetBody(llamaReq).
		Post("/v1/chat/completions")
	if err != nil {
		return "", fmt.Errorf("llm: post request: %w", err)
	}

	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("llm: unexpected HTTP %d: %s",
			resp.StatusCode(), truncateForLog(strings.TrimSpace(resp.String()), 200))
	}

	var apiResp llamaChatResponse
	if err := json.Unmarshal(resp.Body(), &apiResp); err != nil {
		return "", fmt.Errorf("llm: unmarshal response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices array")
	}

	content := strings.TrimSpace(apiResp.Choices[0].Message.Content)
	if content == "" {
		p.logger.Warn("llm: empty message content",
			slog.String("finish_reason", apiResp.Choices[0].FinishReason),
			slog.String("raw", strings.TrimSpace(resp.String())),
		)
		return "", fmt.Errorf("llm: empty message content (finish_reason=%q)", apiResp.Choices[0].FinishReason)
	}

	return content, nil
}
