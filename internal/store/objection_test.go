package store_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *fixture) seedObjectionFinding(pr store.PullRequest, id, severity, status string, commentID int64) store.Finding {
	f.t.Helper()

	finding := f.finding(pr, id, severity, "bug", status)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.Finding{}).Where("id = ?", id).
		Updates(map[string]any{"github_comment_id": commentID, "github_comment_node_id": "PRRC_" + id}).Error; err != nil {
		f.t.Fatalf("link finding to review comment: %v", err)
	}
	finding.GithubCommentID = commentID
	return finding
}

func (f *fixture) objection(pr store.PullRequest, findingID string, commentID int64) store.FindingObjection {
	f.t.Helper()

	intent, _, err := f.s.EnqueueFindingObjection(f.ctx, store.FindingObjection{
		FindingID: findingID, PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		CommentID: commentID, DeliveryID: "delivery-" + findingID, Actor: "octocat",
		Authorized: true, Body: "la función ya valida nil", FilePath: "main.go",
	})
	if err != nil {
		f.t.Fatalf("EnqueueFindingObjection: %v", err)
	}
	return intent
}

func (f *fixture) countObjections() int64 {
	f.t.Helper()

	var n int64
	if err := f.s.DB.WithContext(f.ctx).Model(&store.FindingObjection{}).Count(&n).Error; err != nil {
		f.t.Fatalf("count objections: %v", err)
	}
	return n
}

func TestEnqueueFindingObjectionIsIdempotent(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 31, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-1", "minor", store.FindingOpen, 5001)

	first := f.objection(pr, "obj-1", 5001)
	if first.State != store.ObjectionQueued || first.ID == 0 {
		t.Fatalf("intent = %+v, want a queued row", first)
	}
	second, created, err := f.s.EnqueueFindingObjection(f.ctx, store.FindingObjection{
		FindingID: "obj-1", PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		CommentID: 5001, DeliveryID: "delivery-retry", Actor: "octocat", Authorized: true,
		ReadTokenEnc: "fresh-token",
	})
	if err != nil {
		t.Fatalf("second EnqueueFindingObjection: %v", err)
	}
	if created || second.ID != first.ID {
		t.Errorf("created/id = %v/%d, want the existing intent %d", created, second.ID, first.ID)
	}
	if second.ReadTokenEnc != "fresh-token" {
		t.Errorf("read token = %q, want the redelivery token refreshed", second.ReadTokenEnc)
	}
	if got := f.countObjections(); got != 1 {
		t.Errorf("objection rows = %d, want 1", got)
	}
}

