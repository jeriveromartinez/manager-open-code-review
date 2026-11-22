package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestQAMemberTriagesFindingsAndTheResponseNamesThem(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	qa := f.user("panel-qa", false)
	f.member(findingTeamID(t, f, pr), qa.ID, "qa")

	confirm := fmt.Sprintf("/api/prs/%d/findings/%s/confirm", pr.ID, finding.ID)
	rec := f.request(http.MethodPost, confirm, nil, f.session(qa))
	assertStatus(t, rec, http.StatusOK)

	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["status"] != store.FindingOpen || body["triaged_by"] != qa.Login ||
		body["triage_verdict"] != store.VerdictConfirmed {
		t.Fatalf("confirm response status=%v verdict=%v triaged_by=%v, want open, confirmed by %s",
			body["status"], body["triage_verdict"], body["triaged_by"], qa.Login)
	}
	if at, _ := body["triaged_at"].(string); at == "" {
		t.Errorf("triaged_at = %v, want a timestamp", body["triaged_at"])
	}

	withdraw := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	rec = f.request(http.MethodPost, withdraw, map[string]any{"reason": "falso positivo"}, f.session(qa))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &body)
	if body["status"] != store.FindingWithdrawn {
		t.Errorf("withdraw response status = %v, want %s", body["status"], store.FindingWithdrawn)
	}
}

func TestConfirmingAResolvedFindingConflicts(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	findings := &recordingFindings{}
	f.deps.Findings = findings
	f.rebuildRouter()
	admin := f.user("resolved-admin", true)
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Finding{}).
		Where("id = ?", finding.ID).Update("status", store.FindingResolved).Error; err != nil {
		t.Fatalf("seed resolved finding: %v", err)
	}

	confirm := fmt.Sprintf("/api/prs/%d/findings/%s/confirm", pr.ID, finding.ID)
	assertStatus(t, f.request(http.MethodPost, confirm, nil, f.session(admin)), http.StatusConflict)

	got, err := f.st.Finding(f.ctx, finding.ID)
	if err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingResolved {
		t.Errorf("status = %q, want it still resolved", got.Status)
	}
	if calls := findings.threadCalls(); len(calls) != 0 {
		t.Errorf("thread calls = %v, want none for a rejected triage", calls)
	}
}

func TestQAMemberCanRetryAReview(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1501)
	team := f.team("qa-retry-team")
	repo := f.repo(7501, inst.ID, "acme/qa-retry", &team.ID, true)
	pr := f.pr(repo.ID, 3, store.StatusInconclusive)
	qa := f.user("retry-qa", false)
	f.member(team.ID, qa.ID, "qa")
	dev := f.user("retry-dev", false)
	f.member(team.ID, dev.ID, "developer")

	path := fmt.Sprintf("/api/prs/%d/retry", pr.ID)
	assertStatus(t, f.request(http.MethodPost, path, nil, f.session(qa)), http.StatusAccepted)
	assertStatus(t, f.request(http.MethodPost, path, nil, f.session(dev)), http.StatusForbidden)
}

func TestPRListReportsOpenFindingsAndStatusSince(t *testing.T) {
	f, pr, _ := newFindingFixture(t)
	admin := f.user("list-admin", true)

	rec := f.request(http.MethodGet, "/api/prs", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var prs []map[string]any
	decodeJSON(t, rec, &prs)
	if len(prs) != 1 || uint(prs[0]["id"].(float64)) != pr.ID {
		t.Fatalf("prs = %v, want only PR %d", prs, pr.ID)
	}
	counts, ok := prs[0]["open_findings"].(map[string]any)
	if !ok || counts["blocker"] != float64(1) || counts["major"] != float64(0) {
		t.Errorf("open_findings = %v, want one blocker", prs[0]["open_findings"])
	}
	since, _ := prs[0]["status_since"].(string)
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		t.Errorf("status_since = %q, want an RFC 3339 timestamp: %v", since, err)
	}
}

