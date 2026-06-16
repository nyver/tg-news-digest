package llm

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nyver/tg-news-digest/internal/models"
)

// headerPrefixRe matches the "ЗАГОЛОВОК:"/"HEADER:" label case-insensitively —
// models often mirror the casing used in the user prompt's "Заголовок:" label
// instead of the all-caps form requested in the system prompt.
var headerPrefixRe = regexp.MustCompile(`(?i)^(?:ЗАГОЛОВОК|HEADER):\s*`)

// isDefaultLanguage reports whether lang means "no translation needed" — the
// digest is already produced in Russian by the ranking step.
func isDefaultLanguage(lang string) bool {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "", "ru", "rus", "russian", "русский":
		return true
	default:
		return false
	}
}

// TranslateDigest translates an already-ranked Russian digest (titles,
// summaries and the header label) into targetLang via a single LLM call.
// Links, categories, sources and publish dates are left untouched — only the
// human-readable text is translated. On any failure it returns the original
// (Russian) items/header unchanged along with the error, so callers can fall
// back gracefully instead of failing the whole send.
func (c *Client) TranslateDigest(ctx context.Context, items []models.RankedNewsItem, header, targetLang string) ([]models.RankedNewsItem, string, error) {
	if len(items) == 0 || isDefaultLanguage(targetLang) {
		return items, header, nil
	}

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
		Model: c.cfg.Model,
		Messages: []Message{
			{Role: "system", Content: buildTranslateSystemPrompt(targetLang)},
			{Role: "user", Content: buildTranslateUserPrompt(items, header)},
		},
		Temperature: c.cfg.Temperature,
		MaxTokens:   c.cfg.MaxTokens,
	}

	content, err := c.provider.Chat(ctx, req)
	if err != nil {
		c.logger.Warn("llm: translate request failed, keeping original language",
			slog.String("target_language", targetLang), slog.String("error", err.Error()))
		return items, header, err
	}

	c.logger.Debug("llm: translate raw response", slog.String("response", content))

	translatedHeader, translatedItems, ok := parseTranslatedDigest(content, len(items))
	if !ok {
		c.logger.Warn("llm: translate parse failed, keeping original language", slog.String("target_language", targetLang))
		return items, header, fmt.Errorf("llm: translate parse failed")
	}

	result := make([]models.RankedNewsItem, len(items))
	copy(result, items)
	for i := range result {
		if translatedItems[i].title != "" {
			result[i].Title = translatedItems[i].title
		}
		if translatedItems[i].summary != "" {
			result[i].Summary = translatedItems[i].summary
		}
	}

	if translatedHeader == "" {
		translatedHeader = header
	}

	return result, translatedHeader, nil
}

// buildTranslateSystemPrompt instructs the LLM to act as a faithful translator
// rather than re-ranking or re-summarizing the digest.
func buildTranslateSystemPrompt(targetLang string) string {
	return fmt.Sprintf(`You are a precise translator. Translate the given Russian news digest into %s.
Preserve the meaning exactly — do not summarize further, do not add or remove information, do not add your own comments.
Keep exactly the same number of items, in the same order, numbered the same way as the input.

Output format — first line is the translated header, then a numbered list matching the input numbering exactly:
ЗАГОЛОВОК: <translated header>
1. <translated title>
   <translated summary>
2. <translated title>
   <translated summary>

Output nothing else — no introductions, no explanations, no markup beyond what is shown above.`, targetLang)
}

// buildTranslateUserPrompt assembles the user message containing the header
// and the original (Russian) items to translate.
func buildTranslateUserPrompt(items []models.RankedNewsItem, header string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Заголовок: %s\n\n", header))
	for i, item := range items {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, item.Title))
		if item.Summary != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", item.Summary))
		}
	}
	return sb.String()
}

// translatedItem holds the translated title/summary for one ranked item.
type translatedItem struct {
	title   string
	summary string
}

// parseTranslatedDigest extracts the translated header and per-rank
// title/summary pairs from the LLM response. expected is the number of items
// that were sent for translation — entries beyond the recognized ranks are
// left blank so the caller can fall back to the original text per-item.
// Returns ok=false only if nothing recognizable was found at all.
func parseTranslatedDigest(response string, expected int) (string, []translatedItem, bool) {
	lines := strings.Split(strings.TrimSpace(response), "\n")
	result := make([]translatedItem, expected)
	var header string
	currentIdx := -1
	var summary strings.Builder
	matchedAny := false

	flush := func() {
		if currentIdx >= 0 && currentIdx < expected {
			result[currentIdx].summary = strings.TrimSpace(summary.String())
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if rest, found := stripHeaderPrefix(line); found {
			header = rest
			continue
		}

		if m := rankRe.FindStringSubmatch(line); m != nil {
			flush()
			idx, err := strconv.Atoi(m[1])
			if err != nil {
				currentIdx = -1
				continue
			}
			currentIdx = idx - 1
			summary.Reset()
			if currentIdx >= 0 && currentIdx < expected {
				result[currentIdx].title = strings.TrimSpace(m[2])
				matchedAny = true
			}
			continue
		}

		if currentIdx >= 0 {
			if summary.Len() > 0 {
				summary.WriteString(" ")
			}
			summary.WriteString(line)
		}
	}
	flush()

	if !matchedAny && header == "" {
		return "", nil, false
	}
	return header, result, true
}

// stripHeaderPrefix returns the value after a "ЗАГОЛОВОК:"/"HEADER:" label and
// true, or "", false if line does not start with such a label. Matching is
// case-insensitive since models don't reliably preserve the all-caps form.
func stripHeaderPrefix(line string) (string, bool) {
	if loc := headerPrefixRe.FindStringIndex(line); loc != nil {
		return strings.TrimSpace(line[loc[1]:]), true
	}
	return "", false
}
