package llm

import "strings"

// categoryKeywords maps a lowercased category name to keywords used for
// fallback classification when the LLM doesn't return a usable category.
// Only the categories shipped by default are covered; custom categories
// configured by the operator simply won't be matched by keywords and rely
// on the LLM-provided tag instead.
var categoryKeywords = map[string][]string{
	"большие языковые модели": {
		"llm", "большая языковая модель", "языковая модель", "gpt", "chatgpt",
		"claude", "gemini", "llama", "mistral", "qwen", "deepseek",
	},
	"генеративный ии": {
		"генератив", "diffusion", "text-to-image", "text-to-video", "midjourney",
		"stable diffusion", "sora", "dall-e", "генерация изображен", "генерация видео", "генерация музык",
	},
	"научные исследования и публикации": {
		"исследовани", "научн", "статья", "публикаци", "arxiv", "препринт", "paper", "учёны", "ученые",
	},
	"open source модели": {
		"open source", "опен сорс", "открытый исходный код", "huggingface",
		"веса модели", "open-weight", "лицензи", "репозитор",
	},
	"машинное обучение и инструменты": {
		"машинное обучение", "machine learning", "pytorch", "tensorflow", "scikit",
		"датасет", "обучение модели", "фреймворк", "training",
	},
	"компьютерное зрение": {
		"компьютерное зрение", "computer vision", "распознавание изображен",
		"детекция объект", "сегментация изображен", "распознавание лиц",
	},
	"обработка естественного языка": {
		"nlp", "обработка естественного языка", "токенизац", "эмбеддинг", "семантическ", "machine translation",
	},
	"ии-агенты и автоматизация": {
		"ии-агент", "ai-агент", "автономный агент", "агентная", "автоматизаци", "agentic", "ai agent",
	},
	"робототехника": {
		"робот", "робототехник", "дрон", "манипулятор", "беспилотн", "humanoid",
	},
	"ии в продуктах и сервисах": {
		"интегрировал ии", "добавил ии", "ии-функци", "копилот", "ассистент", "ai-функци",
	},
	"бизнес и стартапы": {
		"стартап", "инвестици", "раунд финансирован", "выручка", "оценка компании", "венчурн", "ipo",
	},
	"аппаратное обеспечение и инфраструктура": {
		"процессор", "чип", "gpu", "видеокарт", "дата-центр", "облачн", "сервер", "кластер", "ускоритель",
	},
	"корпоративные новости": {
		"увольнен", "назначил", "слияние", "поглощение", "квартальн", "отчетност", "компания объявила", "генеральный директор",
	},
	"регулирование и право": {
		"закон", "регулирован", "законопроект", "судебн", "иск", "antitrust", "gdpr", "евросоюз", "запрет",
	},
	"этика и безопасность ии": {
		"этик", "безопасност ии", "ai safety", "дезинформац", "предвзятост", "галлюцинаци", "цензур",
	},
	"влияние на общество и рынок труда": {
		"рынок труда", "сокращение рабочих мест", "замена работников", "увольнения из-за ии", "безработиц",
	},
	"разработка и программирование": {
		"код", "программ", "разработчик", "github", "python", "javascript",
		"typescript", "golang", "rust", "java", "framework", "библиотек",
	},
	"мнения и аналитика": {
		"мнение", "колонка", "аналитик", "обзор", "прогноз", "интервью",
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
