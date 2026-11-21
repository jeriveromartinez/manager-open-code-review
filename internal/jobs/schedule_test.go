package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
)

// TestScheduleRunsImmediatelyAndOnInterval pins the RunOnStart semantics the
// periodic reconciliation relied on: the first run must not wait for a tick.
func TestScheduleRunsImmediatelyAndOnInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int64
	first := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		Schedule(ctx, "test", 20*time.Millisecond, func(context.Context) error {
			if calls.Add(1) == 1 {
				close(first)
			}
			return nil
		})
	}()

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not run the task immediately")
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("task ran %d times in 2s, want at least 2", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not return after the context was canceled")
	}
}

func TestScheduleStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		Schedule(ctx, "test", time.Hour, func(context.Context) error {
			close(ran)
			return nil
		})
	}()

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not run the task immediately")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not return after the context was canceled")
	}
}

func TestScheduleIgnoresNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			var calls atomic.Int64
			Schedule(context.Background(), "test", interval, func(context.Context) error {
				calls.Add(1)
				return nil
			})
			if got := calls.Load(); got != 0 {
				t.Fatalf("task ran %d times, want 0", got)
			}
		})
	}
}

// TestRiverConfigsHaveNoPeriodicJobs is the regression for #35: River executes
// periodic jobs only on the database leader, so a component that lost
// leadership silently stopped its own cadence. The cadence now lives in a
// process-local Schedule and the River configs must not reintroduce periodic
// jobs for it.
func TestRiverConfigsHaveNoPeriodicJobs(t *testing.T) {
	control := controlClientConfig(river.NewWorkers(), nil)
	if got := len(control.PeriodicJobs); got != 0 {
		t.Errorf("control config PeriodicJobs = %d, want 0", got)
	}

	reviewCfg := reviewClientConfig(nil, river.NewWorkers(), nil)
	if got := len(reviewCfg.PeriodicJobs); got != 0 {
		t.Errorf("review config PeriodicJobs = %d, want 0", got)
	}
}
