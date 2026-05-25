package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/nyver/tg-news-digest/internal/config"
)

// RunFunc is the callback that performs the full digest pipeline.
type RunFunc func(ctx context.Context) error

// Scheduler manages the cron-based daily digest schedule.
type Scheduler struct {
	cron   *cron.Cron
	entryID cron.EntryID
	logger *slog.Logger
}

// New creates a new Scheduler with the given cron expression and timezone.
func New(cfg config.ScheduleConfig, logger *slog.Logger) (*Scheduler, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("scheduler: load timezone %q: %w", cfg.Timezone, err)
	}

	c := cron.New(cron.WithLocation(loc), cron.WithChain(
		cron.Recover(cron.DefaultLogger),
	))

	return &Scheduler{cron: c, logger: logger}, nil
}

// AddJob registers a digest run callback with the given cron expression.
func (s *Scheduler) AddJob(cronExpr string, fn RunFunc) error {
	entryID, err := s.cron.AddFunc(cronExpr, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := fn(ctx); err != nil {
			s.logger.Error("scheduler: digest run failed", slog.String("error", err.Error()))
		}
	})
	if err != nil {
		return fmt.Errorf("scheduler: add job: %w", err)
	}

	s.entryID = entryID
	return nil
}

// Start begins processing jobs according to the schedule.
func (s *Scheduler) Start() {
	s.cron.Start()
	s.logger.Info("scheduler: started")
}

// Stop gracefully stops the scheduler, waiting for any running job to finish.
func (s *Scheduler) Stop() {
	s.cron.Stop()
	s.logger.Info("scheduler: stopped")
}

// LastRun returns the time the scheduled job last ran, or zero if never.
func (s *Scheduler) LastRun() time.Time {
	entry := s.cron.Entry(s.entryID)
	return entry.Prev
}
