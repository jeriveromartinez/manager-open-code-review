// This file owns the workers and the River client run by the review process.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	watchdogQueue = "review_watchdog"
	// WatchdogInterval is how often the worker process enqueues a stale-run
	// sweep.
	WatchdogInterval = 5 * time.Minute
	// The sweep feeds the llm_health queue, so it runs on its own queue: it must
	// not hold the only health worker while it looks up the stale profiles.
	llmSweepQueue = "llm_health_sweep"
	// DefaultHealthSweepInterval is the fallback when no health interval is
	// configured.
	DefaultHealthSweepInterval = 30 * time.Minute
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

// LLMHealthSweeper enqueues the health checks whose verdict went stale.
type LLMHealthSweeper interface {
	Sweep(ctx context.Context) (int, error)
}

type reviewRunWorker struct {
	river.WorkerDefaults[ReviewRunArgs]
	runner ReviewRunner
}

// Work turns a saturated profile into a snooze: the run stays queued and the
// job is retried after the backoff the runner computed. Snoozing does not
// consume an attempt, so a busy profile is never discarded just for being busy.
func (w *reviewRunWorker) Work(ctx context.Context, job *river.Job[ReviewRunArgs]) error {
	err := w.runner.Run(ctx, job.Args.RunID)
	var capacity *review.ProfileCapacityError
	if errors.As(err, &capacity) {
		slog.Info("review job snoozed for profile capacity",
			"run_id", job.Args.RunID, "profile_id", capacity.ProfileID,
			"attempt", job.Attempt, "wait", capacity.Wait)
		return river.JobSnooze(capacity.Wait)
	}
	var staleToken *review.CloneTokenStaleError
	if errors.As(err, &staleToken) {
		slog.Info("review job snoozed for clone token refresh",
			"run_id", job.Args.RunID, "attempt", job.Attempt, "wait", staleToken.Wait)
		return river.JobSnooze(staleToken.Wait)
	}
	return err
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

// LLMHealthSweepArgs asks the sweeper to enqueue the checks whose verdict
// went stale, so a profile nobody probes by hand stops looking healthy forever.
type LLMHealthSweepArgs struct{}

func (LLMHealthSweepArgs) Kind() string { return "llm_health_sweep" }

func (LLMHealthSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: llmSweepQueue, MaxAttempts: 1}
}

type llmHealthSweepWorker struct {
	river.WorkerDefaults[LLMHealthSweepArgs]
	sweeper LLMHealthSweeper
}

func (w *llmHealthSweepWorker) Work(ctx context.Context, _ *river.Job[LLMHealthSweepArgs]) error {
	_, err := w.sweeper.Sweep(ctx)
	return err
}

// ReviewDeps are the optional dependencies of the review client.
type ReviewDeps struct {
	Watchdog StaleRunInterrupter
	Health   LLMHealthSweeper
}

// NewReviewClient registers the review pipeline, the LLM health worker and,
// when supplied, the watchdog and health-sweep workers. The periodic cadence
// lives in the worker process (see Schedule), not in River: River runs periodic
// jobs only on the database leader, so a worker that lost leadership would
// silently stop sweeping (#35). The caller starts and stops the returned
// client; the review process owns all the queues, while the control process
// only enqueues jobs on them.
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
	if d.Health != nil {
		river.AddWorker[LLMHealthSweepArgs](workers, &llmHealthSweepWorker{sweeper: d.Health})
		queues[llmSweepQueue] = river.QueueConfig{MaxWorkers: 1}
	}
	if d.Watchdog != nil {
		river.AddWorker[InterruptStaleRunsArgs](workers, &interruptStaleRunsWorker{watchdog: d.Watchdog})
		queues[watchdogQueue] = river.QueueConfig{MaxWorkers: 1}
	}

	client, err := river.NewClient(riverpgxv5.New(pool), reviewClientConfig(runner, workers, queues))
	if err != nil {
		return nil, fmt.Errorf("create River review client: %w", err)
	}
	return client, nil
}

// reviewClientConfig is separate from NewReviewClient so a test can assert the
// job timeout River actually receives and that no periodic job is registered.
// River's one-minute default is far below the review budget (the review_timeout
// setting, 20m by default): without this the job context died before the LLM
// finished and the run stayed "running" after a retry.
func reviewClientConfig(runner *review.Runner, workers *river.Workers, queues map[string]river.QueueConfig) *river.Config {
	jobTimeout := 30 * time.Minute
	if runner != nil && runner.Cfg.Timeout > 0 {
		jobTimeout = runner.Cfg.Timeout + 5*time.Minute
	}
	return &river.Config{
		Queues:     queues,
		Workers:    workers,
		JobTimeout: jobTimeout,
		// The review process enqueues publish jobs that only the control
		// process registers. Without this, River rejects those inserts.
		SkipUnknownJobCheck: true,
	}
}
