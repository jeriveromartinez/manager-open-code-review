// This file owns the workers and the River client run by the review process.
package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/jeriveromartinez/qa-control-center/internal/review"
)

const healthWorkers = 1

// ReviewRunner executes one queued review run.
type ReviewRunner interface {
	Run(ctx context.Context, runID uint) error
}

// HealthChecker verifies an LLM profile for the llm_health queue.
type HealthChecker interface {
	Check(ctx context.Context, profileID uint) error
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

// NewReviewClient registers the review pipeline and the LLM health worker. The
// caller starts and stops the returned client; the review process owns both
// queues, while the control process only enqueues jobs on them.
func NewReviewClient(pool *pgxpool.Pool, runner *review.Runner, checker *review.ProfileChecker, concurrency int) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker[ReviewRunArgs](workers, &reviewRunWorker{runner: runner})
	river.AddWorker[LLMHealthArgs](workers, &llmHealthWorker{checker: checker})

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			reviewQueue:    {MaxWorkers: concurrency},
			llmHealthQueue: {MaxWorkers: healthWorkers},
		},
		Workers: workers,
	})
	if err != nil {
		return nil, fmt.Errorf("create River review client: %w", err)
	}
	return client, nil
}
