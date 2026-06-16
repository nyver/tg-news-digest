package llm

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

func TestParseLLMResponse_Valid(t *testing.T) {
	response := `1. Первая новость
   Краткое описание первой новости. URL: https://example.com/1
2. Вторая новость
   Описание второй. URL: https://example.com/2
3. Третья новость
   Третье описание. URL: https://example.com/3`

	items := []models.NewsItem{
		{ID: "1", Title: "Title 1", Link: "https://example.com/1"},
	}

	ranked, err := parseLLMResponse(response, items, 10, nil)
	if err != nil {
		t.Fatalf("parseLLMResponse error: %v", err)
	}
	if len(ranked) != 3 {
		t.Fatalf("expected 3 items, got %d", len(ranked))
	}
	if ranked[0].Rank != 1 {
		t.Errorf("expected rank 1, got %d", ranked[0].Rank)
	}
	if ranked[0].Title != "Первая новость" {
		t.Errorf("expected 'Первая новость', got %q", ranked[0].Title)
	}
	if ranked[0].Link != "https://example.com/1" {
		t.Errorf("expected link, got %q", ranked[0].Link)
	}
}

func TestParseLLMResponse_LimitedTo10(t *testing.T) {
	var response strings.Builder
	for i := 1; i <= 15; i++ {
		response.WriteString(strconv.Itoa(i) + ". Новость " + strconv.Itoa(i) + "\n   Описание " + strconv.Itoa(i) + ". URL: https://example.com/" + strconv.Itoa(i) + "\n")
	}

	items := []models.NewsItem{}
	ranked, err := parseLLMResponse(response.String(), items, 10, nil)
	if err != nil {
		t.Fatalf("parseLLMResponse error: %v", err)
	}
	if len(ranked) != 10 {
		t.Errorf("expected 10 items max, got %d", len(ranked))
	}
	if ranked[9].Rank != 10 {
		t.Errorf("expected last rank 10, got %d", ranked[9].Rank)
	}
}

