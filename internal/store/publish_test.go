package store_test

import (
	"encoding/json"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// The terminal transaction must record the publication intent, the reconciler
// must find it, and completing the delivery must clear it for good.
func TestPublishIntentLifecycle(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	f.repo(installID, installID, "acme/publish", nil, true)
	pr := f.openPR(installID, 11, store.StatusReviewing, false)
	run := f.run(pr, store.RunRunning)

	if err := f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status: store.RunSucceeded,
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}

	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if !stored.PublishPending {
		t.Fatal("the terminal transaction did not persist the publish intent")
	}

	pending, err := f.s.PendingPublishRuns(f.ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublishRuns: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != run.ID {
		t.Fatalf("pending runs = %+v, want [%d]", pending, run.ID)
	}

	for _, step := range []string{store.PublishStepInline, store.PublishStepSummary} {
		if err := f.s.MarkPublishStep(f.ctx, run.ID, step); err != nil {
			t.Fatalf("MarkPublishStep(%q): %v", step, err)
		}
	}
	stored, err = f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun after steps: %v", err)
	}
	state := map[string]bool{}
	if err := json.Unmarshal(stored.PublishState, &state); err != nil {
		t.Fatalf("decode publish_state %s: %v", stored.PublishState, err)
	}
	for _, step := range []string{store.PublishStepInline, store.PublishStepSummary} {
		if !state[step] {
			t.Errorf("publish_state = %s, want %s", stored.PublishState, step)
		}
	}

	if err := f.s.CompletePublish(f.ctx, run.ID); err != nil {
		t.Fatalf("CompletePublish: %v", err)
	}
	stored, err = f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun after complete: %v", err)
	}
	if stored.PublishPending {
		t.Error("publish_pending stayed true after CompletePublish")
	}
	state = map[string]bool{}
	if err := json.Unmarshal(stored.PublishState, &state); err != nil {
		t.Fatalf("decode publish_state %s: %v", stored.PublishState, err)
	}
	if !state["completed"] {
		t.Errorf("publish_state = %s, want completed", stored.PublishState)
	}

	pending, err = f.s.PendingPublishRuns(f.ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublishRuns after complete: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending runs = %+v, want none", pending)
	}
}

// The per-page cursor survives the process so a retry can resume after the
// annotations GitHub already accepted instead of appending them again.
func TestPublishCursorPersistsAcceptedAnnotations(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	f.repo(installID, installID, "acme/cursor", nil, true)
	pr := f.openPR(installID, 13, store.StatusReviewing, false)
	run := f.run(pr, store.RunRunning)

	if err := f.s.AdvancePublishCursor(f.ctx, run.ID, store.PublishCursorCheckAnnotations, 50); err != nil {
		t.Fatalf("AdvancePublishCursor(50): %v", err)
	}
	if err := f.s.AdvancePublishCursor(f.ctx, run.ID, store.PublishCursorCheckAnnotations, 60); err != nil {
		t.Fatalf("AdvancePublishCursor(60): %v", err)
	}
	if err := f.s.MarkPublishStep(f.ctx, run.ID, store.PublishStepCheck); err != nil {
		t.Fatalf("MarkPublishStep: %v", err)
	}

	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	var state struct {
		Accepted int  `json:"check_annotations"`
		Check    bool `json:"check"`
	}
	if err := json.Unmarshal(stored.PublishState, &state); err != nil {
		t.Fatalf("decode publish_state %s: %v", stored.PublishState, err)
	}
	if state.Accepted != 60 {
		t.Errorf("cursor = %d, want 60", state.Accepted)
	}
	if !state.Check {
		t.Errorf("publish_state = %s, want the check step too", stored.PublishState)
	}
}

