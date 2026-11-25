package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/api"
	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/web"
	"gorm.io/gorm"
)

type recordingHealth struct {
	mu  sync.Mutex
	ids []uint
}

func (h *recordingHealth) EnqueueLLMHealth(_ context.Context, profileID uint) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids = append(h.ids, profileID)
	return nil
}

func (h *recordingHealth) snapshot() []uint {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uint(nil), h.ids...)
}

// failingHealth simulates a queue that cannot accept the automatic health
// check: the profile create/patch must still succeed and only log the failure.
type failingHealth struct {
	err error
}

func (h failingHealth) EnqueueLLMHealth(context.Context, uint) error { return h.err }

type recordingRetrier struct {
	mu  sync.Mutex
	ids []uint
}

func (r *recordingRetrier) Retry(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, prID)
	return nil
}

func (r *recordingRetrier) snapshot() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.ids...)
}

// secureFixture reuses the dashboard fixture but rebuilds the router with the
// dependencies introduced by the agents/policy/retry endpoints.
type secureFixture struct {
	*apiFixture
	deps      api.Deps
	health    *recordingHealth
	reviews   *recordingRetrier
	workerBox *crypto.Box
}

func newSecureFixture(t *testing.T) *secureFixture {
	t.Helper()

	f := newAPIFixture(t)
	workerBox, err := crypto.NewBox(bytes.Repeat([]byte{0x33}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox(worker): %v", err)
	}
	health := &recordingHealth{}
	reviews := &recordingRetrier{}
	provider := ghapp.NewStoreAppProvider(f.st, f.box, f.cfg.GitHubAPIURL)
	a := &auth.Auth{
		Store:     f.st,
		Apps:      provider,
		GitHubURL: f.cfg.GitHubURL,
		APIURL:    f.cfg.GitHubAPIURL,
		PublicURL: f.cfg.PublicURL,
		HTTP:      http.DefaultClient,
	}
	deps := api.Deps{
		Cfg:       f.cfg,
		Store:     f.st,
		Box:       f.box,
		WorkerBox: workerBox,
		Apps:      provider,
		Auth:      a,
		Webhooks:  f.webhooks,
		Broker:    events.NewBroker(f.st.Pool),
		Events:    f.pub,
		Jobs:      f.jobs,
		Static:    web.Handler(),
		Health:    health,
		Reviews:   reviews,
	}
	f.router = api.NewRouter(deps)
	return &secureFixture{apiFixture: f, deps: deps, health: health, reviews: reviews, workerBox: workerBox}
}

func (f *secureFixture) rebuildRouter() {
	f.router = api.NewRouter(f.deps)
}

func profileBody() map[string]any {
	return map[string]any{
		"name":     "primary",
		"provider": "openai",
		"model":    "gpt-4o-mini",
		"api_key":  "sk-test-secret-1234",
		"enabled":  true,
	}
}

func TestAgentsAdminOnly(t *testing.T) {
	f := newSecureFixture(t)
	dev := f.user("dev", false)
	admin := f.user("admin", true)

	rec := f.request(http.MethodGet, "/api/llm-profiles", nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	rec = f.request(http.MethodPost, "/api/llm-profiles", profileBody(), f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	profiles, err := f.st.ListLLMProfiles(f.ctx)
	if err != nil {
		t.Fatalf("ListLLMProfiles: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profiles = %v, want none after member POST", profiles)
	}

	rec = f.request(http.MethodGet, "/api/llm-profiles", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	var body []map[string]any
	decodeJSON(t, rec, &body)
	if len(body) != 0 {
		t.Fatalf("admin profiles = %v, want empty list", body)
	}
}

func TestAgentKeyNeverReturned(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)
	cookie := f.session(admin)
	const plainKey = "sk-test-secret-1234"

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), cookie)
	assertStatus(t, rec, http.StatusCreated)

	rec = f.request(http.MethodGet, "/api/llm-profiles", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var profiles []map[string]any
	decodeJSON(t, rec, &profiles)
	if len(profiles) != 1 {
		t.Fatalf("profiles = %v, want one", profiles)
	}
	profile := profiles[0]
	assertKeys(t, profile, "id", "name", "provider", "model", "api_key_set", "health_status")
	if profile["api_key_set"] != true {
		t.Errorf("api_key_set = %v, want true", profile["api_key_set"])
	}
	for _, forbidden := range []string{"api_key", "api_key_enc", "api_key_plain"} {
		if _, ok := profile[forbidden]; ok {
			t.Errorf("profile leaks %q: %v", forbidden, profile)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, plainKey) {
		t.Errorf("response contains the plaintext key: %q", body)
	}
}

func TestAgentCreateSealsKey(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)
	body := profileBody()
	const plainKey = "sk-test-secret-1234"

	rec := f.request(http.MethodPost, "/api/llm-profiles", body, f.session(admin))
	assertStatus(t, rec, http.StatusCreated)

	profiles, err := f.st.ListLLMProfiles(f.ctx)
	if err != nil {
		t.Fatalf("ListLLMProfiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(profiles))
	}
	stored := profiles[0]
	if stored.APIKeyEnc == "" {
		t.Fatal("stored profile has an empty api_key_enc")
	}
	if strings.Contains(stored.APIKeyEnc, plainKey) {
		t.Fatalf("stored api_key_enc contains the plaintext key: %q", stored.APIKeyEnc)
	}

	opened, err := f.workerBox.Open(stored.APIKeyEnc)
	if err != nil {
		t.Fatalf("Open with worker box: %v", err)
	}
	if string(opened) != plainKey {
		t.Errorf("decrypted key = %q, want %q", opened, plainKey)
	}
	if _, err := f.box.Open(stored.APIKeyEnc); err == nil {
		t.Error("master box decrypted a worker-sealed key")
	}
	// Reaching private repositories is an explicit opt-in, off by default.
	if stored.AllowPrivateData {
		t.Error("a new profile defaults to allow_private_data")
	}

	rec = f.request(http.MethodPost, "/api/llm-profiles", map[string]any{
		"name": "missing", "provider": "openai", "model": "gpt-4o-mini",
	}, f.session(admin))
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestAgentPatchKeepsKeyWhenOmitted(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)
	cookie := f.session(admin)

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), cookie)
	assertStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := uint(created["id"].(float64))

	path := fmt.Sprintf("/api/llm-profiles/%d", id)
	rec = f.request(http.MethodPatch, path, map[string]any{"concurrency": 7, "api_key": ""}, cookie)
	assertStatus(t, rec, http.StatusOK)

	stored, err := f.st.LLMProfile(f.ctx, id)
	if err != nil {
		t.Fatalf("LLMProfile: %v", err)
	}
	if stored.Concurrency != 7 {
		t.Errorf("concurrency = %d, want 7", stored.Concurrency)
	}
	if stored.APIKeyEnc == "" {
		t.Error("PATCH with an absent/empty key wiped the stored ciphertext")
	}
	opened, err := f.workerBox.Open(stored.APIKeyEnc)
	if err != nil || string(opened) != "sk-test-secret-1234" {
		t.Errorf("key after PATCH = %q, err = %v; want the original key", opened, err)
	}
}

func TestAgentHealthEnqueues(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)
	dev := f.user("dev", false)

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), f.session(admin))
	assertStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := uint(created["id"].(float64))

	// Creating a profile already queues its first health check.
	if ids := f.health.snapshot(); len(ids) != 1 || ids[0] != id {
		t.Errorf("automatic EnqueueLLMHealth calls = %v, want [%d]", ids, id)
	}

	path := fmt.Sprintf("/api/llm-profiles/%d/health", id)
	rec = f.request(http.MethodPost, path, nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	rec = f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusAccepted)
	if ids := f.health.snapshot(); len(ids) != 2 || ids[1] != id {
		t.Errorf("EnqueueLLMHealth calls = %v, want the manual check appended after the automatic one", ids)
	}
}

