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
}

// New creates a new Formatter.
func New(mode ParseMode) *Formatter {
	return &Formatter{mode: mode}
}

// DigestHeader returns the date header for the digest.
func DigestHeader(date time.Time, mode ParseMode) string {
	dateStr := date.Format("02.01.2006")
	switch mode {
	case HTML:
		return fmt.Sprintf("📰 <b>Топ-10 новостей за %s</b>", dateStr)
	case MarkdownV2:
		return fmt.Sprintf("📰 *Топ-10 новостей за %s*", escapeMD(dateStr))
	default:
		return fmt.Sprintf("📰 Топ-10 новостей за %s", dateStr)
	}
}

// DigestBody formats the ranked news items into a body string.
func DigestBody(items []models.RankedNewsItem, mode ParseMode) string {
	var sb strings.Builder

	for i, item := range items {
		sb.WriteString(fmt.Sprintf("%d. ", i+1))

		switch mode {
		case HTML:
			// Strip any HTML tags from LLM/RSS content that Telegram doesn't support
			title := stripHTML(item.Title)
			summary := stripHTML(item.Summary)
			sb.WriteString(fmt.Sprintf("<b>%s</b>\n", escapeHTML(title)))
			if summary != "" {
				sb.WriteString(escapeHTML(summary))
			}
			if item.Link != "" {
				sb.WriteString(fmt.Sprintf(". <a href=\"%s\">Подробнее</a>", escapeHTML(item.Link)))
			}
		case MarkdownV2:
			sb.WriteString(fmt.Sprintf("*%s*", escapeMD(item.Title)))
			sb.WriteString("\n")
			if item.Summary != "" {
				sb.WriteString(escapeMD(item.Summary))
			}
			if item.Link != "" {
				sb.WriteString(fmt.Sprintf(". [Подробнее](%s)", item.Link))
			}
		}

		if i < len(items)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// Digest formats a complete digest message (header + body).
func Digest(items []models.RankedNewsItem, date time.Time, mode ParseMode) string {
	return DigestHeader(date, mode) + "\n\n" + DigestBody(items, mode)
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
Отправь <code>/subscribe</code> чтобы подписаться на ежедневный дайджест топ-10 новостей.
Отправь <code>/unsubscribe</code> чтобы отписаться.`
	case MarkdownV2:
		return `*Привет! Я бот дайджеста новостей.*
Отправь /subscribe чтобы подписаться на ежедневный дайджест топ-10 новостей.
Отправь /unsubscribe чтобы отписаться.`
	default:
		return "Привет! Я бот дайджеста новостей.\n\nОтправь /subscribe чтобы подписаться на ежедневный дайджест топ-10 новостей.\nОтправь /unsubscribe чтобы отписаться."
	}
}

// SubscribedMessage returns a subscription confirmation.
func SubscribedMessage(mode ParseMode) string {
	switch mode {
	case HTML:
		return "✅ Вы подписались на ежедневный дайджест новостей."
	case MarkdownV2:
		return "✅ Вы подписались на ежедневный дайджест новостей."
	default:
		return "✅ Вы подписались на ежедневный дайджест новостей."
	}
}

// UnsubscribedMessage returns an unsubscription confirmation.
func UnsubscribedMessage(mode ParseMode) string {
	switch mode {
	case HTML:
		return "❌ Вы отписались от дайджеста новостей."
	case MarkdownV2:
		return "❌ Вы отписались от дайджеста новостей."
	default:
		return "❌ Вы отписались от дайджеста новостей."
	}
}

// UnknownCommandMessage returns an unknown command handler.
func UnknownCommandMessage(mode ParseMode) string {
	switch mode {
	case HTML:
		return "❓ Неизвестная команда. Используйте /start для справки."
	case MarkdownV2:
		return "❓ Неизвестная команда. Используйте /start для справки."
	default:
		return "❓ Неизвестная команда. Используйте /start для справки."
	}
}

// --- Helpers ---

// stripHTML removes all HTML tags from a string.
func stripHTML(s string) string {
	// Replace tags with space to preserve word separation
	s = htmlTagRe.ReplaceAllString(s, " ")
	// Normalize whitespace
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
