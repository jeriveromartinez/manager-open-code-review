package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *fixture) finding(pr store.PullRequest, id, severity, category, status string) store.Finding {
	f.t.Helper()

	saved, err := f.s.UpsertFinding(f.ctx, store.Finding{
		ID: id, PullRequestID: pr.ID, Fingerprint: "fp-" + id,
		Path: "main.go", Severity: severity, Category: category, Status: status,
		FirstSeenRunID: 1, LastSeenRunID: 1,
	})
	if err != nil {
		f.t.Fatalf("UpsertFinding(%q): %v", id, err)
	}
	return saved
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if err := f.s.DB.WithContext(f.ctx).Exec(sql, args...).Error; err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) auditRows(findingID string) int64 {
	f.t.Helper()
	var n int64
	err := f.s.DB.WithContext(f.ctx).Model(&store.AuditLog{}).
		Where("target_type = 'finding' AND target_id = ?", findingID).Count(&n).Error
	if err != nil {
		f.t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func TestQAStatusChangedAtMovesOnlyWhenStatusChanges(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	past := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	f.exec("UPDATE pull_requests SET qa_status_changed_at = ? WHERE id = ?", past, pr.ID)

	f.exec("UPDATE pull_requests SET title = 'renamed', qa_status = qa_status WHERE id = ?", pr.ID)
	if got := f.pullRequest(t, pr.ID).QAStatusChangedAt; !got.Equal(past) {
		t.Fatalf("after an update that keeps qa_status, changed_at = %v, want %v", got, past)
	}

	if err := f.s.SetPRStatus(f.ctx, pr.ID, store.StatusReadyForHuman); err != nil {
		t.Fatalf("SetPRStatus: %v", err)
	}
	if got := f.pullRequest(t, pr.ID).QAStatusChangedAt; !got.After(past) {
		t.Fatalf("after a status change, changed_at = %v, want later than %v", got, past)
	}
}

func TestListVisiblePullRequestsReportsOpenFindingsAndStatusAge(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusChangesRequested, false)

	f.finding(pr, "b1", "blocker", "security", store.FindingOpen)
	f.finding(pr, "m1", "major", "tests", store.FindingOpen)
	f.finding(pr, "m2", "major", "tests", store.FindingOpen)
	f.finding(pr, "n1", "minor", "style", store.FindingWithdrawn)
	f.finding(pr, "x1", "nit", "style", store.FindingResolved)

	since := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Second)
	f.exec("UPDATE pull_requests SET qa_status_changed_at = ? WHERE id = ?", since, pr.ID)

	admin := f.user("admin", true)
	views, err := f.s.ListVisiblePullRequests(f.ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	want := store.SeverityCounts{Blocker: 1, Major: 2}
	if views[0].OpenFindings != want {
		t.Errorf("OpenFindings = %+v, want %+v", views[0].OpenFindings, want)
	}
	if !views[0].StatusSince.Equal(since) {
		t.Errorf("StatusSince = %v, want %v", views[0].StatusSince, since)
	}
}

func TestTriageFindingRecordsWhoDecided(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusChangesRequested, false)
	f.finding(pr, "b1", "blocker", "security", store.FindingOpen)

	confirm := store.FindingAudit{Action: "finding.confirmed", Actor: "mvega"}
	changed, err := f.s.TriageFinding(f.ctx, "b1", store.FindingOpen, confirm)
	if err != nil {
		t.Fatalf("TriageFinding(confirm): %v", err)
	}
	if changed {
		t.Error("confirming an open finding reported a status change")
	}
	got, err := f.s.Finding(f.ctx, "b1")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if got.TriagedBy != "mvega" || got.TriagedAt == nil || got.Status != store.FindingOpen ||
		got.TriageVerdict != store.VerdictConfirmed {
		t.Fatalf("after confirm: status %q, verdict %q, triaged by %q at %v",
			got.Status, got.TriageVerdict, got.TriagedBy, got.TriagedAt)
	}

	if _, err := f.s.TriageFinding(f.ctx, "b1", store.FindingOpen, confirm); err != nil {
		t.Fatalf("TriageFinding(repeat): %v", err)
	}
	if n := f.auditRows("b1"); n != 1 {
		t.Errorf("repeating the same decision left %d audit rows, want 1", n)
	}

	// A new run observing the same fingerprint must not erase the decision.
	f.finding(pr, "b1", "blocker", "security", store.FindingOpen)
	if again, _ := f.s.Finding(f.ctx, "b1"); again.TriagedBy != "mvega" {
		t.Errorf("re-observed finding lost its triage, triaged_by = %q", again.TriagedBy)
	}

	withdraw := store.FindingAudit{Action: "finding.withdrawn", Actor: "mvega", Reason: "convención"}
	changed, err = f.s.TriageFinding(f.ctx, "b1", store.FindingWithdrawn, withdraw)
	if err != nil {
		t.Fatalf("TriageFinding(withdraw): %v", err)
	}
	if !changed {
		t.Error("withdrawing an open finding did not report a status change")
	}
	if n := f.auditRows("b1"); n != 2 {
		t.Errorf("audit rows after withdraw = %d, want 2", n)
	}

	// A run reopening the false positive changes its status, not QA's verdict.
	f.finding(pr, "b1", "blocker", "security", store.FindingOpen)
	reopened, err := f.s.Finding(f.ctx, "b1")
	if err != nil {
		t.Fatalf("Finding(reopened): %v", err)
	}
	if reopened.Status != store.FindingOpen || reopened.TriageVerdict != store.VerdictFalsePositive {
		t.Errorf("reopened finding: status %q verdict %q, want open with the false-positive verdict kept",
			reopened.Status, reopened.TriageVerdict)
	}

	_, err = f.s.TriageFinding(f.ctx, "missing", store.FindingOpen, confirm)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TriageFinding(missing) = %v, want ErrNotFound", err)
	}
}

