package llm

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nyver/tg-news-digest/internal/models"
)

var (
	// urlRe matches URLs within text.
	urlRe = regexp.MustCompile(`https?://[^\s]+`)

	// rankRe matches numbered list items like "1. Title" or "10. Title".
	rankRe = regexp.MustCompile(`^(\d{1,2})\.\s+(.+)`)

	// trailingPunct are characters that LLMs commonly append after a URL.
	trailingPunct = `.,;:)'"` + "`"

	// sentenceEndRe matches sentence-terminating punctuation followed by
	// whitespace or end of string, used to build short fallback summaries.
	sentenceEndRe = regexp.MustCompile(`[.!?…]+(\s|$)`)

	// categoryPrefixRe matches the "Категория:"/"Category:" label
	// case-insensitively — models don't reliably preserve the exact casing
	// shown in the prompt's format example.
	categoryPrefixRe = regexp.MustCompile(`(?i)^(?:Категория|Category):\s*`)
)

// firstSentences returns the first maxSentences sentences of text (split on
// ., !, ?, …), capped at maxChars runes with an ellipsis if still too long.
// Used to build a short 1-2 sentence summary when no LLM-generated one is
// available (i.e. in the no-LLM fallback paths).
func firstSentences(text string, maxSentences, maxChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	cut := text
	if matches := sentenceEndRe.FindAllStringIndex(text, -1); len(matches) > 0 {
		idx := len(matches) - 1
		if len(matches) > maxSentences {
			idx = maxSentences - 1
		}
		cut = strings.TrimSpace(text[:matches[idx][1]])
	}

	if len([]rune(cut)) > maxChars {
		return truncateRunes(cut, maxChars) // truncateRunes already appends an ellipsis.
	}
	return cut
}

// parseLLMResponse extracts ranked news items from the LLM's free-text response.
// Returns an error if structured parsing yields 0 items so the caller can
// distinguish a real parse failure from an empty-but-valid result.
func parseLLMResponse(response string, originalItems []models.NewsItem, topN int, categories []string) ([]models.RankedNewsItem, error) {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, fmt.Errorf("llm: empty response")
	}

	ranked := tryParseStructured(response, topN, categories)
	if len(ranked) > 0 {
		return enrichRankedItems(ranked, originalItems), nil
	}

	slog.Warn("llm: structured parse returned 0 items",
		slog.String("response_sample", truncateForLog(response, 200)),
		slog.Int("available_items", len(originalItems)),
		slog.Int("top_n", topN),
	)
	return nil, fmt.Errorf("llm: structured parse returned 0 items")
}

// truncateForLog truncates a string to maxLen for safe logging output.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// tryParseStructured attempts to parse the LLM response into RankedNewsItem slices.
// Each item's category is taken from an explicit "Категория:" line if present and
// valid; otherwise it is filled in via keyword classification against categories.
func tryParseStructured(response string, topN int, categories []string) []models.RankedNewsItem {
	lines := strings.Split(response, "\n")
	if len(lines) == 0 {
		return nil
	}

	var ranked []models.RankedNewsItem
	var currentRank int
	var currentTitle string
	var currentSummary strings.Builder
	var currentURL string
	var currentCategory string
	inItem := false

	flush := func() {
		if !inItem || currentTitle == "" {
			return
		}
		summary := firstSentences(currentSummary.String(), 2, 240)
		category := matchCategory(currentCategory, categories)
		if category == "" {
			category = classifyCategory(currentTitle, summary, categories)
		}
		ranked = append(ranked, models.RankedNewsItem{
			Rank:     currentRank,
			Title:    currentTitle,
			Summary:  summary,
			Link:     currentURL,
			Category: category,
		})
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Detect numbered item start using package-level compiled regex.
		if match := rankRe.FindStringSubmatch(line); match != nil {
			flush()

			// Start new item.
			inItem = true
			var err error
			currentRank, err = strconv.Atoi(match[1])
			if err != nil {
				currentRank = len(ranked) + 1
			}
			title, summary, inlineURL := splitInlineRankContent(match[2])
			currentTitle = title
			if len([]rune(currentTitle)) > 80 {
				currentTitle = truncateRunes(currentTitle, 80) + "…"
			}
			currentSummary.Reset()
			if summary != "" {
				currentSummary.WriteString(summary)
			}
			currentURL = inlineURL
			currentCategory = ""
			continue
		}

		if !inItem {
			continue
		}

		// Detect an explicit category line, e.g. "Категория: AI".
		if rest, ok := stripCategoryPrefix(line); ok {
			currentCategory = rest
			continue
		}

		// Accumulate summary and extract URL from body lines.
		if u := urlRe.FindString(line); u != "" {
			currentURL = strings.TrimRight(u, trailingPunct)
		}
		// Strip URLs, then clean up label remnants.
		text := urlRe.ReplaceAllString(line, "")
		text = strings.TrimSpace(text)
		text = strings.TrimPrefix(text, "URL:")
		text = strings.TrimPrefix(text, "Ссылка:")
		text = strings.TrimPrefix(text, "Источник:")
		text = strings.TrimSuffix(text, "URL:")
		text = strings.TrimSuffix(text, "Ссылка:")
		// Remove stray "URL:" fragments in the middle of the text.
		text = strings.ReplaceAll(text, " URL:", "")
		text = strings.TrimSpace(text)
		if text != "" {
			if currentSummary.Len() > 0 {
				currentSummary.WriteString(" ")
			}
			currentSummary.WriteString(text)
		}
	}

	// Flush the last item.
	flush()

	if len(ranked) > topN {
		ranked = ranked[:topN]
	}
	for i := range ranked {
		ranked[i].Rank = i + 1
	}

	return ranked
}

