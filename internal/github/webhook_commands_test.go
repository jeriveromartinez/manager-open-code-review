package github_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestWebhookQARetryAuthorized(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-retry.json"), http.StatusAccepted)

	if got := f.reviews.retrySnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("retries = %v, want exactly [%d]", got, pr.ID)
	}
	repos, nums, bodies := f.comments.snapshot()
	if len(nums) != 1 || nums[0] != 142 {
		t.Fatalf("replies = %+v, want one on #142", nums)
	}
	if len(repos) != 1 || repos[0] != "acme/backend-api" {
		t.Errorf("reply repositories = %v, want acme/backend-api", repos)
	}
	if !strings.Contains(strings.ToLower(bodies[0]), "reintento") {
		t.Errorf("reply = %q, want a confirmation about the retry", bodies[0])
	}
}

func TestWebhookQARetryUnauthorized(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.seedOpenPR()
	f.seedDeveloper()

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-retry-unauthorized.json"), http.StatusAccepted)

	if got := f.reviews.retrySnapshot(); len(got) != 0 {
		t.Fatalf("retries = %v, want none", got)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want no side effects", got)
	}
}

func TestWebhookQADismiss(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingWithdrawn {
		t.Fatalf("finding status = %q, want %q", got.Status, store.FindingWithdrawn)
	}
	if got.TriageVerdict != store.VerdictFalsePositive ||
		got.TriageSource != store.TriageSourceHuman || got.TriagedBy != "qa-lead" {
		t.Errorf("finding triage = %q/%q by %q, want a human false positive",
			got.TriageVerdict, got.TriageSource, got.TriagedBy)
	}
	rows := f.auditRows()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(rows))
	}
	if rows[0].Action != "finding.withdrawn" || rows[0].TargetID != "f1" {
		t.Errorf("audit row = %+v, want finding.withdrawn for f1", rows[0])
	}
	if !bytes.Contains(rows[0].Data, []byte(`"qa-lead"`)) {
		t.Errorf("audit data = %s, want the actor login", rows[0].Data)
	}
	if rows[0].ActorUserID == nil {
		t.Error("audit actor_user_id = nil, want the local user")
	}
	if got := f.findings.republishSnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("gate republishes = %+v, want one", got)
	}
	_, _, bodies := f.comments.snapshot()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "f1") {
		t.Errorf("reply = %q, want a confirmation naming the finding", bodies)
	}
}

func TestWebhookQAConfirm(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingWithdrawn)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-confirm.json"), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingOpen {
		t.Fatalf("finding status = %q, want %q", got.Status, store.FindingOpen)
	}
	if got.TriageVerdict != store.VerdictConfirmed ||
		got.TriageSource != store.TriageSourceHuman || got.TriagedBy != "qa-lead" {
		t.Errorf("finding triage = %q/%q by %q, want a human confirmation",
			got.TriageVerdict, got.TriageSource, got.TriagedBy)
	}
	rows := f.auditRows()
	if len(rows) != 1 || rows[0].Action != "finding.confirmed" {
		t.Fatalf("audit rows = %+v, want one finding.confirmed", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("gate republishes = %+v, want one", got)
	}
}

func TestWebhookQADismissOfAResolvedFindingDoesNothing(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingResolved)

	body := []byte(`{"action":"created","issue":{"number":142,"pull_request":{"url":"https://api.github.com/repos/acme/backend-api/pulls/142"}},"comment":{"id":9400,"body":"/qa dismiss f1","user":{"id":8001,"login":"qa-lead","type":"User"}},"repository":{"id":501,"name":"backend-api","full_name":"acme/backend-api"},"sender":{"id":8001,"login":"qa-lead","type":"User"}}`)
	assertStatus(t, f.deliverBody("issue_comment", body, signWebhook(body)), http.StatusAccepted)

	if got := f.finding("f1").Status; got != store.FindingResolved {
		t.Errorf("finding status = %q, want it untouched", got)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want none", got)
	}
	if _, _, bodies := f.comments.snapshot(); len(bodies) != 1 ||
		!strings.Contains(bodies[0], "No hay hallazgos") {
		t.Errorf("reply = %q, want it to report that nothing matched", bodies)
	}
}

