package api_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func strconvUint(id uint) string { return strconv.FormatUint(uint64(id), 10) }

type ruleResponse struct {
	ID               uint                  `json:"id"`
	RepositoryID     int64                 `json:"repository_id"`
	RuleKey          string                `json:"rule_key"`
	Version          int                   `json:"version"`
	Status           string                `json:"status"`
	Decision         string                `json:"decision"`
	Reason           string                `json:"reason"`
	PathScope        string                `json:"path_scope"`
	PositiveExamples []store.RuleExample   `json:"positive_examples"`
	NegativeExamples []store.RuleExample   `json:"negative_examples"`
	Evaluation       *store.RuleEvaluation `json:"evaluation"`
	ApprovedBy       string                `json:"approved_by"`
	Expired          bool                  `json:"expired"`
}

func seedRuleRepo(t *testing.T, f *apiFixture) (store.Repository, store.Finding) {
	t.Helper()

	team := f.team("core")
	profile := f.seedHealthyProfile("primary")
	if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	installation := f.installation(f.nextID())
	teamID := team.ID
	repo := f.repo(900, installation.ID, "acme/app", &teamID, true)
	pr := f.pr(repo.ID, 1, store.StatusReadyForHuman)
	finding, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "rule-finding", PullRequestID: pr.ID, Fingerprint: "fp-rule",
		Path: "internal/review/a.go", Category: "bug", Title: "nil pointer on optional field",
		Severity: "minor", Status: store.FindingOpen, ExistingCode: "if value == nil",
	})
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	if _, err := f.st.TriageFinding(f.ctx, finding.ID, store.FindingWithdrawn, store.FindingAudit{
		Action: "finding.withdrawn", Actor: "lead", Source: store.TriageSourceHuman,
		Reason: "el valor está validado antes de esta rama",
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}
	return repo, finding
}

func TestReviewRuleLifecycleAPI(t *testing.T) {
	f := newAPIFixture(t)
	repo, finding := seedRuleRepo(t, f)
	admin := f.user("admin", true)
	cookie := f.session(admin)
	base := "/api/repos/900/rules"

	rec := f.request(http.MethodPost, base, map[string]any{
		"finding_id": finding.ID,
		"reason":     "el valor está validado antes de esta rama",
		"negative_examples": []map[string]any{{
			"path": "internal/review/a.go", "category": "bug",
			"title": "nil map write in cache", "severity": "major",
		}},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var proposed ruleResponse
	decodeJSON(t, rec, &proposed)
	if proposed.Status != store.RuleStatusProposed || proposed.Decision != store.RuleDecisionWithdraw {
		t.Fatalf("proposed = %+v, want a proposed withdraw rule", proposed)
	}
	if proposed.PathScope != finding.Path || len(proposed.PositiveExamples) != 1 {
		t.Errorf("scope/examples = %q/%d, want the source finding", proposed.PathScope, len(proposed.PositiveExamples))
	}
	if proposed.RepositoryID != repo.ID {
		t.Errorf("repository = %d, want %d", proposed.RepositoryID, repo.ID)
	}

	rec = f.request(http.MethodPut, base+"/"+strconvUint(proposed.ID), map[string]any{
		"action": "activate", "reason": "muestra revisada",
	}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("activate before evaluation = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	rec = f.request(http.MethodPut, base+"/"+strconvUint(proposed.ID), map[string]any{
		"action": "evaluate",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var evaluated ruleResponse
	decodeJSON(t, rec, &evaluated)
	if evaluated.Evaluation == nil || evaluated.Evaluation.MatchedPositive != 1 ||
		evaluated.Evaluation.SevereOmissions != 0 {
		t.Fatalf("evaluation = %+v, want the positive example matched without severe omissions", evaluated.Evaluation)
	}

	rec = f.request(http.MethodPut, base+"/"+strconvUint(proposed.ID), map[string]any{
		"action": "activate", "reason": "muestra revisada",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var active ruleResponse
	decodeJSON(t, rec, &active)
	if active.Status != store.RuleStatusActive || active.ApprovedBy != admin.Login || active.Expired {
		t.Fatalf("active = %+v, want an approved, unexpired active rule", active)
	}

	rec = f.request(http.MethodGet, base, nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var listed []ruleResponse
	decodeJSON(t, rec, &listed)
	if len(listed) != 1 || listed[0].ID != active.ID {
		t.Fatalf("listed = %+v, want the active rule", listed)
	}

	rec = f.request(http.MethodPut, base+"/"+strconvUint(active.ID), map[string]any{
		"action": "disable", "reason": "deja de aplicar",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var disabled ruleResponse
	decodeJSON(t, rec, &disabled)
	if disabled.Status != store.RuleStatusDisabled {
		t.Fatalf("status = %q, want disabled", disabled.Status)
	}

	if rows := auditRows(t, f, "review_rule.proposed"); len(rows) != 1 {
		t.Errorf("proposal audit rows = %d, want 1", len(rows))
	}
	if rows := auditRows(t, f, "review_rule.activate"); len(rows) != 1 {
		t.Errorf("activation audit rows = %d, want 1", len(rows))
	}
}

func TestReviewRuleActivationRejectsSevereOmission(t *testing.T) {
	f := newAPIFixture(t)
	_, finding := seedRuleRepo(t, f)
	admin := f.user("admin", true)
	cookie := f.session(admin)
	base := "/api/repos/900/rules"

	rec := f.request(http.MethodPost, base, map[string]any{
		"finding_id": finding.ID,
		"reason":     "el valor está validado antes de esta rama",
		"positive_examples": []map[string]any{{
			"path": "internal/review/a.go", "category": "bug",
			"title": "nil pointer on optional field",
		}},
		"negative_examples": []map[string]any{{
			"path": "internal/review/a.go", "category": "bug",
			"title": "NIL pointer on optional field", "severity": "major",
		}},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var proposed ruleResponse
	decodeJSON(t, rec, &proposed)

	rec = f.request(http.MethodPut, base+"/"+strconvUint(proposed.ID), map[string]any{"action": "evaluate"}, cookie)
	assertStatus(t, rec, http.StatusOK)
	var evaluated ruleResponse
	decodeJSON(t, rec, &evaluated)
	if evaluated.Evaluation == nil || evaluated.Evaluation.SevereOmissions != 1 {
		t.Fatalf("evaluation = %+v, want one severe omission", evaluated.Evaluation)
	}

	rec = f.request(http.MethodPut, base+"/"+strconvUint(proposed.ID), map[string]any{
		"action": "activate", "reason": "intento de promoción",
	}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("activate with severe omission = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

func TestReviewRuleIsScopedToItsRepository(t *testing.T) {
	f := newAPIFixture(t)
	repo, finding := seedRuleRepo(t, f)
	f.repo(901, repo.InstallationID, "acme/other", repo.TeamID, true)
	admin := f.user("admin", true)
	cookie := f.session(admin)

	rec := f.request(http.MethodPost, "/api/repos/900/rules", map[string]any{
		"finding_id": finding.ID,
		"reason":     "el valor está validado antes de esta rama",
	}, cookie)
	assertStatus(t, rec, http.StatusOK)

	rec = f.request(http.MethodGet, "/api/repos/901/rules", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var listed []ruleResponse
	decodeJSON(t, rec, &listed)
	if len(listed) != 0 {
		t.Fatalf("other repository rules = %+v, want none", listed)
	}
}
