package llm

import (
	"fmt"
	"strings"

	"github.com/nyver/tg-news-digest/internal/models"
)

// buildUserPrompt assembles the user message with the list of news items.
func buildUserPrompt(items []models.NewsItem, dateStr string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Ты — опытный новостной редактор. Выбери ровно 10 самых важных новостей за %s из списка ниже.\n\n", dateStr))
	sb.WriteString("Критерии важности новости (в порядке приоритета):\n")
	sb.WriteString("1. Глобальный масштаб влияния — затрагивает множество людей или стран\n")
	sb.WriteString("2. Актуальность — произошло недавно, имеет прямое отношение к текущей повестке\n")
	sb.WriteString("3. Уникальность — новость не является дублем, имеет оригинальный информационный повод\n")
	sb.WriteString("4. Социальная значимость — политика, экономика, безопасность, наука, технологии\n")
	sb.WriteString("5. Достоверность источника — учитывай авторитетность издания, если она указана\n\n")

	sb.WriteString("Правила отбора:\n")
	sb.WriteString("— Избегай дублирующихся новостей на одну тему (выбирай одну — наиболее полную)\n")
	sb.WriteString("— Не включай рекламные материалы, анонсы и спонсорский контент\n")
	sb.WriteString("— Не учитывай сенсационность заголовка — оценивай реальную значимость события\n")
	sb.WriteString("— При равной важности отдавай предпочтение более свежей новости\n\n")

	sb.WriteString("Отвечай на русском языке. Переведи заголовки и описания на русский, если они на иностранном.\n\n")
	sb.WriteString("НОВОСТИ:\n")

	for i, item := range items {
		sb.WriteString("---\n")
		sb.WriteString(fmt.Sprintf("Заголовок: %s\n", item.Title))
		if item.Description != "" {
			sb.WriteString(fmt.Sprintf("Описание: %s\n", item.Description))
		}
		sb.WriteString(fmt.Sprintf("Ссылка: %s\n", item.Link))
		sb.WriteString("---\n")
		// Limit number of items sent to avoid context overflow
		if i >= 99 {
			sb.WriteString(fmt.Sprintf("...\n[остальные %d элементов пропущены для экономии контекста]\n", len(items)-i-1))
			break
		}
	}

	return sb.String()
}

// truncateForContext cuts descriptions and the item list to fit the character budget.
func truncateForContext(items []models.NewsItem, maxChars int) []models.NewsItem {
	if maxChars <= 0 {
		maxChars = 32000
	}

	// Estimate total chars
	var totalChars int
	for _, item := range items {
		totalChars += len(item.Title) + len(item.Description) + len(item.Link)
	}

	if totalChars <= maxChars {
		return items
	}

	// Truncate descriptions first
	result := make([]models.NewsItem, len(items))
	copy(result, items)
	for i := range result {
		if result[i].Description != "" {
			result[i].Description = truncateRunes(result[i].Description, 150)
		}
	}

	// If still over, limit number of items (keep most recent)
	var currentChars int
	var trimmed []models.NewsItem
	for _, item := range result {
		chars := len(item.Title) + len(item.Description) + len(item.Link)
		if currentChars+chars > maxChars && len(trimmed) >= 10 {
			break
		}
		trimmed = append(trimmed, item)
		currentChars += chars
	}

	if len(trimmed) == 0 {
		trimmed = []models.NewsItem{result[0]}
	}

	return trimmed
}

// truncateRunes limits a string to max runes, adding ellipsis if truncated.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
