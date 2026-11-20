// Package jobs wires the River queues. This file owns the control process:
// one job per installation, periodic reconciliation and publication of
// finished reviews. Review execution lives in review.go.
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
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
)

const (
	controlQueue   = "control"
	reviewQueue    = "review"
	publishQueue   = "publish"
	llmHealthQueue = "llm_health"

	pendingBatchSize = 50
	publishWorkers   = 2
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
// worker is registered by the control client, which owns publication.
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

// ControlDeps are optional dependencies of the control client. The review and
// health workers belong to the review process; the control process only
// enqueues their jobs and runs publication itself.
type ControlDeps struct {
	Trigger   ReviewTrigger
	Publisher *publish.Worker
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

type publishReviewWorker struct {
	river.WorkerDefaults[PublishReviewArgs]
	publisher *publish.Worker
}

func (w *publishReviewWorker) Work(ctx context.Context, job *river.Job[PublishReviewArgs]) error {
	return w.publisher.Publish(ctx, job.Args.RunID)
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
// (when supplied) the publish worker. The caller is responsible for starting
// and stopping the returned client.
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
	if d.Publisher != nil {
		river.AddWorker[PublishReviewArgs](workers, &publishReviewWorker{publisher: d.Publisher})
		queues[publishQueue] = river.QueueConfig{MaxWorkers: publishWorkers}
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queues,
		Workers: workers,
		// Publish and sync do external HTTP calls; River's 1-minute default is
		// too short for a summary + reviews + checks publication.
		JobTimeout: 5 * time.Minute,
		// The control process enqueues review and llm_health jobs that only the
		// worker process registers. Without this, River rejects those inserts.
		SkipUnknownJobCheck: true,
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