func TestTriageFindingRejectsAResolvedFinding(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusReadyForHuman, false)
	f.finding(pr, "fixed", "blocker", "security", store.FindingResolved)

	confirm := store.FindingAudit{Action: "finding.confirmed", Actor: "mvega"}
	_, err := f.s.TriageFinding(f.ctx, "fixed", store.FindingOpen, confirm)
	if !errors.Is(err, store.ErrFindingResolved) {
		t.Fatalf("TriageFinding(resolved) = %v, want ErrFindingResolved", err)
	}
	got, err := f.s.Finding(f.ctx, "fixed")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if got.Status != store.FindingResolved || got.TriageVerdict != "" {
		t.Errorf("resolved finding changed to status %q verdict %q", got.Status, got.TriageVerdict)
	}
	if n := f.auditRows("fixed"); n != 0 {
		t.Errorf("rejected triage left %d audit rows, want 0", n)
	}
}

func TestQASummary(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)

	payments := f.team("Payments")
	search := f.team("Search")
	qa := f.user("mvega", false)
	f.member(payments, qa, "qa")

	repo := f.repo(installID, installID, "acme/payments", &payments.ID, true)
	hidden := f.repo(f.nextID(), installID, "acme/search", &search.ID, true)

	ready := f.openPR(repo.ID, 1, store.StatusReadyForHuman, false)
	f.openPR(repo.ID, 2, store.StatusReadyForHuman, false)
	f.openPR(repo.ID, 3, store.StatusInconclusive, false)
	reviewing := f.openPR(repo.ID, 4, store.StatusReviewing, false)
	merged := f.openPR(repo.ID, 5, store.StatusMerged, false)
	f.exec("UPDATE pull_requests SET state = 'merged' WHERE id = ?", merged.ID)
	other := f.openPR(hidden.ID, 6, store.StatusReadyForHuman, false)

	f.run(reviewing, store.RunRunning)
	f.run(ready, store.RunQueued)
	f.run(other, store.RunRunning)

	f.finding(ready, "u-blocker", "blocker", "security", store.FindingOpen)
	f.finding(ready, "u-major", "major", "tests", store.FindingOpen)
	f.finding(other, "u-hidden", "blocker", "security", store.FindingOpen)

	now := time.Now().UTC()
	triage := func(pr store.PullRequest, id, category, status, verdict string, at time.Time) {
		f.finding(pr, id, "major", category, status)
		f.exec("UPDATE findings SET triage_verdict = ?, triaged_by = 'mvega', triaged_at = ? WHERE id = ?",
			verdict, at, id)
	}
	confirmed, falsePositive := store.VerdictConfirmed, store.VerdictFalsePositive
	for i := 0; i < 3; i++ {
		triage(ready, fmt.Sprintf("c-tests-%d", i), "tests", store.FindingOpen, confirmed, now.Add(-time.Hour))
	}
	triage(merged, "c-merged", "security", store.FindingResolved, confirmed, now.Add(-2*time.Hour))
	triage(ready, "fp-style", "style", store.FindingWithdrawn, falsePositive, now.Add(-time.Hour))
	// Status moved after triage: a run reopened a false positive and a developer
	// dismissed a confirmed finding. The verdict is what counts.
	triage(ready, "fp-reopened", "style", store.FindingOpen, falsePositive, now.Add(-time.Hour))
	triage(ready, "c-disputed", "tests", store.FindingWithdrawn, confirmed, now.Add(-time.Hour))
	triage(ready, "c-old", "security", store.FindingOpen, confirmed, now.Add(-10*24*time.Hour))
	triage(ready, "c-ancient", "docs", store.FindingOpen, confirmed, now.Add(-40*24*time.Hour))
	triage(other, "c-hidden", "tests", store.FindingOpen, confirmed, now.Add(-time.Hour))

	got, err := f.s.QASummary(f.ctx, qa, now, store.QAFilter{})
	if err != nil {
		t.Fatalf("QASummary: %v", err)
	}

	if got.ReadyForHuman != 2 || got.Inconclusive != 1 {
		t.Errorf("queue = %d ready / %d inconclusive, want 2 / 1", got.ReadyForHuman, got.Inconclusive)
	}
	if got.RunsRunning != 1 || got.RunsQueued != 1 {
		t.Errorf("runs = %d running / %d queued, want 1 / 1", got.RunsRunning, got.RunsQueued)
	}
	if want := (store.SeverityCounts{Blocker: 1, Major: 1}); got.Untriaged != want {
		t.Errorf("Untriaged = %+v, want %+v", got.Untriaged, want)
	}
	if got.Confirmed != 5 || got.FalsePositive != 2 {
		t.Errorf("precision = %d confirmed / %d false positive, want 5 / 2", got.Confirmed, got.FalsePositive)
	}
	wantCategories := []store.CategoryCount{{Category: "tests", Count: 4}, {Category: "security", Count: 2}}
	if fmt.Sprint(got.Categories) != fmt.Sprint(wantCategories) {
		t.Errorf("Categories = %v, want %v", got.Categories, wantCategories)
	}
}

