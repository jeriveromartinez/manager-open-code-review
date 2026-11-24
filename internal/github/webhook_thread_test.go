package github_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestWebhookReviewThreadResolved(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingResolved {
		t.Fatalf("finding status = %q, want %q", got.Status, store.FindingResolved)
	}
	if !got.GithubThreadResolved {
		t.Error("thread state = unresolved, want resolved")
	}
	rows := f.auditRows()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].Action != "finding.resolved" || rows[0].TargetID != "f1" {
		t.Errorf("audit row = %+v, want finding.resolved for f1", rows[0])
	}
	if !bytes.Contains(rows[0].Data, []byte(`"qa-lead"`)) {
		t.Errorf("audit data = %s, want the sender login", rows[0].Data)
	}
	if !bytes.Contains(rows[0].Data, []byte(`"reason"`)) {
		t.Errorf("audit data = %s, want a reason", rows[0].Data)
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
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingResolved)

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-unresolved.json"), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingOpen {
		t.Fatalf("finding status = %q, want %q", got.Status, store.FindingOpen)
	}
	if got.GithubThreadResolved {
		t.Error("thread state = resolved, want reopened")
	}
	rows := f.auditRows()
	if len(rows) != 1 || rows[0].Action != "finding.reopened" {
		t.Fatalf("audit rows = %+v, want one finding.reopened", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("gate republishes = %+v, want one", got)
	}
}

// An authorized resolve records the thread state but never rewrites the QA
// verdict or the gate a person already decided.
func TestWebhookReviewThreadAuthorizedResolvePreservesHumanTriage(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	lead := f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)
	if _, err := f.st.TriageFinding(f.ctx, "f1", store.FindingOpen, store.FindingAudit{
		Action: "finding.confirmed", Actor: lead.Login, ActorID: &lead.ID,
		Source: store.TriageSourceHuman, Reason: "revisado",
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}

	assertStatus(t, f.deliver("pull_request_review_thread", "review-thread-resolved.json"), http.StatusAccepted)

	got := f.finding("f1")
	if !got.GithubThreadResolved {
		t.Error("thread state = unresolved, want the GitHub state recorded")
	}
	if got.Status != store.FindingOpen || got.TriageVerdict != store.VerdictConfirmed ||
		got.TriagedBy != lead.Login || got.TriageSource != store.TriageSourceHuman {
		t.Errorf("finding after resolve = %+v, want the human confirmation preserved", got)
	}
	if rows := f.auditRows(); len(rows) != 1 {
		t.Errorf("audit rows = %+v, want only the human decision", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none without a status change", got)
	}
}

// A sender without triage permission can only record the thread state: the
// finding keeps blocking the pull request.
func TestWebhookReviewThreadUnauthorizedResolveDoesNotChangeGate(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedDeveloper()
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-thread-resolved.json"), []byte("8001"), []byte("8002"))
	body = bytes.ReplaceAll(body, []byte("qa-lead"), []byte("dev"))
	assertStatus(t, f.deliverBody("pull_request_review_thread", body, signWebhook(body)), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingOpen || got.TriageVerdict != "" {
		t.Errorf("finding = %+v, want the open finding untouched", got)
	}
	if !got.GithubThreadResolved {
		t.Error("thread state = unresolved, want the GitHub state recorded")
	}
	if rows := f.auditRows(); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for an unauthorized decision", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none", got)
	}
}

// The App's own resolve/unresolve mutation comes back as a delivery. It must
// reconcile the thread state (the local write may have failed) but never act as
// a QA decision.
func TestWebhookReviewThreadAppEchoRecordsStateOnly(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-thread-resolved.json"), []byte(`"type": "User"`), []byte(`"type": "Bot"`))
	assertStatus(t, f.deliverBody("pull_request_review_thread", body, signWebhook(body)), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingOpen || !got.GithubThreadResolved {
		t.Errorf("finding = %+v, want only the thread state recorded", got)
	}
	if rows := f.auditRows(); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none", got)
	}
}

