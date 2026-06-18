package formatter

import (
	"strings"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

func TestDigestHeader_HTML(t *testing.T) {
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	got := DigestHeader(date, HTML, 10)
	want := "📰 <b>Топ-10 новостей за 25.05.2026</b>"
	if got != want {
		t.Errorf("DigestHeader(HTML) = %q, want %q", got, want)
	}
}

func TestDigestHeader_MarkdownV2(t *testing.T) {
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	got := DigestHeader(date, MarkdownV2, 10)
	// Dots in dates are escaped for MarkdownV2 (this is safe)
	want := "📰 *Топ\\-10 новостей за 25\\.05\\.2026*"
	if got != want {
		t.Errorf("DigestHeader(MarkdownV2) = %q, want %q", got, want)
	}
}

func TestDigestBody_Empty(t *testing.T) {
	got := DigestBody(nil, HTML)
	if got != "" {
		t.Errorf("DigestBody(nil) = %q, want empty", got)
	}
}

func TestDigestBody_SingleItem_HTML(t *testing.T) {
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Тест", Summary: "Описание", Link: "https://example.com"},
	}
	got := DigestBody(items, HTML)
	// HTML uses \n for line breaks between title and summary (Telegram doesn't support <br>)
	want := "1. <b>Тест</b>\nОписание. <a href=\"https://example.com\">Подробнее</a>"
	if got != want {
		t.Errorf("DigestBody(HTML) = %q, want %q", got, want)
	}
}

func TestDigestBody_SingleItemWithoutSummary_HTML(t *testing.T) {
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Тест", Link: "https://example.com"},
	}
	got := DigestBody(items, HTML)
	want := "1. <b>Тест</b>\nКраткое саммари недоступно. <a href=\"https://example.com\">Подробнее</a>"
	if got != want {
		t.Errorf("DigestBody(HTML) = %q, want %q", got, want)
	}
}

func TestDigestBody_SingleItem_MarkdownV2(t *testing.T) {
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Тест", Summary: "Описание", Link: "https://example.com"},
	}
	got := DigestBody(items, MarkdownV2)
	// MarkdownV2 escapes asterisks in titles
	want := "1. *Тест*\nОписание. [Подробнее](https://example.com)"
	if got != want {
		t.Errorf("DigestBody(MD) = %q, want %q", got, want)
	}
}

func TestDigestBody_MultipleItems_HTML(t *testing.T) {
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Первый", Summary: "Саммари 1", Link: "https://a.com"},
		{Rank: 2, Title: "Второй", Summary: "Саммари 2", Link: "https://b.com"},
	}
	got := DigestBody(items, HTML)
	if !strings.Contains(got, "1. <b>Первый</b>") {
		t.Errorf("missing item 1 in %q", got)
	}
	if !strings.Contains(got, "2. <b>Второй</b>") {
		t.Errorf("missing item 2 in %q", got)
	}
	// Should NOT contain </a> for the second item's link
	if strings.Count(got, "<a ") != 2 {
		t.Errorf("expected 2 links in %q", got)
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello world", "hello world"},
		{"hello<br>world", "hello world"},
		{"hello<br/>world", "hello world"},
		{"<b>bold</b> text", "bold text"},
		{"<br><br><br>", ""},
		{"a < b &amp; c", "a < b &amp; c"}, // html entities preserved
	}
	for _, tc := range tests {
		got := stripHTML(tc.input)
		if got != tc.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDigestBody_HTML_StripsTags(t *testing.T) {
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Test <br> News", Summary: "Summary<br/>with <b>tags", Link: "https://example.com"},
	}
	got := DigestBody(items, HTML)
	if strings.Contains(got, "<br") {
		t.Errorf("DigestBody should not contain <br> tags: %q", got)
	}
	if !strings.Contains(got, "<b>Test News</b>") {
		t.Errorf("expected stripped title in %q", got)
	}
	if !strings.Contains(got, "Summary with tags") {
		t.Errorf("expected stripped summary in %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"hello", "hello"},
		{"a & b", "a &amp; b"},
		{"a < b", "a &lt; b"},
		{"a > b", "a &gt; b"},
		{"a \"b\"", "a &#34;b&#34;"}, // Go html.EscapeString uses &#34;
	}
	for _, tc := range tests {
		got := escapeHTML(tc.input)
		if got != tc.want {
			t.Errorf("escapeHTML(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEscapeMD(t *testing.T) {
	input := "Test [link] and _bold_ *star*"
	got := escapeMD(input)
	want := `Test \[link\] and \_bold\_ \*star\*`
	if got != want {
		t.Errorf("escapeMD(%q) = %q, want %q", input, got, want)
	}
}

func TestSafeLink(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "https", input: "https://example.com/a?b=c", want: "https://example.com/a?b=c"},
		{name: "trim", input: " https://example.com/a ", want: "https://example.com/a"},
		{name: "javascript", input: "javascript:alert(1)", want: ""},
		{name: "relative", input: "/news/1", want: ""},
		{name: "control", input: "https://example.com/\nnext", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeLink(tc.input); got != tc.want {
				t.Fatalf("safeLink(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDigest_Full(t *testing.T) {
	date := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	items := []models.RankedNewsItem{
		{Rank: 1, Title: "Test News", Summary: "A summary.", Link: "https://example.com"},
	}
	got := Digest(items, date, HTML, 10)
	if !strings.Contains(got, "Топ-10 новостей за 25.05.2026") {
		t.Errorf("missing date header in %q", got)
	}
	if !strings.Contains(got, "<b>Test News</b>") {
		t.Errorf("missing item title in %q", got)
	}
}

func TestStartMessage_HTML(t *testing.T) {
	got := StartMessage(HTML)
	if !strings.Contains(got, "<b>") {
		t.Errorf("expected bold in HTML start message: %q", got)
	}
	if !strings.Contains(got, "/subscribe") {
		t.Errorf("expected /subscribe in start message: %q", got)
	}
}

func TestStatusMessage_Failed(t *testing.T) {
	got := StatusMessage("fallback", "25.05.2026", 10, HTML)
	if !strings.Contains(got, "⚠️") {
		t.Errorf("expected warning icon for fallback: %q", got)
	}
}

func TestStatusMessage_Success(t *testing.T) {
	got := StatusMessage("success", "25.05.2026", 10, HTML)
	if !strings.Contains(got, "✅") {
		t.Errorf("expected success icon: %q", got)
	}
}
