package store_test

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestComparisonCoveredBy(t *testing.T) {
	pr := store.PullRequest{HeadSHA: "head", BaseRef: "main", MergeBaseSHA: "mb-1"}
	tests := []struct {
		name string
		run  store.ReviewRun
		want bool
	}{
		{
			name: "same comparison",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main", BaseSHA: "mb-1"},
			want: true,
		},
		{
			name: "legacy run without a key is a wildcard",
			run:  store.ReviewRun{HeadSHA: "head"},
			want: true,
		},
		{
			name: "legacy run with the same merge base and no base branch covers",
			run:  store.ReviewRun{HeadSHA: "head", BaseSHA: "mb-1"},
			want: true,
		},
		{
			name: "legacy run with a different merge base does not cover",
			run:  store.ReviewRun{HeadSHA: "head", BaseSHA: "mb-old"},
			want: false,
		},
		{
			name: "a resolved run with an unknown merge base does not cover a resolved comparison",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main"},
			want: false,
		},
		{
			name: "an unknown merge base covers an unresolved comparison",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main"},
			want: true,
		},
		{
			name: "an unknown merge base does not cover another base branch",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main"},
			want: false,
		},
		{
			name: "another head never covers",
			run:  store.ReviewRun{HeadSHA: "other", BaseRef: "main", BaseSHA: "mb-1"},
			want: false,
		},
		{
			name: "another base branch never covers even with the same merge base",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "release", BaseSHA: "mb-1"},
			want: false,
		},
		{
			name: "another merge base never covers",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main", BaseSHA: "mb-old"},
			want: false,
		},
		{
			name: "a resolved run stops covering while the base is unresolved",
			run:  store.ReviewRun{HeadSHA: "head", BaseRef: "main", BaseSHA: "mb-1"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current := pr
			switch tc.name {
			case "a resolved run stops covering while the base is unresolved",
				"an unknown merge base covers an unresolved comparison":
				current.MergeBaseSHA = ""
			case "an unknown merge base does not cover another base branch":
				current.MergeBaseSHA = ""
				current.BaseRef = "release"
			}
			if got := current.CoveredBy(tc.run); got != tc.want {
				t.Errorf("CoveredBy(%+v, %+v) = %v, want %v", current, tc.run, got, tc.want)
			}
		})
	}
}

func TestPendingReviewPRsComparesComparisonIdentity(t *testing.T) {
	f := newFixture(t)
	db := f.s.DB.WithContext(f.ctx)

	installID := f.nextID()
	f.installation(installID)
	team := f.team("qa")
	profile := f.llmProfile("comparison-profile", true)
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	repo := f.repo(installID, installID, "acme/comparison", &team.ID, true)

	covered := f.openPR(repo.ID, 1, store.StatusPending, false)
	retarget := f.openPR(repo.ID, 2, store.StatusPending, false)
	rewrittenBase := f.openPR(repo.ID, 3, store.StatusPending, false)
	legacy := f.openPR(repo.ID, 4, store.StatusPending, false)
	unresolved := f.openPR(repo.ID, 5, store.StatusPending, false)
	unresolvedRetarget := f.openPR(repo.ID, 6, store.StatusPending, false)
	unresolvedSameRef := f.openPR(repo.ID, 7, store.StatusPending, false)

	setPR := func(pr store.PullRequest, baseRef, baseSHA, mergeBase string) {
		t.Helper()
		if err := db.Model(&store.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{
			"base_ref": baseRef, "base_sha": baseSHA, "merge_base_sha": mergeBase,
		}).Error; err != nil {
			t.Fatalf("set comparison of PR #%d: %v", pr.Number, err)
		}
	}
	setRun := func(pr store.PullRequest, baseRef, baseSHA string) {
		t.Helper()
		if err := db.Model(&store.ReviewRun{}).Where("pull_request_id = ?", pr.ID).Updates(map[string]any{
			"base_ref": baseRef, "base_sha": baseSHA,
		}).Error; err != nil {
			t.Fatalf("set run comparison of PR #%d: %v", pr.Number, err)
		}
	}

	setPR(covered, "main", "tip-1", "mb-1")
	f.run(covered, store.RunSucceeded)
	setRun(covered, "main", "mb-1")

	setPR(retarget, "release", "tip-2", "mb-2")
	f.run(retarget, store.RunSucceeded)
	setRun(retarget, "main", "mb-2")

	setPR(rewrittenBase, "main", "tip-rewritten", "mb-new")
	f.run(rewrittenBase, store.RunSucceeded)
	setRun(rewrittenBase, "main", "mb-old")

	setPR(legacy, "main", "tip-4", "mb-4")
	f.run(legacy, store.RunSucceeded)

	// The base moved and the merge base has not been resolved again: the old
	// run cannot cover the delivery, so the poller must schedule it.
	setPR(unresolved, "main", "tip-5", "mb-5")
	f.run(unresolved, store.RunSucceeded)
	setRun(unresolved, "main", "mb-5")
	if err := db.Model(&store.PullRequest{}).Where("id = ?", unresolved.ID).
		Update("merge_base_sha", "").Error; err != nil {
		t.Fatalf("clear unresolved merge base: %v", err)
	}

	// A failed-before-clone run has no merge base. It covers the same base
	// branch only; a retarget must still be pending.
	setPR(unresolvedRetarget, "release", "tip-6", "")
	f.run(unresolvedRetarget, store.RunFailed)
	setRun(unresolvedRetarget, "main", "")
	setPR(unresolvedSameRef, "main", "tip-7", "")
	f.run(unresolvedSameRef, store.RunFailed)
	setRun(unresolvedSameRef, "main", "")

	got, err := f.s.PendingReviewPRs(f.ctx, 50)
	if err != nil {
		t.Fatalf("PendingReviewPRs: %v", err)
	}
	ids := prIDs(got)
	for _, pr := range []store.PullRequest{retarget, rewrittenBase, unresolved, unresolvedRetarget} {
		if !hasID(ids, pr.ID) {
			t.Errorf("PR #%d should be pending; got IDs %v", pr.Number, ids)
		}
	}
	for _, pr := range []store.PullRequest{covered, legacy, unresolvedSameRef} {
		if hasID(ids, pr.ID) {
			t.Errorf("PR #%d should be covered; got IDs %v", pr.Number, ids)
		}
	}
}

