package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// profileWithSlots creates an enabled profile with an explicit slot count.
func (f *fixture) profileWithSlots(name string, slots int) store.LLMProfile {
	f.t.Helper()

	p := f.llmProfile(name, true)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", p.ID).Update("concurrency", slots).Error; err != nil {
		f.t.Fatalf("set concurrency of profile %q: %v", name, err)
	}
	p.Concurrency = slots
	return p
}

func TestAcquireProfileSlotSerializesAtOneSlot(t *testing.T) {
	f := newFixture(t)
	profile := f.profileWithSlots("solo", 1)

	unlock, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("first AcquireProfileSlot() = %v/%v, want acquired", ok, err)
	}

	if _, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
		t.Fatalf("second AcquireProfileSlot(): %v", err)
	} else if ok {
		t.Fatal("second AcquireProfileSlot acquired while the only slot was held")
	}

	// Another profile has its own slots.
	other := f.profileWithSlots("solo-otro", 1)
	unlockOther, ok, err := f.s.AcquireProfileSlot(f.ctx, other.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot(other) = %v/%v, want acquired", ok, err)
	}
	unlockOther()

	unlock()
	unlock() // idempotent

	third, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot() after unlock = %v/%v, want acquired", ok, err)
	}
	third()
}

func TestAcquireProfileSlotAllowsConfiguredConcurrency(t *testing.T) {
	f := newFixture(t)
	profile := f.profileWithSlots("par", 2)

	first, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("first AcquireProfileSlot() = %v/%v, want acquired", ok, err)
	}
	second, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("second AcquireProfileSlot() = %v/%v, want acquired", ok, err)
	}

	if _, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
		t.Fatalf("third AcquireProfileSlot(): %v", err)
	} else if ok {
		t.Fatal("third AcquireProfileSlot acquired with concurrency 2 and both slots held")
	}

	first()
	third, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot() after releasing one slot = %v/%v, want acquired", ok, err)
	}
	second()
	third()
}

func TestAcquireProfileSlotIsVisibleToAnotherConnection(t *testing.T) {
	f := newFixture(t)
	profile := f.profileWithSlots("compartido", 1)

	other, err := store.Open(f.ctx, f.s.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	defer other.Close()

	unlock, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot() = %v/%v, want acquired", ok, err)
	}

	if _, ok, err := other.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
		t.Fatalf("AcquireProfileSlot() on the second store: %v", err)
	} else if ok {
		t.Fatal("the second store ignored the slot held by the first one")
	}

	unlock()
	unlockSecond, ok, err := other.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot() after unlock = %v/%v, want acquired", ok, err)
	}
	unlockSecond()
}

func TestAcquireProfileSlotTreatsNonPositiveConcurrencyAsOne(t *testing.T) {
	for _, slots := range []int{0, -3} {
		t.Run(fmt.Sprintf("concurrency_%d", slots), func(t *testing.T) {
			f := newFixture(t)
			profile := f.profileWithSlots("sin-slots", slots)

			unlock, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
			if err != nil || !ok {
				t.Fatalf("first AcquireProfileSlot() = %v/%v, want acquired", ok, err)
			}
			if _, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
				t.Fatalf("second AcquireProfileSlot(): %v", err)
			} else if ok {
				t.Fatal("a non-positive concurrency granted more than one slot")
			}
			unlock()
		})
	}
}

func TestAcquireProfileSlotReReadsConcurrency(t *testing.T) {
	f := newFixture(t)
	profile := f.profileWithSlots("cambiante", 1)

	unlock, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("first AcquireProfileSlot() = %v/%v, want acquired", ok, err)
	}
	if _, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID); err != nil {
		t.Fatalf("second AcquireProfileSlot(): %v", err)
	} else if ok {
		t.Fatal("second AcquireProfileSlot acquired with concurrency 1")
	}

	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", profile.ID).Update("concurrency", 2).Error; err != nil {
		t.Fatalf("raise concurrency: %v", err)
	}

	second, ok, err := f.s.AcquireProfileSlot(f.ctx, profile.ID)
	if err != nil || !ok {
		t.Fatalf("AcquireProfileSlot() after raising concurrency = %v/%v, want acquired", ok, err)
	}
	second()
	unlock()
}

func TestAcquireProfileSlotUnknownProfileIsNotFound(t *testing.T) {
	f := newFixture(t)

	if _, _, err := f.s.AcquireProfileSlot(f.ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AcquireProfileSlot(unknown) error = %v, want ErrNotFound", err)
	}
}

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

	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, old.Comparison()); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(same sha): %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun(old): %v", err)
	}
	if got.Status != store.RunQueued {
		t.Errorf("same-SHA run status = %q, want %q", got.Status, store.RunQueued)
	}

	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, failed.Comparison()); err != nil {
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
