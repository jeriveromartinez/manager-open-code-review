package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func newHealthSweepStore(t *testing.T) *store.Store {
	t.Helper()
	return testutil.NewStore(t)
}

func sealHealthKey(t *testing.T) *crypto.Box {
	t.Helper()
	box, err := crypto.NewBox(make([]byte, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

func TestHealthSweeperEnqueuesStaleProfiles(t *testing.T) {
	s := newHealthSweepStore(t)
	ctx := context.Background()
	box := sealHealthKey(t)
	sealed, err := box.Seal([]byte("sk-test"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour)
	create := func(name string, checked *time.Time, status string) store.LLMProfile {
		t.Helper()
		profile, err := s.CreateLLMProfile(ctx, store.LLMProfile{
			Name: name, Provider: "openai", Model: "gpt-4o-mini",
			APIKeyEnc: sealed, Enabled: true, HealthStatus: status, HealthCheckedAt: checked,
		})
		if err != nil {
			t.Fatalf("CreateLLMProfile(%s): %v", name, err)
		}
		return profile
	}

	stale := create("stale", &old, store.HealthOK)
	never := create("never", nil, store.HealthUnknown)
	recent := time.Now().Add(-time.Minute)
	create("recent", &recent, store.HealthOK)

	var enqueued []uint
	sweeper := &HealthSweeper{
		Store:      s,
		StaleAfter: time.Hour,
		Enqueue: func(_ context.Context, id uint) error {
			enqueued = append(enqueued, id)
			return nil
		},
	}

	count, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (the stale and the never-checked profile)", count)
	}
	if len(enqueued) != 2 {
		t.Fatalf("enqueued = %v, want two profiles", enqueued)
	}
	if enqueued[0] != never.ID || enqueued[1] != stale.ID {
		t.Errorf("enqueued = %v, want never-checked first (%d) then stale (%d)", enqueued, never.ID, stale.ID)
	}
}

// TestHealthSweeperBacksOffFailedProfiles is the quota guard: a provider whose
// key is wrong must not be retried on every cycle.
func TestHealthSweeperBacksOffFailedProfiles(t *testing.T) {
	s := newHealthSweepStore(t)
	ctx := context.Background()
	box := sealHealthKey(t)
	sealed, err := box.Seal([]byte("sk-bad"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	justFailed := time.Now().Add(-2 * time.Minute)
	profile, err := s.CreateLLMProfile(ctx, store.LLMProfile{
		Name: "broken", Provider: "openai", Model: "gpt-4o-mini",
		APIKeyEnc: sealed, Enabled: true, HealthStatus: store.HealthError, HealthCheckedAt: &justFailed,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	var enqueued []uint
	sweeper := &HealthSweeper{
		Store:      s,
		StaleAfter: time.Hour,
		RetryAfter: 15 * time.Minute,
		Enqueue: func(_ context.Context, id uint) error {
			enqueued = append(enqueued, id)
			return nil
		},
	}

	// The profile is stale by age but inside the retry backoff.
	count, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if count != 0 || len(enqueued) != 0 {
		t.Fatalf("enqueued %v too early: a failed profile must wait for the backoff", enqueued)
	}

	// Once the backoff passes it is retried, so a recovered provider is
	// noticed without an admin pressing the button.
	oldFailure := time.Now().Add(-30 * time.Minute)
	if err := s.DB.WithContext(ctx).Model(&store.LLMProfile{}).
		Where("id = ?", profile.ID).Update("health_checked_at", oldFailure).Error; err != nil {
		t.Fatalf("age the failure: %v", err)
	}
	count, err = sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep after backoff: %v", err)
	}
	if count != 1 || len(enqueued) != 1 || enqueued[0] != profile.ID {
		t.Fatalf("enqueued = %v, want the failed profile after the backoff", enqueued)
	}
}

func TestHealthSweeperHonoursBatch(t *testing.T) {
	s := newHealthSweepStore(t)
	ctx := context.Background()
	box := sealHealthKey(t)
	sealed, err := box.Seal([]byte("sk-test"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.CreateLLMProfile(ctx, store.LLMProfile{
			Name: "p" + string(rune('a'+i)), Provider: "openai", Model: "m",
			APIKeyEnc: sealed, Enabled: true, HealthStatus: store.HealthUnknown,
		}); err != nil {
			t.Fatalf("CreateLLMProfile: %v", err)
		}
	}

	var enqueued []uint
	sweeper := &HealthSweeper{
		Store: s, Batch: 2,
		Enqueue: func(_ context.Context, id uint) error {
			enqueued = append(enqueued, id)
			return nil
		},
	}
	count, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if count != 2 || len(enqueued) != 2 {
		t.Errorf("enqueued = %v (count %d), want exactly the batch of 2", enqueued, count)
	}
}

func TestHealthSweeperSkipsDisabledProfiles(t *testing.T) {
	s := newHealthSweepStore(t)
	ctx := context.Background()
	box := sealHealthKey(t)
	sealed, err := box.Seal([]byte("sk-test"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s.CreateLLMProfile(ctx, store.LLMProfile{
		Name: "off", Provider: "openai", Model: "m",
		APIKeyEnc: sealed, Enabled: false, HealthStatus: store.HealthUnknown,
	}); err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	sweeper := &HealthSweeper{Store: s, Enqueue: func(context.Context, uint) error {
		t.Error("a disabled profile was enqueued")
		return nil
	}}
	count, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestHealthSweeperRequiresEnqueuer(t *testing.T) {
	sweeper := &HealthSweeper{Store: newHealthSweepStore(t)}
	if _, err := sweeper.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep without an enqueuer returned nil error")
	}
}

func TestHealthSweeperKeepsGoingAfterEnqueueError(t *testing.T) {
	s := newHealthSweepStore(t)
	ctx := context.Background()
	box := sealHealthKey(t)
	sealed, err := box.Seal([]byte("sk-test"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.CreateLLMProfile(ctx, store.LLMProfile{
			Name: "q" + string(rune('a'+i)), Provider: "openai", Model: "m",
			APIKeyEnc: sealed, Enabled: true, HealthStatus: store.HealthUnknown,
		}); err != nil {
			t.Fatalf("CreateLLMProfile: %v", err)
		}
	}

	calls := 0
	sweeper := &HealthSweeper{Store: s, Enqueue: func(context.Context, uint) error {
		calls++
		if calls == 1 {
			return errors.New("river unavailable")
		}
		return nil
	}}
	count, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if calls != 3 {
		t.Errorf("enqueue attempts = %d, want 3", calls)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 successful enqueues", count)
	}
}
