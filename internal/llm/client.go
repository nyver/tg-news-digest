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
	"github.com/nyver/tg-news-digest/internal/models"
)

// Client handles communication with the LLM server (llama.cpp OpenAI-compatible API).
type Client struct {
	httpClient *resty.Client
	cfg        config.LLMConfig
	logger     *slog.Logger
}

// New creates a new LLM client.
func New(cfg config.LLMConfig, logger *slog.Logger) *Client {
	httpClient := resty.New().
		SetBaseURL(cfg.Endpoint).
		SetRetryCount(2).
		SetRetryWaitTime(1 * time.Second).
		SetRetryMaxWaitTime(4 * time.Second)

	return &Client{
		httpClient: httpClient,
		cfg:        cfg,
		logger:     logger,
	}
}

// --- HTTP request/response types ---

type chatRequest struct {
	Model       string       `json:"model"`
	Messages    []msgPayload `json:"messages"`
	Temperature float64      `json:"temperature"`
	MaxTokens   int          `json:"max_tokens"`
	Stream      bool         `json:"stream"`
}

type msgPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []choicePayload `json:"choices"`
}

type choicePayload struct {
	Message      msgPayload `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// --- System prompt ---

const systemPrompt = `Ты — редактор новостного дайджеста на русском языке. Из списка ниже выбери ровно 10 самых важных и актуальных новостей за текущие сутки.
Отранжируй их по значимости, удали дубли и малозначимые события.
Для каждой новости укажи:
1. Краткий заголовок (до 10 слов)
2. Одно предложение с сутью
3. Ссылку на источник
Отформатируй вывод строго в виде нумерованного списка от 1 до 10.
Каждая новость в формате:
1. ЗАГОЛОВОК
   Краткое описание (1 предложение). URL: ссылка

ВСЕ ответы должны быть на русском языке. Если новость на иностранном языке — переведи заголовок и описание на русский, сохранив смысл.
Не добавляй вступлений, заключений, комментариев или пояснений. Выведи только нумерованный список новостей.`

// RankWithLLM sends collected news to the LLM and returns the top-10 ranked items with summaries.
// Returns (ranked items, llmUsed, error). On any failure, falls back to raw top-10 by date.
func (c *Client) RankWithLLM(ctx context.Context, items []models.NewsItem) ([]models.RankedNewsItem, bool, error) {
	if len(items) == 0 {
		return nil, false, nil
	}

	// Sort items by publication time (most recent first)
	sorted := make([]models.NewsItem, len(items))
	copy(sorted, items)
	sortItemsByDate(sorted)

	// Truncate to fit context window budget
	contextCharLimit := int(float64(c.cfg.ContextWindow) * 0.6)
	sorted = truncateForContext(sorted, contextCharLimit)

	dateStr := sorted[0].PublishedAt.Format("02.01.2006")
	prompt := buildUserPrompt(sorted, dateStr)

	req := chatRequest{
		Model: c.cfg.Model,
		Messages: []msgPayload{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
		Stream:      false,
	}

	content, err := c.sendRequest(ctx, req)
	if err != nil {
		c.logger.Warn("llm: request failed, using fallback", slog.String("error", err.Error()))
		return createFallback(sorted), false, nil
	}

	c.logger.Debug("llm: raw response", slog.String("response", content))

	ranked, err := parseLLMResponse(content, sorted)
	if err != nil {
		c.logger.Warn("llm: parse failed, using fallback", slog.String("error", err.Error()))
		return createFallback(sorted), false, nil
	}

	c.logger.Info("llm: parsed ranked items", slog.Int("count", len(ranked)))

	return ranked, true, nil
}

func (c *Client) sendRequest(ctx context.Context, req chatRequest) (string, error) {
	// Derive a context with an HTTP-level timeout so that Resty's retries
	// are bounded by a single deadline instead of the default context.
	// We subtract 2 minutes from the parent deadline to leave room for
	// response parsing and network round-trips after the HTTP call finishes.
	deadline, ok := ctx.Deadline()
	if !ok {
		// No deadline on the parent context — fall back to config timeout.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	} else {
		httpTimeout := time.Until(deadline) - 2*time.Minute
		if httpTimeout <= 0 {
			httpTimeout = 30 * time.Second
		}
		ctx, _ = context.WithTimeout(ctx, httpTimeout)
	}
	resp, err := c.httpClient.R().
		SetContext(ctx).
		SetBody(req).
		Post("/v1/chat/completions")
	if err != nil {
		return "", fmt.Errorf("llm: post request: %w", err)
	}

	if resp.StatusCode() != 200 {
		return "", fmt.Errorf("llm: unexpected HTTP %d: %s", resp.StatusCode(), strings.TrimSpace(resp.String()))
	}

	var apiResp chatResponse
	if err := json.Unmarshal(resp.Body(), &apiResp); err != nil {
		return "", fmt.Errorf("llm: unmarshal response: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices array")
	}

	content := strings.TrimSpace(apiResp.Choices[0].Message.Content)
	if content == "" {
		c.logger.Warn("llm: empty message content",
			slog.String("finish_reason", apiResp.Choices[0].FinishReason),
			slog.String("raw", strings.TrimSpace(resp.String())),
		)
		return "", fmt.Errorf("llm: empty message content (finish_reason=%q)", apiResp.Choices[0].FinishReason)
	}

	return content, nil
}
