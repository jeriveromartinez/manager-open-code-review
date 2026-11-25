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
	// Retryable jobs failed but have attempts left; Discarded jobs exhausted
	// them. Both explain why a queue is not advancing.
	Retryable int `json:"retryable"`
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
		WHERE state IN ('available', 'running', 'scheduled', 'retryable', 'discarded')
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
		case "retryable":
			stats.Retryable = row.Count
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

// Critical stages of the review pipeline. A stage is observed on its own so a
// busy sync or review cannot hide a stopped publication queue (#154).
const (
	StageQueue   = "queue"
	StageClone   = "clone"
	StageReview  = "review"
	StageVerify  = "verify"
	StagePublish = "publish"

	StageKindQueue = "queue"
	StageKindRun   = "run"
)

// StageStats is one observable step of the pipeline. Queue stages describe
// River jobs waiting in a queue; run stages describe review runs executing a
// phase. OldestPendingSeconds is how long the oldest pending job (or the
// oldest run in the phase) has been waiting, and LastProgressSeconds is how
// long since the stage last moved: the last job attempt for a queue, or the
// newest run-log line for a phase. A heartbeat alone is not progress, because
// a separate goroutine keeps it fresh even when the phase is hung; a run with
// recent logs is progressing and a run that only beats is not.
type StageStats struct {
	Name                 string `json:"name"`
	Kind                 string `json:"kind"`
	Available            int    `json:"available"`
	Running              int    `json:"running"`
	Scheduled            int    `json:"scheduled"`
	Retryable            int    `json:"retryable"`
	Discarded            int    `json:"discarded"`
	OldestPendingSeconds int    `json:"oldest_pending_seconds"`
	LastProgressSeconds  int    `json:"last_progress_seconds"`
}

