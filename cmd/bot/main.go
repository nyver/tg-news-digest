package main

import (
	"context"
	"errors"
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
	cfg := config.MustLoad()

	logger := setupLogger(cfg.App)
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
	runPipeline := buildPipeline(logger, store, rssFetcher, llmClient, cfg.App.DigestTopN)

	// Initialize bot — use a pointer so the broadcastFn can reference it
	var botRef *bot.Bot
	botRef, err = bot.New(cfg.Bot, fmttr, store, func(ctx context.Context) error {
		// /digest command uses the same window as the next scheduled cron:
		// fetch news since the last cron run.
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

	// Initialize healthcheck
	hc := healthcheck.New(*cfg, store, logger).WithPort(cfg.App.HealthPort)
	_, healthShutdown := hc.StartHTTPServer(ctx)
	defer healthShutdown()
	logger.Info("healthcheck: enabled", slog.Int("port", cfg.App.HealthPort))

	// Initialize scheduler
	sched, err := scheduler.New(cfg.Schedule, logger)
	if err != nil {
		logger.Error("failed to initialize scheduler", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Schedule daily digest.
	if err := sched.AddJob(cfg.Schedule.Cron, func(ctx context.Context) error {
		_, ranked, err := runPipeline(ctx, "cron")
		if err != nil {
			return err
		}
		if len(ranked) == 0 {
			logger.Info("pipeline: no items to broadcast")
			return nil
		}
		return b.Broadcast(ctx, ranked, time.Now())
	}); err != nil {
		logger.Error("failed to add digest job", slog.String("error", err.Error()))
		os.Exit(1)
	}

	sched.Start()

	// Periodic cleanup of old records (daily, older than 30 days).
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		runCleanup := func() {
			if n, err := store.CleanupOldDigestRuns(ctx, 30*24*time.Hour); err != nil {
				logger.Warn("cleanup: old digest runs", slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Info("cleanup: removed old digest runs", slog.Int("count", n))
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

// buildPipeline creates the digest pipeline: fetch RSS → rank with LLM → save.
// trigger is "cron" or "command". Only cron runs advance the window for future fetches.
func buildPipeline(
	logger *slog.Logger,
	store *storage.Store,
	rssFetcher *rss.Fetcher,
	llmClient *llm.Client,
	topN int,
) func(ctx context.Context, trigger string) (*models.DigestRun, []models.RankedNewsItem, error) {
	return func(ctx context.Context, trigger string) (*models.DigestRun, []models.RankedNewsItem, error) {
		logger.Info("pipeline: starting digest run", slog.String("trigger", trigger))

		// 1. Compute fetch window: since last cron run, default to last 24h.
		since := time.Now().Add(-24 * time.Hour)
		if lastCron, err := store.GetLastCronRun(ctx); err == nil && lastCron != nil {
			since = *lastCron
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
func setupLogger(cfg config.AppConfig) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     parseLogLevel(cfg.LogLevel),
		AddSource: true,
	}

	var handlers []slog.Handler

	if cfg.DigestLogPath != "" {
		f, err := os.OpenFile(cfg.DigestLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
		} else {
			handlers = append(handlers, slog.NewJSONHandler(f, opts))
		}
	}

	handlers = append(handlers, slog.NewTextHandler(os.Stdout, opts))

	if len(handlers) == 0 {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}

	return slog.New(&multiHandler{handlers: handlers})
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
