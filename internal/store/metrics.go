package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ping reports whether the database still answers, for the worker health
// endpoint. A pool that lost its server returns an error here long before a
// job would notice.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// QueueStats is the state of the River queue: what is waiting, what is running
// and what has failed. The worker exposes it so an operator does not need SQL
// access to know whether the review pipeline is moving.
type QueueStats struct {
	Available int `json:"available"`
	Running   int `json:"running"`
	Scheduled int `json:"scheduled"`
	Discarded int `json:"discarded"`
	// OldestAvailable is how long the oldest waiting job has been waiting.
	// A queue that keeps growing while nothing runs is the signal that the
	// worker is down.
	OldestAvailableSeconds int `json:"oldest_available_seconds"`
}

// QueueStats reads the River job table grouped by state. River is the only
// writer, so a read with the current time is enough: a job in `running` past
// the timeout is the watchdog's business, not this report's.
func (s *Store) QueueStats(ctx context.Context) (QueueStats, error) {
	var rows []struct {
		State string
		Count int
	}
	err := s.DB.WithContext(ctx).Raw(`
		SELECT state, count(*)::int AS count
		FROM river_job
		WHERE state IN ('available', 'running', 'scheduled', 'discarded')
		GROUP BY state`).Scan(&rows).Error
	if err != nil {
		return QueueStats{}, fmt.Errorf("read queue stats: %w", err)
	}

	var stats QueueStats
	for _, row := range rows {
		switch row.State {
		case "available":
			stats.Available = row.Count
		case "running":
			stats.Running = row.Count
		case "scheduled":
			stats.Scheduled = row.Count
		case "discarded":
			stats.Discarded = row.Count
		}
	}

	var oldest **time.Time
	// min() over an empty set is NULL, and pgx cannot scan that into
	// time.Time: the double pointer keeps "no rows" distinguishable from a
	// zero timestamp.
	err = s.DB.WithContext(ctx).Raw(`
		SELECT min(scheduled_at) FROM river_job WHERE state = 'available'`).Row().Scan(&oldest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return QueueStats{}, fmt.Errorf("read oldest queued job: %w", err)
	}
	if oldest != nil && *oldest != nil && !(*oldest).IsZero() {
		stats.OldestAvailableSeconds = int(time.Since(**oldest).Seconds())
		if stats.OldestAvailableSeconds < 0 {
			stats.OldestAvailableSeconds = 0
		}
	}
	return stats, nil
}

// RunStats aggregates finished runs over a window so the worker can report how
// the pipeline is doing without exposing per-run data.
type RunStats struct {
	Succeeded    int   `json:"succeeded"`
	Failed       int   `json:"failed"`
	Inconclusive int   `json:"inconclusive"`
	// DurationP95Seconds is the 95th percentile of finished runs. A p95 close
	// to QA_REVIEW_TIMEOUT means the budget is too tight for the profile.
	DurationP95Seconds int `json:"duration_p95_seconds"`
}

// RunStatsSince aggregates runs finished after the cutoff.
func (s *Store) RunStatsSince(ctx context.Context, cutoff time.Time) (RunStats, error) {
	var rows []struct {
		Status string
		Count  int
	}
	err := s.DB.WithContext(ctx).Raw(`
		SELECT status, count(*)::int AS count
		FROM review_runs
		WHERE finished_at IS NOT NULL AND finished_at >= ?
		GROUP BY status`, cutoff).Scan(&rows).Error
	if err != nil {
		return RunStats{}, fmt.Errorf("read run stats: %w", err)
	}

	var stats RunStats
	for _, row := range rows {
		switch row.Status {
		case RunSucceeded:
			stats.Succeeded = row.Count
		case RunFailed:
			stats.Failed = row.Count
		case RunInconclusive:
			stats.Inconclusive = row.Count
		}
	}

	var p95 *float64
	err = s.DB.WithContext(ctx).Raw(`
		SELECT percentile_disc(0.95) WITHIN GROUP (
			ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at))
		)
		FROM review_runs
		WHERE finished_at IS NOT NULL AND started_at IS NOT NULL AND finished_at >= ?`, cutoff).Scan(&p95).Error
	if err != nil {
		return RunStats{}, fmt.Errorf("read run duration: %w", err)
	}
	if p95 != nil && *p95 > 0 {
		stats.DurationP95Seconds = int(*p95)
	}
	return stats, nil
}
