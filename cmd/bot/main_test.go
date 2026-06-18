package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/llm"
	"github.com/nyver/tg-news-digest/internal/rss"
	"github.com/nyver/tg-news-digest/internal/storage"
)

func TestDigestFetchSince_CommandStartsAtCurrentDay(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	now := time.Date(2026, 6, 17, 21, 29, 45, 0, loc)
	lastCron := now.Add(-3 * time.Minute)

	got := digestFetchSince(now, loc, "command", &lastCron)
	want := time.Date(2026, 6, 17, 0, 0, 0, 0, loc)

	if !got.Equal(want) {
		t.Fatalf("command fetch since = %s, want %s", got, want)
	}
}

func TestDigestFetchSince_CronUsesLastCronRun(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	now := time.Date(2026, 6, 17, 21, 29, 45, 0, loc)
	lastCron := time.Date(2026, 6, 17, 9, 0, 0, 0, loc)

	got := digestFetchSince(now, loc, "cron", &lastCron)

	if !got.Equal(lastCron) {
		t.Fatalf("cron fetch since = %s, want %s", got, lastCron)
	}
}

func TestDigestFetchSince_CronDefaultsToLast24Hours(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	now := time.Date(2026, 6, 17, 21, 29, 45, 0, loc)

	got := digestFetchSince(now, loc, "cron", nil)
	want := now.Add(-24 * time.Hour)

	if !got.Equal(want) {
		t.Fatalf("cron fetch since = %s, want %s", got, want)
	}
}

func TestMaxDigestTopN(t *testing.T) {
	if got := maxDigestTopN(10); got != 20 {
		t.Fatalf("maxDigestTopN(10) = %d, want 20", got)
	}
	if got := maxDigestTopN(25); got != 25 {
		t.Fatalf("maxDigestTopN(25) = %d, want 25", got)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"nope":  slog.LevelInfo,
	}
	for input, want := range tests {
		if got := parseLogLevel(input); got != want {
			t.Fatalf("parseLogLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestSetupLogger_WithFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.log")
	logger, file := setupLogger(config.AppConfig{DigestLogPath: path, LogLevel: "debug"})
	if file == nil {
		t.Fatal("expected log file to be opened")
	}
	defer file.Close()

	logger.Debug("test log")
	if err := file.Sync(); err != nil {
		t.Fatalf("sync log file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("expected log file to contain records")
	}
}

type testHandler struct {
	enabled bool
	err     error
	attrs   []slog.Attr
	group   string
	handled int
}

func (h *testHandler) Enabled(context.Context, slog.Level) bool { return h.enabled }
func (h *testHandler) Handle(context.Context, slog.Record) error {
	h.handled++
	return h.err
}
func (h *testHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(cp.attrs, attrs...)
	return &cp
}
func (h *testHandler) WithGroup(name string) slog.Handler {
	cp := *h
	cp.group = name
	return &cp
}

func TestMultiHandler(t *testing.T) {
	errOne := errors.New("one")
	h1 := &testHandler{enabled: false, err: errOne}
	h2 := &testHandler{enabled: true}
	m := &multiHandler{handlers: []slog.Handler{h1, h2}}

	if !m.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("expected enabled when any child handler is enabled")
	}
	if err := m.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)); !errors.Is(err, errOne) {
		t.Fatalf("expected joined handler error, got %v", err)
	}
	if h1.handled != 1 || h2.handled != 1 {
		t.Fatalf("expected both handlers to be called, got %d/%d", h1.handled, h2.handled)
	}
	if _, ok := m.WithAttrs([]slog.Attr{slog.String("k", "v")}).(*multiHandler); !ok {
		t.Fatal("WithAttrs should return multiHandler")
	}
	if _, ok := m.WithGroup("g").(*multiHandler); !ok {
		t.Fatal("WithGroup should return multiHandler")
	}
}

func newPipelineTestDeps(t *testing.T, rssXML string, llmStatus int, llmBody string) (*storage.Store, *rss.Fetcher, *llm.Client) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.New(ctx, ":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	rssServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, rssXML)
	}))
	t.Cleanup(rssServer.Close)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(llmStatus)
		_, _ = io.WriteString(w, llmBody)
	}))
	t.Cleanup(llmServer.Close)

	fetcher := rss.New(config.RSSConfig{
		Feeds:           []string{rssServer.URL},
		MaxItemsPerFeed: 10,
		FetchTimeout:    time.Second,
		CacheTTL:        time.Hour,
	}, store)
	client := llm.New(config.LLMConfig{
		Provider:      "llama-cpp",
		Endpoint:      llmServer.URL + "/v1/chat/completions",
		Model:         "test",
		ContextWindow: 8192,
		Timeout:       time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return store, fetcher, client
}

func TestBuildPipeline_NoNewItems(t *testing.T) {
	emptyFeed := `<?xml version="1.0"?><rss version="2.0"><channel><title>Empty</title></channel></rss>`
	store, fetcher, client := newPipelineTestDeps(t, emptyFeed, http.StatusInternalServerError, "unused")
	runPipeline := buildPipeline(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fetcher, client, 10, time.UTC)

	run, ranked, err := runPipeline(context.Background(), "cron")
	if err != nil {
		t.Fatalf("pipeline error: %v", err)
	}
	if run == nil || run.Status != "success" || run.ItemCount != 0 {
		t.Fatalf("unexpected run: %+v", run)
	}
	if ranked != nil {
		t.Fatalf("expected nil ranked items, got %d", len(ranked))
	}
}

func TestBuildPipeline_FallbackRanking(t *testing.T) {
	published := time.Now().UTC().Add(-time.Hour).Format(time.RFC1123Z)
	feed := `<?xml version="1.0"?><rss version="2.0"><channel><title>Feed</title><item><title>News</title><description>Desc</description><link>https://example.com/1</link><pubDate>` + published + `</pubDate></item></channel></rss>`
	store, fetcher, client := newPipelineTestDeps(t, feed, http.StatusInternalServerError, "boom")
	runPipeline := buildPipeline(slog.New(slog.NewTextHandler(io.Discard, nil)), store, fetcher, client, 10, time.UTC)

	run, ranked, err := runPipeline(context.Background(), "command")
	if err != nil {
		t.Fatalf("pipeline error: %v", err)
	}
	if run == nil || run.Status != "fallback" || run.ItemCount != 1 || run.LLMUsed {
		t.Fatalf("unexpected run: %+v", run)
	}
	if len(ranked) != 1 || ranked[0].Title != "News" {
		t.Fatalf("unexpected ranked items: %+v", ranked)
	}
}
