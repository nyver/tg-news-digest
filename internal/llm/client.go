package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/models"
)

// Client handles communication with LLM providers via a unified Provider interface.
type Client struct {
	provider Provider
	cfg      config.LLMConfig
	logger   *slog.Logger
}

// New creates a new LLM client backed by the provider specified in config.
func New(cfg config.LLMConfig, logger *slog.Logger) *Client {
	return &Client{
		provider: NewProvider(cfg, logger),
		cfg:      cfg,
		logger:   logger,
	}
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

	// Derive a context with a timeout for the provider call.
	deadline, ok := ctx.Deadline()
	if !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
		defer cancel()
	} else {
		httpTimeout := time.Until(deadline) - 2*time.Minute
		if httpTimeout <= 0 {
			httpTimeout = 30 * time.Second
		}
		var cancelHTTP context.CancelFunc
		ctx, cancelHTTP = context.WithTimeout(ctx, httpTimeout)
		defer cancelHTTP()
	}

	req := &ChatRequest{
		Model:       c.cfg.Model,
		Messages:    []Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: prompt}},
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}

	content, err := c.provider.Chat(ctx, req)
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
