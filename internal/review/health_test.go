package review

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

func newProbeProfile(t *testing.T, s *store.Store, box *crypto.Box, name, providerID, model string, baseURL *string) store.LLMProfile {
	t.Helper()
	sealed, err := box.Seal([]byte("sk-probe"))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	profile := store.LLMProfile{
		Name: name, Provider: providerID, Model: model, BaseURL: baseURL,
		APIKeyEnc: sealed, Enabled: true, HealthStatus: store.HealthUnknown,
	}
	if err := s.DB.WithContext(context.Background()).Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	return profile
}

func loadHealth(t *testing.T, s *store.Store, id uint) store.LLMProfile {
	t.Helper()
	var got store.LLMProfile
	if err := s.DB.WithContext(context.Background()).First(&got, id).Error; err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	return got
}

// TestProfileCheckUsesOneMinimalRequest is the cost guard of #75: a healthy
// profile is checked with one tool-free request well below 200 input tokens,
// not with a full agent run.
func TestProfileCheckUsesOneMinimalRequest(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	provider := testutil.NewFakeProvider(t, nil)
	base := provider.URL()
	profile := newProbeProfile(t, s, box, "probe-one-call", "nvidia", "nemotron", &base)

	checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check: %v", err)
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want exactly one", len(requests))
	}
	got := requests[0]
	if len(got.Tools) != 0 {
		t.Errorf("tools = %v, want none", got.Tools)
	}
	if got.EstimatedInputTokens >= 200 {
		t.Errorf("estimated input tokens = %d, want fewer than 200", got.EstimatedInputTokens)
	}
	if got.Model != "nemotron" {
		t.Errorf("model = %q, want the profile model", got.Model)
	}

	if health := loadHealth(t, s, profile.ID); health.HealthStatus != store.HealthOK {
		t.Errorf("health_status = %q, want %q", health.HealthStatus, store.HealthOK)
	}
}

func TestProfileCheckMapsProviderErrors(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"rejected key", 401, `{"error":{"message":"invalid api key"}}`, []string{"credencial rechazada", "HTTP 401", "API key"}},
		{"quota", 429, `{"error":{"message":"rate limited"}}`, []string{"cuota", "HTTP 429"}},
		{"missing model", 404, `{"error":{"message":"model not found"}}`, []string{"no existe en el proveedor", "nvidia/nemotron"}},
		{"unavailable", 503, `{"error":{"message":"down"}}`, []string{"proveedor no disponible", "HTTP 503"}},
		{"rejected request", 400, `{"error":{"message":"bad params"}}`, []string{"rechazó la petición", "HTTP 400"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := testutil.NewFakeProvider(t, func(testutil.ProviderRequest) testutil.ProviderReply {
				return testutil.ProviderReply{Status: tc.status, Body: tc.body}
			})
			base := provider.URL()
			profile := newProbeProfile(t, s, box, "probe-"+tc.name, "nvidia", "nemotron", &base)

			checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
			if err := checker.Check(ctx, profile.ID); err == nil {
				t.Fatal("Check returned nil error for a failing provider")
			}
			health := loadHealth(t, s, profile.ID)
			if health.HealthStatus != store.HealthError {
				t.Fatalf("health_status = %q, want %q", health.HealthStatus, store.HealthError)
			}
			for _, fragment := range tc.want {
				if !strings.Contains(health.HealthError, fragment) {
					t.Errorf("health_error = %q, want it to contain %q", health.HealthError, fragment)
				}
			}
			if !strings.Contains(health.HealthError, "profile ") {
				t.Errorf("health_error = %q, want the profile id", health.HealthError)
			}
		})
	}
}

func TestProfileCheckReportsNetworkAndTimeout(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	t.Run("network", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		base := closed.URL + "/v1"
		closed.Close()
		profile := newProbeProfile(t, s, box, "probe-network", "nvidia", "nemotron", &base)

		checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
		if err := checker.Check(ctx, profile.ID); err == nil {
			t.Fatal("Check returned nil error for a closed provider")
		}
		health := loadHealth(t, s, profile.ID)
		if !strings.Contains(health.HealthError, "fallo de red/DNS") {
			t.Errorf("health_error = %q, want the network sentence", health.HealthError)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		provider := testutil.NewFakeProvider(t, func(testutil.ProviderRequest) testutil.ProviderReply {
			time.Sleep(300 * time.Millisecond)
			return testutil.ProviderReply{Text: `{"ok":true}`}
		})
		base := provider.URL()
		profile := newProbeProfile(t, s, box, "probe-timeout", "nvidia", "nemotron", &base)

		checker := &ProfileChecker{Store: s, Box: box, Timeout: 30 * time.Millisecond}
		if err := checker.Check(ctx, profile.ID); err == nil {
			t.Fatal("Check returned nil error for a slow provider")
		}
		health := loadHealth(t, s, profile.ID)
		if !strings.Contains(health.HealthError, "tiempo de espera agotado") {
			t.Errorf("health_error = %q, want the timeout sentence", health.HealthError)
		}
	})
}

func latestHealthCheck(t *testing.T, s *store.Store, profileID uint) store.LLMHealthCheck {
	t.Helper()
	var check store.LLMHealthCheck
	err := s.DB.WithContext(context.Background()).Table("llm_health_checks").
		Where("profile_id = ?", profileID).Order("id DESC").First(&check).Error
	if err != nil {
		t.Fatalf("load health check for profile %d: %v", profileID, err)
	}
	return check
}

