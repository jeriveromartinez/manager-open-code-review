// This file owns the workers and the River client run by the review process.
package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/jeriveromartinez/qa-control-center/internal/review"
)

const (
	healthWorkers = 1
	// The sweep is cheap, but a staggered interval keeps a slow review queue
	// from delaying the recovery of orphaned runs for long.
	watchdogQueue    = "review_watchdog"
	watchdogInterval = 5 * time.Minute
)

// ReviewRunner executes one queued review run.
type ReviewRunner interface {
	Run(ctx context.Context, runID uint) error
}

// HealthChecker verifies an LLM profile for the llm_health queue.
type HealthChecker interface {
	Check(ctx context.Context, profileID uint) error
}

// StaleRunInterrupter fails runs left running by a vanished worker.
type StaleRunInterrupter interface {
	Sweep(ctx context.Context) error
}

type reviewRunWorker struct {
	river.WorkerDefaults[ReviewRunArgs]
	runner ReviewRunner
}

func (w *reviewRunWorker) Work(ctx context.Context, job *river.Job[ReviewRunArgs]) error {
	return w.runner.Run(ctx, job.Args.RunID)
}

type llmHealthWorker struct {
	river.WorkerDefaults[LLMHealthArgs]
	checker HealthChecker
}

func (w *llmHealthWorker) Work(ctx context.Context, job *river.Job[LLMHealthArgs]) error {
	return w.checker.Check(ctx, job.Args.ProfileID)
}

// InterruptStaleRunsArgs asks the watchdog to fail the runs nobody is
// executing anymore.
type InterruptStaleRunsArgs struct{}

func (InterruptStaleRunsArgs) Kind() string { return "review_runs_interrupt_stale" }

func (InterruptStaleRunsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: watchdogQueue, MaxAttempts: 1}
}

type interruptStaleRunsWorker struct {
	river.WorkerDefaults[InterruptStaleRunsArgs]
	watchdog StaleRunInterrupter
}

func (w *interruptStaleRunsWorker) Work(ctx context.Context, _ *river.Job[InterruptStaleRunsArgs]) error {
	return w.watchdog.Sweep(ctx)
}

// ReviewDeps are the optional dependencies of the review client.
type ReviewDeps struct {
	Watchdog StaleRunInterrupter
}

// NewReviewClient registers the review pipeline and the LLM health worker. When
// a watchdog is supplied it also sweeps orphaned runs on start and every
// watchdogInterval. The caller starts and stops the returned client; the review
// process owns all the queues, while the control process only enqueues jobs on
// them.
func NewReviewClient(pool *pgxpool.Pool, runner *review.Runner, checker *review.ProfileChecker, concurrency int, deps ...ReviewDeps) (*river.Client[pgx.Tx], error) {
	var d ReviewDeps
	if len(deps) > 0 {
		d = deps[0]
	}

	workers := river.NewWorkers()
	river.AddWorker[ReviewRunArgs](workers, &reviewRunWorker{runner: runner})
	river.AddWorker[LLMHealthArgs](workers, &llmHealthWorker{checker: checker})
	queues := map[string]river.QueueConfig{
		reviewQueue:    {MaxWorkers: concurrency},
		llmHealthQueue: {MaxWorkers: healthWorkers},
	}
	var periodic []*river.PeriodicJob
	if d.Watchdog != nil {
		river.AddWorker[InterruptStaleRunsArgs](workers, &interruptStaleRunsWorker{watchdog: d.Watchdog})
		queues[watchdogQueue] = river.QueueConfig{MaxWorkers: 1}
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(watchdogInterval),
			func() (river.JobArgs, *river.InsertOpts) { return InterruptStaleRunsArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	// River's default job timeout is one minute, far below the review budget
	// (QA_REVIEW_TIMEOUT, 20m by default). Without this the job context dies
	// before the LLM finishes and the run stays "running" after a retry.
	jobTimeout := 30 * time.Minute
	if runner != nil && runner.Cfg.Timeout > 0 {
		jobTimeout = runner.Cfg.Timeout + 5*time.Minute
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:       queues,
		Workers:      workers,
		JobTimeout:   jobTimeout,
		PeriodicJobs: periodic,
		// The review process enqueues publish jobs that only the control
		// process registers. Without this, River rejects those inserts.
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create River review client: %w", err)
	}
	return client, nil
}
