package llm

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nyver/tg-news-digest/internal/models"
)

var (
	// Matches URLs within text
	urlRe = regexp.MustCompile(`https?://[^\s]+`)
)

// parseLLMResponse extracts ranked news items from the LLM's free-text response.
// It tries to match numbered items and extract title, summary, and URL.
// If parsing fails, falls back to raw top-10 by date.
func parseLLMResponse(response string, originalItems []models.NewsItem) ([]models.RankedNewsItem, error) {
	response = strings.TrimSpace(response)
	if response == "" {
		return nil, fmt.Errorf("llm: empty response")
	}

	// Try structured parsing first
	ranked := tryParseStructured(response)
	if len(ranked) > 0 {
		return ranked, nil
	}

	// Fallback: use the original items sorted by date when parse fails
	slog.Warn("llm: structured parse returned 0 items, falling back to raw top-10",
		slog.String("response_sample", truncateForLog(response, 200)),
		slog.Int("available_items", len(originalItems)),
	)
	return createFallback(originalItems), nil
}

// truncateForLog truncates a string to maxLen for safe logging output.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// tryParseStructured attempts to parse the LLM response into RankedNewsItem slices.
func tryParseStructured(response string) []models.RankedNewsItem {
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

		// Detect numbered item start
		rankMatch := regexp.MustCompile(`^(\d{1,2})\.\s+(.+)`)
		if match := rankMatch.FindStringSubmatch(line); match != nil {
			// Save previous item
			if inItem && currentTitle != "" {
				ranked = append(ranked, models.RankedNewsItem{
					Rank:    currentRank,
					Title:   currentTitle,
					Summary: strings.TrimSpace(currentSummary.String()),
					Link:    currentURL,
				})
			}

			// Start new item
			inItem = true
			var err error
			currentRank, err = strconv.Atoi(match[1])
			if err != nil {
				currentRank = len(ranked) + 1
			}
			currentTitle = strings.TrimSpace(match[2])
			// Limit title length
			if len([]rune(currentTitle)) > 80 {
				currentTitle = truncateRunes(currentTitle, 80) + "…"
			}
			currentSummary.Reset()
			currentURL = ""
			continue
		}

		// If we're in an item, accumulate summary and look for URL
		if inItem {
			// Look for URL in this line
			if urls := urlRe.FindString(line); urls != "" {
				currentURL = urls
			}
			// Accumulate non-URL text as summary
			textWithoutURL := urlRe.ReplaceAllString(line, "")
			textWithoutURL = strings.TrimSpace(textWithoutURL)
			if textWithoutURL != "" && currentSummary.Len() == 0 {
				// First non-empty line after title = summary
				// Remove common prefixes
				textWithoutURL = strings.TrimPrefix(textWithoutURL, "URL:")
				textWithoutURL = strings.TrimPrefix(textWithoutURL, "Ссылка:")
				textWithoutURL = strings.TrimPrefix(textWithoutURL, "Источник:")
				textWithoutURL = strings.TrimSpace(textWithoutURL)
				currentSummary.WriteString(textWithoutURL)
				currentSummary.WriteString(" ")
			}
		}
	}

	// Don't forget the last item
	if inItem && currentTitle != "" {
		ranked = append(ranked, models.RankedNewsItem{
			Rank:    currentRank,
			Title:   currentTitle,
			Summary: strings.TrimSpace(currentSummary.String()),
			Link:    currentURL,
		})
	}

	// Limit to top 10
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}

	// Re-rank sequentially
	for i := range ranked {
		ranked[i].Rank = i + 1
	}

	return ranked
}

// createFallback generates top-N (up to 10) ranked items from the original list
// sorted by publication time (most recent first).
func createFallback(items []models.NewsItem) []models.RankedNewsItem {
	if len(items) == 0 {
		return nil
	}

	sorted := make([]models.NewsItem, len(items))
	copy(sorted, items)
	sortItemsByDate(sorted)

	maxN := 10
	if len(sorted) < maxN {
		maxN = len(sorted)
	}

	result := make([]models.RankedNewsItem, maxN)
	for i := 0; i < maxN; i++ {
		desc := sorted[i].Description
		if desc == "" {
			desc = "Подробнее по ссылке"
		}
		// Truncate for Telegram message length
		if len([]rune(desc)) > 200 {
			desc = truncateRunes(desc, 200) + "…"
		}
		result[i] = models.RankedNewsItem{
			Rank:        i + 1,
			Title:       sorted[i].Title,
			Summary:     desc,
			Link:        sorted[i].Link,
			PublishedAt: sorted[i].PublishedAt,
		}
	}

	return result
}

// sortItemsByDate sorts items in-place by PublishedAt descending (most recent first).
func sortItemsByDate(items []models.NewsItem) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].PublishedAt.After(items[j].PublishedAt)
	})
}