func splitInlineRankContent(content string) (title, summary, link string) {
	content = strings.TrimSpace(content)
	if u := urlRe.FindString(content); u != "" {
		link = strings.TrimRight(u, trailingPunct)
		content = strings.TrimSpace(urlRe.ReplaceAllString(content, ""))
		content = strings.TrimSuffix(strings.TrimSpace(content), "URL:")
		content = strings.TrimSuffix(strings.TrimSpace(content), "Ссылка:")
		content = strings.TrimSuffix(strings.TrimSpace(content), "Источник:")
		content = strings.TrimSpace(content)
	}

	for _, sep := range []string{" — ", " – ", " - "} {
		if before, after, ok := strings.Cut(content, sep); ok {
			title = strings.TrimSpace(before)
			summary = strings.TrimSpace(after)
			return title, summary, link
		}
	}

	return content, "", link
}

// stripCategoryPrefix returns the value after a "Категория:"/"Category:" label
// and true, or "", false if line does not start with such a label. Matching
// is case-insensitive since models don't reliably preserve exact casing.
func stripCategoryPrefix(line string) (string, bool) {
	if loc := categoryPrefixRe.FindStringIndex(line); loc != nil {
		return strings.TrimSpace(line[loc[1]:]), true
	}
	return "", false
}

// createFallback generates top-N ranked items from the given list
// sorted by publication time (most recent first).
func createFallback(items []models.NewsItem, topN int, categories []string) []models.RankedNewsItem {
	if len(items) == 0 {
		return nil
	}

	sorted := make([]models.NewsItem, len(items))
	copy(sorted, items)
	sortItemsByDate(sorted)

	maxN := topN
	if len(sorted) < maxN {
		maxN = len(sorted)
	}

	result := make([]models.RankedNewsItem, maxN)
	for i := 0; i < maxN; i++ {
		desc := sorted[i].Description
		if desc == "" {
			desc = "Подробнее по ссылке"
		} else {
			desc = firstSentences(desc, 2, 200)
		}
		result[i] = models.RankedNewsItem{
			Rank:        i + 1,
			Title:       sorted[i].Title,
			Summary:     desc,
			Link:        sorted[i].Link,
			PublishedAt: sorted[i].PublishedAt,
			Source:      extractSourceName(sorted[i].FeedURL),
			Category:    classifyCategory(sorted[i].Title, sorted[i].Description, categories),
		}
	}

	return result
}

// topUpRanked fills ranked out to topN using additional items from the full
// original list (already sorted by date, most recent first) that the LLM
// didn't already select. This guards against the LLM under-delivering (e.g.
// returning 12 items when topN=20 because it judged the rest unimportant or
// duplicate) when there is in fact enough fresh material to fill the quota.
// Items are deduplicated by link; Rank is renumbered across the full result.
func topUpRanked(ranked []models.RankedNewsItem, all []models.NewsItem, topN int, categories []string) []models.RankedNewsItem {
	if len(ranked) >= topN {
		return ranked
	}

	used := make(map[string]bool, len(ranked))
	for _, r := range ranked {
		if r.Link != "" {
			used[r.Link] = true
		}
	}

	for _, item := range all {
		if len(ranked) >= topN {
			break
		}
		if item.Link == "" || used[item.Link] {
			continue
		}
		used[item.Link] = true

		desc := item.Description
		if desc == "" {
			desc = "Подробнее по ссылке"
		} else {
			desc = firstSentences(desc, 2, 200)
		}
		ranked = append(ranked, models.RankedNewsItem{
			Title:       item.Title,
			Summary:     desc,
			Link:        item.Link,
			PublishedAt: item.PublishedAt,
			Source:      extractSourceName(item.FeedURL),
			Category:    classifyCategory(item.Title, item.Description, categories),
		})
	}

	for i := range ranked {
		ranked[i].Rank = i + 1
	}
	return ranked
}

// enrichRankedItems fills PublishedAt, Source and (if missing) Summary on
// ranked items by matching their links back to the original RSS items.
// A missing Summary means the LLM either skipped it for that item or merged
// it into the title line in a way the parser couldn't split out — in both
// cases we backfill a short summary from the original RSS description rather
// than show the item with no descriptive text at all.
func enrichRankedItems(ranked []models.RankedNewsItem, originals []models.NewsItem) []models.RankedNewsItem {
	byURL := make(map[string]models.NewsItem, len(originals))
	for _, item := range originals {
		byURL[item.Link] = item
	}
	for i := range ranked {
		orig, ok := byURL[ranked[i].Link]
		if !ok {
			continue
		}
		if ranked[i].PublishedAt.IsZero() {
			ranked[i].PublishedAt = orig.PublishedAt
		}
		if ranked[i].Source == "" {
			ranked[i].Source = extractSourceName(orig.FeedURL)
		}
		if ranked[i].Summary == "" {
			if orig.Description != "" {
				ranked[i].Summary = firstSentences(orig.Description, 2, 200)
			} else {
				ranked[i].Summary = "Подробнее по ссылке"
			}
		}
	}
	return ranked
}

// extractSourceName returns the hostname from a feed URL, stripping "www.".
func extractSourceName(feedURL string) string {
	if feedURL == "" {
		return ""
	}
	u, err := url.Parse(feedURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}

// sortItemsByDate sorts items in-place by PublishedAt descending (most recent first).
func sortItemsByDate(items []models.NewsItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].PublishedAt.After(items[j].PublishedAt)
	})
}
