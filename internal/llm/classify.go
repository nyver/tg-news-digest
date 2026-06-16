package llm

import "strings"

// DefaultCategories is used when no categories are configured.
var DefaultCategories = []string{"AI", "LLM", "Программирование", "IT"}

// categoryKeywords maps a lowercased category name to keywords used for
// fallback classification when the LLM doesn't return a usable category.
// Only the categories shipped by default are covered; custom categories
// configured by the operator simply won't be matched by keywords and rely
// on the LLM-provided tag instead.
var categoryKeywords = map[string][]string{
	"ai": {
		"искусственный интеллект", "нейросет", "machine learning", "машинное обучение",
		"neural network", "artificial intelligence", " ai ", "ai-",
	},
	"llm": {
		"llm", "большая языковая модель", "языковая модель", "gpt", "chatgpt",
		"claude", "gemini", "llama", "mistral", "qwen",
	},
	"программирование": {
		"код", "программ", "разработчик", "github", "python", "javascript",
		"typescript", "golang", "rust", "java", "framework", "библиотек", "репозитор",
	},
	"it": {
		"технологи", "стартап", "процессор", "чип", "сервер", "облач",
		"кибербезопасност", "гаджет", "смартфон", "софт",
	},
}

// matchCategory returns the canonical category name from categories that
// matches candidate case-insensitively, or "" if no match is found.
func matchCategory(candidate string, categories []string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	low := strings.ToLower(candidate)
	for _, c := range categories {
		if strings.ToLower(c) == low {
			return c
		}
	}
	return ""
}

// classifyCategory assigns a category to a news item via keyword matching.
// Used as a fallback when the LLM did not provide (or provided an invalid)
// category, and as the sole classifier when the LLM call itself failed.
// Returns "" if nothing matches.
func classifyCategory(title, description string, categories []string) string {
	text := strings.ToLower(title + " " + description)
	for _, c := range categories {
		keywords, ok := categoryKeywords[strings.ToLower(c)]
		if !ok {
			continue
		}
		for _, kw := range keywords {
			if strings.Contains(text, kw) {
				return c
			}
		}
	}
	return ""
}
