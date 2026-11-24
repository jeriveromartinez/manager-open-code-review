package publish_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func seedEvaluatedObjection(t *testing.T, st *store.Store, pr store.PullRequest, action string) store.FindingObjection {
	t.Helper()

	ctx := context.Background()
	finding := store.Finding{
		ID: "f-obj", PullRequestID: pr.ID, Fingerprint: "fp-obj", IdentityVersion: 2,
		Path: "src/a.go", StartLine: 1, EndLine: 1, Severity: "minor", Category: "bug",
		Status: store.FindingOpen, GithubCommentID: 5001, GithubCommentNodeID: "NC_resolve",
	}
	if err := st.DB.WithContext(ctx).Create(&finding).Error; err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	intent, _, err := st.EnqueueFindingObjection(ctx, store.FindingObjection{
		FindingID: "f-obj", PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		CommentID: 5002, Actor: "octocat", Authorized: true,
		Body: "no es un bug", FilePath: "src/a.go",
	})
	if err != nil {
		t.Fatalf("enqueue objection: %v", err)
	}
	if _, err := st.RecordFindingObjectionEvaluation(ctx, intent.ID, store.ObjectionEvaluation{
		Action: action, Reason: "requiere revisión humana",
	}); err != nil {
		t.Fatalf("record evaluation: %v", err)
	}
	return intent
}

func TestPublishObjectionRepliesExactlyOnce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedEvaluatedObjection(t, st, pr, store.ObjectionEscalate)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection: %v", err)
	}
	// A retry after the intent is published must not answer twice.
	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("retried PublishObjection: %v", err)
	}

	gh.mu.Lock()
	replies := append([]string(nil), gh.replies...)
	gh.mu.Unlock()
	if len(replies) != 1 {
		t.Fatalf("replies = %d, want exactly one", len(replies))
	}
	if !strings.Contains(replies[0], fmt.Sprintf("<!-- qa-objection:%d -->", intent.ID)) {
		t.Errorf("reply = %q, want the idempotency marker", replies[0])
	}
	if !strings.Contains(replies[0], "gate no se libera") {
		t.Errorf("reply = %q, want the escalation explained", replies[0])
	}
	published, err := st.FindingObjection(ctx, intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if published.State != store.ObjectionPublished || published.RepliedAt == nil {
		t.Errorf("intent = %+v, want it marked published", published)
	}
}

// A crash between the GitHub call and the local write leaves the intent
// evaluated. The next attempt must find the marker and not post again.
func TestPublishObjectionSkipsReplyWhenMarkerExists(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedEvaluatedObjection(t, st, pr, store.ObjectionEscalate)
	gh := newFakeGitHub(t)
	gh.threadCommentBody = fmt.Sprintf("<!-- qa-objection:%d -->\nrespuesta anterior", intent.ID)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection: %v", err)
	}
	gh.mu.Lock()
	replies := len(gh.replies)
	gh.mu.Unlock()
	if replies != 0 {
		t.Fatalf("replies = %d, want none when the marker already exists", replies)
	}
	published, err := st.FindingObjection(ctx, intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if published.State != store.ObjectionPublished {
		t.Errorf("intent state = %q, want published", published.State)
	}
}

func seedWithdrawalObjection(t *testing.T, st *store.Store, pr store.PullRequest) store.FindingObjection {
	t.Helper()

	ctx := context.Background()
	finding := store.Finding{
		ID: "f-withdraw", PullRequestID: pr.ID, Fingerprint: "fp-withdraw", IdentityVersion: 2,
		Path: "src/a.go", StartLine: 1, EndLine: 1, Severity: "minor", Category: "bug",
		Status: store.FindingOpen, GithubCommentID: 5001, GithubCommentNodeID: "NC_resolve",
	}
	if err := st.DB.WithContext(ctx).Create(&finding).Error; err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	intent, _, err := st.EnqueueFindingObjection(ctx, store.FindingObjection{
		FindingID: "f-withdraw", PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		CommentID: 5002, Actor: "octocat", Authorized: true,
		Body: "no es un bug", FilePath: "src/a.go",
	})
	if err != nil {
		t.Fatalf("enqueue objection: %v", err)
	}
	if _, err := st.RecordFindingObjectionEvaluation(ctx, intent.ID, store.ObjectionEvaluation{
		Action: store.ObjectionWithdraw, Status: store.FindingWithdrawn, Apply: true,
		Reason: "la evidencia refuta el hallazgo", Evidence: "func guard() {",
		Verification: []byte(`{"verdict":"refute","evidence":"func guard() {","head_sha":"abcdef1234567890"}`),
	}); err != nil {
		t.Fatalf("record withdrawal: %v", err)
	}
	return intent
}