// TestProfileCheckRecordsUsage pins the per-check accounting: the usage of the
// minimal ping lands in the append-only history with the provider's naming
// already mapped to the structured columns (#73).
func TestProfileCheckRecordsUsage(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	provider := testutil.NewFakeProvider(t, func(testutil.ProviderRequest) testutil.ProviderReply {
		return testutil.ProviderReply{
			Text: `{"ok":true}`,
			Usage: testutil.ProviderUsage{
				PromptTokens: 120, CompletionTokens: 8, TotalTokens: 128,
				CachedTokens: 40, ReasoningTokens: 3,
			},
		}
	})
	base := provider.URL()
	profile := newProbeProfile(t, s, box, "probe-usage", "nvidia", "nemotron", &base)

	checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check: %v", err)
	}

	check := latestHealthCheck(t, s, profile.ID)
	if check.Status != store.HealthOK {
		t.Errorf("status = %q, want %q", check.Status, store.HealthOK)
	}
	if check.InputTokens == nil || *check.InputTokens != 120 {
		t.Errorf("input tokens = %v, want 120", check.InputTokens)
	}
	if check.OutputTokens == nil || *check.OutputTokens != 8 {
		t.Errorf("output tokens = %v, want 8", check.OutputTokens)
	}
	if check.ReasoningTokens == nil || *check.ReasoningTokens != 3 {
		t.Errorf("reasoning tokens = %v, want 3", check.ReasoningTokens)
	}
	if check.CacheReadTokens == nil || *check.CacheReadTokens != 40 {
		t.Errorf("cache read tokens = %v, want 40", check.CacheReadTokens)
	}
	if check.TotalTokens == nil || *check.TotalTokens != 128 {
		t.Errorf("total tokens = %v, want 128", check.TotalTokens)
	}
	if check.MaxContextTokens == nil || *check.MaxContextTokens != 120 {
		t.Errorf("max context = %v, want 120", check.MaxContextTokens)
	}
}

// A provider that omits usage still passes the check; the history records the
// check with NULL tokens so it does not enter an average as zero.
func TestProfileCheckRecordsUnknownUsageAsNull(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	base := server.URL
	profile := newProbeProfile(t, s, box, "probe-no-usage", "nvidia", "nemotron", &base)

	checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if health := loadHealth(t, s, profile.ID); health.HealthStatus != store.HealthOK {
		t.Fatalf("health_status = %q, want %q", health.HealthStatus, store.HealthOK)
	}
	check := latestHealthCheck(t, s, profile.ID)
	if check.TotalTokens != nil || check.InputTokens != nil || check.OutputTokens != nil {
		t.Errorf("check = %+v, want NULL token columns", check)
	}
}

// A failed probe records the check but no usage: NULL, never zero.
func TestProfileCheckFailureRecordsNoUsage(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	provider := testutil.NewFakeProvider(t, func(testutil.ProviderRequest) testutil.ProviderReply {
		return testutil.ProviderReply{Status: http.StatusInternalServerError, Body: "provider down"}
	})
	base := provider.URL()
	profile := newProbeProfile(t, s, box, "probe-failure-usage", "nvidia", "nemotron", &base)

	checker := &ProfileChecker{Store: s, Box: box, Timeout: 5 * time.Second}
	if err := checker.Check(ctx, profile.ID); err == nil {
		t.Fatal("Check returned nil error for a failing provider")
	}

	check := latestHealthCheck(t, s, profile.ID)
	if check.Status != store.HealthError {
		t.Errorf("status = %q, want %q", check.Status, store.HealthError)
	}
	if check.TotalTokens != nil || check.InputTokens != nil {
		t.Errorf("check = %+v, want no usage on a failed probe", check)
	}
}

// OpenCode is the fallback for providers outside the closed table; its raw
// usage JSON must reach the same structured columns.
func TestProfileCheckAgentFallbackRecordsUsage(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{9}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "usage-opencode.sh")
	script := `#!/bin/sh
printf '%s\n' '{"type":"step_finish","part":{"tokens":{"input":10,"output":2,"reasoning":1,"total":12,"cache":{"read":3,"write":1}}}}'
printf '%s\n' '{"type":"text","part":{"type":"text","text":"{\"ok\":true}"}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write usage binary: %v", err)
	}
	// No base URL and an unknown provider: the checker uses the agent binary.
	profile := newProbeProfile(t, s, box, "probe-agent-usage", "nvidia", "nemotron", nil)

	checker := &ProfileChecker{Store: s, Box: box, Bin: bin, Timeout: 5 * time.Second}
	if err := checker.Check(ctx, profile.ID); err != nil {
		t.Fatalf("Check: %v", err)
	}

	check := latestHealthCheck(t, s, profile.ID)
	if check.InputTokens == nil || *check.InputTokens != 10 ||
		check.OutputTokens == nil || *check.OutputTokens != 2 ||
		check.ReasoningTokens == nil || *check.ReasoningTokens != 1 ||
		check.CacheReadTokens == nil || *check.CacheReadTokens != 3 ||
		check.CacheWriteTokens == nil || *check.CacheWriteTokens != 1 ||
		check.TotalTokens == nil || *check.TotalTokens != 12 ||
		check.MaxContextTokens == nil || *check.MaxContextTokens != 14 {
		t.Errorf("check = %+v, want 10/2/1/3/1/12/14", check)
	}
}