// The reconciler must not pick up queued, running or settled runs.
func TestPendingPublishRunsOnlyTerminalUnsettled(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	f.repo(installID, installID, "acme/pending", nil, true)
	pr := f.openPR(installID, 12, store.StatusReviewing, false)

	queued := f.run(pr, store.RunQueued)
	settled := f.run(pr, store.RunSucceeded)
	if err := f.s.CompletePublish(f.ctx, settled.ID); err != nil {
		t.Fatalf("CompletePublish: %v", err)
	}
	want := f.run(pr, store.RunFailed)
	if err := f.s.DB.Model(&store.ReviewRun{}).Where("id = ?", want.ID).
		Update("publish_pending", true).Error; err != nil {
		t.Fatalf("mark pending: %v", err)
	}

	pending, err := f.s.PendingPublishRuns(f.ctx, 10)
	if err != nil {
		t.Fatalf("PendingPublishRuns: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != want.ID {
		t.Fatalf("pending runs = %+v, want [%d] (queued %d)", pending, want.ID, queued.ID)
	}
}

// Only the latest terminal run of the current head/base branch owns the shared
// QA status; an in-flight run or a resolved-but-different comparison does not.
func TestCurrentVerdictRun(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	f.repo(installID, installID, "acme/verdict", nil, true)
	pr := f.openPR(installID, 21, store.StatusReviewing, false)
	older := f.run(pr, store.RunSucceeded)

	owner, ok, err := f.s.CurrentVerdictRun(f.ctx, pr)
	if err != nil {
		t.Fatalf("CurrentVerdictRun: %v", err)
	}
	if !ok || owner.ID != older.ID {
		t.Fatalf("current verdict = %+v/%v, want %d", owner, ok, older.ID)
	}

	// A newer run in flight leaves the comparison without a verdict.
	newer := f.run(pr, store.RunQueued)
	if _, ok, err := f.s.CurrentVerdictRun(f.ctx, pr); err != nil {
		t.Fatalf("CurrentVerdictRun with a queued run: %v", err)
	} else if ok {
		t.Fatal("a queued newest run still reported a current verdict")
	}

	// Once it finishes it owns the status.
	if err := f.s.DB.Model(&store.ReviewRun{}).Where("id = ?", newer.ID).
		Update("status", store.RunSucceeded).Error; err != nil {
		t.Fatalf("complete newer run: %v", err)
	}
	owner, ok, err = f.s.CurrentVerdictRun(f.ctx, pr)
	if err != nil {
		t.Fatalf("CurrentVerdictRun after completion: %v", err)
	}
	if !ok || owner.ID != newer.ID {
		t.Fatalf("current verdict = %+v/%v, want %d", owner, ok, newer.ID)
	}

	// A run that finished before the base was resolved still owns the status:
	// an empty current merge base is unknown, not a mismatch.
	if err := f.s.DB.Model(&store.ReviewRun{}).Where("id = ?", newer.ID).
		Updates(map[string]any{"base_ref": "main", "base_sha": "mb-1"}).Error; err != nil {
		t.Fatalf("set run comparison: %v", err)
	}
	if err := f.s.DB.Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"base_ref": "main", "merge_base_sha": ""}).Error; err != nil {
		t.Fatalf("clear PR merge base: %v", err)
	}
	pr = f.pullRequest(t, pr.ID)
	owner, ok, err = f.s.CurrentVerdictRun(f.ctx, pr)
	if err != nil {
		t.Fatalf("CurrentVerdictRun unresolved: %v", err)
	}
	if !ok || owner.ID != newer.ID {
		t.Fatalf("unresolved current verdict = %+v/%v, want %d", owner, ok, newer.ID)
	}

	// A resolved merge base that disagrees is genuine staleness.
	if err := f.s.DB.Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("merge_base_sha", "mb-2").Error; err != nil {
		t.Fatalf("move PR merge base: %v", err)
	}
	pr = f.pullRequest(t, pr.ID)
	if _, ok, err := f.s.CurrentVerdictRun(f.ctx, pr); err != nil {
		t.Fatalf("CurrentVerdictRun with a moved merge base: %v", err)
	} else if ok {
		t.Fatal("a different resolved merge base reported a current verdict")
	}

	// Adoption (or the base returning to the reviewed merge base) restores it.
	if err := f.s.DB.Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("merge_base_sha", "mb-1").Error; err != nil {
		t.Fatalf("adopt comparison: %v", err)
	}
	pr = f.pullRequest(t, pr.ID)
	owner, ok, err = f.s.CurrentVerdictRun(f.ctx, pr)
	if err != nil {
		t.Fatalf("CurrentVerdictRun after adoption: %v", err)
	}
	if !ok || owner.ID != newer.ID {
		t.Fatalf("current verdict after adoption = %+v/%v, want %d", owner, ok, newer.ID)
	}
}