func TestAgentCreateEnqueuesHealth(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), f.session(admin))
	assertStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := uint(created["id"].(float64))

	if ids := f.health.snapshot(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("EnqueueLLMHealth calls = %v, want [%d] after create", ids, id)
	}
}

func TestAgentCreateSurvivesHealthEnqueueFailure(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.Health = failingHealth{err: errors.New("queue unavailable")}
	f.rebuildRouter()
	admin := f.user("admin", true)

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), f.session(admin))
	assertStatus(t, rec, http.StatusCreated)

	profiles, err := f.st.ListLLMProfiles(f.ctx)
	if err != nil {
		t.Fatalf("ListLLMProfiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want the profile persisted despite the enqueue failure", len(profiles))
	}
}

func TestAgentPatchEnqueuesHealthWhenEndpointChanges(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("admin", true)
	cookie := f.session(admin)

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), cookie)
	assertStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id := uint(created["id"].(float64))
	path := fmt.Sprintf("/api/llm-profiles/%d", id)

	// A change unrelated to the endpoint must not queue another check.
	rec = f.request(http.MethodPatch, path, map[string]any{"concurrency": 3}, cookie)
	assertStatus(t, rec, http.StatusOK)
	if ids := f.health.snapshot(); len(ids) != 1 {
		t.Errorf("EnqueueLLMHealth calls after a concurrency PATCH = %v, want only the create one", ids)
	}

	rec = f.request(http.MethodPatch, path, map[string]any{"model": "gpt-4o"}, cookie)
	assertStatus(t, rec, http.StatusOK)
	if ids := f.health.snapshot(); len(ids) != 2 || ids[1] != id {
		t.Errorf("EnqueueLLMHealth calls after a model PATCH = %v, want [%d %d]", ids, id, id)
	}
}

