package scheduler

import (
	"context"
	"testing"
	"time"

	"log/slog"

	"github.com/nyver/tg-news-digest/internal/config"
)

func TestScheduler_NewAndStop(t *testing.T) {
	cfg := config.ScheduleConfig{
		Cron:     "0 9 * * *",
		Timezone: "UTC",
	}

	s, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	s.Start()
	s.Stop() // should not panic
}

func TestScheduler_LastRunNeverCalled(t *testing.T) {
	cfg := config.ScheduleConfig{
		Cron:     "0 9 * * *",
		Timezone: "UTC",
	}

	s, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Stop()

	lastRun := s.LastRun()
	if !lastRun.IsZero() {
		t.Errorf("expected zero time for lastRun, got %v", lastRun)
	}
}

func TestScheduler_InvalidTimezone(t *testing.T) {
	cfg := config.ScheduleConfig{
		Cron:     "0 9 * * *",
		Timezone: "Invalid/Zone",
	}

	_, err := New(cfg, slog.Default())
	if err == nil {
		t.Error("expected error for invalid timezone")
	}
}

func TestScheduler_AddJob(t *testing.T) {
	cfg := config.ScheduleConfig{
		Cron:     "0 9 * * *",
		Timezone: "UTC",
	}

	s, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Stop()

	fn := func(ctx context.Context) error {
		return nil
	}

	// Regular 5-field cron should work
	if err := s.AddJob("0 * * * *", fn); err != nil {
		t.Fatalf("AddJob() error = %v", err)
	}
}

func TestScheduler_RunCallback(t *testing.T) {
	t.Skip("slow test: waits for cron to fire")
	cfg := config.ScheduleConfig{
		Cron:     "0 9 * * *",
		Timezone: "UTC",
	}

	s, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer s.Stop()

	callCount := 0
	fn := func(ctx context.Context) error {
		callCount++
		return nil
	}

	s.Start()
	// Schedule for the next minute boundary
	if err := s.AddJob("* * * * *", fn); err != nil {
		t.Fatalf("AddJob() error = %v", err)
	}

	// Wait ~65 seconds for the next minute boundary to fire
	time.Sleep(65 * time.Second)

	if callCount < 1 {
		t.Errorf("expected at least 1 call, got %d", callCount)
	}
}
