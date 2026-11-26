package publish_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestRepublishRecomputesGateFromOpenFindings(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	// The run itself left two blocking findings open, so the published gate is
	// a failure. A human resolving both threads must flip it to success.
	f1 := seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	f2 := seedFinding(t, st, pr.ID, run.ID, "f2", 2, "blocker", store.FindingOpen)
	for _, f := range []store.Finding{f1, f2} {
		if err := st.UpdateFindingStatus(ctx, f.ID, store.FindingResolved, run.ID); err != nil {
			t.Fatalf("resolve %s: %v", f.ID, err)
		}
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Republish(ctx, pr.ID); err != nil {
		t.Fatalf("Republish() error = %v", err)
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) != 1 || patches[0].Conclusion != "success" {
		t.Fatalf("check patches = %+v, want one success", patches)
	}
	var reloaded store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&reloaded, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.QAStatus != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", reloaded.QAStatus, store.StatusReadyForHuman)
	}
}

func TestRepublishRestoresFailureWhenFindingsReopen(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Republish(ctx, pr.ID); err != nil {
		t.Fatalf("Republish() error = %v", err)
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) != 1 || patches[0].Conclusion != "failure" {
		t.Fatalf("check patches = %+v, want one failure", patches)
	}
	var reloaded store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&reloaded, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.QAStatus != store.StatusChangesRequested {
		t.Errorf("qa_status = %q, want %q", reloaded.QAStatus, store.StatusChangesRequested)
	}
}

func TestRepublishKeepsTheVerificationGateOfEachMode(t *testing.T) {
	cases := []struct {
		mode       string
		verified   bool
		conclusion string
		status     string
	}{
		{store.AutoTriageOff, false, "failure", store.StatusChangesRequested},
		{store.AutoTriageDryRun, true, "failure", store.StatusChangesRequested},
		{store.AutoTriageApply, true, "action_required", store.StatusInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
			enableAutoTriage(t, st, tc.mode)
			seedFinding(t, st, pr.ID, run.ID, "uncertain", 2, "major", store.FindingOpen)
			if tc.verified {
				setRunVerificationSession(t, st, run.ID, "verify-session")
				markVerified(t, st, "uncertain", "uncertain", "")
			}
			gh := newFakeGitHub(t)
			w, _ := newPublishWorker(t, st, gh)

			if err := w.Republish(ctx, pr.ID); err != nil {
				t.Fatalf("Republish() error = %v", err)
			}

			gh.mu.Lock()
			patches := append([]checkPatch(nil), gh.checkPatches...)
			gh.mu.Unlock()
			if len(patches) != 1 || patches[0].Conclusion != tc.conclusion {
				t.Fatalf("check patches = %+v, want one %s", patches, tc.conclusion)
			}
			var reloaded store.PullRequest
			if err := st.DB.WithContext(ctx).Take(&reloaded, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if reloaded.QAStatus != tc.status {
				t.Errorf("qa_status = %q, want %q", reloaded.QAStatus, tc.status)
			}
		})
	}
}

// The gate of another comparison must not be rewritten from a stale terminal
// run, and the shared review thread must not be moved either.
func TestRepublishSkipsRunOfAnotherComparison(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"base_ref": "main", "base_sha": "mb-old"}).Error; err != nil {
		t.Fatalf("set run comparison: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"base_ref": "release", "merge_base_sha": "mb-old"}).Error; err != nil {
		t.Fatalf("set PR comparison: %v", err)
	}
	f := seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	if err := st.SetFindingPublished(ctx, f.ID, 123, "NC_resolve", ""); err != nil {
		t.Fatalf("publish finding: %v", err)
	}
	f.GithubCommentID, f.GithubCommentNodeID = 123, "NC_resolve"
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Republish(ctx, pr.ID); err != nil {
		t.Fatalf("Republish() error = %v", err)
	}
	supported, err := w.SetFindingThread(ctx, pr.ID, f, true)
	if err != nil {
		t.Fatalf("SetFindingThread() error = %v", err)
	}
	if supported {
		t.Error("SetFindingThread() = true, want false for another comparison")
	}

	gh.mu.Lock()
	patches, queries := len(gh.checkPatches), len(gh.gqlQueries)
	gh.mu.Unlock()
	if patches != 0 || queries != 0 {
		t.Errorf("check patches = %d, graphql queries = %d; want none for another comparison", patches, queries)
	}
	var saved store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.QAStatus != store.StatusReviewing {
		t.Errorf("qa_status = %q, want unchanged", saved.QAStatus)
	}
}

func TestRepublishWithoutFinishedRunIsNoOp(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunQueued, "major")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Republish(ctx, pr.ID); err != nil {
		t.Fatalf("Republish() error = %v", err)
	}
	gh.mu.Lock()
	patches := len(gh.checkPatches)
	gh.mu.Unlock()
	if patches != 0 {
		t.Errorf("check patches = %d, want none without a verdict", patches)
	}
}

func TestSetFindingThreadResolvesAndReopens(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	f := seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	if err := st.SetFindingPublished(ctx, f.ID, 123, "NC_resolve", ""); err != nil {
		t.Fatalf("publish finding: %v", err)
	}
	f.GithubCommentID, f.GithubCommentNodeID = 123, "NC_resolve"
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	supported, err := w.SetFindingThread(ctx, pr.ID, f, true)
	if err != nil {
		t.Fatalf("SetFindingThread(resolve) error = %v", err)
	}
	if !supported {
		t.Fatal("SetFindingThread(resolve) = false, want true")
	}
	supported, err = w.SetFindingThread(ctx, pr.ID, f, false)
	if err != nil {
		t.Fatalf("SetFindingThread(reopen) error = %v", err)
	}
	if !supported {
		t.Fatal("SetFindingThread(reopen) = false, want true")
	}

	gh.mu.Lock()
	queries := append([]string(nil), gh.gqlQueries...)
	gh.mu.Unlock()
	var resolves, unresolves int
	for _, q := range queries {
		if strings.Contains(q, "unresolveReviewThread") {
			unresolves++
		}
		if strings.Contains(q, "resolveReviewThread") && !strings.Contains(q, "unresolveReviewThread") {
			resolves++
		}
	}
	if resolves != 1 || unresolves != 1 {
		t.Errorf("resolves = %d, unresolves = %d, want one of each", resolves, unresolves)
	}
	saved, err := st.Finding(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.GithubThreadID != "THREAD_1" {
		t.Errorf("thread id = %q, want THREAD_1", saved.GithubThreadID)
	}
}

func TestSetFindingThreadWithoutCoordinatesIsUnsupported(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	f := seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	supported, err := w.SetFindingThread(ctx, pr.ID, f, true)
	if err != nil {
		t.Fatalf("SetFindingThread() error = %v", err)
	}
	if supported {
		t.Error("SetFindingThread() = true, want false without published coordinates")
	}
}
