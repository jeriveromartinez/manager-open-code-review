package review

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// TestHealthFailureMessageNamesProfile pins the persisted failure text: even a
// generic OpenCode error must tell the operator which endpoint was checked.
// This is pure on purpose; the .sh fakes cannot run on Windows.
func TestHealthFailureMessageNamesProfile(t *testing.T) {
	profile := store.LLMProfile{Provider: "nvidia", Model: "nemotron"}

	message := healthFailureMessage(42, profile, "review: opencode: exit status 1: UnknownError")
	for _, fragment := range []string{"profile 42", "nvidia/nemotron", "UnknownError"} {
		if !strings.Contains(message, fragment) {
			t.Errorf("healthFailureMessage() = %q, want it to contain %q", message, fragment)
		}
	}
	if len(message) > maxHealthMessage {
		t.Errorf("healthFailureMessage() has %d chars, want at most %d", len(message), maxHealthMessage)
	}
}

func TestCheckProfileUpdatesHealth(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	sealed, err := box.Seal([]byte("sk-health-check"))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}

	profile := store.LLMProfile{
		Name:         "health-check",
		Provider:     "nvidia",
		Model:        "nemotron",
		APIKeyEnc:    sealed,
		Enabled:      true,
		HealthStatus: store.HealthUnknown,
	}
	if err := s.DB.WithContext(ctx).Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}

	checker := &ProfileChecker{
		Store:   s,
		Box:     box,
		Bin:     fakeOpencodeBin(t),
		Timeout: 5 * time.Second,
	}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check with healthy binary: %v", err)
	}

	var got store.LLMProfile
	if err := s.DB.WithContext(ctx).First(&got, profile.ID).Error; err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if got.HealthStatus != store.HealthOK {
		t.Fatalf("health_status = %q, want %q", got.HealthStatus, store.HealthOK)
	}
	if got.HealthCheckedAt == nil {
		t.Fatal("health_checked_at was not set")
	}
	if got.HealthError != "" {
		t.Fatalf("health_error = %q, want empty", got.HealthError)
	}

	failBin := filepath.Join(t.TempDir(), "fail-opencode.sh")
	if err := os.WriteFile(failBin, []byte("#!/bin/sh\necho 'provider unavailable' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing binary: %v", err)
	}
	checker.Bin = failBin
	if err := checker.Check(ctx, profile.ID); err == nil {
		t.Fatal("Check with failing binary returned nil error")
	}

	if err := s.DB.WithContext(ctx).First(&got, profile.ID).Error; err != nil {
		t.Fatalf("reload profile after failure: %v", err)
	}
	if got.HealthStatus != store.HealthError {
		t.Fatalf("health_status = %q, want %q", got.HealthStatus, store.HealthError)
	}
	if strings.TrimSpace(got.HealthError) == "" {
		t.Fatal("health_error is empty after failure")
	}
	if len(got.HealthError) > 500 {
		t.Fatalf("health_error has %d chars, want at most 500", len(got.HealthError))
	}
}

func TestProfileCheckNotifies(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	sealed, err := box.Seal([]byte("sk-health-notify"))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	profile := store.LLMProfile{
		Name: "health-notify", Provider: "nvidia", Model: "nemotron",
		APIKeyEnc: sealed, Enabled: true, HealthStatus: store.HealthUnknown,
	}
	if err := s.DB.WithContext(ctx).Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}

	pub := &runTestEvents{}
	checker := &ProfileChecker{
		Store: s, Box: box, Bin: fakeOpencodeBin(t), Timeout: 5 * time.Second, Events: pub,
	}
	want := events.Event{Type: "llm_profile.updated", ID: strconv.FormatUint(uint64(profile.ID), 10)}

	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check with healthy binary: %v", err)
	}
	if got := pub.snapshot(); !reflect.DeepEqual(got, []events.Event{want}) {
		t.Fatalf("events after an ok check = %+v, want [%+v]", got, want)
	}

	failBin := filepath.Join(t.TempDir(), "fail-opencode.sh")
	if err := os.WriteFile(failBin, []byte("#!/bin/sh\necho 'provider unavailable' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing binary: %v", err)
	}
	checker.Bin = failBin
	if err := checker.Check(ctx, profile.ID); err == nil {
		t.Fatal("Check with failing binary returned nil error")
	}
	if got := pub.snapshot(); !reflect.DeepEqual(got, []events.Event{want, want}) {
		t.Errorf("events after a failed check = %+v, want [%+v %+v]", got, want, want)
	}

	emptyBin := filepath.Join(t.TempDir(), "empty-opencode.sh")
	if err := os.WriteFile(emptyBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write silent binary: %v", err)
	}
	checker.Bin = emptyBin
	if err := checker.Check(ctx, profile.ID); err == nil {
		t.Fatal("Check with an empty answer returned nil error")
	}
	if got := pub.snapshot(); !reflect.DeepEqual(got, []events.Event{want, want, want}) {
		t.Errorf("events after an empty answer = %+v, want three %+v", got, want)
	}

	// A failed notification must not turn a healthy verdict into an error.
	checker.Bin = fakeOpencodeBin(t)
	checker.Events = failingRunEvents{}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Errorf("Check with a failing publisher: %v", err)
	}
}

func TestRunnerCheckProfilePassesEvents(t *testing.T) {
	f := newRunTestFixture(t)
	r := f.runner()
	if err := r.CheckProfile(f.ctx, f.policy.LLMProfileID); err != nil {
		t.Fatalf("CheckProfile() error = %v", err)
	}
	want := []events.Event{{Type: "llm_profile.updated", ID: strconv.FormatUint(uint64(f.policy.LLMProfileID), 10)}}
	if got := f.events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
}
