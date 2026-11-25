package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestRecoverableNativeFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", errors.New("ocr review: exit status 1: 429 rate limited"), true},
		{"quota", errors.New("provider quota exceeded"), true},
		{"unavailable", errors.New("provider returned HTTP 503"), true},
		{"timeout", context.DeadlineExceeded, true},
		{"network", errors.New("dial tcp: connection refused"), true},
		{"bad key", errors.New("provider returned HTTP 401: invalid api key"), false},
		{"missing model", errors.New("model not found"), false},
		{"configuration", errors.New("invalid config file"), false},
		{"unknown", errors.New("algo raro pasó"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoverableNativeFailure(tc.err); got != tc.want {
				t.Errorf("recoverableNativeFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRecoverableNativeResult(t *testing.T) {
	for _, code := range []string{"llm_error", "timeout"} {
		if !recoverableNativeResult(code) {
			t.Errorf("recoverableNativeResult(%q) = false, want true", code)
		}
	}
	for _, code := range []string{"config", "token_budget", "coverage_incomplete", "no_coverage", ""} {
		if recoverableNativeResult(code) {
			t.Errorf("recoverableNativeResult(%q) = true, want false", code)
		}
	}
}

func TestCanStartFallbackRespectsBudgetAndTime(t *testing.T) {
	const budget = int64(100)
	ctx := context.Background()
	if !canStartFallback(ctx, budget, tokenUsage{Total: 50}) {
		t.Error("fallback with budget left and no deadline = false, want true")
	}
	if canStartFallback(ctx, budget, tokenUsage{Total: 100}) {
		t.Error("fallback with the budget exhausted = true, want false")
	}
	if canStartFallback(ctx, budget, tokenUsage{Total: 101}) {
		t.Error("fallback over budget = true, want false")
	}
	// A budget of zero disables the cap and ignores the spent amount.
	if !canStartFallback(ctx, 0, tokenUsage{Total: 1 << 30}) {
		t.Error("fallback without a budget = false, want true")
	}
	// A cancelled run never starts a fallback.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if canStartFallback(cancelled, 0, tokenUsage{}) {
		t.Error("fallback after cancellation = true, want false")
	}
	// Too little time left is the same as no time.
	short, cancelShort := context.WithTimeout(ctx, 10*time.Second)
	defer cancelShort()
	if canStartFallback(short, 0, tokenUsage{}) {
		t.Error("fallback with less than a minute left = true, want false")
	}
}

func TestAttemptConfigRevisionTracksProfileAndPolicy(t *testing.T) {
	profile := store.LLMProfile{ID: 7, Provider: "openai", Model: "gpt-test"}
	policy := store.QAPolicy{ID: 3, ReviewEffort: store.ReviewEffortLow, UpdatedAt: time.Unix(100, 0)}
	cfg := store.ReviewConfig{Profile: profile, Policy: policy}

	base := attemptConfigRevision(cfg, profile)
	if base == "" {
		t.Fatal("attemptConfigRevision() is empty")
	}
	if got := attemptConfigRevision(cfg, profile); got != base {
		t.Errorf("revision is not stable: %q != %q", got, base)
	}

	changedModel := profile
	changedModel.Model = "gpt-next"
	if got := attemptConfigRevision(cfg, changedModel); got == base {
		t.Error("revision did not change with the model")
	}

	changedPolicy := cfg
	changedPolicy.Policy.UpdatedAt = time.Unix(200, 0)
	if got := attemptConfigRevision(changedPolicy, profile); got == base {
		t.Error("revision did not change with the policy")
	}
}

func TestReviewProfilesKeepsOrderAndAuthorization(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	create := func(name, provider string, enabled bool, health string, allowPrivate bool, baseURL *string) store.LLMProfile {
		t.Helper()
		profile := store.LLMProfile{
			Name: name, Provider: provider, Model: "model-" + name, BaseURL: baseURL,
			APIKeyEnc: "sealed", Enabled: enabled, HealthStatus: health,
			AllowPrivateData: allowPrivate,
		}
		if err := s.DB.WithContext(ctx).Create(&profile).Error; err != nil {
			t.Fatalf("create profile %q: %v", name, err)
		}
		return profile
	}

	primary := create("primary", "openai", true, store.HealthOK, true, nil)
	backup := create("backup", "openai", true, store.HealthOK, true, nil)
	publicOnly := create("public-only", "openai", true, store.HealthOK, false, nil)
	unknownHealth := create("unknown-health", "openai", true, store.HealthUnknown, true, nil)
	unreachable := create("unreachable", "nvidia", true, store.HealthOK, true, nil)
	disabled := create("disabled", "openai", false, store.HealthOK, true, nil)

	fallbacks, err := store.NormalizeFallbackProfileIDs([]uint{
		publicOnly.ID, backup.ID, unknownHealth.ID, unreachable.ID, disabled.ID, backup.ID,
	})
	if err != nil {
		t.Fatalf("normalize fallback ids: %v", err)
	}
	cfg := store.ReviewConfig{
		Repo:    store.Repository{Private: true},
		Profile: primary,
		Policy:  store.QAPolicy{TeamID: 1, FallbackProfileIDs: fallbacks},
	}
	r := &Runner{Store: s}

	got := r.reviewProfiles(ctx, cfg)
	if len(got) != 2 || got[0].ID != primary.ID || got[1].ID != backup.ID {
		t.Fatalf("private candidates = %v, want [primary backup]", profileIDs(got))
	}

	// A profile that denies private code is skipped for a private repository but
	// allowed for a public one, in the order the team configured.
	cfg.Repo.Private = false
	got = r.reviewProfiles(ctx, cfg)
	if len(got) != 3 || got[0].ID != primary.ID || got[1].ID != publicOnly.ID || got[2].ID != backup.ID {
		t.Fatalf("public candidates = %v, want [primary public-only backup]", profileIDs(got))
	}
}

func profileIDs(profiles []store.LLMProfile) []uint {
	ids := make([]uint, 0, len(profiles))
	for _, profile := range profiles {
		ids = append(ids, profile.ID)
	}
	return ids
}
