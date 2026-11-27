package jobs_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// TestNewReviewClientBuilds verifies that the review process can build a River
// client from its runner, its profile checker and the configured concurrency.
func TestNewReviewClientBuilds(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}

	runner := &review.Runner{Store: s, Box: box}
	checker := &review.ProfileChecker{Store: s, Box: box}
	client, err := jobs.NewReviewClient(s.Pool, runner, checker, 2)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewReviewClient() returned a nil client")
	}
}

// TestNewReviewClientEnqueuesPublish verifies that the review process can insert
// the publish job handled by the control process, the other half of the queue
// split. River's unknown-kind check rejected it before.
func TestNewReviewClientEnqueuesPublish(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box}, &review.ProfileChecker{Store: s, Box: box}, 1)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}
	if err := jobs.NewEnqueuer(client).EnqueuePublish(context.Background(), 3); err != nil {
		t.Errorf("EnqueuePublish() error = %v", err)
	}
}

func TestPublishReviewArgsAreUniquePerRun(t *testing.T) {
	opts := jobs.PublishReviewArgs{}.InsertOpts()
	if opts.Queue != "publish" {
		t.Errorf("queue = %q, want publish", opts.Queue)
	}
	if !opts.UniqueOpts.ByArgs {
		t.Error("publish jobs must be unique per run: the terminal write and the reconciler can both enqueue it")
	}
	for _, state := range opts.UniqueOpts.ByState {
		switch state {
		case rivertype.JobStateCompleted, rivertype.JobStateDiscarded, rivertype.JobStateCancelled:
			t.Errorf("unique state %q would block a later delivery of an unsettled run", state)
		}
	}
}

// A publish that returns without settling (an unresolved comparison) completes
// its job; the adoption trigger and the reconciler must still be able to
// enqueue that run again.
func TestPublishJobCanBeReinsertedAfterCompletion(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}
	ctx := context.Background()
	args := jobs.PublishReviewArgs{RunID: 42}

	first, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("first Insert() error = %v", err)
	}
	if first.UniqueSkippedAsDuplicate {
		t.Fatal("first publish job was deduplicated")
	}

	live, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("live Insert() error = %v", err)
	}
	if !live.UniqueSkippedAsDuplicate {
		t.Error("a second live publish job for the same run was not deduplicated")
	}

	// The job ran and returned without settling the run's delivery.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, first.Job.ID); err != nil {
		t.Fatalf("complete first job: %v", err)
	}

	again, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("reinsert Insert() error = %v", err)
	}
	if again.UniqueSkippedAsDuplicate {
		t.Error("a settled job blocked the re-enqueue of a still-pending run")
	}
}

func TestInterruptStaleRunsJobArgs(t *testing.T) {
	args := jobs.InterruptStaleRunsArgs{}
	if args.Kind() != "review_runs_interrupt_stale" {
		t.Errorf("Kind() = %q, want review_runs_interrupt_stale", args.Kind())
	}
	if opts := args.InsertOpts(); opts.Queue != "review_watchdog" {
		t.Errorf("InsertOpts() queue = %q, want review_watchdog", opts.Queue)
	}
}

type recordingWatchdog struct {
	mu    sync.Mutex
	swept int
	done  chan struct{}
}

func (w *recordingWatchdog) Sweep(context.Context) error {
	w.mu.Lock()
	w.swept++
	first := w.swept == 1
	w.mu.Unlock()
	if first {
		close(w.done)
	}
	return nil
}

// TestReviewClientRunsEnqueuedWatchdog verifies the watchdog worker is wired.
// The worker process enqueues its sweeps from a process-local schedule now:
// River runs periodic jobs only on the database leader, so the previous
// periodic job stopped sweeping whenever the worker lost leadership (#35).
func TestReviewClientRunsEnqueuedWatchdog(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	watchdog := &recordingWatchdog{done: make(chan struct{})}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1, jobs.ReviewDeps{Watchdog: watchdog})
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	if err := jobs.NewEnqueuer(client).EnqueueWatchdog(ctx); err != nil {
		t.Fatalf("EnqueueWatchdog() error = %v", err)
	}

	select {
	case <-watchdog.done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog did not sweep after EnqueueWatchdog")
	}
}

type recordingSweeper struct {
	mu    sync.Mutex
	swept int
	done  chan struct{}
}

func (s *recordingSweeper) Sweep(context.Context) (int, error) {
	s.mu.Lock()
	s.swept++
	first := s.swept == 1
	s.mu.Unlock()
	if first {
		close(s.done)
	}
	return 0, nil
}

func TestLLMHealthSweepJobArgs(t *testing.T) {
	args := jobs.LLMHealthSweepArgs{}
	if args.Kind() != "llm_health_sweep" {
		t.Errorf("Kind() = %q, want llm_health_sweep", args.Kind())
	}
	if opts := args.InsertOpts(); opts.Queue != "llm_health_sweep" {
		t.Errorf("InsertOpts() queue = %q, want llm_health_sweep (it must not block the llm_health worker it feeds)", opts.Queue)
	}
}

// TestReviewClientRunsEnqueuedHealthSweep verifies the sweeper worker is
// wired: the worker process enqueues the sweep locally because River runs
// periodic jobs only on the database leader (#35).
func TestReviewClientRunsEnqueuedHealthSweep(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	sweeper := &recordingSweeper{done: make(chan struct{})}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1,
		jobs.ReviewDeps{Health: sweeper})
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	if err := jobs.NewEnqueuer(client).EnqueueHealthSweep(ctx); err != nil {
		t.Fatalf("EnqueueHealthSweep() error = %v", err)
	}

	select {
	case <-sweeper.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the health sweeper did not run after EnqueueHealthSweep")
	}
}

// TestReviewClientWithoutSweeper keeps the callers that predate the sweeper
// working: no sweeper means no periodic sweep, not a nil dereference.
func TestReviewClientWithoutSweeper(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

// TestReviewRecoveryJobIsNotHiddenByTheAbandonedOne pins the delivery that
// recovers an orphan: the job that claimed the run and died is still "running"
// in River, so the recovery delivery needs its own uniqueness key (#228).
func TestReviewRecoveryJobIsNotHiddenByTheAbandonedOne(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}
	ctx := context.Background()
	args := jobs.ReviewRunArgs{RunID: 42}

	abandoned, err := client.Insert(ctx, args, nil)
	if err != nil {
		t.Fatalf("insert abandoned job: %v", err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE river_job SET state = 'running', attempted_at = now() WHERE id = $1`, abandoned.Job.ID); err != nil {
		t.Fatalf("mark abandoned job running: %v", err)
	}

	enqueuer := jobs.NewEnqueuer(client)
	if err := enqueuer.EnqueueReviewRecovery(ctx, 42, 1); err != nil {
		t.Fatalf("EnqueueReviewRecovery() error = %v", err)
	}
	recovery, err := client.Insert(ctx, jobs.ReviewRunArgs{RunID: 42, Recovery: 1}, nil)
	if err != nil {
		t.Fatalf("insert duplicate recovery job: %v", err)
	}
	if !recovery.UniqueSkippedAsDuplicate {
		t.Error("a second live recovery job of the same generation was not deduplicated")
	}

	var count int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE kind = 'review_run' AND args->>'recovery_generation' = '1'`).Scan(&count); err != nil {
		t.Fatalf("count recovery jobs: %v", err)
	}
	if count != 1 {
		t.Errorf("recovery jobs = %d, want one next to the abandoned job", count)
	}
}