func TestWebhookQAUnknownCommand(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-unknown.json"), http.StatusAccepted)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want it untouched", got)
	}
	if got := f.reviews.retrySnapshot(); len(got) != 0 {
		t.Errorf("retries = %v, want none", got)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want none", got)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none", got)
	}
	if _, _, bodies := f.comments.snapshot(); len(bodies) != 1 {
		t.Errorf("replies = %q, want one explaining the unknown command", bodies)
	}
}

func TestWebhookQANonPullRequestIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	body := []byte(`{"action":"created","issue":{"number":900},"comment":{"id":9100,"body":"/qa dismiss","user":{"id":8001,"login":"qa-lead"}},"repository":{"id":501}}`)
	assertStatus(t, f.deliverBody("issue_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want it untouched for a plain issue", got)
	}
}

func TestWebhookQADismissRedeliveryIsNoOp(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusAccepted)
	if got := f.auditRows(); len(got) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(got))
	}

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusNoContent)
	if got := f.auditRows(); len(got) != 1 {
		t.Errorf("audit rows after redelivery = %d, want 1", len(got))
	}
	if got := f.findings.republishSnapshot(); len(got) != 1 {
		t.Errorf("gate republishes after redelivery = %d, want 1", len(got))
	}
}

func TestWebhookQADismissReportsUnsupportedThread(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)
	// A nil publishing client is the "not wired" case the reply must mention.
	f.handler.Findings = nil

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusAccepted)

	_, _, bodies := f.comments.snapshot()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "hilo") {
		t.Errorf("reply = %q, want it to report the untouched GitHub thread", bodies)
	}
}

func TestWebhookQAConfirmRestoresNamedFinding(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingWithdrawn)
	f.seedThreadFinding(pr.ID, "f2", store.FindingWithdrawn)

	body := []byte(`{"action":"created","issue":{"number":142,"pull_request":{"url":"https://api.github.com/repos/acme/backend-api/pulls/142"}},"comment":{"id":9200,"body":"/qa confirm f2","user":{"id":8001,"login":"qa-lead"}},"repository":{"id":501,"full_name":"acme/backend-api"},"sender":{"id":8001,"login":"qa-lead"}}`)
	assertStatus(t, f.deliverBody("issue_comment", body, signWebhook(body)), http.StatusAccepted)

	if got := f.finding("f2").Status; got != store.FindingOpen {
		t.Errorf("named finding status = %q, want %q", got, store.FindingOpen)
	}
	if got := f.finding("f1").Status; got != store.FindingWithdrawn {
		t.Errorf("other finding status = %q, want it left withdrawn", got)
	}
}

func TestWebhookQACommandMustStartTheComment(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	body := []byte(`{"action":"created","issue":{"number":142,"pull_request":{"url":"https://api.github.com/repos/acme/backend-api/pulls/142"}},"comment":{"id":9300,"body":"Gracias!\n/qa dismiss"}, "repository":{"id":501,"full_name":"acme/backend-api"},"sender":{"id":8001,"login":"qa-lead"}}`)
	assertStatus(t, f.deliverBody("issue_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want it untouched when /qa is not the first line", got)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want none", got)
	}
}

func TestWebhookQAAdminCanAct(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	if err := f.st.DB.WithContext(f.ctx).Create(&store.User{GitHubID: 8001, Login: "qa-lead", IsAdmin: true}).Error; err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusAccepted)
	if got := f.finding("f1").Status; got != store.FindingWithdrawn {
		t.Errorf("finding status = %q, want %q for an admin", got, store.FindingWithdrawn)
	}
}

func TestWebhookQAUnknownUserIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("issue_comment", "issue-comment-qa-dismiss.json"), http.StatusAccepted)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want no action for an account with no local user", got)
	}
	if _, _, bodies := f.comments.snapshot(); len(bodies) != 1 {
		t.Errorf("replies = %q, want one polite refusal", bodies)
	}
}

func TestWebhookQAPrefixMustEndTheWord(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	body := []byte(`{"action":"created","issue":{"number":142,"pull_request":{"url":"https://api.github.com/repos/acme/backend-api/pulls/142"}},"comment":{"id":9400,"body":"/qax dismiss"},"repository":{"id":501,"full_name":"acme/backend-api"},"sender":{"id":8001,"login":"qa-lead"}}`)
	assertStatus(t, f.deliverBody("issue_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want it untouched for /qax", got)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want none", got)
	}
}
