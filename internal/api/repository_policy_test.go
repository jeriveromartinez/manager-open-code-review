package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type repoPolicyBody struct {
	RepositoryID int64             `json:"repository_id"`
	TeamID       *uint             `json:"team_id"`
	Inherited    bool              `json:"inherited"`
	Revision     string            `json:"revision"`
	Provenance   map[string]string `json:"provenance"`
	Overrides    struct {
		ObservationMode   bool    `json:"observation_mode"`
		BlockingThreshold *string `json:"blocking_threshold"`
		MaxFiles          *int    `json:"max_files"`
	} `json:"overrides"`
	Effective struct {
		BlockingThreshold string `json:"blocking_threshold"`
		MaxFiles          int    `json:"max_files"`
		RequireGreenCI    bool   `json:"require_green_ci"`
		Revision          string `json:"revision"`
	} `json:"effective"`
	Enforced struct {
		BlockingThreshold string `json:"blocking_threshold"`
		RequireGreenCI    bool   `json:"require_green_ci"`
	} `json:"enforced"`
	UpdatedAt *string `json:"updated_at"`
}

func seedRepoPolicyFixture(t *testing.T, f *apiFixture) (store.Repository, store.Team) {
	t.Helper()

	team := f.team("core")
	profile := f.seedHealthyProfile("primary")
	if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "major", MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000,
		MaxPasses: 4, ReviewEffort: "low",
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	installation := f.installation(f.nextID())
	teamID := team.ID
	repo := f.repo(900, installation.ID, "acme/app", &teamID, true)
	return repo, team
}

func auditRows(t *testing.T, f *apiFixture, action string) []store.AuditLog {
	t.Helper()

	var rows []store.AuditLog
	if err := f.st.DB.WithContext(f.ctx).Where("action = ?", action).
		Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("load audit rows %q: %v", action, err)
	}
	return rows
}

func TestRepositoryPolicyExposesProvenanceAndAuditsChanges(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	_, _ = seedRepoPolicyFixture(t, f)
	cookie := f.session(admin)
	path := "/api/repos/900/policy"

	var inherited repoPolicyBody
	rec := f.request(http.MethodGet, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &inherited)
	if !inherited.Inherited || inherited.Overrides.BlockingThreshold != nil {
		t.Fatalf("inherited = %+v, want a repository without an override", inherited)
	}
	if inherited.Effective.BlockingThreshold != "major" || inherited.Effective.MaxFiles != 40 {
		t.Errorf("effective = %+v, want the team policy", inherited.Effective)
	}
	if inherited.Provenance["blocking_threshold"] != store.PolicySourceTeam {
		t.Errorf("provenance = %v, want the threshold from the team", inherited.Provenance)
	}
	if inherited.Revision == "" {
		t.Fatal("revision is empty, want an identifiable effective policy")
	}

	rec = f.request(http.MethodPut, path, map[string]any{
		"blocking_threshold": "blocker",
		"max_files":          10,
		"observation_mode":   true,
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var overridden repoPolicyBody
	decodeJSON(t, rec, &overridden)
	if overridden.Inherited {
		t.Error("inherited = true after saving an override, want false")
	}
	if overridden.Revision == inherited.Revision {
		t.Error("revision did not change with the override")
	}
	if overridden.Effective.BlockingThreshold != "blocker" || overridden.Effective.MaxFiles != 10 {
		t.Errorf("effective = %+v, want the observed override", overridden.Effective)
	}
	if overridden.Enforced.BlockingThreshold != "major" {
		t.Errorf("enforced threshold = %q, want the team value while observing",
			overridden.Enforced.BlockingThreshold)
	}
	if overridden.Provenance["blocking_threshold"] != store.PolicySourceRepo ||
		overridden.Provenance["max_files"] != store.PolicySourceRepo {
		t.Errorf("provenance = %v, want the overridden fields from the repo", overridden.Provenance)
	}
	if overridden.UpdatedAt == nil {
		t.Error("updated_at is null for a saved override")
	}

	rows := auditRows(t, f, "repo.policy.updated")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want one for the saved override", len(rows))
	}
	var audited map[string]any
	if err := json.Unmarshal(rows[0].Data, &audited); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if audited["revision"] != overridden.Revision {
		t.Errorf("audited revision = %v, want %q", audited["revision"], overridden.Revision)
	}
	if audited["observation_mode"] != true {
		t.Errorf("audited observation_mode = %v, want true", audited["observation_mode"])
	}

	// The repository list reflects the override without resolving the policy.
	rec = f.request(http.MethodGet, "/api/repos", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var repos []map[string]any
	decodeJSON(t, rec, &repos)
	if len(repos) != 1 || repos[0]["has_policy_override"] != true || repos[0]["policy_observation"] != true {
		t.Errorf("repos = %v, want the override and its observation flag", repos)
	}

	rec = f.request(http.MethodDelete, path, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var cleared repoPolicyBody
	decodeJSON(t, rec, &cleared)
	if !cleared.Inherited || cleared.UpdatedAt != nil {
		t.Errorf("cleared = %+v, want inheritance and no updated_at", cleared)
	}
	if cleared.Effective.BlockingThreshold != "major" || cleared.Effective.MaxFiles != 40 {
		t.Errorf("effective = %+v, want the team policy after clearing", cleared.Effective)
	}
	if len(auditRows(t, f, "repo.policy.cleared")) != 1 {
		t.Error("clearing the override was not audited")
	}
}

func TestRepositoryPolicyRejectsInvalidOverride(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	seedRepoPolicyFixture(t, f)

	rec := f.request(http.MethodPut, "/api/repos/900/policy", map[string]any{
		"blocking_threshold": "critical",
	}, f.session(admin))
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestRepositoryPolicyRequiresAdminOrTeamLead(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	dev := f.user("dev", false)
	lead := f.user("lead", false)
	installation := f.installation(f.nextID())
	f.repo(901, installation.ID, "acme/no-team", nil, true)

	rec := f.request(http.MethodGet, "/api/repos/901/policy", nil, f.session(admin))
	assertStatus(t, rec, http.StatusConflict)
	// An outsider gets 403, never the no-team conflict that would reveal it.
	rec = f.request(http.MethodGet, "/api/repos/901/policy", nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	_, team := seedRepoPolicyFixture(t, f)
	f.member(team.ID, lead.ID, "lead")

	rec = f.request(http.MethodGet, "/api/repos/900/policy", nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	rec = f.request(http.MethodGet, "/api/repos/900/policy", nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)

	rec = f.request(http.MethodPut, "/api/repos/900/policy", map[string]any{
		"max_files": 5,
	}, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
}
