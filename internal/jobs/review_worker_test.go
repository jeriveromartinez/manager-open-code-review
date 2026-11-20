package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type stubReviewRunner struct {
	err   error
	calls atomic.Int64
}

func (r *stubReviewRunner) Run(context.Context, uint) error {
	r.calls.Add(1)
	return r.err
}

// TestReviewWorkerSnoozesSaturatedProfile proves the whole snooze path against
// a real River client: a run waiting for a profile slot must leave the job
// rescheduled, not failed, and must not burn one of its attempts.
func TestReviewWorkerSnoozesSaturatedProfile(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &stubReviewRunner{err: &review.ProfileCapacityError{ProfileID: 4, Wait: 15 * time.Second}}
	workers := river.NewWorkers()
	river.AddWorker[ReviewRunArgs](workers, &reviewRunWorker{runner: runner})

	client, err := river.NewClient(riverpgxv5.New(s.Pool), &river.Config{
		Queues:              map[string]river.QueueConfig{reviewQueue: {MaxWorkers: 1}},
		Workers:             workers,
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("create River client: %v", err)
	}

	inserted, err := client.Insert(ctx, ReviewRunArgs{RunID: 9}, nil)
	if err != nil {
		t.Fatalf("insert review job: %v", err)
	}

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		job, err := client.JobGet(ctx, inserted.Job.ID)
		if err != nil {
			t.Fatalf("JobGet: %v", err)
		}
		if calls := runner.calls.Load(); calls > 1 {
			t.Fatalf("runner calls = %d, want 1", calls)
		}
		if job.State == rivertype.JobStateScheduled || job.State == rivertype.JobStateAvailable {
			// The state can be read before the worker picked the job up, so
			// the assertions wait for the actual invocation.
			if runner.calls.Load() == 1 {
				if job.Attempt != 0 {
					t.Errorf("job attempt = %d, want 0 (a snooze must not consume an attempt)", job.Attempt)
				}
				if job.FinalizedAt != nil {
					t.Error("a snoozed job was finalized")
				}
				if got := time.Until(job.ScheduledAt); got < 10*time.Second {
					t.Errorf("job scheduled in %v, want at least 10s", got)
				}
				return
			}
		}
		if job.State == rivertype.JobStateCompleted || job.State == rivertype.JobStateDiscarded {
			t.Fatalf("job state = %q, want it rescheduled", job.State)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job state = %q after 15s (runner calls = %d), want it rescheduled",
				job.State, runner.calls.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReviewWorkerPropagatesOtherErrors(t *testing.T) {
	boom := errors.New("boom")
	w := &reviewRunWorker{runner: &stubReviewRunner{err: boom}}
	job := &river.Job[ReviewRunArgs]{JobRow: &rivertype.JobRow{}, Args: ReviewRunArgs{RunID: 1}}

	if err := w.Work(context.Background(), job); !errors.Is(err, boom) {
		t.Fatalf("Work() error = %v, want %v", err, boom)
	}
}