func TestEnqueueFindingObjectionRejectsFinalFindings(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 32, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-human", "minor", store.FindingOpen, 5002)
	if _, err := f.s.TriageFinding(f.ctx, "obj-human", store.FindingOpen, store.FindingAudit{
		Action: "finding.confirmed", Actor: "lead", Source: store.TriageSourceHuman,
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}
	if _, _, err := f.s.EnqueueFindingObjection(f.ctx, store.FindingObjection{
		FindingID: "obj-human", PullRequestID: pr.ID, CommentID: 9002,
	}); !errors.Is(err, store.ErrFindingHumanTriaged) {
		t.Errorf("EnqueueFindingObjection(human) = %v, want ErrFindingHumanTriaged", err)
	}

	f.seedObjectionFinding(pr, "obj-resolved", "minor", store.FindingResolved, 5003)
	if _, _, err := f.s.EnqueueFindingObjection(f.ctx, store.FindingObjection{
		FindingID: "obj-resolved", PullRequestID: pr.ID, CommentID: 9003,
	}); !errors.Is(err, store.ErrFindingResolved) {
		t.Errorf("EnqueueFindingObjection(resolved) = %v, want ErrFindingResolved", err)
	}
}

func TestRecordFindingObjectionEvaluationWithdrawsAtomically(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 33, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-2", "minor", store.FindingOpen, 5004)
	intent := f.objection(pr, "obj-2", 5004)

	verification := []byte(`{"verdict":"refute","evidence":"func guard() {","head_sha":"sha-33"}`)
	evaluated, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionWithdraw, Status: store.FindingWithdrawn, Apply: true,
		Reason: "la evidencia refuta el hallazgo", Evidence: "func guard() {", Verification: verification,
	})
	if err != nil {
		t.Fatalf("RecordFindingObjectionEvaluation: %v", err)
	}
	if evaluated.State != store.ObjectionEvaluated || evaluated.Action != store.ObjectionWithdraw {
		t.Errorf("intent = %+v, want an evaluated withdrawal", evaluated)
	}

	got, err := f.s.Finding(f.ctx, "obj-2")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if got.Status != store.FindingWithdrawn || got.TriageSource != store.TriageSourceVerifier ||
		got.TriageVerdict != store.VerdictFalsePositive || got.GithubThreadDecisionAt == nil {
		t.Errorf("finding = %+v, want the verifier withdrawal", got)
	}
	if !bytes.Contains(got.Verification, []byte(`"evidence"`)) || !bytes.Contains(got.Verification, []byte("func guard() {")) {
		t.Errorf("verification = %s, want the audited evidence", got.Verification)
	}
	var audit store.AuditLog
	if err := f.s.DB.WithContext(f.ctx).Where("target_type = 'finding' AND target_id = 'obj-2'").Take(&audit).Error; err != nil {
		t.Fatalf("load audit row: %v", err)
	}
	if audit.Action != "finding.objection.withdraw" {
		t.Errorf("audit action = %q", audit.Action)
	}
	for _, want := range []string{`"evidence"`, `func guard() {`, `"head_sha"`, `sha-33`, `"objection"`} {
		if !bytes.Contains(audit.Data, []byte(want)) {
			t.Errorf("audit data = %s, want %s", audit.Data, want)
		}
	}

	// Repeating the same evaluation is a no-op and cannot duplicate the audit.
	if _, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionWithdraw, Status: store.FindingWithdrawn, Apply: true,
	}); err != nil {
		t.Fatalf("second evaluation: %v", err)
	}
	if got := f.auditRows("obj-2"); got != 1 {
		t.Errorf("audit rows = %d, want 1", got)
	}
}

func TestRecordFindingObjectionEvaluationNeverTouchesHumanTriage(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 34, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-3", "minor", store.FindingOpen, 5005)
	intent := f.objection(pr, "obj-3", 5005)
	if _, err := f.s.TriageFinding(f.ctx, "obj-3", store.FindingOpen, store.FindingAudit{
		Action: "finding.confirmed", Actor: "lead", Source: store.TriageSourceHuman,
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}

	evaluated, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionWithdraw, Status: store.FindingWithdrawn, Apply: true, Evidence: "func guard() {",
	})
	if err != nil {
		t.Fatalf("RecordFindingObjectionEvaluation: %v", err)
	}
	if evaluated.Action != store.ObjectionIgnored {
		t.Errorf("action = %q, want ignored after the human decision", evaluated.Action)
	}
	got, err := f.s.Finding(f.ctx, "obj-3")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if got.Status != store.FindingOpen || got.TriageSource != store.TriageSourceHuman {
		t.Errorf("finding = %+v, want the human decision preserved", got)
	}
}

func TestRecordFindingObjectionEvaluationUnauthorizedStateOnly(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 35, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-4", "minor", store.FindingOpen, 5006)
	intent, _, err := f.s.EnqueueFindingObjection(f.ctx, store.FindingObjection{
		FindingID: "obj-4", PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		CommentID: 5006, Actor: "outsider", Authorized: false, Body: "no es un bug",
	})
	if err != nil {
		t.Fatalf("EnqueueFindingObjection: %v", err)
	}

	if _, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionIgnored, Reason: "el autor no puede cambiar el gate",
		Verification: []byte(`{"verdict":"refute"}`),
	}); err != nil {
		t.Fatalf("RecordFindingObjectionEvaluation: %v", err)
	}
	got, err := f.s.Finding(f.ctx, "obj-4")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if got.Status != store.FindingOpen || len(got.Verification) != 0 {
		t.Errorf("finding = %+v, want the open finding untouched", got)
	}
}