func TestPublishObjectionWithdrawalRepublishesAndResolves(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedWithdrawalObjection(t, st, pr)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection: %v", err)
	}

	gh.mu.Lock()
	patches := len(gh.checkPatches)
	queries := strings.Join(gh.gqlQueries, "\n")
	replies := append([]string(nil), gh.replies...)
	gh.mu.Unlock()
	if patches == 0 {
		t.Error("check run was not republished")
	}
	if !strings.Contains(queries, "resolveReviewThread") {
		t.Error("finding thread was not resolved")
	}
	if len(replies) != 1 || !strings.Contains(replies[0], "retiré el hallazgo") {
		t.Fatalf("replies = %q, want the withdrawal explained", replies)
	}
}

// A human reopening the finding after the evaluation wins: the publisher must
// not claim it withdrew anything or touch the gate.
func TestPublishObjectionSupersededByHumanDecision(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedWithdrawalObjection(t, st, pr)
	if _, err := st.TriageFinding(ctx, "f-withdraw", store.FindingOpen, store.FindingAudit{
		Action: "finding.confirmed", Actor: "lead", Source: store.TriageSourceHuman, Reason: "revisado",
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection: %v", err)
	}

	gh.mu.Lock()
	patches := len(gh.checkPatches)
	queries := strings.Join(gh.gqlQueries, "\n")
	replies := append([]string(nil), gh.replies...)
	gh.mu.Unlock()
	if patches != 0 || strings.Contains(queries, "resolveReviewThread") {
		t.Errorf("publication touched the gate after a human decision (patches=%d queries=%q)", patches, queries)
	}
	if len(replies) != 1 || strings.Contains(replies[0], "retiré") || !strings.Contains(replies[0], "reemplazada") {
		t.Fatalf("replies = %q, want the superseded notice instead of a stale withdrawal", replies)
	}
}

func TestPublishObjectionFindsMarkerOnLaterPage(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedEvaluatedObjection(t, st, pr, store.ObjectionEscalate)
	gh := newFakeGitHub(t)
	gh.threadCommentBody = fmt.Sprintf("<!-- qa-objection:%d -->\nrespuesta anterior", intent.ID)
	gh.markerOnPage = 2
	w, _ := newPublishWorker(t, st, gh)

	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection: %v", err)
	}
	gh.mu.Lock()
	replies := len(gh.replies)
	gh.mu.Unlock()
	if replies != 0 {
		t.Fatalf("replies = %d, want none when the marker is on a later page", replies)
	}
}

func TestFailObjectionPublicationMarksTerminal(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	intent := seedEvaluatedObjection(t, st, pr, store.ObjectionEscalate)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.FailObjectionPublication(ctx, intent.ID, "thread deleted"); err != nil {
		t.Fatalf("FailObjectionPublication: %v", err)
	}
	failed, err := st.FindingObjection(ctx, intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if failed.State != store.ObjectionFailed || failed.Failure != "thread deleted" {
		t.Errorf("intent = %+v, want the terminal failure recorded", failed)
	}
	pending, err := st.PendingObjectionPublications(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Errorf("pending publications = %+v (err=%v), want none", pending, err)
	}
	// The sweeper no longer re-enqueues a failed intent when a retry runs it.
	if err := w.PublishObjection(ctx, intent.ID); err != nil {
		t.Fatalf("PublishObjection after failure: %v", err)
	}
	if err := w.FailObjectionPublication(ctx, intent.ID, "again"); err != nil {
		t.Fatalf("second FailObjectionPublication: %v", err)
	}
	again, err := st.FindingObjection(ctx, intent.ID)
	if err != nil || again.Failure != "thread deleted" {
		t.Errorf("failure = %q (err=%v), want the first terminal cause preserved", again.Failure, err)
	}
}
