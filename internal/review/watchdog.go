package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Watchdog fails runs whose worker vanished before writing a terminal state.
// Without it a crashed, killed or redeployed worker leaves the run "running"
// forever and its check run in_progress.
type Watchdog struct {
	Store      *store.Store
	Events     events.Publisher
	Jobs       Enqueuer
	StaleAfter time.Duration
}

// Sweep interrupts every run started longer ago than StaleAfter and publishes
// the consequence of each one exactly like a terminal failure: the publish
// worker closes the check and moves the pull request to inconclusive.
func (w *Watchdog) Sweep(ctx context.Context) error {
	if w.Store == nil {
		return errors.New("review: watchdog has no store")
	}
	if w.StaleAfter <= 0 {
		return errors.New("review: watchdog has no stale threshold")
	}

	cutoff := time.Now().UTC().Add(-w.StaleAfter)
	runs, err := w.Store.InterruptStaleRuns(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("interrupt stale review runs: %w", err)
	}

	var errs []error
	for _, run := range runs {
		// run.go moves a failed run's PR to inconclusive before enqueueing
		// publication. Doing the same here keeps the panel honest even when
		// the publish job itself never succeeds.
		if err := w.Store.SetPRStatus(ctx, run.PullRequestID, store.StatusInconclusive); err != nil {
			errs = append(errs, fmt.Errorf("set QA status of pull request %d: %w", run.PullRequestID, err))
		}
		if w.Events != nil {
			for _, ev := range []events.Event{
				{Type: "run.updated", ID: strconv.FormatUint(uint64(run.ID), 10)},
				{Type: "pr.updated", ID: strconv.FormatUint(uint64(run.PullRequestID), 10)},
			} {
				if err := w.Events.Publish(ctx, ev); err != nil {
					errs = append(errs, fmt.Errorf("publish %s for run %d: %w", ev.Type, run.ID, err))
				}
			}
		}
		if w.Jobs != nil {
			if err := w.Jobs.EnqueuePublish(ctx, run.ID); err != nil {
				errs = append(errs, fmt.Errorf("enqueue publish for run %d: %w", run.ID, err))
			}
		}
	}

	if len(runs) > 0 {
		slog.Info("interrupted stale review runs",
			"count", len(runs),
			"cutoff", cutoff.Format(time.RFC3339),
		)
	}
	return errors.Join(errs...)
}