func TestQASummaryForOneTeam(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)

	payments := f.team("Payments")
	web := f.team("Web")
	lead := f.user("lmartin", false)
	f.member(payments, lead, "lead")
	f.member(web, lead, "lead")

	api := f.repo(installID, installID, "acme/payments-api", &payments.ID, true)
	sdk := f.repo(f.nextID(), installID, "acme/payments-sdk", &payments.ID, true)
	portal := f.repo(f.nextID(), installID, "acme/web-portal", &web.ID, true)

	apiPR := f.openPR(api.ID, 1, store.StatusReadyForHuman, false)
	sdkPR := f.openPR(sdk.ID, 2, store.StatusChangesRequested, false)
	portalPR := f.openPR(portal.ID, 3, store.StatusReadyForHuman, false)

	now := time.Now().UTC()
	confirm := func(pr store.PullRequest, id string) {
		f.finding(pr, id, "major", "tests", store.FindingOpen)
		f.exec("UPDATE findings SET triage_verdict = ?, triaged_by = 'mvega', triaged_at = ? WHERE id = ?",
			store.VerdictConfirmed, now.Add(-time.Hour), id)
	}
	confirm(apiPR, "api-1")
	confirm(apiPR, "api-2")
	confirm(sdkPR, "sdk-1")
	confirm(portalPR, "portal-1")

	got, err := f.s.QASummary(f.ctx, lead, now, store.QAFilter{TeamID: &payments.ID})
	if err != nil {
		t.Fatalf("QASummary(Payments): %v", err)
	}
	if got.ReadyForHuman != 1 || got.Confirmed != 3 {
		t.Errorf("Payments: %d ready / %d confirmed, want 1 / 3", got.ReadyForHuman, got.Confirmed)
	}
	wantRepos := []store.RepoCount{{Repo: "acme/payments-api", Count: 2}, {Repo: "acme/payments-sdk", Count: 1}}
	if fmt.Sprint(got.ConfirmedByRepo) != fmt.Sprint(wantRepos) {
		t.Errorf("ConfirmedByRepo = %v, want %v", got.ConfirmedByRepo, wantRepos)
	}

	all, err := f.s.QASummary(f.ctx, lead, now, store.QAFilter{})
	if err != nil {
		t.Fatalf("QASummary(all): %v", err)
	}
	if all.ReadyForHuman != 2 || all.Confirmed != 4 {
		t.Errorf("all teams: %d ready / %d confirmed, want 2 / 4", all.ReadyForHuman, all.Confirmed)
	}

	// A team the user is not in stays out of reach even when asked for by id.
	outsider := f.team("Search")
	hidden, err := f.s.QASummary(f.ctx, lead, now, store.QAFilter{TeamID: &outsider.ID})
	if err != nil {
		t.Fatalf("QASummary(outsider): %v", err)
	}
	if hidden.ReadyForHuman != 0 || hidden.Confirmed != 0 || len(hidden.ConfirmedByRepo) != 0 {
		t.Errorf("foreign team leaked: %+v", hidden)
	}
}

func TestListVisiblePullRequestsReportsTheTeam(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	team := f.team("Payments")
	assigned := f.repo(installID, installID, "acme/app", &team.ID, true)
	orphan := f.repo(f.nextID(), installID, "acme/orphan", nil, true)
	f.openPR(assigned.ID, 1, store.StatusPending, false)
	f.openPR(orphan.ID, 2, store.StatusPending, false)

	views, err := f.s.ListVisiblePullRequests(f.ctx, f.user("admin", true), store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests: %v", err)
	}
	byRepo := map[string]*uint{}
	for _, v := range views {
		byRepo[v.Repo] = v.TeamID
	}
	if got := byRepo["acme/app"]; got == nil || *got != team.ID {
		t.Errorf("acme/app team = %v, want %d", got, team.ID)
	}
	if got := byRepo["acme/orphan"]; got != nil {
		t.Errorf("acme/orphan team = %d, want none", *got)
	}
}
