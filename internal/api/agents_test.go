package api_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
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
		Cfg:        f.cfg,
		Store:      f.st,
		Box:        f.box,
		WorkerBox:  workerBox,
		Apps:       provider,
		Auth:       a,
		Webhooks:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		Broker:     events.NewBroker(f.st.Pool),
		Events:     f.pub,
		Jobs:       f.jobs,
		SetupToken: testSetupToken,
		Static: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("spa"))
		}),
		Health:  health,
		Reviews: reviews,
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
	path := fmt.Sprintf("/api/llm-profiles/%d/health", id)

	rec = f.request(http.MethodPost, path, nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	rec = f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusAccepted)
	if ids := f.health.snapshot(); len(ids) != 1 || ids[0] != id {
		t.Errorf("EnqueueLLMHealth calls = %v, want [%d]", ids, id)
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
		"max_diff_lines", "max_passes", "exclude_globs", "require_green_ci")
	if current["require_green_ci"] != false {
		t.Errorf("default require_green_ci = %v, want false", current["require_green_ci"])
	}

	body := map[string]any{"llm_profile_id": profile.ID, "require_green_ci": true}
	rec = f.request(http.MethodPut, path, body, cookie)
	assertStatus(t, rec, http.StatusOK)
	var saved map[string]any
	decodeJSON(t, rec, &saved)
	if saved["require_green_ci"] != true {
		t.Errorf("PUT require_green_ci = %v, want true", saved["require_green_ci"])
	}

	policy, err := f.st.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if !policy.RequireGreenCI {
		t.Error("stored RequireGreenCI = false, want true")
	}

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
	stale := time.Now().UTC().Add(-3 * time.Hour)
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