// StageStats reads every critical stage in one pass. It is deliberately
// separate from QueueStats: that one aggregates all queues, which is what let
// a running sync job mask a stopped publish queue.
func (s *Store) StageStats(ctx context.Context) ([]StageStats, error) {
	now := time.Now().UTC()
	stages := []StageStats{
		{Name: StageQueue, Kind: StageKindQueue},
		{Name: StageClone, Kind: StageKindRun},
		{Name: StageReview, Kind: StageKindRun},
		{Name: StageVerify, Kind: StageKindRun},
		{Name: StagePublish, Kind: StageKindQueue},
	}
	index := make(map[string]int, len(stages))
	for i := range stages {
		index[stages[i].Name] = i
	}

	var stateRows []struct {
		Queue string
		State string
		Count int
	}
	err := s.DB.WithContext(ctx).Raw(`
		SELECT queue, state, count(*)::int AS count
		FROM river_job
		WHERE queue IN ('review', 'publish')
		  AND state IN ('available', 'running', 'scheduled', 'retryable', 'discarded')
		GROUP BY queue, state`).Scan(&stateRows).Error
	if err != nil {
		return nil, fmt.Errorf("read stage queue states: %w", err)
	}

	var oldestRows []struct {
		Queue  string
		Oldest *time.Time
	}
	err = s.DB.WithContext(ctx).Raw(`
		SELECT queue, min(scheduled_at) AS oldest
		FROM river_job
		WHERE queue IN ('review', 'publish') AND state IN ('available', 'retryable')
		GROUP BY queue`).Scan(&oldestRows).Error
	if err != nil {
		return nil, fmt.Errorf("read stage pending age: %w", err)
	}

	var progressRows []struct {
		Queue string
		Last  *time.Time
	}
	err = s.DB.WithContext(ctx).Raw(`
		SELECT queue, max(attempted_at) AS last
		FROM river_job
		WHERE queue IN ('review', 'publish')
		GROUP BY queue`).Scan(&progressRows).Error
	if err != nil {
		return nil, fmt.Errorf("read stage progress: %w", err)
	}

	reviewStage, publishStage := index[StageQueue], index[StagePublish]
	for _, row := range stateRows {
		stage := reviewStage
		if row.Queue == "publish" {
			stage = publishStage
		}
		switch row.State {
		case "available":
			stages[stage].Available = row.Count
		case "running":
			stages[stage].Running = row.Count
		case "scheduled":
			stages[stage].Scheduled = row.Count
		case "retryable":
			stages[stage].Retryable = row.Count
		case "discarded":
			stages[stage].Discarded = row.Count
		}
	}
	for _, row := range oldestRows {
		stage := reviewStage
		if row.Queue == "publish" {
			stage = publishStage
		}
		stages[stage].OldestPendingSeconds = ageSeconds(now, row.Oldest)
	}
	queueProgress := map[string]bool{}
	for _, row := range progressRows {
		stage := reviewStage
		if row.Queue == "publish" {
			stage = publishStage
		}
		if row.Last != nil && !row.Last.IsZero() {
			// A zero age is a real sample: activity happened this second.
			stages[stage].LastProgressSeconds = ageSeconds(now, row.Last)
			queueProgress[row.Queue] = true
			continue
		}
		stages[stage].LastProgressSeconds = 0
	}
	for _, row := range oldestRows {
		stage := reviewStage
		if row.Queue == "publish" {
			stage = publishStage
		}
		if !queueProgress[row.Queue] && stages[stage].OldestPendingSeconds > 0 {
			stages[stage].LastProgressSeconds = stages[stage].OldestPendingSeconds
		}
	}

	var phaseRows []struct {
		Phase  string
		Count  int
		Oldest *time.Time
		Last   *time.Time
	}
	err = s.DB.WithContext(ctx).Raw(`
		SELECT r.phase, count(*)::int AS count,
		       min(r.phase_started_at) AS oldest,
		       max(GREATEST(l.last_log, r.phase_started_at, r.started_at)) AS last
		FROM review_runs r
		LEFT JOIN (
			SELECT run_id, max(ts) AS last_log
			FROM review_run_logs
			GROUP BY run_id
		) l ON l.run_id = r.id
		WHERE r.status = 'running'
		GROUP BY r.phase`).Scan(&phaseRows).Error
	if err != nil {
		return nil, fmt.Errorf("read review phase stats: %w", err)
	}

	lastProgress := map[int]int{}
	for _, row := range phaseRows {
		name := stageForPhase(row.Phase)
		if name == "" {
			continue
		}
		stage := index[name]
		stages[stage].Running += row.Count
		if age := ageSeconds(now, row.Oldest); age > stages[stage].OldestPendingSeconds {
			stages[stage].OldestPendingSeconds = age
		}
		// Any phase of the stage making progress is enough: the freshest
		// activity decides whether the stage as a whole is alive.
		if row.Last != nil {
			age := ageSeconds(now, row.Last)
			if _, seen := lastProgress[stage]; !seen || age < lastProgress[stage] {
				lastProgress[stage] = age
			}
		}
	}
	for stage, age := range lastProgress {
		stages[stage].LastProgressSeconds = age
	}
	for i := range stages {
		// No sample at all falls back to the phase age; a real zero (activity
		// this second) must be preserved.
		if _, seen := lastProgress[i]; !seen && stages[i].Kind == StageKindRun &&
			stages[i].OldestPendingSeconds > 0 {
			stages[i].LastProgressSeconds = stages[i].OldestPendingSeconds
		}
	}

	return stages, nil
}

func stageForPhase(phase string) string {
	switch phase {
	// An empty phase is a run claimed before the first phase write, or a run
	// that predates phases (#123); it is still in preparation, so it belongs
	// to the clone stage rather than disappearing from the report.
	case "", RunPhasePreparing, RunPhaseCloning:
		return StageClone
	case RunPhaseReviewing, RunPhaseChunking, RunPhaseFinishing:
		return StageReview
	case RunPhaseVerifying:
		return StageVerify
	default:
		return ""
	}
}

func ageSeconds(now time.Time, then *time.Time) int {
	if then == nil || then.IsZero() {
		return 0
	}
	seconds := int(now.Sub(*then).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

// RunStats aggregates finished runs over a window so the worker can report how
// the pipeline is doing without exposing per-run data.
type RunStats struct {
	Succeeded    int `json:"succeeded"`
	Failed       int `json:"failed"`
	Inconclusive int `json:"inconclusive"`
	// DurationP95Seconds is the 95th percentile of finished runs. A p95 close
	// to review_timeout means the budget is too tight for the profile.
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
