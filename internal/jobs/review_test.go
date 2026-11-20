package jobs_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

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

// TestReviewClientSweepsStaleRunsOnStart verifies the watchdog is wired as a
// periodic job that runs when the worker starts: a redeploy or crash must not
// leave orphaned runs "running" until the first interval elapses.
func TestReviewClientSweepsStaleRunsOnStart(t *testing.T) {
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

	select {
	case <-watchdog.done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog did not sweep when the review client started")
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

// TestReviewClientSweepsLLMHealthOnStart verifies the sweeper runs with the
// worker: a profile nobody probes by hand must not stay "healthy" forever.
func TestReviewClientSweepsLLMHealthOnStart(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}
	sweeper := &recordingSweeper{done: make(chan struct{})}
	client, err := jobs.NewReviewClient(s.Pool, &review.Runner{Store: s, Box: box},
		&review.ProfileChecker{Store: s, Box: box}, 1,
		jobs.ReviewDeps{Health: sweeper, HealthInterval: time.Minute})
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

	select {
	case <-sweeper.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the health sweeper did not run when the review client started")
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
