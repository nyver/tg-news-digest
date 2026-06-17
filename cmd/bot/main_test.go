package main

import (
	"testing"
	"time"
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
