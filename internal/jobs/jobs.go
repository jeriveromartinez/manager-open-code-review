// Package jobs wires the River queue used for synchronization work: one job per
// installation plus a periodic reconciliation of all installations.
package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
)

const controlQueue = "control"

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

type syncInstallationWorker struct {
	river.WorkerDefaults[SyncInstallationArgs]
	syncer *syncer.Syncer
}

func (w *syncInstallationWorker) Work(ctx context.Context, job *river.Job[SyncInstallationArgs]) error {
	return w.syncer.SyncInstallation(ctx, job.Args.InstallationID)
}

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	syncer *syncer.Syncer
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	return w.syncer.ReconcileAll(ctx)
}

// Enqueuer schedules installation syncs. It has the same method as
// github.SyncEnqueuer so webhook and API handlers can depend on either.
type Enqueuer interface {
	EnqueueSync(ctx context.Context, installationID int64) error
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

var _ github.SyncEnqueuer = clientEnqueuer{}

// NewControlClient registers both workers and the periodic reconciliation job.
// The caller is responsible for starting and stopping the returned client.
func NewControlClient(pool *pgxpool.Pool, s *syncer.Syncer, interval time.Duration) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker[SyncInstallationArgs](workers, &syncInstallationWorker{syncer: s})
	river.AddWorker[ReconcileArgs](workers, &reconcileWorker{syncer: s})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			controlQueue: {MaxWorkers: 4},
		},
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

// NewEnqueuer returns an Enqueuer backed by a started or insert-only client.
func NewEnqueuer(c *river.Client[pgx.Tx]) Enqueuer {
	return clientEnqueuer{client: c}
}
