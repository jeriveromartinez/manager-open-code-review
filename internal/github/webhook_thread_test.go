package github_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestWebhookReviewThreadResolved(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)
	pr := f.seedOpenPR()
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusAccepted)

	if got := f.finding("f1").Status; got != store.FindingResolved {
		t.Fatalf("finding status = %q, want %q", got, store.FindingResolved)
	}
	rows := f.auditRows()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].Action != "finding.resolved" || rows[0].TargetID != "f1" {
		t.Errorf("audit row = %+v, want finding.resolved for f1", rows[0])
	}
	if !bytes.Contains(rows[0].Data, []byte(`"alice"`)) {
		t.Errorf("audit data = %s, want the sender login", rows[0].Data)
	}
	syncs := f.findings.republishSnapshot()
	if len(syncs) != 1 || syncs[0] != pr.ID {
		t.Fatalf("gate republishes = %+v, want one for pull request %d", syncs, pr.ID)
	}

	// A redelivery of the same thread event must not audit or republish again.
	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusNoContent)
	if got := len(f.auditRows()); got != 1 {
		t.Fatalf("audit rows after redelivery = %d, want 1", got)
	}
	if got := len(f.findings.republishSnapshot()); got != 1 {
		t.Fatalf("gate republishes after redelivery = %d, want 1", got)
	}
}

func TestWebhookReviewThreadUnresolved(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)
	pr := f.seedOpenPR()
	f.seedThreadFinding(pr.ID, "f1", store.FindingResolved)

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-unresolved.json"), http.StatusAccepted)

	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Fatalf("finding status = %q, want %q", got, store.FindingOpen)
	}
	rows := f.auditRows()
	if len(rows) != 1 || rows[0].Action != "finding.reopened" {
		t.Fatalf("audit rows = %+v, want one finding.reopened", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("gate republishes = %+v, want one", got)
	}
}

func TestWebhookReviewThreadUnknownThreadIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(true)
	pr := f.seedOpenPR()
	unrelated, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "other", PullRequestID: pr.ID, Fingerprint: "fp-other",
		Title: "Otro hallazgo", Status: store.FindingOpen,
		GithubCommentNodeID: "PRRC_other",
	})
	if err != nil {
		t.Fatalf("seed unrelated finding: %v", err)
	}

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusNoContent)

	if got := f.finding(unrelated.ID).Status; got != store.FindingOpen {
		t.Errorf("unrelated finding status = %q, want %q", got, store.FindingOpen)
	}
	if got := f.auditRows(); len(got) != 0 {
		t.Errorf("audit rows = %+v, want none", got)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none", got)
	}
}

func TestWebhookReviewThreadDisabledRepoIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepo(false)
	pr := f.seedOpenPR()
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusNoContent)
	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want %q on a disabled repository", got, store.FindingOpen)
	}
}

