package formatter

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

// htmlTagRe matches HTML tags like <br>, <b>, etc.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// ParseMode represents Telegram's message parse mode.
type ParseMode string

const (
	HTML       ParseMode = "HTML"
	MarkdownV2 ParseMode = "MarkdownV2"
)

// Formatter creates Telegram-compatible formatted messages from ranked news.
type Formatter struct {
	mode ParseMode
	topN int
}

// New creates a new Formatter.
func New(mode ParseMode, topN int) *Formatter {
	if topN <= 0 {
		topN = 10
	}
	return &Formatter{mode: mode, topN: topN}
}

// Digest formats a complete digest message using the formatter's mode and topN.
func (f *Formatter) Digest(items []models.RankedNewsItem, date time.Time) string {
	return Digest(items, date, f.mode, f.topN)
}

// DigestParts splits the digest into Telegram-safe chunks (≤4096 runes each).
// The header is always in the first chunk; each news item is kept intact.
// If a single item alone exceeds maxLen it is truncated with an ellipsis.
func (f *Formatter) DigestParts(items []models.RankedNewsItem, date time.Time) []string {
	const maxLen = 4096

	header := DigestHeader(date, f.mode, f.topN)

	// Build per-item strings with correct rank numbers.
	itemStrings := make([]string, len(items))
	for i, item := range items {
		itemStrings[i] = digestBodyItem(item, i+1, f.mode)
		// Guard: a single item that exceeds the limit is truncated.
		if runes := []rune(itemStrings[i]); len(runes) > maxLen {
			itemStrings[i] = string(runes[:maxLen-1]) + "…"
		}
	}

	var parts []string
	current := header
	for _, s := range itemStrings {
		candidate := current + "\n\n" + s
		if len([]rune(candidate)) > maxLen {
			// current is non-empty (at minimum the header); flush it.
			parts = append(parts, current)
			current = s
		} else {
			current = candidate
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// digestBodyItem formats a single ranked item with the given display rank number.
func digestBodyItem(item models.RankedNewsItem, rank int, mode ParseMode) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d. ", rank))
	switch mode {
	case HTML:
		title := stripHTML(item.Title)
		summary := stripHTML(item.Summary)
		sb.WriteString(fmt.Sprintf("<b>%s</b>\n", escapeHTML(title)))
		if summary != "" {
			sb.WriteString(escapeHTML(summary))
		}
		if link := safeLink(item.Link); link != "" {
			sb.WriteString(fmt.Sprintf(". <a href=\"%s\">Подробнее</a>", escapeHTML(link)))
		}
		if meta := buildMeta(item, mode); meta != "" {
			sb.WriteString(fmt.Sprintf("\n<i>%s</i>", meta))
		}
	case MarkdownV2:
		sb.WriteString(fmt.Sprintf("*%s*\n", escapeMD(stripHTML(item.Title))))
		if item.Summary != "" {
			sb.WriteString(escapeMD(item.Summary))
		}
		if link := safeLink(item.Link); link != "" {
			sb.WriteString(fmt.Sprintf(". [Подробнее](%s)", link))
		}
		if meta := buildMeta(item, mode); meta != "" {
			sb.WriteString(fmt.Sprintf("\n_%s_", meta))
		}
	}
	return sb.String()
}

// safeLink returns link only if it uses http/https scheme; otherwise returns "".
func safeLink(link string) string {
	if strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://") {
		return link
	}
	return ""
}

// DigestHeader returns the date header for the digest.
func DigestHeader(date time.Time, mode ParseMode, topN int) string {
	dateStr := date.Format("02.01.2006")
	label := fmt.Sprintf("Топ-%d новостей за %s", topN, dateStr)
	switch mode {
	case HTML:
		return fmt.Sprintf("📰 <b>%s</b>", label)
	case MarkdownV2:
		return fmt.Sprintf("📰 *%s*", escapeMD(label))
	default:
		return fmt.Sprintf("📰 %s", label)
	}
}

// DigestBody formats the ranked news items into a body string.
func DigestBody(items []models.RankedNewsItem, mode ParseMode) string {
	var sb strings.Builder

	for i, item := range items {
		sb.WriteString(fmt.Sprintf("%d. ", i+1))

		switch mode {
		case HTML:
			title := stripHTML(item.Title)
			summary := stripHTML(item.Summary)
			sb.WriteString(fmt.Sprintf("<b>%s</b>\n", escapeHTML(title)))
			if summary != "" {
				sb.WriteString(escapeHTML(summary))
			}
			if link := safeLink(item.Link); link != "" {
				sb.WriteString(fmt.Sprintf(". <a href=\"%s\">Подробнее</a>", escapeHTML(link)))
			}
			if meta := buildMeta(item, mode); meta != "" {
				sb.WriteString(fmt.Sprintf("\n<i>%s</i>", meta))
			}
		case MarkdownV2:
			sb.WriteString(fmt.Sprintf("*%s*\n", escapeMD(item.Title)))
			if item.Summary != "" {
				sb.WriteString(escapeMD(item.Summary))
			}
			if link := safeLink(item.Link); link != "" {
				sb.WriteString(fmt.Sprintf(". [Подробнее](%s)", link))
			}
			if meta := buildMeta(item, mode); meta != "" {
				sb.WriteString(fmt.Sprintf("\n_%s_", meta))
			}
		}

		if i < len(items)-1 {
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// buildMeta returns a "source • date" metadata string for an item, or "" if both are absent.
func buildMeta(item models.RankedNewsItem, mode ParseMode) string {
	var parts []string
	if item.Category != "" {
		parts = append(parts, item.Category)
	}
	if item.Source != "" {
		parts = append(parts, item.Source)
	}
	if !item.PublishedAt.IsZero() {
		parts = append(parts, item.PublishedAt.Format("02.01.2006"))
	}
	if len(parts) == 0 {
		return ""
	}
	meta := strings.Join(parts, " • ")
	if mode == MarkdownV2 {
		meta = escapeMD(meta)
	}
	return meta
}

// Digest formats a complete digest message (header + body).
func Digest(items []models.RankedNewsItem, date time.Time, mode ParseMode, topN int) string {
	return DigestHeader(date, mode, topN) + "\n\n" + DigestBody(items, mode)
}

// StatusMessage formats a digest run status update.
func StatusMessage(runStatus, dateStr string, itemCount int, mode ParseMode) string {
	switch mode {
	case HTML:
		icon := "✅"
		if runStatus == "failed" || runStatus == "fallback" {
			icon = "⚠️"
		}
		return fmt.Sprintf("%s Дайджест за %s: %d новостей (%s)",
			icon, dateStr, itemCount, runStatus)
	case MarkdownV2:
		icon := "✅"
		if runStatus == "failed" || runStatus == "fallback" {
			icon = "⚠️"
		}
		return fmt.Sprintf("%s Дайджест за %s: %d новостей (%s)",
			icon, escapeMD(dateStr), itemCount, escapeMD(runStatus))
	default:
		return fmt.Sprintf("Дайджест за %s: %d новостей (%s)", dateStr, itemCount, runStatus)
	}
}

// StartMessage returns the /start greeting.
func StartMessage(mode ParseMode) string {
	switch mode {
	case HTML:
		return `<b>Привет! Я бот дайджеста новостей.</b>
Отправь <code>/subscribe</code> чтобы подписаться на ежедневный дайджест.
Отправь <code>/unsubscribe</code> чтобы отписаться.
Отправь <code>/categories</code> чтобы выбрать интересующие темы.`
	case MarkdownV2:
		return `*Привет! Я бот дайджеста новостей.*
Отправь /subscribe чтобы подписаться на ежедневный дайджест.
Отправь /unsubscribe чтобы отписаться.
Отправь /categories чтобы выбрать интересующие темы.`
	default:
		return "Привет! Я бот дайджеста новостей.\n\nОтправь /subscribe чтобы подписаться на ежедневный дайджест.\nОтправь /unsubscribe чтобы отписаться.\nОтправь /categories чтобы выбрать интересующие темы."
	}
}

// SubscribedMessage returns a subscription confirmation.
func SubscribedMessage(mode ParseMode) string {
	return "✅ Вы подписались на ежедневный дайджест новостей."
}

// UnsubscribedMessage returns an unsubscription confirmation.
func UnsubscribedMessage(mode ParseMode) string {
	return "❌ Вы отписались от дайджеста новостей."
}

// UnknownCommandMessage returns an unknown command handler.
func UnknownCommandMessage(mode ParseMode) string {
	return "❓ Неизвестная команда. Используйте /start для справки."
}

// --- Helpers ---

// stripHTML removes all HTML tags from a string.
func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// escapeHTML escapes HTML special characters.
func escapeHTML(s string) string {
	return html.EscapeString(s)
}

// escapeMD escapes MarkdownV2 special characters.
func escapeMD(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
			sb.WriteString("\\")
			sb.WriteRune(r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
