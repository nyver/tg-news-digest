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

const (
	openrouterEndpoint = "https://openrouter.ai/api/v1"
	openrouterAuth     = "Authorization"
	openrouterHeader   = "HTTP-Referer"
	openrouterName     = "tg-news-digest-bot"
)

// OpenRouter implements Provider for OpenRouter.ai.
type OpenRouter struct {
	httpClient *resty.Client
	cfg        config.LLMConfig
	logger     *slog.Logger
}

// NewOpenRouter creates a new OpenRouter provider.
func NewOpenRouter(cfg config.LLMConfig, logger *slog.Logger) Provider {
	httpClient := resty.New().
		SetBaseURL(openrouterEndpoint).
		SetHeader(openrouterHeader, openrouterName).
		SetHeader("Content-Type", "application/json").
		SetAuthToken(cfg.APIKey).
		SetRetryCount(2).
		SetRetryWaitTime(1 * time.Second).
		SetRetryMaxWaitTime(4 * time.Second)

	return &OpenRouter{
		httpClient: httpClient,
		cfg:        cfg,
		logger:     logger,
	}
}

type orChatRequest struct {
	Model       string      `json:"model"`
	Messages    []orMessage `json:"messages"`
	Temperature float64     `json:"temperature,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Stream      bool        `json:"stream"`
}

type orMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type orChatResponse struct {
	Choices []orChoice `json:"choices"`
}

type orChoice struct {
	Message      orMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

// Chat sends a chat completion request to OpenRouter.
func (p *OpenRouter) Chat(ctx context.Context, req *ChatRequest) (string, error) {
	orReq := orChatRequest{
		Model:       req.Model,
		Messages:    make([]orMessage, len(req.Messages)),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
	for i, msg := range req.Messages {
		orReq.Messages[i] = orMessage{Role: msg.Role, Content: msg.Content}
	}

	resp, err := p.httpClient.R().
		SetContext(ctx).
		SetBody(orReq).
		Post("/chat/completions")
	if err != nil {
		return "", fmt.Errorf("llm: openrouter request: %w", err)
	}

	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("llm: openrouter unexpected HTTP %d: %s",
			resp.StatusCode(), truncateForLog(strings.TrimSpace(resp.String()), 200))
	}

	var apiResp orChatResponse
	if err := json.Unmarshal(resp.Body(), &apiResp); err != nil {
		return "", fmt.Errorf("llm: openrouter unmarshal response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("llm: openrouter empty choices array")
	}

	content := strings.TrimSpace(apiResp.Choices[0].Message.Content)
	if content == "" {
		p.logger.Warn("llm: openrouter empty message content",
			slog.String("finish_reason", apiResp.Choices[0].FinishReason),
			slog.String("raw", strings.TrimSpace(resp.String())),
		)
		return "", fmt.Errorf("llm: openrouter empty message content (finish_reason=%q)", apiResp.Choices[0].FinishReason)
	}

	return content, nil
}
