package store_test

import (
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func seedHumanVerdictPR(t *testing.T, f *fixture, repoID int64) store.PullRequest {
	t.Helper()

	pr := f.openPR(repoID, 1, store.StatusInconclusive, false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{
			"head_sha":       "head-1",
			"base_ref":       "main",
			"base_sha":       "tip-1",
			"merge_base_sha": "mb-1",
		}).Error; err != nil {
		t.Fatalf("set comparison: %v", err)
	}
	return f.pullRequest(t, pr.ID)
}

func recordHumanVerdict(t *testing.T, f *fixture, prID uint, decision string, complete bool) store.HumanVerdict {
	t.Helper()

	scope := store.HumanVerdictScope{
		Files: []store.HumanVerdictRange{
			{Path: "internal/auth.go", StartLine: 10, EndLine: 12},
		},
		Complete: complete,
	}
	encoded, err := store.EncodeHumanVerdictScope(scope)
	if err != nil {
		t.Fatalf("EncodeHumanVerdictScope: %v", err)
	}
	saved, err := f.s.RecordHumanVerdict(f.ctx, store.HumanVerdict{
		PullRequestID: prID,
		Decision:      decision,
		Conclusion:    "conclusión humana",
		Reason:        "motivo humano",
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

func TestRecordHumanVerdictUsesCurrentComparisonAndReplacesThePrevious(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/human-verdict", nil, true)
	pr := seedHumanVerdictPR(t, f, repo.ID)

	first := recordHumanVerdict(t, f, pr.ID, store.HumanVerdictApproved, true)
	if first.HeadSHA != "head-1" || first.BaseRef != "main" || first.MergeBaseSHA != "mb-1" {
		t.Errorf("verdict comparison = %q/%q/%q, want the current one",
			first.HeadSHA, first.BaseRef, first.MergeBaseSHA)
	}
	if first.ActorLogin != "qa-lead" || !first.ScopeComplete || first.Conclusion == "" || first.Reason == "" {
		t.Errorf("verdict = %+v, want actor, scope, conclusion and reason", first)
	}

	current, ok, err := f.s.CurrentHumanVerdict(f.ctx, f.pullRequest(t, pr.ID))
	if err != nil || !ok || current.ID != first.ID {
		t.Fatalf("CurrentHumanVerdict = %+v, %v, %v; want the first verdict", current, ok, err)
	}

	// A new decision replaces the previous one instead of stacking two active
	// verdicts on the same comparison.
	second := recordHumanVerdict(t, f, pr.ID, store.HumanVerdictChangesRequested, false)
	current, ok, err = f.s.CurrentHumanVerdict(f.ctx, f.pullRequest(t, pr.ID))
	if err != nil || !ok || current.ID != second.ID {
		t.Fatalf("CurrentHumanVerdict = %+v, %v, %v; want the replacement", current, ok, err)
	}
	latest, ok, err := f.s.LatestHumanVerdict(f.ctx, pr.ID)
	if err != nil || !ok || latest.ID != second.ID {
		t.Fatalf("LatestHumanVerdict = %+v, %v, %v; want the replacement", latest, ok, err)
	}
	var replaced store.HumanVerdict
	if err := f.s.DB.WithContext(f.ctx).Take(&replaced, first.ID).Error; err != nil {
		t.Fatalf("reload first verdict: %v", err)
	}
	if replaced.InvalidatedAt == nil || replaced.InvalidatedReason != store.HumanVerdictInvalidatedReplaced {
		t.Errorf("previous verdict = %+v, want it invalidated as superseded", replaced)
	}
}

func TestHumanVerdictIsInvalidatedByPushAndRetarget(t *testing.T) {
	f := newFixture(t)
	instID := f.nextID()
	f.installation(instID)
	repo := f.repo(instID, instID, "acme/human-verdict-sync", nil, true)
	pr := seedHumanVerdictPR(t, f, repo.ID)
	first := recordHumanVerdict(t, f, pr.ID, store.HumanVerdictApproved, true)

	// A push moves the head: the decision no longer covers the comparison.
	pushed := pr
	pushed.HeadSHA = "head-2"
	pushed.GitHubUpdatedAt = pr.GitHubUpdatedAt.Add(time.Minute)
	if _, changed, err := f.s.SyncPullRequest(f.ctx, pushed); err != nil || !changed {
		t.Fatalf("SyncPullRequest(push) changed=%v err=%v", changed, err)
	}
	if _, ok, err := f.s.CurrentHumanVerdict(f.ctx, f.pullRequest(t, pr.ID)); err != nil || ok {
		t.Fatalf("CurrentHumanVerdict after push = %v, %v; want none", ok, err)
	}
	var invalidated store.HumanVerdict
	if err := f.s.DB.WithContext(f.ctx).Take(&invalidated, first.ID).Error; err != nil {
		t.Fatalf("reload pushed verdict: %v", err)
	}
	if invalidated.InvalidatedAt == nil || invalidated.InvalidatedReason != store.HumanVerdictInvalidatedHeadMoved {
		t.Errorf("verdict after push = %+v, want invalidated by the head move", invalidated)
	}
	if got := f.pullRequest(t, pr.ID).QAStatus; got != store.StatusPending {
		t.Errorf("qa_status after push = %q, want %q", got, store.StatusPending)
	}

	// A retarget with the same head also invalidates the decision and asks for a
	// new one, because the comparison identity changed.
	retargetBase := f.pullRequest(t, pr.ID)
	second := recordHumanVerdict(t, f, pr.ID, store.HumanVerdictApproved, true)
	retarget := retargetBase
	retarget.BaseRef = "release"
	retarget.BaseSHA = "tip-2"
	retarget.GitHubUpdatedAt = retargetBase.GitHubUpdatedAt.Add(time.Minute)
	if _, changed, err := f.s.SyncPullRequest(f.ctx, retarget); err != nil || !changed {
		t.Fatalf("SyncPullRequest(retarget) changed=%v err=%v", changed, err)
	}
	if _, ok, err := f.s.CurrentHumanVerdict(f.ctx, f.pullRequest(t, pr.ID)); err != nil || ok {
		t.Fatalf("CurrentHumanVerdict after retarget = %v, %v; want none", ok, err)
	}
	invalidated = store.HumanVerdict{}
	if err := f.s.DB.WithContext(f.ctx).Take(&invalidated, second.ID).Error; err != nil {
		t.Fatalf("reload retargeted verdict: %v", err)
	}
	if invalidated.InvalidatedAt == nil || invalidated.InvalidatedReason != store.HumanVerdictInvalidatedRetarget {
		t.Errorf("verdict after retarget = %+v, want invalidated by the retarget", invalidated)
	}
	if got := f.pullRequest(t, pr.ID).QAStatus; got != store.StatusInconclusive {
		t.Errorf("qa_status after retarget = %q, want the human request requested again", got)
	}
}

func TestHumanVerdictRequiresScopeEvidence(t *testing.T) {
	if _, err := store.EncodeHumanVerdictScope(store.HumanVerdictScope{}); err == nil {
		t.Error("EncodeHumanVerdictScope(empty) succeeded, want an error")
	}
	if _, err := store.EncodeHumanVerdictScope(store.HumanVerdictScope{
		Files: []store.HumanVerdictRange{{Path: "a.go", StartLine: 9, EndLine: 3}},
	}); err == nil {
		t.Error("EncodeHumanVerdictScope(end before start) succeeded, want an error")
	}
	if _, err := store.EncodeHumanVerdictScope(store.HumanVerdictScope{
		Files:    []store.HumanVerdictRange{{Path: "a.go"}},
		Complete: false,
	}); err != nil {
		t.Errorf("EncodeHumanVerdictScope(file range) error = %v", err)
	}
}
