package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestLockPRSerializesPerPullRequest(t *testing.T) {
	f := newFixture(t)

	unlock, err := f.s.LockPR(f.ctx, 7)
	if err != nil {
		t.Fatalf("LockPR(7): %v", err)
	}

	type result struct {
		unlock func()
		err    error
	}
	second := make(chan result, 1)
	go func() {
		u, err := f.s.LockPR(context.Background(), 7)
		second <- result{unlock: u, err: err}
	}()
	select {
	case got := <-second:
		t.Fatalf("second LockPR(7) acquired while first was held: %v", got.err)
	case <-time.After(200 * time.Millisecond):
	}

	// A different pull request uses a different advisory-lock key.
	unlockOther, err := f.s.LockPR(f.ctx, 8)
	if err != nil {
		t.Fatalf("LockPR(8): %v", err)
	}
	unlockOther()

	unlock()
	unlock() // idempotent
	select {
	case got := <-second:
		if got.err != nil {
			t.Fatalf("second LockPR(7): %v", got.err)
		}
		got.unlock()
		got.unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("second LockPR(7) did not acquire after unlock")
	}
}

func TestSupersedeOtherActiveRunsKeepsRequestedSHA(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 7, store.StatusPending, false)

	old, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: "sha-old", Trigger: "webhook",
		Status: store.RunQueued, Attempt: 1,
	})
	if err != nil {
		t.Fatalf("seed queued run: %v", err)
	}
	failed, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: "sha-failed", Trigger: "webhook",
		Status: store.RunFailed, Attempt: 1,
	})
	if err != nil {
		t.Fatalf("seed failed run: %v", err)
	}

	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, old.HeadSHA); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(same sha): %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun(old): %v", err)
	}
	if got.Status != store.RunQueued {
		t.Errorf("same-SHA run status = %q, want %q", got.Status, store.RunQueued)
	}

	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, failed.HeadSHA); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(other sha): %v", err)
	}
	got, err = f.s.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun(old): %v", err)
	}
	if got.Status != store.RunSuperseded {
		t.Errorf("other-SHA run status = %q, want %q", got.Status, store.RunSuperseded)
	}
	got, err = f.s.ReviewRun(f.ctx, failed.ID)
	if err != nil {
		t.Fatalf("ReviewRun(failed): %v", err)
	}
	if got.Status != store.RunFailed {
		t.Errorf("terminal run status = %q, want %q", got.Status, store.RunFailed)
	}
}
