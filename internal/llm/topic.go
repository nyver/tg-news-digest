package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

// noRelevantSentinel is the exact phrase the LLM is instructed to output when
// none of the supplied news items are relevant to the requested topic.
const noRelevantSentinel = "НЕТ РЕЛЕВАНТНЫХ НОВОСТЕЙ"

// RankByTopic asks the LLM to select and summarize only the items relevant to
// a free-text topic, returning up to maxN ranked items. Unlike RankWithLLM,
// an empty result is a legitimate outcome (nothing relevant today) rather than
// a failure — callers should only treat a non-nil error as a real problem.
func (c *Client) RankByTopic(ctx context.Context, items []models.NewsItem, topic string, maxN int) ([]models.RankedNewsItem, bool, error) {
	topic = strings.TrimSpace(topic)
	if len(items) == 0 || topic == "" {
		return nil, false, nil
	}

	sorted := make([]models.NewsItem, len(items))
	copy(sorted, items)
	sortItemsByDate(sorted)

	contextCharLimit := int(float64(c.cfg.ContextWindow) * 0.6)
	forLLM := truncateForContext(sorted, contextCharLimit)

	dateStr := forLLM[0].PublishedAt.Format("02.01.2006")
	prompt := buildTopicUserPrompt(forLLM, topic, dateStr, maxN)

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
		Messages:    []Message{{Role: "system", Content: buildTopicSystemPrompt(maxN)}, {Role: "user", Content: prompt}},
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}

	content, err := c.provider.Chat(ctx, req)
	if err != nil {
		c.logger.Warn("llm: topic request failed, using keyword fallback", slog.String("error", err.Error()))
		return fallbackByTopic(sorted, topic, maxN), false, nil
	}

	c.logger.Debug("llm: topic raw response", slog.String("response", content))

	trimmed := strings.TrimSpace(content)
	if trimmed == "" || strings.Contains(strings.ToUpper(trimmed), noRelevantSentinel) {
		return nil, true, nil
	}

	ranked := tryParseStructured(trimmed, maxN, nil)
	if len(ranked) == 0 {
		c.logger.Warn("llm: topic parse returned 0 items, using keyword fallback")
		return fallbackByTopic(sorted, topic, maxN), false, nil
	}

	return enrichRankedItems(ranked, forLLM), true, nil
}

// buildTopicSystemPrompt instructs the LLM to act as a relevance filter for a
// user-supplied topic rather than a fixed-size top-N ranker.
func buildTopicSystemPrompt(maxN int) string {
	return fmt.Sprintf(`IMPORTANT / ВАЖНО: You MUST write ALL output exclusively in Russian language.
Every title and every description MUST be in Russian, regardless of the original language of the news.
If the source material is in English or any other language — translate it fully into Russian.

Ты — редактор новостного дайджеста на русском языке. Пользователь интересуется конкретной темой.
Из списка новостей ниже выбери ТОЛЬКО те, что релевантны указанной теме — не более %d штук, отранжированных по релевантности и значимости.
Если новость лишь отдалённо связана с темой — не включай её. Удаляй дубли.

Если среди новостей нет ни одной релевантной теме, выведи ровно одну строку без пояснений: %s

Если релевантные новости есть, для каждой напиши:
1. Краткий заголовок на русском языке (до 10 слов)
2. Подробное описание на русском языке (2-4 предложения)
3. Ссылку на источник (оригинальную, без изменений)

Формат вывода — строго нумерованный список:
1. Заголовок на русском
   Описание на русском. URL: https://...
2. Заголовок на русском
   Описание на русском. URL: https://...

Не добавляй вступлений, заключений, комментариев.`, maxN, noRelevantSentinel)
}

// buildTopicUserPrompt assembles the user message for a topic-filtered digest.
func buildTopicUserPrompt(items []models.NewsItem, topic, dateStr string, maxN int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Тема пользователя: %q\n\n", topic))
	sb.WriteString(fmt.Sprintf("Выбери до %d новостей за %s, релевантных этой теме, из списка ниже.\n\n", maxN, dateStr))
	sb.WriteString("НОВОСТИ:\n")

	for i, item := range items {
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("Заголовок: %s\n", item.Title))
		if item.Description != "" {
			sb.WriteString(fmt.Sprintf("Описание: %s\n", item.Description))
		}
		sb.WriteString(fmt.Sprintf("Ссылка: %s\n", item.Link))
		sb.WriteString("---\n")
		if i >= 99 {
			sb.WriteString(fmt.Sprintf("...\n[остальные %d элементов пропущены для экономии контекста]\n", len(items)-i-1))
			break
		}
	}

	return sb.String()
}

// fallbackByTopic performs naive keyword matching when the LLM is unavailable
// or fails to produce a usable response. It is intentionally simple — a
// substring match against the user's topic words.
func fallbackByTopic(items []models.NewsItem, topic string, maxN int) []models.RankedNewsItem {
	words := topicWords(topic)
	if len(words) == 0 {
		return nil
	}

	var matched []models.NewsItem
	for _, item := range items {
		text := strings.ToLower(item.Title + " " + item.Description)
		for _, w := range words {
			if strings.Contains(text, w) {
				matched = append(matched, item)
				break
			}
		}
	}

	if len(matched) > maxN {
		matched = matched[:maxN]
	}

	result := make([]models.RankedNewsItem, len(matched))
	for i, item := range matched {
		desc := item.Description
		if desc == "" {
			desc = "Подробнее по ссылке"
		}
		if len([]rune(desc)) > 200 {
			desc = truncateRunes(desc, 200) + "…"
		}
		result[i] = models.RankedNewsItem{
			Rank:        i + 1,
			Title:       item.Title,
			Summary:     desc,
			Link:        item.Link,
			PublishedAt: item.PublishedAt,
			Source:      extractSourceName(item.FeedURL),
		}
	}
	return result
}

// topicWords splits a free-text topic into lowercase words of length >= 3,
// used for substring matching in the keyword fallback path.
func topicWords(topic string) []string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(topic)) {
		w = strings.Trim(w, ".,!?;:()\"'«»")
		if len([]rune(w)) >= 3 {
			words = append(words, w)
		}
	}
	return words
}