// A stale resolve delivered for the first time after a newer reopen must not
// flip the state back. Delivery ids differ, so only the payload timestamp can
// order the events.
func TestWebhookReviewThreadStaleDeliveryIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	resolved := f.fixture("review-thread-resolved.json")
	unresolved := f.fixture("review-thread-unresolved.json")

	assertStatus(t, f.deliverBodyWithDelivery(
		"pull_request_review_thread", resolved, signWebhook(resolved), "delivery-1"), http.StatusAccepted)
	assertStatus(t, f.deliverBodyWithDelivery(
		"pull_request_review_thread", unresolved, signWebhook(unresolved), "delivery-2"), http.StatusAccepted)

	// A first-time delayed delivery of the older resolve: its timestamp is
	// older than the reopen already stored.
	assertStatus(t, f.deliverBodyWithDelivery(
		"pull_request_review_thread", resolved, signWebhook(resolved), "delivery-3"), http.StatusNoContent)

	got := f.finding("f1")
	if got.Status != store.FindingOpen || got.GithubThreadResolved {
		t.Errorf("finding = %+v, want the newer reopen to win", got)
	}
	if rows := f.auditRows(); len(rows) != 2 {
		t.Errorf("audit rows = %d, want the resolve and the reopen only", len(rows))
	}
	if got := f.findings.republishSnapshot(); len(got) != 2 {
		t.Errorf("gate republishes = %+v, want one per applied transition", got)
	}
}

// Without updated_at there is no ordering token: the event must still apply
// instead of being dropped as stale.
func TestWebhookReviewThreadNewerEventWithoutTimestampApplies(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedThreadFinding(pr.ID, "f1", store.FindingOpen)

	resolved := f.fixture("review-thread-resolved.json")
	unresolved := f.fixture("review-thread-unresolved.json")
	unresolved = bytes.Replace(unresolved, []byte(`"updated_at": "2026-01-01T00:05:00Z"`),
		[]byte(`"ignored": "no-timestamp"`), 1)

	assertStatus(t, f.deliverBodyWithDelivery(
		"pull_request_review_thread", resolved, signWebhook(resolved), "delivery-1"), http.StatusAccepted)
	assertStatus(t, f.deliverBodyWithDelivery(
		"pull_request_review_thread", unresolved, signWebhook(unresolved), "delivery-2"), http.StatusAccepted)

	got := f.finding("f1")
	if got.Status != store.FindingOpen || got.GithubThreadResolved {
		t.Errorf("finding = %+v, want the untimestamped reopen applied", got)
	}
}

// A human withdrawal is final against thread events in both directions: the
// finding keeps its withdrawn status and the thread state still follows GitHub.
func TestWebhookReviewThreadKeepsHumanWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		resolved bool
	}{
		{name: "authorized resolve", fixture: "review-thread-resolved.json", resolved: true},
		{name: "authorized unresolved", fixture: "review-thread-unresolved.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWebhookFixture(t)
			f.seedRepoWithTeam()
			pr := f.seedOpenPR()
			lead := f.seedQALead(f.repoTeam())
			f.seedThreadFinding(pr.ID, "f1", store.FindingWithdrawn)
			if _, err := f.st.TriageFinding(f.ctx, "f1", store.FindingWithdrawn, store.FindingAudit{
				Action: "finding.withdrawn", Actor: lead.Login, ActorID: &lead.ID,
				Source: store.TriageSourceHuman, Reason: "falso positivo",
			}); err != nil {
				t.Fatalf("TriageFinding: %v", err)
			}

			assertStatus(t, f.deliver("pull_request_review_thread", tc.fixture), http.StatusAccepted)

			got := f.finding("f1")
			if got.Status != store.FindingWithdrawn {
				t.Errorf("status = %q, want the human %q", got.Status, store.FindingWithdrawn)
			}
			if got.GithubThreadResolved != tc.resolved {
				t.Errorf("thread state = %v, want %v", got.GithubThreadResolved, tc.resolved)
			}
			if rows := f.auditRows(); len(rows) != 1 {
				t.Errorf("audit rows = %d, want only the human decision", len(rows))
			}
			if got := f.findings.republishSnapshot(); len(got) != 0 {
				t.Errorf("gate republishes = %+v, want none", got)
			}
		})
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
