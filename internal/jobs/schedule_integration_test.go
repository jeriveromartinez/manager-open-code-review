package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type countingTrigger struct {
	calls atomic.Int64
}

func (c *countingTrigger) EnqueuePending(context.Context, int) (int, error) {
	c.calls.Add(1)
	return 0, nil
}

type unavailableAppProvider struct{}

func (unavailableAppProvider) App(context.Context) (*ghapp.App, error) {
	return nil, errors.New("github app not configured")
}

func startClient(t *testing.T, ctx context.Context, client *river.Client[pgx.Tx]) {
	t.Helper()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
}

func waitForLeader(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		var leaderID string
		lastErr = s.Pool.QueryRow(ctx, `SELECT leader_id FROM river_leader WHERE name = 'default'`).Scan(&leaderID)
		if lastErr == nil && leaderID != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("river leader was not elected within 15s: %v", lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWatchLeadershipToleratesEmptyLeaderTable covers the startup window
// before River elects its first leader and verifies the watcher stops with the
// process context instead of leaking.
func TestWatchLeadershipToleratesEmptyLeaderTable(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		jobs.WatchLeadership(ctx, s.Pool, "test", 10*time.Millisecond)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchLeadership did not return after the context was canceled")
	}
}

// TestReconcileScheduleRunsWithoutLeadership is the regression for #35: the
// follower never holds River leadership while the first client is alive, yet
// its process-local reconcile schedule must keep inserting jobs. Before the
// fix the cadence was a River periodic job and only the leader ran it.
func TestReconcileScheduleRunsWithoutLeadership(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sy := &syncer.Syncer{Store: s, Apps: unavailableAppProvider{}}
	leaderTrigger := &countingTrigger{}
	followerTrigger := &countingTrigger{}

	leader, err := jobs.NewControlClient(s.Pool, sy, jobs.ControlDeps{Trigger: leaderTrigger})
	if err != nil {
		t.Fatalf("NewControlClient(leader) error = %v", err)
	}
	startClient(t, ctx, leader)
	waitForLeader(t, ctx, s)

	follower, err := jobs.NewControlClient(s.Pool, sy, jobs.ControlDeps{Trigger: followerTrigger})
	if err != nil {
		t.Fatalf("NewControlClient(follower) error = %v", err)
	}
	startClient(t, ctx, follower)

	go jobs.Schedule(ctx, "reconcile", 50*time.Millisecond, jobs.NewEnqueuer(follower).EnqueueReconcile)

	total := func() int64 { return leaderTrigger.calls.Load() + followerTrigger.calls.Load() }
	deadline := time.Now().Add(10 * time.Second)
	for total() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("reconcile ran %d times in 10s, want at least 3 while the follower is not leader", total())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
