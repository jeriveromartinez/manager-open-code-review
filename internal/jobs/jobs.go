// Package jobs wires the River queue used for synchronization and reviews:
// one job per installation, a periodic reconciliation, and the review pipeline.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
)

const (
	controlQueue   = "control"
	reviewQueue    = "review"
	publishQueue   = "publish"
	llmHealthQueue = "llm_health"

	pendingBatchSize = 50
	reviewWorkers    = 2
	healthWorkers    = 1
)

// SyncInstallationArgs asks the syncer to reconcile a single installation.
// Repeated requests for the same installation within a minute collapse into one
// job, which keeps webhook bursts from stampeding GitHub.
type SyncInstallationArgs struct {
	InstallationID int64 `json:"installation_id"`
}

func (SyncInstallationArgs) Kind() string { return "sync_installation" }

func (SyncInstallationArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: controlQueue,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: time.Minute,
		},
	}
}

// ReconcileArgs asks the syncer to walk every known installation.
type ReconcileArgs struct{}

func (ReconcileArgs) Kind() string { return "reconcile" }

func (ReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: controlQueue}
}

// ReviewRunArgs executes one queued review run.
type ReviewRunArgs struct {
	RunID uint `json:"run_id"`
}

func (ReviewRunArgs) Kind() string { return "review_run" }

func (ReviewRunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: reviewQueue, MaxAttempts: 3}
}

// PublishReviewArgs publishes the artefacts of a finished review run. Its
// worker is registered in a later phase; the queue and args exist now so the
// runner can already enqueue the job.
type PublishReviewArgs struct {
	RunID uint `json:"run_id"`
}

func (PublishReviewArgs) Kind() string { return "publish_review" }

func (PublishReviewArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: publishQueue, MaxAttempts: 5}
}

// LLMHealthArgs verifies one configured LLM profile.
type LLMHealthArgs struct {
	ProfileID uint `json:"profile_id"`
}

func (LLMHealthArgs) Kind() string { return "llm_health" }

func (LLMHealthArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: llmHealthQueue, MaxAttempts: 1}
}

type syncInstallationWorker struct {
	river.WorkerDefaults[SyncInstallationArgs]
	syncer *syncer.Syncer
}

func (w *syncInstallationWorker) Work(ctx context.Context, job *river.Job[SyncInstallationArgs]) error {
	return w.syncer.SyncInstallation(ctx, job.Args.InstallationID)
}

// ReviewTrigger is the slice of review.Trigger needed by the reconciler.
type ReviewTrigger interface {
	EnqueuePending(ctx context.Context, limit int) (int, error)
}

// ReviewRunner executes one queued review run.
type ReviewRunner interface {
	Run(ctx context.Context, runID uint) error
}

// HealthChecker verifies an LLM profile for the llm_health queue.
type HealthChecker interface {
	Check(ctx context.Context, profileID uint) error
}

// ControlDeps are optional review dependencies of the control client. Leaving
// them zero keeps the old sync-only client, which existing callers use.
type ControlDeps struct {
	Trigger           ReviewTrigger
	Runner            ReviewRunner
	Checker           HealthChecker
	ReviewConcurrency int
}

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	syncer  *syncer.Syncer
	trigger ReviewTrigger
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	if err := w.syncer.ReconcileAll(ctx); err != nil {
		return err
	}
	if w.trigger == nil {
		return nil
	}
	created, err := w.trigger.EnqueuePending(ctx, pendingBatchSize)
	if err != nil {
		// A failed batch must not fail the sync: River will retry the
		// reconciliation on its own schedule.
		slog.Error("enqueue pending reviews", "error", err)
		return nil
	}
	if created > 0 {
		slog.Info("enqueued pending reviews", "count", created)
	}
	return nil
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

// Enqueuer schedules installation syncs. It has the same method as
// github.SyncEnqueuer so webhook and API handlers can depend on either.
type Enqueuer interface {
	EnqueueSync(ctx context.Context, installationID int64) error
}

// FullEnqueuer adds the review queues to the narrow sync contract. It still
// satisfies Enqueuer, github.SyncEnqueuer and review.Enqueuer.
type FullEnqueuer interface {
	Enqueuer
	EnqueueReview(ctx context.Context, runID uint) error
	EnqueuePublish(ctx context.Context, runID uint) error
	EnqueueLLMHealth(ctx context.Context, profileID uint) error
}

type clientEnqueuer struct {
	client *river.Client[pgx.Tx]
}

func (e clientEnqueuer) EnqueueSync(ctx context.Context, installationID int64) error {
	if _, err := e.client.Insert(ctx, SyncInstallationArgs{InstallationID: installationID}, nil); err != nil {
		return fmt.Errorf("enqueue sync of installation %d: %w", installationID, err)
	}
	return nil
}

func (e clientEnqueuer) EnqueueReview(ctx context.Context, runID uint) error {
	if _, err := e.client.Insert(ctx, ReviewRunArgs{RunID: runID}, nil); err != nil {
		return fmt.Errorf("enqueue review run %d: %w", runID, err)
	}
	return nil
}

func (e clientEnqueuer) EnqueuePublish(ctx context.Context, runID uint) error {
	if _, err := e.client.Insert(ctx, PublishReviewArgs{RunID: runID}, nil); err != nil {
		return fmt.Errorf("enqueue publish of review run %d: %w", runID, err)
	}
	return nil
}

func (e clientEnqueuer) EnqueueLLMHealth(ctx context.Context, profileID uint) error {
	if _, err := e.client.Insert(ctx, LLMHealthArgs{ProfileID: profileID}, nil); err != nil {
		return fmt.Errorf("enqueue health check of llm profile %d: %w", profileID, err)
	}
	return nil
}

var (
	_ github.SyncEnqueuer = clientEnqueuer{}
	_ FullEnqueuer        = clientEnqueuer{}
)

// NewControlClient registers the sync workers, the periodic reconciliation and
// (when supplied) the review and health workers. The caller is responsible for
// starting and stopping the returned client. The publish queue is registered in
// a later phase, when its worker is implemented.
func NewControlClient(pool *pgxpool.Pool, s *syncer.Syncer, interval time.Duration, deps ...ControlDeps) (*river.Client[pgx.Tx], error) {
	var d ControlDeps
	if len(deps) > 0 {
		d = deps[0]
	}

	workers := river.NewWorkers()
	river.AddWorker[SyncInstallationArgs](workers, &syncInstallationWorker{syncer: s})
	river.AddWorker[ReconcileArgs](workers, &reconcileWorker{syncer: s, trigger: d.Trigger})
	queues := map[string]river.QueueConfig{
		controlQueue: {MaxWorkers: 4},
	}
	if d.Runner != nil {
		river.AddWorker[ReviewRunArgs](workers, &reviewRunWorker{runner: d.Runner})
		workersCount := reviewWorkers
		if d.ReviewConcurrency > 0 {
			workersCount = d.ReviewConcurrency
		}
		queues[reviewQueue] = river.QueueConfig{MaxWorkers: workersCount}
	}
	if d.Checker != nil {
		river.AddWorker[LLMHealthArgs](workers, &llmHealthWorker{checker: d.Checker})
		queues[llmHealthQueue] = river.QueueConfig{MaxWorkers: healthWorkers}
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queues,
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) { return ReconcileArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create River control client: %w", err)
	}
	return client, nil
}

// NewEnqueuer returns a FullEnqueuer backed by a started or insert-only client.
func NewEnqueuer(c *river.Client[pgx.Tx]) FullEnqueuer {
	return clientEnqueuer{client: c}
}
