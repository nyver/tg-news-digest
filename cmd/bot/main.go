package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nyver/tg-news-digest/internal/bot"
	"github.com/nyver/tg-news-digest/internal/config"
	"github.com/nyver/tg-news-digest/internal/formatter"
	"github.com/nyver/tg-news-digest/internal/healthcheck"
	"github.com/nyver/tg-news-digest/internal/llm"
	"github.com/nyver/tg-news-digest/internal/models"
	"github.com/nyver/tg-news-digest/internal/rss"
	"github.com/nyver/tg-news-digest/internal/scheduler"
	"github.com/nyver/tg-news-digest/internal/storage"
)

func main() {
	configPath, err := configPathFromArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		os.Exit(2)
	}

	cfg := config.MustLoad(configPath)

	logger, logFile := setupLogger(cfg.App)
	if logFile != nil {
		defer logFile.Close()
	}
	logger.Info("bot: starting",
		slog.String("db_path", cfg.App.DBPath),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize storage
	store, err := storage.New(ctx, cfg.App.DBPath)
	if err != nil {
		logger.Error("failed to initialize storage", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer store.Close()

	if err := store.DB().Ping(); err != nil {
		logger.Error("database ping failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Initialize formatter
	fmttr := formatter.New(formatter.ParseMode(cfg.Bot.ParseMode), cfg.App.DigestTopN)

	// Initialize LLM client
	llmClient := llm.New(cfg.LLM, logger)

	// Initialize RSS fetcher
	rssFetcher := rss.New(cfg.RSS, store)

	// Pipeline: fetch RSS → rank with LLM → save → return ranked items
	scheduleLoc, err := time.LoadLocation(cfg.Schedule.Timezone)
	if err != nil {
		logger.Error("failed to load schedule timezone", slog.String("error", err.Error()))
		os.Exit(1)
	}
	runPipeline := buildPipeline(logger, store, rssFetcher, llmClient, maxDigestTopN(cfg.App.DigestTopN), scheduleLoc)

	// Initialize bot — use a pointer so the broadcastFn can reference it
	var botRef *bot.Bot
	botRef, err = bot.New(cfg.Bot, fmttr, store, func(ctx context.Context) error {
		// /digest command always rebuilds today's digest, regardless of scheduled runs.
		_, ranked, err := runPipeline(ctx, "command")
		if err != nil {
			return err
		}
		if len(ranked) == 0 {
			logger.Info("pipeline: no items to broadcast")
			return nil
		}
		return botRef.Broadcast(ctx, ranked, time.Now())
	}, logger)
	if err != nil {
		logger.Error("failed to initialize bot", slog.String("error", err.Error()))
		os.Exit(1)
	}
	b := botRef

	// Wire the on-demand "/digest <тема>" handler: it reuses already-fetched
	// items from storage instead of re-polling RSS feeds, and asks the LLM to
	// filter by relevance rather than rank a fixed top-N.
	b.SetTopicDigestFunc(func(ctx context.Context, topic string) ([]models.RankedNewsItem, bool, error) {
		recent, err := store.GetRecentItems(ctx, 500)
		if err != nil {
			return nil, false, fmt.Errorf("topic digest: get recent items: %w", err)
		}
		if len(recent) == 0 {
			return nil, false, nil
		}
		return llmClient.RankByTopic(ctx, recent, topic, cfg.App.DigestTopN)
	})

	// Wire per-subscriber language translation: titles/summaries are produced
	// in Russian by the ranking step, then translated on demand into whatever
	// language a subscriber chose via /language.
	b.SetTranslateFunc(func(ctx context.Context, items []models.RankedNewsItem, header, targetLang string) ([]models.RankedNewsItem, string, error) {
		return llmClient.TranslateDigest(ctx, items, header, targetLang)
	})

	// Initialize healthcheck
	hc := healthcheck.New(*cfg, store, logger).
		WithHost(cfg.App.HealthHost).
		WithPort(cfg.App.HealthPort)
	_, healthShutdown := hc.StartHTTPServer(ctx)
	defer healthShutdown()
	logger.Info("healthcheck: enabled",
		slog.String("host", cfg.App.HealthHost),
		slog.Int("port", cfg.App.HealthPort),
	)

	// Initialize scheduler
	sched, err := scheduler.New(cfg.Schedule, logger)
	if err != nil {
		logger.Error("failed to initialize scheduler", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Personal schedules are checked every minute. Subscribers without custom
	// settings keep the old default: 09:00 Europe/Moscow.
	if err := sched.AddJob(cfg.Schedule.Cron, func(ctx context.Context) error {
		now := time.Now().UTC()
		due, err := store.GetDueSubscriberSettings(ctx, now)
		if err != nil {
			return fmt.Errorf("scheduler: get due subscribers: %w", err)
		}
		if len(due) == 0 {
			logger.Debug("scheduler: no subscribers due")
			return nil
		}

		_, ranked, err := runPipeline(ctx, "cron")
		if err != nil {
			return err
		}
		if len(ranked) == 0 {
			logger.Info("pipeline: no items to broadcast")
			return nil
		}
		if err := b.BroadcastTo(ctx, ranked, time.Now(), due); err != nil {
			if errors.Is(err, bot.ErrBroadcastNoDeliveries) {
				logger.Info("pipeline: broadcast skipped, no matching subscriber filters")
			} else {
				return err
			}
		}
		for _, st := range due {
			loc, err := time.LoadLocation(st.Timezone)
			if err != nil {
				loc = time.UTC
			}
			localDate := now.In(loc).Format("2006-01-02")
			if err := store.MarkSubscriberDigestSent(ctx, st.ChatID, localDate); err != nil {
				logger.Warn("scheduler: mark subscriber digest sent failed",
					slog.Int64("chat_id", st.ChatID), slog.String("error", err.Error()))
			}
		}
		return nil
	}); err != nil {
		logger.Error("failed to add digest job", slog.String("error", err.Error()))
		os.Exit(1)
	}

	sched.Start()

	// Periodic cleanup of old records (daily, older than 30 days).
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		fetchedItemsMaxAge := time.Duration(cfg.App.FetchedItemsRetentionDays) * 24 * time.Hour

		runCleanup := func() {
			if n, err := store.CleanupOldDigestRuns(ctx, 30*24*time.Hour); err != nil {
				logger.Warn("cleanup: old digest runs", slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Info("cleanup: removed old digest runs", slog.Int("count", n))
			}

			if n, err := store.CleanupOldFetchedItems(ctx, fetchedItemsMaxAge); err != nil {
				logger.Warn("cleanup: old fetched items", slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Info("cleanup: removed old fetched items",
					slog.Int("count", n), slog.Int("retention_days", cfg.App.FetchedItemsRetentionDays))
			}
		}

		runCleanup()
		for {
			select {
			case <-ticker.C:
				runCleanup()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Register signal handler before starting polling so no signal is missed.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start bot polling in background so the signal handler below is reachable.
	pollErr := make(chan error, 1)
	go func() {
		logger.Info("bot: starting polling")
		pollErr <- b.Start(ctx)
	}()

	// Block until a shutdown signal or a fatal polling error.
	select {
	case sig := <-quit:
		logger.Info("bot: shutdown signal received", slog.String("signal", sig.String()))
	case err := <-pollErr:
		if err != nil {
			logger.Error("bot: polling error", slog.String("error", err.Error()))
		}
	}

	cancel()
	sched.Stop()

	// Grace period for cleanup operations and goroutine shutdown
	time.Sleep(2 * time.Second)
	logger.Info("bot: stopped")
}

func configPathFromArgs(args []string) (string, error) {
	fs := flag.NewFlagSet("tg-news-digest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *configPath, nil
}

func maxDigestTopN(configured int) int {
	if configured < 20 {
		return 20
	}
	return configured
}

// buildPipeline creates the digest pipeline: fetch RSS → rank with LLM → save.
// trigger is "cron" or "command". Only cron runs advance the window for future fetches.
func buildPipeline(
	logger *slog.Logger,
	store *storage.Store,
	rssFetcher *rss.Fetcher,
	llmClient *llm.Client,
	topN int,
	scheduleLoc *time.Location,
) func(ctx context.Context, trigger string) (*models.DigestRun, []models.RankedNewsItem, error) {
	return func(ctx context.Context, trigger string) (*models.DigestRun, []models.RankedNewsItem, error) {
		logger.Info("pipeline: starting digest run", slog.String("trigger", trigger))

		// 1. Compute fetch window.
		now := time.Now()
		since := digestFetchSince(now, scheduleLoc, trigger, nil)
		if trigger != "command" {
			if lastRun, err := store.GetLastSuccessfulCronRun(ctx); err == nil && lastRun != nil {
				since = digestFetchSince(now, scheduleLoc, trigger, lastRun)
			} else if err != nil {
				logger.Warn("pipeline: get last successful cron run failed", slog.String("error", err.Error()))
			}
		}
		logger.Info("pipeline: fetch window", slog.Time("since", since))

		// 2. Fetch RSS
		fetchResult, err := rssFetcher.FetchAll(ctx, since)
		if err != nil {
			return nil, nil, fmt.Errorf("pipeline: fetch: %w", err)
		}

		logger.Info("pipeline: fetch complete",
			slog.Int("items", len(fetchResult.Items)),
			slog.Int("feeds_ok", fetchResult.FeedsOK),
			slog.Int("feeds_err", fetchResult.FeedsErr),
		)

		if len(fetchResult.Items) == 0 {
			logger.Info("pipeline: no new items")
			run := &models.DigestRun{
				RunAt:     time.Now(),
				Status:    "success",
				Trigger:   trigger,
				ItemCount: 0,
				LLMUsed:   false,
			}
			if _, err := store.SaveDigestRun(ctx, *run); err != nil {
				logger.Warn("pipeline: save run record failed", slog.String("error", err.Error()))
			}
			return run, nil, nil
		}

		// 3. Save items for health monitoring
		if err := rssFetcher.SaveAndCleanup(ctx, fetchResult.Items); err != nil {
			logger.Warn("pipeline: save items failed",
				slog.String("error", err.Error()),
				slog.Int("item_count", len(fetchResult.Items)),
			)
		}

		// 4. Rank with LLM
		ranked, llmUsed, err := llmClient.RankWithLLM(ctx, fetchResult.Items, topN)
		if err != nil {
			logger.Error("pipeline: rank failed", slog.String("error", err.Error()))
			run := &models.DigestRun{
				RunAt:    time.Now(),
				Status:   "failed",
				Trigger:  trigger,
				LLMUsed:  false,
				ErrorMsg: err.Error(),
			}
			store.SaveDigestRun(ctx, *run) //nolint:errcheck
			return run, nil, err
		}

		runStatus := "success"
		if !llmUsed {
			runStatus = "fallback"
		}

		logger.Info("pipeline: ranking complete",
			slog.Int("ranked_items", len(ranked)),
			slog.Bool("llm_used", llmUsed),
		)

		// 5. Save digest run record.
		run := &models.DigestRun{
			RunAt:     time.Now(),
			Status:    runStatus,
			Trigger:   trigger,
			ItemCount: len(ranked),
			LLMUsed:   llmUsed,
		}
		if _, err := store.SaveDigestRun(ctx, *run); err != nil {
			logger.Warn("pipeline: save run record failed", slog.String("error", err.Error()))
		}

		return run, ranked, nil
	}
}

func digestFetchSince(now time.Time, loc *time.Location, trigger string, lastCronRun *time.Time) time.Time {
	if loc == nil {
		loc = time.Local
	}
	if trigger == "command" {
		localNow := now.In(loc)
		return time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	}
	if lastCronRun != nil {
		return *lastCronRun
	}
	return now.Add(-24 * time.Hour)
}

// multiHandler delegates logging to multiple underlying handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if err := h.Handle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nested := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		nested[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: nested}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	nested := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		nested[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: nested}
}

// setupLogger configures slog with both file (JSON) and console (text) output.
// The returned *os.File must be closed by the caller when the process exits.
func setupLogger(cfg config.AppConfig) (*slog.Logger, *os.File) {
	opts := &slog.HandlerOptions{
		Level:     parseLogLevel(cfg.LogLevel),
		AddSource: true,
	}

	var handlers []slog.Handler
	var logFile *os.File

	if cfg.DigestLogPath != "" {
		f, err := os.OpenFile(cfg.DigestLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
		} else {
			logFile = f
			handlers = append(handlers, slog.NewJSONHandler(f, opts))
		}
	}

	handlers = append(handlers, slog.NewTextHandler(os.Stdout, opts))

	return slog.New(&multiHandler{handlers: handlers}), logFile
}

// parseLogLevel converts a string to a slog.Level.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
