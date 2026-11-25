package publish_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func seedInconclusivePublishContext(t *testing.T, st *store.Store) (store.ReviewRun, store.PullRequest) {
	t.Helper()

	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("error_code", "coverage_incomplete").Error; err != nil {
		t.Fatalf("mark run error: %v", err)
	}
	// The human verdict is recorded for an open pull request.
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("state", "open").Error; err != nil {
		t.Fatalf("open pull request: %v", err)
	}
	reloaded, err := st.ReviewRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return reloaded, pr
}

func recordHumanOutcome(t *testing.T, st *store.Store, pr store.PullRequest, decision string, complete bool) store.HumanVerdict {
	t.Helper()

	encoded, err := store.EncodeHumanVerdictScope(store.HumanVerdictScope{
		Files:    []store.HumanVerdictRange{{Path: "src/a.go", StartLine: 10, EndLine: 12}},
		Complete: complete,
	})
	if err != nil {
		t.Fatalf("EncodeHumanVerdictScope: %v", err)
	}
	saved, err := st.RecordHumanVerdict(context.Background(), store.HumanVerdict{
		PullRequestID: pr.ID,
		Decision:      decision,
		Conclusion:    "Revisión humana del rango pendiente",
		Reason:        "el motor no cubrió el archivo",
		Scope:         encoded,
		ScopeComplete: complete,
		ActorLogin:    "qa-lead",
		Source:        store.HumanVerdictSourcePanel,
	})
	if err != nil {
		t.Fatalf("RecordHumanVerdict: %v", err)
	}
	return saved
}

func lastCheckPatch(t *testing.T, gh *fakeGitHub) checkPatch {
	t.Helper()
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.checkPatches) == 0 {
		t.Fatal("no check patch published")
	}
	return gh.checkPatches[len(gh.checkPatches)-1]
}

// The check shows the automated result and the human result apart, and the
// gate follows the human decision without rewriting the run.
func TestPublishAppliesScopedHumanVerdict(t *testing.T) {
	tests := []struct {
		name       string
		decision   string
		complete   bool
		conclusion string
		status     string
	}{
		{
			name: "complete approval opens the gate", decision: store.HumanVerdictApproved,
			complete: true, conclusion: "success", status: store.StatusReadyForHuman,
		},
		{
			name: "partial approval keeps the case open", decision: store.HumanVerdictApproved,
			complete: false, conclusion: "action_required", status: store.StatusInconclusive,
		},
		{
			name: "requested changes fail the gate", decision: store.HumanVerdictChangesRequested,
			complete: true, conclusion: "failure", status: store.StatusChangesRequested,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedInconclusivePublishContext(t, st)
			recordHumanOutcome(t, st, pr, tc.decision, tc.complete)
			gh := newFakeGitHub(t)
			w, _ := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			patch := lastCheckPatch(t, gh)
			if patch.Conclusion != tc.conclusion {
				t.Errorf("check conclusion = %q, want %q", patch.Conclusion, tc.conclusion)
			}
			if got := storedQAStatus(t, st, pr.ID); got != tc.status {
				t.Errorf("qa_status = %q, want %q", got, tc.status)
			}
			// The automated result is not rewritten by the human record.
			saved, err := st.ReviewRun(ctx, run.ID)
			if err != nil {
				t.Fatalf("ReviewRun: %v", err)
			}
			if saved.Status != store.RunInconclusive {
				t.Errorf("run status = %q, want %q", saved.Status, store.RunInconclusive)
			}
			for _, want := range []string{"Resultado humano", "qa-lead", "src/a.go:10-12",
				"Revisión humana del rango pendiente", "no completó la cobertura"} {
				if !strings.Contains(patch.Output.Text, want) {
					t.Errorf("check text %q does not contain %q", patch.Output.Text, want)
				}
			}
			if tc.complete {
				if !strings.Contains(patch.Output.Text, "completo") {
					t.Errorf("check text %q does not say the human scope was complete", patch.Output.Text)
				}
			} else if !strings.Contains(patch.Output.Text, "parcial") {
				t.Errorf("check text %q does not say the human scope was partial", patch.Output.Text)
			}
			// The automated result stays visible as its own result.
			if !strings.Contains(patch.Output.Text, "coverage_incomplete") {
				t.Errorf("check text %q lost the automated result", patch.Output.Text)
			}

			summary := publishedSummary(t, gh)
			if !strings.Contains(summary, "Revisión no concluyente") ||
				!strings.Contains(summary, "Resultado humano") {
				t.Errorf("summary %q does not show both results apart", summary)
			}
		})
	}
}

// Recording a human decision after publication recomputes the existing check.
func TestRepublishAppliesHumanVerdict(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedInconclusivePublishContext(t, st)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "action_required" {
		t.Fatalf("check conclusion without a human verdict = %q, want action_required", got)
	}

	recordHumanOutcome(t, st, pr, store.HumanVerdictApproved, true)
	if err := w.Republish(ctx, pr.ID); err != nil {
		t.Fatalf("Republish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "success" {
		t.Errorf("check conclusion after the human approval = %q, want success", got)
	}
	if got := storedQAStatus(t, st, pr.ID); got != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", got, store.StatusReadyForHuman)
	}
	patch := lastCheckPatch(t, gh)
	if !strings.Contains(patch.Output.Text, "Resultado humano") ||
		!strings.Contains(patch.Output.Text, "coverage_incomplete") {
		t.Errorf("recomputed check does not show both results: %q", patch.Output.Text)
	}
}

// Without a human verdict an inconclusive run keeps today's gate.
func TestInconclusiveWithoutHumanVerdictStaysActionRequired(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _ := seedInconclusivePublishContext(t, st)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "action_required" {
		t.Errorf("check conclusion = %q, want action_required", got)
	}
	if text := lastCheckPatch(t, gh).Output.Text; strings.Contains(text, "Resultado humano") {
		t.Errorf("check text %q has a human result without a verdict", text)
	}
}
