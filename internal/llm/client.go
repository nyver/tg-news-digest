package llm

import (
	"context"
	"fmt"
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

// buildSystemPrompt returns a system prompt requesting exactly topN items.
func buildSystemPrompt(topN int) string {
	return fmt.Sprintf(`Ты — редактор новостного дайджеста на русском языке. Из списка ниже выбери ровно %d самых важных и актуальных новостей за текущие сутки.
Отранжируй их по значимости, удали дубли и малозначимые события.
Для каждой новости укажи:
1. Краткий заголовок (до 10 слов)
2. Подробное описание (3-5 предложений) — раскрой суть, контекст и возможные последствия события
3. Ссылку на источник
Отформатируй вывод строго в виде нумерованного списка от 1 до %d.
Каждая новость в формате:
N. ЗАГОЛОВОК
   Описание (3-5 предложений). URL: ссылка

ОБЯЗАТЕЛЬНО: ВСЕ тексты (заголовки и описания) ДОЛЖНЫ быть на русском языке.
Если новость на английском или любом другом иностранном языке — ПЕРЕВЕДИ заголовок и описание на русский язык полностью, сохранив смысл.
НЕ оставляй ни одного слова на иностранном языке в заголовках или описаниях.
Не добавляй вступлений, заключений, комментариев или пояснений. Выведи только нумерованный список новостей.`, topN, topN)
}

// RankWithLLM sends collected news to the LLM and returns the top-N ranked items with summaries.
// Returns (ranked items, llmUsed, error). On any LLM/parse failure, falls back to raw top-N by date
// from the FULL original list (not the context-truncated subset).
func (c *Client) RankWithLLM(ctx context.Context, items []models.NewsItem, topN int) ([]models.RankedNewsItem, bool, error) {
	if len(items) == 0 {
		return nil, false, nil
	}

	// Sort a copy by date; keep the original for fallback so truncation does not
	// hide the most recent items when we fall back to sort-by-date.
	sorted := make([]models.NewsItem, len(items))
	copy(sorted, items)
	sortItemsByDate(sorted)

	// Truncate to fit context window budget (for LLM input only).
	contextCharLimit := int(float64(c.cfg.ContextWindow) * 0.6)
	forLLM := truncateForContext(sorted, contextCharLimit)

	dateStr := forLLM[0].PublishedAt.Format("02.01.2006")
	prompt := buildUserPrompt(forLLM, dateStr, topN)

	// Derive a context with a timeout for the provider call.
	deadline, ok := ctx.Deadline()
	if !ok {
		if c.cfg.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.cfg.Timeout)
			defer cancel()
		}
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
		Messages:    []Message{{Role: "system", Content: buildSystemPrompt(topN)}, {Role: "user", Content: prompt}},
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}

	content, err := c.provider.Chat(ctx, req)
	if err != nil {
		c.logger.Warn("llm: request failed, using fallback", slog.String("error", err.Error()))
		return createFallback(sorted, topN), false, nil
	}

	c.logger.Debug("llm: raw response", slog.String("response", content))

	ranked, err := parseLLMResponse(content, forLLM, topN)
	if err != nil {
		c.logger.Warn("llm: parse failed, using fallback", slog.String("error", err.Error()))
		return createFallback(sorted, topN), false, nil
	}

	c.logger.Info("llm: parsed ranked items", slog.Int("count", len(ranked)))

	return ranked, true, nil
}