func TestSetPRMergeBaseKeepsTheComparisonItResolved(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/merge-base", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	stored, err := f.s.SetPRMergeBase(f.ctx, pr.ID, pr.HeadSHA, pr.BaseRef, pr.BaseSHA, "mb-1")
	if err != nil || !stored {
		t.Fatalf("SetPRMergeBase(new) = %v, %v; want true", stored, err)
	}
	stored, err = f.s.SetPRMergeBase(f.ctx, pr.ID, pr.HeadSHA, pr.BaseRef, pr.BaseSHA, "mb-1")
	if err != nil || !stored {
		t.Fatalf("SetPRMergeBase(same) = %v, %v; want true", stored, err)
	}
	stored, err = f.s.SetPRMergeBase(f.ctx, pr.ID, pr.HeadSHA, pr.BaseRef, pr.BaseSHA, "mb-2")
	if err != nil || stored {
		t.Fatalf("SetPRMergeBase(other merge base) = %v, %v; want false", stored, err)
	}
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("base_sha", "tip-1").Error; err != nil {
		t.Fatalf("move the base tip: %v", err)
	}
	stored, err = f.s.SetPRMergeBase(f.ctx, pr.ID, pr.HeadSHA, pr.BaseRef, "tip-0", "mb-1")
	if err != nil || stored {
		t.Fatalf("SetPRMergeBase(stale base tip) = %v, %v; want false", stored, err)
	}
	stored, err = f.s.SetPRMergeBase(f.ctx, pr.ID, pr.HeadSHA, pr.BaseRef, "tip-1", "mb-1")
	if err != nil || !stored {
		t.Fatalf("SetPRMergeBase(current base tip) = %v, %v; want true", stored, err)
	}
	stored, err = f.s.SetPRMergeBase(f.ctx, pr.ID, "another-head", pr.BaseRef, "tip-1", "mb-1")
	if err != nil || stored {
		t.Fatalf("SetPRMergeBase(moved head) = %v, %v; want false", stored, err)
	}

	var saved store.PullRequest
	if err := f.s.DB.WithContext(f.ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatalf("reload pull request: %v", err)
	}
	if saved.MergeBaseSHA != "mb-1" {
		t.Errorf("merge_base_sha = %q, want mb-1", saved.MergeBaseSHA)
	}
}

func TestSupersedeOtherActiveRunsUnresolvedBaseRef(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/unresolved", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	active, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main",
		Status: store.RunRunning,
	})
	if err != nil {
		t.Fatalf("seed active run: %v", err)
	}

	// Same head and both comparisons unresolved, but another base branch: Go
	// Covers is false, so the active run must be superseded too.
	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID,
		store.Comparison{HeadSHA: pr.HeadSHA, BaseRef: "release"}); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(retarget): %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, active.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if got.Status != store.RunSuperseded {
		t.Fatalf("run status = %q, want superseded for another base branch", got.Status)
	}

	same, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "release",
		Status: store.RunRunning,
	})
	if err != nil {
		t.Fatalf("seed same-ref run: %v", err)
	}
	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID,
		store.Comparison{HeadSHA: pr.HeadSHA, BaseRef: "release"}); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(same ref): %v", err)
	}
	got, err = f.s.ReviewRun(f.ctx, same.ID)
	if err != nil {
		t.Fatalf("ReviewRun(same): %v", err)
	}
	if got.Status != store.RunRunning {
		t.Errorf("same-ref unresolved run status = %q, want running", got.Status)
	}
}

func TestSupersedeOtherActiveRunsByComparison(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/supersede", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	old, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main", BaseSHA: "mb-old",
		Status: store.RunRunning,
	})
	if err != nil {
		t.Fatalf("seed running run: %v", err)
	}

	// Same head, another comparison: the active run must be superseded.
	target := store.Comparison{HeadSHA: pr.HeadSHA, BaseRef: "main", MergeBaseSHA: "mb-new"}
	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, target); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(other key): %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, old.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if got.Status != store.RunSuperseded {
		t.Fatalf("same-head other-key run status = %q, want superseded", got.Status)
	}

	kept, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA, BaseRef: "main", BaseSHA: "mb-new",
		Status: store.RunRunning,
	})
	if err != nil {
		t.Fatalf("seed current run: %v", err)
	}
	if err := f.s.SupersedeOtherActiveRuns(f.ctx, pr.ID, target); err != nil {
		t.Fatalf("SupersedeOtherActiveRuns(same key): %v", err)
	}
	got, err = f.s.ReviewRun(f.ctx, kept.ID)
	if err != nil {
		t.Fatalf("ReviewRun(kept): %v", err)
	}
	if got.Status != store.RunRunning {
		t.Errorf("same-key run status = %q, want running", got.Status)
	}
}
