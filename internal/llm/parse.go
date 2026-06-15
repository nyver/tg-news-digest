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
)

// parseLLMResponse extracts ranked news items from the LLM's free-text response.
// Returns an error if structured parsing yields 0 items so the caller can
// distinguish a real parse failure from an empty-but-valid result.
func parseLLMResponse(response string, originalItems []models.NewsItem, topN int) ([]models.RankedNewsItem, error) {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, fmt.Errorf("llm: empty response")
	}

	ranked := tryParseStructured(response, topN)
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
func tryParseStructured(response string, topN int) []models.RankedNewsItem {
	lines := strings.Split(response, "\n")
	if len(lines) == 0 {
		return nil
	}

	var ranked []models.RankedNewsItem
	var currentRank int
	var currentTitle string
	var currentSummary strings.Builder
	var currentURL string
	inItem := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Detect numbered item start using package-level compiled regex.
		if match := rankRe.FindStringSubmatch(line); match != nil {
			// Save previous item.
			if inItem && currentTitle != "" {
				ranked = append(ranked, models.RankedNewsItem{
					Rank:    currentRank,
					Title:   currentTitle,
					Summary: strings.TrimSpace(currentSummary.String()),
					Link:    currentURL,
				})
			}

			// Start new item.
			inItem = true
			var err error
			currentRank, err = strconv.Atoi(match[1])
			if err != nil {
				currentRank = len(ranked) + 1
			}
			currentTitle = strings.TrimSpace(match[2])
			if len([]rune(currentTitle)) > 80 {
				currentTitle = truncateRunes(currentTitle, 80) + "…"
			}
			currentSummary.Reset()
			currentURL = ""
			continue
		}

		// Accumulate summary and extract URL from body lines.
		if inItem {
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
	}

	// Flush the last item.
	if inItem && currentTitle != "" {
		ranked = append(ranked, models.RankedNewsItem{
			Rank:    currentRank,
			Title:   currentTitle,
			Summary: strings.TrimSpace(currentSummary.String()),
			Link:    currentURL,
		})
	}

	if len(ranked) > topN {
		ranked = ranked[:topN]
	}
	for i := range ranked {
		ranked[i].Rank = i + 1
	}

	return ranked
}

// createFallback generates top-N ranked items from the given list
// sorted by publication time (most recent first).
func createFallback(items []models.NewsItem, topN int) []models.RankedNewsItem {
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
		}
		if len([]rune(desc)) > 200 {
			desc = truncateRunes(desc, 200) + "…"
		}
		result[i] = models.RankedNewsItem{
			Rank:        i + 1,
			Title:       sorted[i].Title,
			Summary:     desc,
			Link:        sorted[i].Link,
			PublishedAt: sorted[i].PublishedAt,
			Source:      extractSourceName(sorted[i].FeedURL),
		}
	}

	return result
}

// enrichRankedItems fills PublishedAt and Source on ranked items by matching
// their links back to the original RSS items.
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