func TestPolicyLeadCanEditOwnTeam(t *testing.T) {
	f := newSecureFixture(t)
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
		HealthStatus: store.HealthOK,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	ownTeam := f.team("own-team")
	otherTeam := f.team("other-team")
	lead := f.user("lead", false)
	f.member(ownTeam.ID, lead.ID, "lead")
	cookie := f.session(lead)

	ownPath := fmt.Sprintf("/api/teams/%d/policy", ownTeam.ID)
	otherPath := fmt.Sprintf("/api/teams/%d/policy", otherTeam.ID)

	rec := f.request(http.MethodGet, ownPath, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var current map[string]any
	decodeJSON(t, rec, &current)
	assertKeys(t, current, "team_id", "llm_profile_id", "blocking_threshold", "max_files",
		"max_diff_lines", "max_passes", "exclude_globs")

	body := map[string]any{
		"llm_profile_id":     profile.ID,
		"blocking_threshold": "major",
		"max_files":          12,
		"max_diff_lines":     900,
		"max_passes":         3,
		"exclude_globs":      []string{"*.lock"},
	}
	rec = f.request(http.MethodPut, ownPath, body, cookie)
	assertStatus(t, rec, http.StatusOK)

	saved, err := f.st.QAPolicyForTeam(f.ctx, ownTeam.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if saved.LLMProfileID != profile.ID || saved.BlockingThreshold != "major" ||
		saved.MaxFilesPerPass != 12 || saved.MaxDiffLinesPerPass != 900 || saved.MaxPasses != 3 {
		t.Errorf("saved policy = %+v, want the submitted values", saved)
	}
	if string(saved.ExcludeGlobs) != `["*.lock"]` {
		t.Errorf("exclude_globs = %s, want [\"*.lock\"]", saved.ExcludeGlobs)
	}

	rec = f.request(http.MethodGet, otherPath, nil, cookie)
	assertStatus(t, rec, http.StatusForbidden)
	rec = f.request(http.MethodPut, otherPath, body, cookie)
	assertStatus(t, rec, http.StatusForbidden)
	if _, err := f.st.QAPolicyForTeam(f.ctx, otherTeam.ID); err == nil {
		t.Error("lead of another team was able to save that team's policy")
	}

	rec = f.request(http.MethodPut, ownPath, map[string]any{"blocking_threshold": "bogus"}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	rec = f.request(http.MethodPut, ownPath, map[string]any{"max_files": 0}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	rec = f.request(http.MethodPut, ownPath, map[string]any{"require_green_ci": "yes"}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestPolicyRequireGreenCIRoundTrip(t *testing.T) {
	f := newSecureFixture(t)
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
		HealthStatus: store.HealthOK,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	team := f.team("gated-team")
	admin := f.user("admin", true)
	cookie := f.session(admin)
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	// A team without a policy still renders a complete form, including the new
	// option at its default.
	rec := f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var current map[string]any
	decodeJSON(t, rec, &current)
	assertKeys(t, current, "team_id", "llm_profile_id", "blocking_threshold", "max_files",
		"max_diff_lines", "max_passes", "exclude_globs", "require_green_ci", "required_checks")
	if current["require_green_ci"] != false {
		t.Errorf("default require_green_ci = %v, want false", current["require_green_ci"])
	}

	body := map[string]any{
		"llm_profile_id":   profile.ID,
		"require_green_ci": true,
		"required_checks":  []map[string]string{{"name": "build", "origin": "app:7"}},
	}
	rec = f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["require_green_ci"] != true {
		t.Errorf("PUT require_green_ci = %v, want true", saved["require_green_ci"])
	}
	if checks, ok := saved["required_checks"].([]any); !ok || len(checks) != 1 {
		t.Errorf("PUT required_checks = %v, want one entry", saved["required_checks"])
	}

	policy, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if !policy.RequireGreenCI {
		t.Error("stored RequireGreenCI = false, want true")
	}
	if got := store.ParseRequiredChecks(policy.RequiredChecks); len(got) != 1 ||
		got[0].Name != "build" || got[0].Origin != "app:7" {
		t.Errorf("stored required checks = %+v, want build from app:7", got)
	}

	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id":  profile.ID,
		"required_checks": []map[string]string{{"name": "build", "origin": "bogus"}},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &current)
	if current["require_green_ci"] != true {
		t.Errorf("GET require_green_ci = %v, want true", current["require_green_ci"])
	}

	// PUT replaces the whole policy: an explicit false must clear the flag.
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "require_green_ci": false,
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	if policy, err = f.st.QAPolicyForTeam(f.ctx, team.ID); err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if policy.RequireGreenCI {
		t.Error("RequireGreenCI = true after an explicit false")
	}
	// An omitted required_checks keeps the stored list: the panel does not
	// always send it and PUT rebuilds the policy from zero.
	if got := store.ParseRequiredChecks(policy.RequiredChecks); len(got) != 1 ||
		got[0].Name != "build" || got[0].Origin != "app:7" {
		t.Errorf("omitted required_checks cleared the stored list: %+v", got)
	}

	// An explicit empty list does clear it.
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "required_checks": []map[string]string{},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	if policy, err = f.st.QAPolicyForTeam(f.ctx, team.ID); err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if got := store.ParseRequiredChecks(policy.RequiredChecks); len(got) != 0 {
		t.Errorf("explicit empty required_checks = %+v, want none", got)
	}
}

func TestPolicyReviewerTargets(t *testing.T) {
	f := newSecureFixture(t)
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
		HealthStatus: store.HealthOK,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	team := f.team("review-team")
	cookie := f.session(f.user("admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	rec := f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID,
		"reviewer_login": "qa-reviewer",
		"reviewer_team":  "qa-team",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["reviewer_login"] != "qa-reviewer" || saved["reviewer_team"] != "qa-team" {
		t.Errorf("PUT reviewers = %v/%v, want qa-reviewer/qa-team",
			saved["reviewer_login"], saved["reviewer_team"])
	}

	// The panel does not send the new fields yet, so an unrelated save keeps
	// the stored target.
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "blocking_threshold": "blocker",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	policy, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if policy.ReviewerLogin != "qa-reviewer" || policy.ReviewerTeam != "qa-team" {
		t.Errorf("omitted reviewers = %q/%q, want the stored target", policy.ReviewerLogin, policy.ReviewerTeam)
	}

	// Invalid tokens are a client error, not an internal one.
	for field, value := range map[string]string{
		"reviewer_login": "bad/login",
		"reviewer_team":  "bad team",
	} {
		rec = f.request(http.MethodPut, path, map[string]any{
			"llm_profile_id": profile.ID, field: value,
		}, cookie)
		assertStatus(t, rec, http.StatusBadRequest)
	}

	// An explicit empty value clears the target.
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "reviewer_login": "", "reviewer_team": "",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	if policy, err = f.st.QAPolicyForTeam(f.ctx, team.ID); err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if policy.ReviewerLogin != "" || policy.ReviewerTeam != "" {
		t.Errorf("explicit empty reviewers = %q/%q, want cleared", policy.ReviewerLogin, policy.ReviewerTeam)
	}
}

func TestPolicyReviewControls(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "primary", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("cost-team")
	cookie := f.session(f.user("admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	// A team without a policy still renders complete controls: inherit the
	// global ceiling, low effort and no spending cap.
	rec := f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var current map[string]any
	decodeJSON(t, rec, &current)
	assertKeys(t, current, "review_context_ceiling", "global_context_ceiling",
		"effective_context_ceiling", "review_effort", "review_token_budget")
	if current["review_context_ceiling"] != nil {
		t.Errorf("default review_context_ceiling = %v, want null", current["review_context_ceiling"])
	}
	if current["global_context_ceiling"] != float64(270000) ||
		current["effective_context_ceiling"] != float64(270000) {
		t.Errorf("default ceilings = %v/%v, want 270000/270000",
			current["global_context_ceiling"], current["effective_context_ceiling"])
	}
	if current["review_effort"] != "low" {
		t.Errorf("default review_effort = %v, want low", current["review_effort"])
	}
	if current["review_token_budget"] != float64(0) {
		t.Errorf("default review_token_budget = %v, want 0", current["review_token_budget"])
	}

	body := func() map[string]any {
		return map[string]any{
			"llm_profile_id":         profile.ID,
			"review_context_ceiling": 120000,
			"review_effort":          "high",
			"review_token_budget":    750000,
		}
	}
	rec = f.request(http.MethodPut, path, body(), cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["review_context_ceiling"] != float64(120000) ||
		saved["global_context_ceiling"] != float64(270000) ||
		saved["effective_context_ceiling"] != float64(120000) ||
		saved["review_effort"] != "high" || saved["review_token_budget"] != float64(750000) {
		t.Errorf("PUT controls = %v", saved)
	}
	stored, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if stored.ReviewContextCeiling == nil || *stored.ReviewContextCeiling != 120000 ||
		stored.ReviewEffort != "high" || stored.ReviewTokenBudget != 750000 {
		t.Errorf("stored controls = %+v", stored)
	}

	rec = f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &current)
	if current["review_context_ceiling"] != float64(120000) ||
		current["global_context_ceiling"] != float64(270000) ||
		current["effective_context_ceiling"] != float64(120000) {
		t.Errorf("GET controls = %v, want the team override to win", current)
	}

	// An explicit null clears the override and the global value applies again.
	clearing := body()
	clearing["review_context_ceiling"] = nil
	clearing["review_effort"] = "medium"
	clearing["review_token_budget"] = 0
	rec = f.request(http.MethodPut, path, clearing, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["review_context_ceiling"] != nil || saved["effective_context_ceiling"] != float64(270000) ||
		saved["review_effort"] != "medium" || saved["review_token_budget"] != float64(0) {
		t.Errorf("PUT clear = %v, want null/270000/medium/0", saved)
	}

	// PUT replaces the whole policy, so omitting the ceiling also clears it.
	rec = f.request(http.MethodPut, path, body(), cookie)
	assertStatus(t, rec, http.StatusOK)
	omitted := map[string]any{"llm_profile_id": profile.ID, "review_effort": "low"}
	rec = f.request(http.MethodPut, path, omitted, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["review_context_ceiling"] != nil || saved["global_context_ceiling"] != float64(270000) ||
		saved["effective_context_ceiling"] != float64(270000) {
		t.Errorf("PUT without ceiling = %v, want it cleared", saved)
	}

	// The global ceiling comes from the stored settings, not from the startup
	// config: a saved change must move the inherited value and leave a team
	// override untouched.
	if err := f.st.SaveSettings(f.ctx, nil, map[string]string{"review_context_ceiling": "300000"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	rec = f.request(http.MethodPut, path, body(), cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["review_context_ceiling"] != float64(120000) ||
		saved["global_context_ceiling"] != float64(300000) ||
		saved["effective_context_ceiling"] != float64(120000) {
		t.Errorf("PUT after global change = %v, want 120000/300000/120000", saved)
	}

	rec = f.request(http.MethodPut, path, omitted, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["review_context_ceiling"] != nil || saved["global_context_ceiling"] != float64(300000) ||
		saved["effective_context_ceiling"] != float64(300000) {
		t.Errorf("PUT inherited = %v, want null/300000/300000", saved)
	}

	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"ceiling zero", "review_context_ceiling", 0},
		{"ceiling negative", "review_context_ceiling", -1},
		{"ceiling below min", "review_context_ceiling", 4095},
		{"ceiling above max", "review_context_ceiling", 2000001},
		{"effort outside enum", "review_effort", "extreme"},
		{"budget negative", "review_token_budget", -1},
		{"budget above max", "review_token_budget", 1000000001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := body()
			invalid[tc.field] = tc.value
			rec := f.request(http.MethodPut, path, invalid, cookie)
			assertStatus(t, rec, http.StatusBadRequest)
			var apiErr map[string]string
			decodeJSON(t, rec, &apiErr)
			if want := "invalid " + tc.field; apiErr["error"] != want {
				t.Errorf("error = %q, want %q", apiErr["error"], want)
			}
		})
	}
}

func TestPolicyReportsPromptCache(t *testing.T) {
	f := newSecureFixture(t)
	cookie := f.session(f.user("cache-admin", true))

	seedPolicy := func(t *testing.T, name, provider string) (uint, uint) {
		t.Helper()
		team := f.team(name)
		profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
			Name: name, Provider: provider, Model: "test-model",
			APIKeyEnc: "sealed", Concurrency: 1, Enabled: true, HealthStatus: store.HealthOK,
		})
		if err != nil {
			t.Fatalf("CreateLLMProfile(%q): %v", name, err)
		}
		if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
			TeamID: team.ID, LLMProfileID: profile.ID,
		}); err != nil {
			t.Fatalf("SaveQAPolicy(%q): %v", name, err)
		}
		return team.ID, profile.ID
	}

	readPromptCache := func(t *testing.T, rec *httptest.ResponseRecorder) any {
		t.Helper()
		assertStatus(t, rec, http.StatusOK)
		var body map[string]any
		decodeJSON(t, rec, &body)
		return body["prompt_cache"]
	}

	for _, tc := range []struct {
		name     string
		provider string
		want     bool
	}{
		{"openai-compatible", "groq", true},
		{"anthropic", "anthropic", false},
		{"custom", "private-gateway", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			teamID, profileID := seedPolicy(t, "cache-"+tc.name, tc.provider)
			path := fmt.Sprintf("/api/teams/%d/policy", teamID)
			if got := readPromptCache(t, f.request(http.MethodGet, path, nil, cookie)); got != tc.want {
				t.Errorf("GET prompt_cache = %v, want %v", got, tc.want)
			}
			rec := f.request(http.MethodPut, path, policyBodyFor(profileID), cookie)
			if got := readPromptCache(t, rec); got != tc.want {
				t.Errorf("PUT prompt_cache = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("no profile", func(t *testing.T) {
		team := f.team("cache-none")
		rec := f.request(http.MethodGet, fmt.Sprintf("/api/teams/%d/policy", team.ID), nil, cookie)
		if got := readPromptCache(t, rec); got != false {
			t.Errorf("prompt_cache = %v, want false", got)
		}
	})

	t.Run("deleted profile", func(t *testing.T) {
		team := f.team("cache-deleted")
		profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
			Name: "cache-deleted", Provider: "groq", Model: "test-model",
			APIKeyEnc: "sealed", Concurrency: 1, Enabled: true, HealthStatus: store.HealthOK,
		})
		if err != nil {
			t.Fatalf("CreateLLMProfile: %v", err)
		}
		if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
			TeamID: team.ID, LLMProfileID: profile.ID,
		}); err != nil {
			t.Fatalf("SaveQAPolicy: %v", err)
		}
		// The store refuses to delete a profile in use, so remove the row with
		// FK enforcement off to reproduce a policy left behind by a deletion.
		if err := f.st.DB.WithContext(f.ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL session_replication_role = replica").Error; err != nil {
				return err
			}
			return tx.Delete(&store.LLMProfile{}, profile.ID).Error
		}); err != nil {
			t.Fatalf("delete profile: %v", err)
		}
		rec := f.request(http.MethodGet, fmt.Sprintf("/api/teams/%d/policy", team.ID), nil, cookie)
		if got := readPromptCache(t, rec); got != false {
			t.Errorf("prompt_cache = %v, want false", got)
		}
	})
}

func seedProfile(t *testing.T, f *secureFixture, name string, profile store.LLMProfile) store.LLMProfile {
	t.Helper()
	profile.Name = name
	profile.Provider = "openai"
	profile.Model = "gpt-4o-mini"
	profile.APIKeyEnc = "sealed"
	created, err := f.st.CreateLLMProfile(f.ctx, profile)
	if err != nil {
		t.Fatalf("CreateLLMProfile(%s): %v", name, err)
	}
	return created
}

func policyBodyFor(profileID uint) map[string]any {
	return map[string]any{
		"llm_profile_id":     profileID,
		"blocking_threshold": "major",
		"max_files":          12,
		"max_diff_lines":     900,
		"max_passes":         3,
	}
}

// TestPolicyRefusesUnhealthyProfile is the 409 contract: an operator must not
// point a team at a profile that is off, broken or never proven to work without
// saying so explicitly.
func TestPolicyRefusesUnhealthyProfile(t *testing.T) {
	checked := time.Now().UTC().Add(-time.Hour)
	cases := []struct {
		name    string
		profile store.LLMProfile
	}{
		{"disabled", store.LLMProfile{Enabled: false, HealthStatus: store.HealthOK, HealthCheckedAt: &checked}},
		{"error", store.LLMProfile{Enabled: true, HealthStatus: store.HealthError, HealthCheckedAt: &checked, HealthError: "401 unauthorized"}},
		{"never checked", store.LLMProfile{Enabled: true, HealthStatus: store.HealthUnknown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSecureFixture(t)
			profile := seedProfile(t, f, "primary", tc.profile)
			team := f.team("team")
			admin := f.user("admin", true)
			cookie := f.session(admin)
			path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

			rec := f.request(http.MethodPut, path, policyBodyFor(profile.ID), cookie)
			assertStatus(t, rec, http.StatusConflict)
			var body map[string]any
			decodeJSON(t, rec, &body)
			assertKeys(t, body, "error", "profile_id", "reason", "health_status", "health_checked_at", "hint")
			if body["error"] != "unhealthy_profile" {
				t.Errorf("error = %v, want unhealthy_profile", body["error"])
			}
			if uint(body["profile_id"].(float64)) != profile.ID {
				t.Errorf("profile_id = %v, want %d", body["profile_id"], profile.ID)
			}
			if body["health_status"] != tc.profile.HealthStatus {
				t.Errorf("health_status = %v, want %q", body["health_status"], tc.profile.HealthStatus)
			}
			if body["reason"] == "" || body["hint"] == "" {
				t.Errorf("reason/hint empty: %v", body)
			}
			if _, err := f.st.QAPolicyForTeam(f.ctx, team.ID); err == nil {
				t.Error("the policy was saved despite the 409")
			}
		})
	}
}

func TestPolicyConfirmUnhealthyProfileSaves(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "broken", store.LLMProfile{
		Enabled: true, HealthStatus: store.HealthError, HealthError: "401 unauthorized",
	})
	team := f.team("team")
	cookie := f.session(f.user("admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	body := policyBodyFor(profile.ID)
	body["confirm_unhealthy_profile"] = true
	rec := f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusOK)

	saved, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if saved.LLMProfileID != profile.ID {
		t.Errorf("saved profile = %d, want %d", saved.LLMProfileID, profile.ID)
	}
}

func TestPolicyRejectsInvalidConfirmationFlag(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "primary", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("team")
	cookie := f.session(f.user("admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	body := policyBodyFor(profile.ID)
	body["confirm_unhealthy_profile"] = "yes"
	rec := f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
}

// TestProfilesExposeHealthAge covers the panel's staleness signal: a healthy
// profile whose verdict is old must not look current.
func TestProfilesExposeHealthAge(t *testing.T) {
	f := newSecureFixture(t)
	fresh := time.Now().UTC().Add(-time.Minute)
	// The panel's staleness threshold is twice the 6h sweep interval.
	stale := time.Now().UTC().Add(-13 * time.Hour)
	seedProfile(t, f, "fresh", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK, HealthCheckedAt: &fresh})
	seedProfile(t, f, "stale", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK, HealthCheckedAt: &stale})
	seedProfile(t, f, "never", store.LLMProfile{Enabled: true, HealthStatus: store.HealthUnknown})
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodGet, "/api/llm-profiles", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var profiles []map[string]any
	decodeJSON(t, rec, &profiles)
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
	}

	byName := map[string]map[string]any{}
	for _, profile := range profiles {
		assertKeys(t, profile, "health_age_seconds", "health_stale")
		byName[profile["name"].(string)] = profile
	}
	if age := byName["fresh"]["health_age_seconds"].(float64); age < 0 || age > 120 {
		t.Errorf("fresh age = %v, want a minute at most", age)
	}
	if byName["fresh"]["health_stale"] != false {
		t.Errorf("fresh health_stale = %v, want false", byName["fresh"]["health_stale"])
	}
	if byName["stale"]["health_stale"] != true {
		t.Errorf("stale health_stale = %v, want true", byName["stale"]["health_stale"])
	}
	if byName["never"]["health_stale"] != true {
		t.Errorf("never-checked health_stale = %v, want true", byName["never"]["health_stale"])
	}
}

func (f *secureFixture) createProfile(name string) store.LLMProfile {
	f.t.Helper()
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: name, Provider: "anthropic", Model: "claude-test",
		APIKeyEnc: "sealed", Concurrency: 1, Enabled: true,
	})
	if err != nil {
		f.t.Fatalf("CreateLLMProfile(%q): %v", name, err)
	}
	return profile
}

func TestProfileUsageRequiresAdmin(t *testing.T) {
	f := newSecureFixture(t)
	dev := f.user("usage-dev", false)

	assertStatus(t, f.request(http.MethodGet, "/api/llm-profiles/usage", nil, f.session(dev)), http.StatusForbidden)
	assertStatus(t, f.request(http.MethodGet, "/api/llm-profiles/usage", nil, nil), http.StatusUnauthorized)
}

func TestProfileUsageEmptyState(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("usage-admin", true)
	profile := f.createProfile("usage-empty")

	rec := f.request(http.MethodGet, "/api/llm-profiles/usage", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	var raw map[string]any
	decodeJSON(t, rec, &raw)

	assertKeys(t, raw, "window_days", "has_data", "profiles")
	if raw["window_days"] != float64(7) {
		t.Errorf("window_days = %v, want 7", raw["window_days"])
	}
	if raw["has_data"] != false {
		t.Errorf("has_data = %v, want false", raw["has_data"])
	}
	profiles := raw["profiles"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("profiles = %v, want one entry", profiles)
	}
	entry := profiles[0].(map[string]any)
	assertKeys(t, entry, "profile_id", "name", "provider", "model", "has_data",
		"runs", "checks", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_tokens", "total_tokens", "average_tokens")
	if uint(entry["profile_id"].(float64)) != profile.ID {
		t.Errorf("profile_id = %v, want %d", entry["profile_id"], profile.ID)
	}
	if entry["has_data"] != false || entry["total_tokens"] != nil {
		t.Errorf("entry = %v, want no data and a null total", entry)
	}
}

func TestProfileUsageAggregatesRunsAndChecks(t *testing.T) {
	f := newSecureFixture(t)
	admin := f.user("usage-admin", true)
	profile := f.createProfile("usage-data")

	inst := f.installation(2101)
	repo := f.repo(2101, inst.ID, "acme/usage", nil, true)
	pr := f.pr(repo.ID, 1, store.StatusReadyForHuman)
	finished := time.Now().UTC().Add(-time.Hour)
	total := int64(300)
	input := int64(240)
	output := int64(10)
	cacheRead := int64(50)
	run := store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, Status: store.RunSucceeded,
		FinishedAt: &finished, TotalTokens: &total, InputTokens: &input,
		OutputTokens: &output, CacheReadTokens: &cacheRead,
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	if err := f.st.RecordLLMHealth(f.ctx, profile.ID, store.HealthOK, "", &store.HealthUsage{
		Input: 10, Output: 1, Total: 11,
	}); err != nil {
		t.Fatalf("RecordLLMHealth: %v", err)
	}

	rec := f.request(http.MethodGet, "/api/llm-profiles/usage", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	var raw map[string]any
	decodeJSON(t, rec, &raw)

	if raw["has_data"] != true {
		t.Errorf("has_data = %v, want true", raw["has_data"])
	}
	profiles := raw["profiles"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("profiles = %v, want one", profiles)
	}
	entry := profiles[0].(map[string]any)
	if entry["runs"] != float64(1) || entry["checks"] != float64(1) {
		t.Errorf("counts = %v runs / %v checks, want 1 / 1", entry["runs"], entry["checks"])
	}
	if entry["total_tokens"] != float64(311) {
		t.Errorf("total_tokens = %v, want 311", entry["total_tokens"])
	}
	if entry["input_tokens"] != float64(250) || entry["output_tokens"] != float64(11) {
		t.Errorf("input/output = %v/%v, want 250/11", entry["input_tokens"], entry["output_tokens"])
	}
	if entry["cache_read_tokens"] != float64(50) {
		t.Errorf("cache_read_tokens = %v, want 50", entry["cache_read_tokens"])
	}
	if entry["average_tokens"] != float64(155) {
		t.Errorf("average_tokens = %v, want 155 (311/2)", entry["average_tokens"])
	}
}

func autoTriageBody(profileID uint) map[string]any {
	return map[string]any{
		"llm_profile_id":      profileID,
		"auto_triage_mode":    "apply",
		"auto_withdraw":       true,
		"verify_max_findings": 10,
		"verify_max_files":    5,
		"verify_token_share":  30,
		"verify_time_share":   10,
		"verify_token_max":    200000,
	}
}

func TestPolicyAcceptsAutoTriageFields(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "triage", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("triage-team")
	cookie := f.session(f.user("triage-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	rec := f.request(http.MethodPut, path, autoTriageBody(profile.ID), cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["auto_triage_mode"] != "apply" || saved["auto_withdraw"] != true ||
		saved["verify_max_findings"] != float64(10) || saved["verify_max_files"] != float64(5) ||
		saved["verify_token_share"] != float64(30) || saved["verify_time_share"] != float64(10) ||
		saved["verify_token_max"] != float64(200000) {
		t.Errorf("saved auto-triage = %v", saved)
	}

	stored, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if stored.AutoTriageMode != store.AutoTriageApply || !stored.AutoWithdraw ||
		stored.VerifyMaxFindings != 10 || stored.VerifyMaxFiles != 5 ||
		stored.VerifyTokenShare != 30 || stored.VerifyTimeShare != 10 ||
		stored.VerifyTokenMax != 200000 {
		t.Errorf("stored auto-triage = %+v", stored)
	}

	rec = f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["auto_triage_mode"] != "apply" || saved["verify_token_max"] != float64(200000) {
		t.Errorf("GET auto-triage = %v", saved)
	}
}

func TestPolicyRejectsInvalidAutoTriageMode(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "triage", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("triage-team")
	cookie := f.session(f.user("triage-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	body := autoTriageBody(profile.ID)
	body["auto_triage_mode"] = "sometimes"
	rec := f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if apiErr["error"] != "invalid auto_triage_mode" {
		t.Errorf("error = %q, want invalid auto_triage_mode", apiErr["error"])
	}
}

func TestPolicyRejectsVerifySharesOutsideRange(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "triage", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("triage-team")
	cookie := f.session(f.user("triage-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	for _, tc := range []struct {
		field string
		value int
	}{
		{"verify_token_share", -1},
		{"verify_token_share", 101},
		{"verify_time_share", -1},
		{"verify_time_share", 101},
		{"verify_max_findings", -1},
		{"verify_max_files", -1},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.field, tc.value), func(t *testing.T) {
			body := autoTriageBody(profile.ID)
			body[tc.field] = tc.value
			rec := f.request(http.MethodPut, path, body, cookie)
			assertStatus(t, rec, http.StatusBadRequest)
			var apiErr map[string]string
			decodeJSON(t, rec, &apiErr)
			if want := "invalid " + tc.field; apiErr["error"] != want {
				t.Errorf("error = %q, want %q", apiErr["error"], want)
			}
		})
	}
}

func TestPolicyRejectsVerifyTokenMaxOutOfRange(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "triage", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("triage-team")
	cookie := f.session(f.user("triage-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	for _, value := range []int64{store.VerifyTokenMaxMin - 1, store.ReviewTokenBudgetMax + 1} {
		body := autoTriageBody(profile.ID)
		body["verify_token_max"] = value
		rec := f.request(http.MethodPut, path, body, cookie)
		assertStatus(t, rec, http.StatusBadRequest)
		var apiErr map[string]string
		decodeJSON(t, rec, &apiErr)
		if apiErr["error"] != "invalid verify_token_max" {
			t.Errorf("value %d: error = %q, want invalid verify_token_max", value, apiErr["error"])
		}
	}
}

func TestPolicyDefaultsAutoTriageOff(t *testing.T) {
	f := newSecureFixture(t)
	profile := seedProfile(t, f, "triage", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("triage-team")
	cookie := f.session(f.user("triage-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	rec := f.request(http.MethodPut, path, map[string]any{"llm_profile_id": profile.ID}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["auto_triage_mode"] != "off" || saved["auto_withdraw"] != false ||
		saved["verify_max_findings"] != float64(store.DefaultVerifyMaxFindings) ||
		saved["verify_max_files"] != float64(store.DefaultVerifyMaxFiles) ||
		saved["verify_token_share"] != float64(store.DefaultVerifyTokenShare) ||
		saved["verify_time_share"] != float64(store.DefaultVerifyTimeShare) ||
		saved["verify_token_max"] != float64(store.DefaultVerifyTokenMax) {
		t.Errorf("default auto-triage = %v", saved)
	}
}

func TestPolicyFallbackProfilesRoundTrip(t *testing.T) {
	f := newSecureFixture(t)
	primary := seedProfile(t, f, "fallback-primary", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	first := seedProfile(t, f, "fallback-first", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	second := seedProfile(t, f, "fallback-second", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	team := f.team("fallback-team")
	cookie := f.session(f.user("fallback-admin", true))
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	body := map[string]any{
		"llm_profile_id":       primary.ID,
		"fallback_profile_ids": []uint{second.ID, first.ID},
	}
	rec := f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	assertFallbackOrder(t, saved["fallback_profile_ids"], second.ID, first.ID)

	stored, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	got := store.FallbackProfileIDs(stored)
	if len(got) != 2 || got[0] != second.ID || got[1] != first.ID {
		t.Fatalf("stored fallbacks = %v, want [%d %d]", got, second.ID, first.ID)
	}

	// An omitted list keeps the authorized backups; an explicit empty list
	// clears them.
	rec = f.request(http.MethodPut, path, map[string]any{"llm_profile_id": primary.ID}, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	assertFallbackOrder(t, saved["fallback_profile_ids"], second.ID, first.ID)

	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": primary.ID, "fallback_profile_ids": []uint{},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	assertFallbackOrder(t, saved["fallback_profile_ids"])

	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": primary.ID, "fallback_profile_ids": []uint{999999},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
}

func assertFallbackOrder(t *testing.T, value any, want ...uint) {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("fallback_profile_ids = %#v, want an array", value)
	}
	if len(items) != len(want) {
		t.Fatalf("fallback_profile_ids = %v, want %v", items, want)
	}
	for i := range want {
		if items[i] != float64(want[i]) {
			t.Fatalf("fallback_profile_ids = %v, want %v", items, want)
		}
	}
}
func TestPolicyExternalContributionFields(t *testing.T) {
	f := newSecureFixture(t)
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
		HealthStatus: store.HealthOK,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	team := f.team("external-team")
	admin := f.user("admin", true)
	cookie := f.session(admin)
	path := fmt.Sprintf("/api/teams/%d/policy", team.ID)

	rec := f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var current map[string]any
	decodeJSON(t, rec, &current)
	if current["allow_external_reviews"] != false ||
		current["max_external_diff_lines"] != float64(store.DefaultExternalDiffLines) ||
		current["max_external_runs_per_day"] != float64(store.DefaultExternalRunsPerDay) ||
		current["external_daily_token_budget"] != float64(0) {
		t.Fatalf("external defaults = %+v", current)
	}

	body := map[string]any{
		"llm_profile_id":                profile.ID,
		"allow_external_reviews":        true,
		"external_trusted_associations": []string{"OWNER", "MEMBER"},
		"max_external_diff_lines":       500,
		"max_external_runs_per_day":     3,
		"external_daily_token_budget":   9000,
	}
	rec = f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["allow_external_reviews"] != true ||
		saved["max_external_diff_lines"] != float64(500) ||
		saved["max_external_runs_per_day"] != float64(3) ||
		saved["external_daily_token_budget"] != float64(9000) {
		t.Fatalf("saved external policy = %+v", saved)
	}

	// A PUT that only carries one external field keeps the rest from the
	// stored policy instead of resetting the budget to unlimited.
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id":         profile.ID,
		"allow_external_reviews": false,
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &saved)
	if saved["allow_external_reviews"] != false ||
		saved["max_external_diff_lines"] != float64(500) ||
		saved["max_external_runs_per_day"] != float64(3) ||
		saved["external_daily_token_budget"] != float64(9000) {
		t.Fatalf("partial PUT reset external limits: %+v", saved)
	}

	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "max_external_runs_per_day": 0,
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	rec = f.request(http.MethodPut, path, map[string]any{
		"llm_profile_id": profile.ID, "external_daily_token_budget": -1,
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
}
