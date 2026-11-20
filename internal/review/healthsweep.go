package review

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// HealthSweeper enqueues a health check for every profile whose verdict is
// old. It runs from the review process, which owns the llm_health queue.
type HealthSweeper struct {
	Store *store.Store
	// Enqueue schedules one check. It is the jobs enqueuer, kept as a func so
	// the sweeper does not depend on the jobs package.
	Enqueue func(ctx context.Context, profileID uint) error
	// StaleAfter is how old a verdict may be before it is re-checked. It
	// doubles as the panel's "stale" threshold.
	StaleAfter time.Duration
	// RetryAfter is the backoff applied to a profile whose last check failed:
	// a broken provider must not burn quota every cycle.
	RetryAfter time.Duration
	// Batch bounds how many profiles one sweep enqueues, so a large
	// installation does not stampede the provider in a single pass.
	Batch int
}

const (
	defaultHealthStaleAfter = time.Hour
	defaultHealthRetryAfter = 15 * time.Minute
	defaultHealthBatch      = 20
)

// Sweep enqueues the checks and reports how many it scheduled. A profile in
// error waits RetryAfter instead of StaleAfter, which is what keeps a bad key
// from being retried every cycle.
func (s *HealthSweeper) Sweep(ctx context.Context) (int, error) {
	if s == nil || s.Store == nil {
		return 0, errors.New("review: health sweeper without store")
	}
	if s.Enqueue == nil {
		return 0, errors.New("review: health sweeper without enqueuer")
	}

	staleAfter := s.StaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultHealthStaleAfter
	}
	retryAfter := s.RetryAfter
	if retryAfter <= 0 {
		retryAfter = defaultHealthRetryAfter
	}
	batch := s.Batch
	if batch <= 0 {
		batch = defaultHealthBatch
	}

	// A failing profile is retried after RetryAfter, which is shorter than
	// StaleAfter, so the query uses the widest window and the per-profile rule
	// below decides.
	cutoff := time.Now().Add(-staleAfter)
	if retryAfter < staleAfter {
		cutoff = time.Now().Add(-retryAfter)
	}
	profiles, err := s.Store.StaleLLMProfiles(ctx, cutoff)
	if err != nil {
		return 0, err
	}

	scheduled := 0
	for _, profile := range profiles {
		if scheduled >= batch {
			break
		}
		if profile.HealthCheckedAt != nil {
			age := time.Since(*profile.HealthCheckedAt)
			limit := staleAfter
			if profile.HealthStatus == store.HealthError {
				limit = retryAfter
			}
			if age < limit {
				continue
			}
		}
		if err := s.Enqueue(ctx, profile.ID); err != nil {
			slog.Error("enqueue llm health check", "profile_id", profile.ID, "error", err)
			continue
		}
		scheduled++
	}

	if scheduled > 0 {
		slog.Info("scheduled llm health checks", "count", scheduled, "stale_after", staleAfter)
	}
	return scheduled, nil
}