func TestParseLLMResponse_Empty(t *testing.T) {
	items := []models.NewsItem{{ID: "1", Title: "T", Link: "http://x"}}
	_, err := parseLLMResponse("", items, 10, nil)
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestParseLLMResponse_NoNumberedItems(t *testing.T) {
	response := `This is just plain text without any numbered items.`
	items := []models.NewsItem{
		{ID: "1", Title: "Fallback 1", Link: "http://a", PublishedAt: time.Now()},
		{ID: "2", Title: "Fallback 2", Link: "http://b", PublishedAt: time.Now().Add(-1 * time.Hour)},
	}

	// parseLLMResponse now returns an error when structured parse yields 0 items;
	// the caller (RankWithLLM) is responsible for calling createFallback with llmUsed=false.
	ranked, err := parseLLMResponse(response, items, 10, nil)
	if err == nil {
		t.Fatal("expected error when response has no numbered items")
	}
	if ranked != nil {
		t.Errorf("expected nil ranked on parse error, got %d items", len(ranked))
	}
}

func TestParseLLMResponse_WithCategory(t *testing.T) {
	response := `1. Новая модель GPT
   Описание про языковую модель. URL: https://example.com/1
   Категория: LLM
2. Релиз Linux ядра
   Описание про разработку. URL: https://example.com/2
   Категория: IT`

	items := []models.NewsItem{}
	categories := []string{"AI", "LLM", "Программирование", "IT"}

	ranked, err := parseLLMResponse(response, items, 10, categories)
	if err != nil {
		t.Fatalf("parseLLMResponse error: %v", err)
	}
	if len(ranked) != 2 {
		t.Fatalf("expected 2 items, got %d", len(ranked))
	}
	if ranked[0].Category != "LLM" {
		t.Errorf("expected category LLM, got %q", ranked[0].Category)
	}
	if ranked[1].Category != "IT" {
		t.Errorf("expected category IT, got %q", ranked[1].Category)
	}
	if strings.Contains(ranked[0].Summary, "Категория") {
		t.Errorf("category line leaked into summary: %q", ranked[0].Summary)
	}
}

func TestParseLLMResponse_InvalidCategory_FallsBackToKeyword(t *testing.T) {
	response := `1. Новая большая языковая модель GPT представлена компанией
   Подробности о llm модели. URL: https://example.com/1
   Категория: Развлечения`

	categories := []string{"AI", "LLM", "Программирование", "IT"}

	ranked, err := parseLLMResponse(response, nil, 10, categories)
	if err != nil {
		t.Fatalf("parseLLMResponse error: %v", err)
	}
	if len(ranked) != 1 {
		t.Fatalf("expected 1 item, got %d", len(ranked))
	}
	if ranked[0].Category != "LLM" {
		t.Errorf("expected keyword-classified category LLM, got %q", ranked[0].Category)
	}
}

func TestClassifyCategory(t *testing.T) {
	categories := []string{"AI", "LLM", "Программирование", "IT"}

	if got := classifyCategory("Вышла новая языковая модель Gemini", "", categories); got != "LLM" {
		t.Errorf("expected LLM, got %q", got)
	}
	if got := classifyCategory("Разработчики выложили код на GitHub", "", categories); got != "Программирование" {
		t.Errorf("expected Программирование, got %q", got)
	}
	if got := classifyCategory("Совершенно не по теме", "", categories); got != "" {
		t.Errorf("expected no match, got %q", got)
	}
}

func TestMatchCategory(t *testing.T) {
	categories := []string{"AI", "LLM", "Программирование", "IT"}

	if got := matchCategory("llm", categories); got != "LLM" {
		t.Errorf("expected case-insensitive match LLM, got %q", got)
	}
	if got := matchCategory("Развлечения", categories); got != "" {
		t.Errorf("expected no match for unknown category, got %q", got)
	}
	if got := matchCategory("", categories); got != "" {
		t.Errorf("expected empty for empty candidate, got %q", got)
	}
}

func TestCreateFallback(t *testing.T) {
	items := []models.NewsItem{
		{ID: "1", Title: "Old", Link: "http://a", PublishedAt: time.Now().Add(-5 * time.Hour)},
		{ID: "2", Title: "Recent", Link: "http://b", PublishedAt: time.Now().Add(-1 * time.Hour)},
		{ID: "3", Title: "Mid", Link: "http://c", PublishedAt: time.Now().Add(-3 * time.Hour)},
	}

	fb := createFallback(items, 10, nil)
	if len(fb) != 3 {
		t.Fatalf("expected 3 fallback items, got %d", len(fb))
	}
	// Most recent first
	if fb[0].Title != "Recent" {
		t.Errorf("expected 'Recent' first, got %q", fb[0].Title)
	}
	if fb[1].Title != "Mid" {
		t.Errorf("expected 'Mid' second, got %q", fb[1].Title)
	}
	if fb[2].Title != "Old" {
		t.Errorf("expected 'Old' third, got %q", fb[2].Title)
	}
}

func TestCreateFallback_Empty(t *testing.T) {
	fb := createFallback(nil, 10, nil)
	if fb != nil {
		t.Errorf("expected nil for empty input, got %d items", len(fb))
	}
}

func TestCreateFallback_Limit10(t *testing.T) {
	var items []models.NewsItem
	for i := 0; i < 20; i++ {
		items = append(items, models.NewsItem{
			ID:          string(rune('0' + i)),
			Title:       "Title",
			Link:        "http://x",
			PublishedAt: time.Now().Add(time.Duration(-i) * time.Hour),
		})
	}

	fb := createFallback(items, 10, nil)
	if len(fb) != 10 {
		t.Errorf("expected 10 fallback items, got %d", len(fb))
	}
}

func TestSortItemsByDate(t *testing.T) {
	items := []models.NewsItem{
		{PublishedAt: time.Now().Add(-3 * time.Hour)},
		{PublishedAt: time.Now()},
		{PublishedAt: time.Now().Add(-1 * time.Hour)},
	}

	sortItemsByDate(items)

	if !items[0].PublishedAt.After(items[1].PublishedAt) {
		t.Error("expected items sorted by date descending")
	}
	if !items[1].PublishedAt.After(items[2].PublishedAt) {
		t.Error("expected items sorted by date descending")
	}
}

func TestTruncateForContext(t *testing.T) {
	longDesc := strings.Repeat("x", 500)
	items := []models.NewsItem{
		{Title: "T", Description: longDesc, Link: "http://x"},
	}

	result := truncateForContext(items, 100)
	if len(result) != 1 {
		t.Errorf("expected 1 item, got %d", len(result))
	}
	// truncateRunes limits to 150 runes + ellipsis, but with char limit of 100,
	// items should be further trimmed
	if len(result[0].Description) > 200 {
		t.Errorf("description too long after truncation: %d chars", len(result[0].Description))
	}
}

func TestTruncateRunes(t *testing.T) {
	s := "Привет мир"
	got := truncateRunes(s, 5)
	// 5 runes: П, р, и, в, е + … would be 6, so it truncates to 5 runes: "Приве…"
	want := "Приве…"
	if got != want {
		t.Errorf("truncateRunes(%q, 5) = %q, want %q", s, got, want)
	}
}

func TestBuildUserPrompt(t *testing.T) {
	items := []models.NewsItem{
		{Title: "Title 1", Description: "Desc 1", Link: "https://example.com/1"},
		{Title: "Title 2", Description: "Desc 2", Link: "https://example.com/2"},
	}

	prompt := buildUserPrompt(items, "25.05.2026", 10)
	if !strings.Contains(prompt, "25.05.2026") {
		t.Error("prompt missing date")
	}
	if !strings.Contains(prompt, "Title 1") {
		t.Error("prompt missing item 1 title")
	}
	if !strings.Contains(prompt, "https://example.com/1") {
		t.Error("prompt missing item 1 link")
	}
}

// --- helpers ---

func formatListItem(rank int, prefix string, num int) string {
	return strconv.Itoa(rank) + ". " + prefix + " " + strconv.Itoa(num) + "\n   Описание. URL: https://example.com/" + strconv.Itoa(num) + "\n"
}