func TestQASummaryEndpoint(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	f.deps.Cfg.QAValidationSLA = 2 * time.Hour
	f.rebuildRouter()
	qa := f.user("summary-qa", false)
	f.member(findingTeamID(t, f, pr), qa.ID, "qa")
	f.pr(pr.RepositoryID, 22, store.StatusReadyForHuman)

	rec := f.request(http.MethodGet, "/api/qa/summary", nil, f.session(qa))
	assertStatus(t, rec, http.StatusOK)
	var raw map[string]any
	decodeJSON(t, rec, &raw)
	assertKeys(t, raw, "ready_for_human", "inconclusive", "runs_running", "runs_queued",
		"untriaged", "confirmed", "false_positive", "precision_window_days",
		"categories", "category_window_days", "tokens", "validation_sla_seconds")
	tokens := raw["tokens"].(map[string]any)
	assertKeys(t, tokens, "window_days", "reviews", "input", "output", "cache_read",
		"cache_write", "total", "average", "max_context", "max_context_known")
	if tokens["window_days"] != float64(7) || tokens["reviews"] != float64(0) || tokens["average"] != float64(0) {
		t.Errorf("tokens = %v, want an empty 7-day window", tokens)
	}
	if tokens["max_context_known"] != false {
		t.Errorf("max_context_known = %v, want false for an empty window", tokens["max_context_known"])
	}
	if raw["ready_for_human"] != float64(1) {
		t.Errorf("ready_for_human = %v, want 1", raw["ready_for_human"])
	}
	if raw["validation_sla_seconds"] != float64(7200) {
		t.Errorf("validation_sla_seconds = %v, want 7200", raw["validation_sla_seconds"])
	}
	if untriaged := raw["untriaged"].(map[string]any); untriaged["blocker"] != float64(1) {
		t.Errorf("untriaged = %v, want one blocker", untriaged)
	}

	confirm := fmt.Sprintf("/api/prs/%d/findings/%s/confirm", pr.ID, finding.ID)
	assertStatus(t, f.request(http.MethodPost, confirm, nil, f.session(qa)), http.StatusOK)

	rec = f.request(http.MethodGet, "/api/qa/summary", nil, f.session(qa))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &raw)
	if raw["confirmed"] != float64(1) {
		t.Errorf("confirmed after triage = %v, want 1", raw["confirmed"])
	}
	categories := raw["categories"].([]any)
	if len(categories) != 1 || categories[0].(map[string]any)["category"] != "security" {
		t.Errorf("categories = %v, want [security]", categories)
	}

	assertStatus(t, f.request(http.MethodGet, "/api/qa/summary", nil, nil), http.StatusUnauthorized)
}

func TestQASummaryNarrowsToOneTeam(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1601)
	payments := f.team("lead-payments")
	web := f.team("lead-web")
	lead := f.user("summary-lead", false)
	f.member(payments.ID, lead.ID, "lead")
	f.member(web.ID, lead.ID, "lead")
	paymentsRepo := f.repo(7601, inst.ID, "acme/lead-payments", &payments.ID, true)
	webRepo := f.repo(7602, inst.ID, "acme/lead-web", &web.ID, true)
	f.pr(paymentsRepo.ID, 1, store.StatusReadyForHuman)
	f.pr(webRepo.ID, 2, store.StatusReadyForHuman)
	f.pr(webRepo.ID, 3, store.StatusReadyForHuman)

	var raw map[string]any
	rec := f.request(http.MethodGet, fmt.Sprintf("/api/qa/summary?team_id=%d", payments.ID), nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &raw)
	if raw["ready_for_human"] != float64(1) {
		t.Errorf("Payments ready_for_human = %v, want 1", raw["ready_for_human"])
	}
	if _, ok := raw["confirmed_by_repo"].([]any); !ok {
		t.Errorf("confirmed_by_repo = %v, want a list", raw["confirmed_by_repo"])
	}

	rec = f.request(http.MethodGet, "/api/prs", nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
	var prs []map[string]any
	decodeJSON(t, rec, &prs)
	if len(prs) != 3 {
		t.Fatalf("got %d PRs, want the 3 of both led teams", len(prs))
	}
	for _, pr := range prs {
		if pr["team_id"] == nil {
			t.Errorf("PR %v has no team_id", pr["id"])
		}
	}

	for _, bad := range []string{"abc", "0", "-1"} {
		path := "/api/qa/summary?team_id=" + bad
		assertStatus(t, f.request(http.MethodGet, path, nil, f.session(lead)), http.StatusBadRequest)
	}
}