func TestPendingObjectionsAndPublished(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 36, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-5", "minor", store.FindingOpen, 5007)
	intent := f.objection(pr, "obj-5", 5007)

	queued, err := f.s.PendingObjectionEvaluations(f.ctx, time.Now().Add(time.Hour), 10)
	if err != nil || len(queued) != 1 || queued[0].ID != intent.ID {
		t.Fatalf("pending evaluations = %+v (err=%v), want the intent", queued, err)
	}
	if _, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionEscalate, Reason: "requiere humano",
	}); err != nil {
		t.Fatalf("RecordFindingObjectionEvaluation: %v", err)
	}
	pending, err := f.s.PendingObjectionPublications(f.ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != intent.ID {
		t.Fatalf("pending publications = %+v (err=%v), want the intent", pending, err)
	}
	if err := f.s.MarkFindingObjectionPublished(f.ctx, intent.ID); err != nil {
		t.Fatalf("MarkFindingObjectionPublished: %v", err)
	}
	if err := f.s.MarkFindingObjectionPublished(f.ctx, intent.ID); err != nil {
		t.Fatalf("second MarkFindingObjectionPublished: %v", err)
	}
	pending, err = f.s.PendingObjectionPublications(f.ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Errorf("pending publications after publish = %+v (err=%v), want none", pending, err)
	}
}

func TestFindingForReviewComment(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 37, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-6", "minor", store.FindingOpen, 5008)

	got, err := f.s.FindingForReviewComment(f.ctx, pr.ID, 5008)
	if err != nil {
		t.Fatalf("FindingForReviewComment: %v", err)
	}
	if got.ID != "obj-6" {
		t.Errorf("finding = %q, want obj-6", got.ID)
	}
	if _, err := f.s.FindingForReviewComment(f.ctx, pr.ID, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown comment = %v, want ErrNotFound", err)
	}
}

func TestFailFindingObjectionPublicationIsTerminal(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 38, store.StatusChangesRequested, false)
	f.seedObjectionFinding(pr, "obj-7", "minor", store.FindingOpen, 5009)
	intent := f.objection(pr, "obj-7", 5009)
	if _, err := f.s.RecordFindingObjectionEvaluation(f.ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionEscalate, Reason: "requiere humano",
	}); err != nil {
		t.Fatalf("record evaluation: %v", err)
	}

	if err := f.s.FailFindingObjectionPublication(f.ctx, intent.ID, "hilo eliminado"); err != nil {
		t.Fatalf("FailFindingObjectionPublication: %v", err)
	}
	failed, err := f.s.FindingObjection(f.ctx, intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if failed.State != store.ObjectionFailed || failed.Failure != "hilo eliminado" {
		t.Errorf("intent = %+v, want the terminal failure recorded", failed)
	}
	pending, err := f.s.PendingObjectionPublications(f.ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Errorf("pending publications = %+v (err=%v), want none after the failure", pending, err)
	}
	var audit store.AuditLog
	if err := f.s.DB.WithContext(f.ctx).
		Where("target_type = 'finding' AND target_id = 'obj-7' AND action = ?", "finding.objection.publish_failed").
		Take(&audit).Error; err != nil {
		t.Fatalf("load failure audit: %v", err)
	}
	// Repeating the failure is a no-op, not a second audit row.
	if err := f.s.FailFindingObjectionPublication(f.ctx, intent.ID, "otra vez"); err != nil {
		t.Fatalf("second FailFindingObjectionPublication: %v", err)
	}
	if got := f.auditRows("obj-7"); got != 2 {
		t.Errorf("audit rows = %d, want the evaluation and one publish failure", got)
	}
}
