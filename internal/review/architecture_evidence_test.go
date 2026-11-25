package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const evidenceTestRule = "architecture-rules/v1:test"

func evidenceDigests(values map[string]string) ArchitectureSourceDigest {
	return func(path string) (string, bool) {
		value, ok := values[path]
		return value, ok
	}
}

func evidenceVerdict(dimensions ...store.ArchitectureDimension) store.ArchitectureVerdict {
	return store.ArchitectureVerdict{
		Status:    store.ArchitectureComplete,
		Requested: true,
		Mode:      store.ArchitectureAnalysisObservation,
		Comparison: store.ArchitectureComparison{
			BaseSHA: "base-sha", HeadSHA: "head-1",
		},
		Scope: store.ArchitectureScope{Included: []store.ArchitectureScopeFile{
			{Path: "internal/review/run.go", Kind: changedModified},
		}},
		Dimensions: dimensions,
		Evidence:   store.ArchitectureEvidence{Markdown: "## Boundaries\n- reviewed"},
	}
}

func boundEvidenceVerdict(t *testing.T, digests map[string]string, dimensions ...store.ArchitectureDimension) store.ArchitectureVerdict {
	t.Helper()
	verdict := bindArchitectureEvidence(evidenceVerdict(dimensions...), evidenceTestRule, evidenceDigests(digests))
	if verdict.Evidence.RuleVersion != evidenceTestRule {
		t.Fatalf("rule version = %q, want %q", verdict.Evidence.RuleVersion, evidenceTestRule)
	}
	return verdict
}

func TestRevalidationMarksCitedSourceStaleAndRequiresReanalysis(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{
			"internal/store/architecture.go": "digest-1",
			"internal/review/run.go":         "digest-2",
		},
		store.ArchitectureDimension{
			Name:    "Module boundaries",
			Content: "The new call goes through `internal/store/architecture.go`.",
		},
		store.ArchitectureDimension{
			Name:    "Risks",
			Content: "No contract change was observed.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule,
		[]string{"internal/store/architecture.go"},
		false,
		evidenceDigests(map[string]string{
			"internal/store/architecture.go": "digest-1-changed",
			"internal/review/run.go":         "digest-2",
		}))

	if !revalidation.NeedsFullReview {
		t.Fatal("a changed cited source must force the accumulated review")
	}
	if revalidation.Verdict.Status == store.ArchitectureComplete {
		t.Fatal("stale evidence still asserts complete")
	}
	if !revalidation.Verdict.Dimensions[0].Stale {
		t.Errorf("cited dimension is not stale: %+v", revalidation.Verdict.Dimensions[0])
	}
	if revalidation.Verdict.Dimensions[1].Stale {
		t.Errorf("uncited dimension became stale on an unrelated source: %+v", revalidation.Verdict.Dimensions[1])
	}
	if !strings.Contains(revalidation.Verdict.Dimensions[0].StaleReason, "source_changed:internal/store/architecture.go") {
		t.Errorf("stale reason = %q", revalidation.Verdict.Dimensions[0].StaleReason)
	}
}

func TestRevalidationInvalidatesOnDigestMismatchDespiteEmptyInventory(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{"internal/store/architecture.go": "digest-1"},
		store.ArchitectureDimension{
			Name:    "Persistence",
			Content: "The migration is described in `internal/store/architecture.go`.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule, nil, false,
		evidenceDigests(map[string]string{"internal/store/architecture.go": "digest-2"}))

	if !revalidation.Verdict.Dimensions[0].Stale || !revalidation.NeedsFullReview {
		t.Fatalf("digest mismatch did not invalidate the evidence: %+v", revalidation)
	}
	if revalidation.Verdict.Status != store.ArchitectureStale {
		t.Errorf("status = %q, want stale", revalidation.Verdict.Status)
	}
}

func TestRevalidationKeepsEvidenceForUnrelatedPush(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{
			"internal/store/architecture.go": "digest-1",
			"internal/review/run.go":         "digest-2",
		},
		store.ArchitectureDimension{
			Name:    "Module boundaries",
			Content: "The new call goes through `internal/store/architecture.go`.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule,
		[]string{"docs/readme.md", "web/src/views/ReposView.vue"},
		false,
		evidenceDigests(map[string]string{
			"internal/store/architecture.go": "digest-1",
			"internal/review/run.go":         "digest-2",
		}))

	if revalidation.NeedsFullReview {
		t.Fatalf("unrelated push forced a full review: %v", revalidation.Reasons)
	}
	if revalidation.Verdict.Status != store.ArchitectureComplete {
		t.Errorf("status = %q, want the preserved complete verdict", revalidation.Verdict.Status)
	}
	if revalidation.Verdict.Dimensions[0].Stale {
		t.Errorf("unrelated push marked the dimension stale: %+v", revalidation.Verdict.Dimensions[0])
	}
}

func TestRevalidationInvalidatesEvidenceBoundToAnotherRuleVersion(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{"internal/store/architecture.go": "digest-1"},
		store.ArchitectureDimension{
			Name:    "Module boundaries",
			Content: "The new call goes through `internal/store/architecture.go`.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, "architecture-rules/v2:new", nil, false,
		evidenceDigests(map[string]string{"internal/store/architecture.go": "digest-1"}))

	if !revalidation.NeedsFullReview {
		t.Fatal("a rule change must force revalidation")
	}
	if revalidation.Verdict.Status != store.ArchitectureStale {
		t.Errorf("status = %q, want stale", revalidation.Verdict.Status)
	}
	if !revalidation.Verdict.Dimensions[0].Stale {
		t.Errorf("dimension bound to the old rule is not stale: %+v", revalidation.Verdict.Dimensions[0])
	}
	if !strings.Contains(revalidation.Verdict.Dimensions[0].StaleReason, "rule_version_changed") {
		t.Errorf("stale reason = %q", revalidation.Verdict.Dimensions[0].StaleReason)
	}
}

func TestRevalidationInvalidatesOnBaseChange(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{"internal/store/architecture.go": "digest-1"},
		store.ArchitectureDimension{
			Name:    "Module boundaries",
			Content: "The new call goes through `internal/store/architecture.go`.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule, nil, true,
		evidenceDigests(map[string]string{"internal/store/architecture.go": "digest-1"}))

	if !revalidation.NeedsFullReview || revalidation.Verdict.Status != store.ArchitectureStale {
		t.Fatalf("base change did not invalidate the verdict: %+v", revalidation)
	}
	for _, dimension := range revalidation.Verdict.Dimensions {
		if !dimension.Stale {
			t.Errorf("dimension %q survived a base change", dimension.Name)
		}
	}
}

func TestRevalidationInvalidatesEvidenceWhenOnlyADependencyChanges(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{"go.mod": "mod-1", "internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{
			Name:    "Dependencies",
			Content: "The new dependency is declared in `go.mod`.",
		},
	)

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule,
		[]string{"go.mod"}, false,
		evidenceDigests(map[string]string{"go.mod": "mod-2", "internal/review/run.go": "digest-1"}))

	if !revalidation.NeedsFullReview {
		t.Fatalf("a change only in a dependency did not force revalidation: %v", revalidation.Reasons)
	}
	if !revalidation.Verdict.Dimensions[0].Stale ||
		!strings.Contains(revalidation.Verdict.Dimensions[0].StaleReason, "source_changed:go.mod") {
		t.Errorf("dependency evidence is not stale: %+v", revalidation.Verdict.Dimensions[0])
	}
}

func TestRevalidationWithoutBoundEvidenceDoesNotForceReanalysis(t *testing.T) {
	prev := store.ArchitectureVerdict{
		Status:     store.ArchitectureInconclusive,
		Requested:  true,
		Mode:       store.ArchitectureAnalysisObservation,
		Comparison: store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-1"},
	}

	revalidation := revalidateArchitectureEvidence(prev, evidenceTestRule, nil, false,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))

	if revalidation.NeedsFullReview {
		t.Errorf("an unbound inconclusive verdict forced reanalysis: %v", revalidation.Reasons)
	}
}

func TestRevalidationAccumulatesAcrossTwoPushes(t *testing.T) {
	digests := map[string]string{
		"internal/a.go": "a-1",
		"internal/b.go": "b-1",
	}
	first := boundEvidenceVerdict(t, digests,
		store.ArchitectureDimension{
			Name:    "Boundary A and B",
			Content: "The boundary spans `internal/a.go` and `internal/b.go`.",
		},
	)

	// First push touches a cited source: its evidence is stale and must be
	// re-evidenced.
	firstRevalidation := revalidateArchitectureEvidence(first, evidenceTestRule,
		[]string{"internal/a.go"}, false,
		evidenceDigests(map[string]string{"internal/a.go": "a-2", "internal/b.go": "b-1"}))
	if !firstRevalidation.NeedsFullReview || firstRevalidation.Verdict.Status != store.ArchitectureStale {
		t.Fatalf("first push did not invalidate the evidence: %+v", firstRevalidation)
	}

	reEvidenced := bindArchitectureEvidence(first, evidenceTestRule,
		evidenceDigests(map[string]string{"internal/a.go": "a-2", "internal/b.go": "b-1"}))
	reEvidenced.Status = store.ArchitectureComplete

	// Second push touches the other cited source. The accumulated impact is
	// invalidated again instead of keeping the first push's conclusion.
	secondRevalidation := revalidateArchitectureEvidence(reEvidenced, evidenceTestRule,
		[]string{"internal/b.go"}, false,
		evidenceDigests(map[string]string{"internal/a.go": "a-2", "internal/b.go": "b-2"}))
	if !secondRevalidation.NeedsFullReview {
		t.Fatalf("second push did not invalidate the accumulated evidence: %v", secondRevalidation.Reasons)
	}
	if secondRevalidation.Verdict.Status != store.ArchitectureStale {
		t.Errorf("status = %q, want stale", secondRevalidation.Verdict.Status)
	}
}

func TestArchitectureEvidenceBindingIsReproducible(t *testing.T) {
	build := func() store.ArchitectureVerdict {
		return boundEvidenceVerdict(t,
			map[string]string{
				"internal/store/architecture.go": "digest-1",
				"go.mod":                         "digest-2",
			},
			store.ArchitectureDimension{
				Name:    "Boundaries",
				Content: "Check `go.mod` and `internal/store/architecture.go`.",
			},
			store.ArchitectureDimension{
				Name:    "Risks",
				Content: "No contract change.",
			},
		)
	}
	first, err := json.Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("binding is not reproducible:\n%s\n%s", first, second)
	}

	paths := extractArchitecturePaths("Check `go.mod`, `internal/store/architecture.go` and internal/review/run.go.")
	want := []string{"go.mod", "internal/review/run.go", "internal/store/architecture.go"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestArchitectureRuleVersionChangesWithRules(t *testing.T) {
	policy := store.QAPolicy{ArchitectureAnalysisMode: store.ArchitectureAnalysisObservation}
	first := architectureRuleVersion(policy, []string{"vendor/**", "generated/**"})
	second := architectureRuleVersion(policy, []string{"generated/**", "vendor/**"})
	if first != second {
		t.Errorf("rule version depends on exclusion order: %q != %q", first, second)
	}
	changed := architectureRuleVersion(policy, []string{"vendor/**"})
	if changed == first {
		t.Error("a changed rule set kept the same version")
	}
}

func TestMergeArchitectureEvidenceKeepsEarlierDimensions(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/store/architecture.go": "digest-1", "internal/a.go": "a-1"},
		store.ArchitectureDimension{
			Name:    "Boundaries",
			Content: "Check `internal/store/architecture.go`.",
		},
		store.ArchitectureDimension{
			Name:    "Persistence",
			Content: "Check `internal/a.go`.",
		},
	)
	next := store.ArchitectureVerdict{
		Status:     store.ArchitecturePartial,
		Requested:  true,
		Mode:       store.ArchitectureAnalysisObservation,
		Comparison: store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-2"},
		Dimensions: []store.ArchitectureDimension{
			{Name: "Boundaries", Content: "Re-evidenced boundary."},
		},
	}

	merged := mergeArchitectureEvidence(next, carried)
	if len(merged.Dimensions) != 2 {
		t.Fatalf("dimensions = %d, want the carried dimension preserved", len(merged.Dimensions))
	}
	names := []string{merged.Dimensions[0].Name, merged.Dimensions[1].Name}
	if names[0] != "Boundaries" || names[1] != "Persistence" {
		t.Errorf("dimensions = %v, want the new one to win and the earlier one to survive", names)
	}
	if merged.Status != store.ArchitecturePartial {
		t.Errorf("status = %q, want partial", merged.Status)
	}
	if merged.CarriedFrom != "head-1" {
		t.Errorf("carried from = %q, want head-1", merged.CarriedFrom)
	}
}

func TestMergeNeverReportsCompleteWithStaleDimensions(t *testing.T) {
	stale := store.ArchitectureDimension{Name: "Boundaries", Content: "old", Stale: true}
	next := store.ArchitectureVerdict{Status: store.ArchitectureComplete, Requested: true}
	merged := mergeArchitectureEvidence(next, store.ArchitectureVerdict{
		Requested:  true,
		Dimensions: []store.ArchitectureDimension{stale},
	})
	if merged.Status != store.ArchitectureStale {
		t.Errorf("status = %q, want stale", merged.Status)
	}
}

func TestCarryArchitectureVerdictRebindsHeadAndRecordsSource(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/store/architecture.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Check `internal/store/architecture.go`."},
	)
	got := carryArchitectureVerdict(carried, "base-sha", "head-2", store.ArchitectureAnalysisObservation)
	if got.Comparison.HeadSHA != "head-2" || got.Comparison.BaseSHA != "base-sha" {
		t.Errorf("comparison = %+v", got.Comparison)
	}
	if got.CarriedFrom != "head-1" {
		t.Errorf("carried from = %q, want head-1", got.CarriedFrom)
	}
	if got.Status != store.ArchitectureComplete || len(got.Dimensions) != 1 {
		t.Errorf("carried verdict lost evidence: %+v", got)
	}
}

func TestFinalizeBindsPartialVerdict(t *testing.T) {
	next := evidenceVerdict(store.ArchitectureDimension{
		Name:    "Boundaries",
		Content: "Check `internal/review/run.go:42:7`.",
	})
	next.Status = store.ArchitecturePartial

	got := finalizeArchitectureVerdict(next, store.ArchitectureVerdict{}, false, evidenceTestRule,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))

	if got.Evidence.RuleVersion != evidenceTestRule {
		t.Errorf("partial verdict was not bound: rule version = %q", got.Evidence.RuleVersion)
	}
	if len(got.Dimensions[0].Sources) != 1 || got.Dimensions[0].Sources[0].Path != "internal/review/run.go" {
		t.Errorf("sources = %+v, want the line suffix stripped", got.Dimensions[0].Sources)
	}
	if got.Dimensions[0].Sources[0].Digest != "digest-1" {
		t.Errorf("source digest = %q", got.Dimensions[0].Sources[0].Digest)
	}
}

func TestFinalizeKeepsInconclusiveWhenMergingCarriedEvidence(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Check `internal/review/run.go`."},
	)
	next := store.ArchitectureVerdict{
		Status:     store.ArchitectureInconclusive,
		Requested:  true,
		Mode:       store.ArchitectureAnalysisObservation,
		Comparison: store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-2"},
	}

	got := finalizeArchitectureVerdict(next, carried, true, evidenceTestRule,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))

	if got.Status != store.ArchitectureInconclusive {
		t.Errorf("status = %q, want inconclusive (an unverified merge is not partial)", got.Status)
	}
	if len(got.Dimensions) != 1 {
		t.Fatalf("dimensions = %d, want the carried dimension preserved", len(got.Dimensions))
	}
	if got.CarriedFrom != "head-1" {
		t.Errorf("carried from = %q", got.CarriedFrom)
	}
}

func TestFinalizeCompleteVerdictDoesNotMergeOldDimensions(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Old", Content: "Old evidence."},
	)
	next := evidenceVerdict(store.ArchitectureDimension{Name: "New", Content: "Fresh evidence."})

	got := finalizeArchitectureVerdict(next, carried, true, evidenceTestRule,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))

	if len(got.Dimensions) != 1 || got.Dimensions[0].Name != "New" {
		t.Errorf("dimensions = %+v, want only the fresh section", got.Dimensions)
	}
}

func TestMergePreservesTrustedViolationStatus(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Check `internal/review/run.go`."},
	)
	next := store.ArchitectureVerdict{
		Status:     store.ArchitectureViolation,
		Requested:  true,
		Mode:       store.ArchitectureAnalysisObservation,
		Baseline:   &store.ArchitectureBaseline{Status: store.ArchitectureBaselineViolation},
		Comparison: store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-2"},
	}

	merged := mergeArchitectureEvidence(next, carried)
	if merged.Status != store.ArchitectureViolation {
		t.Errorf("status = %q, want the deterministic baseline violation preserved", merged.Status)
	}
	if merged.Baseline == nil || merged.Baseline.Status != store.ArchitectureBaselineViolation {
		t.Errorf("baseline = %+v, want the deterministic result kept", merged.Baseline)
	}
}

func TestFinalizeKeepsStaleCarriedEvidence(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Check `internal/review/run.go`."},
	)
	carried.Dimensions[0].Stale = true
	carried.Status = store.ArchitectureStale
	next := store.ArchitectureVerdict{
		Status:     store.ArchitectureInconclusive,
		Requested:  true,
		Mode:       store.ArchitectureAnalysisObservation,
		Comparison: store.ArchitectureComparison{BaseSHA: "base-sha", HeadSHA: "head-2"},
	}

	got := finalizeArchitectureVerdict(next, carried, true, evidenceTestRule,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))
	if got.Status != store.ArchitectureStale {
		t.Errorf("status = %q, want stale so the panel still asks for revalidation", got.Status)
	}
	if len(got.Dimensions) != 1 || !got.Dimensions[0].Stale {
		t.Errorf("dimensions = %+v, want the stale carried evidence kept", got.Dimensions)
	}
}

func TestMergeKeepsCarriedSectionWhenNewSectionIsEmpty(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Real earlier evidence."},
	)
	next := store.ArchitectureVerdict{
		Status:     store.ArchitecturePartial,
		Requested:  true,
		Dimensions: []store.ArchitectureDimension{{Name: "Boundaries", Content: "   \n"}},
	}

	merged := mergeArchitectureEvidence(next, carried)
	if merged.Dimensions[0].Content != "Real earlier evidence." {
		t.Errorf("content = %q, want the carried section", merged.Dimensions[0].Content)
	}
	if len(merged.Dimensions[0].Sources) != 1 {
		t.Errorf("sources = %+v, want the carried binding", merged.Dimensions[0].Sources)
	}
	if merged.Evidence.Markdown == "" {
		t.Error("earlier report was dropped")
	}
}

func TestMergeRetainsBothReports(t *testing.T) {
	carried := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Boundaries", Content: "Real earlier evidence."},
	)
	carried.Evidence.Markdown = "## Boundaries\n- earlier full report"
	next := evidenceVerdict(store.ArchitectureDimension{Name: "Boundaries", Content: "Fresh partial evidence."})
	next.Status = store.ArchitecturePartial
	next.Evidence.Markdown = "## Boundaries\n- partial report"

	merged := mergeArchitectureEvidence(next, carried)
	if !strings.Contains(merged.Evidence.Markdown, "partial report") ||
		!strings.Contains(merged.Evidence.Markdown, "earlier full report") {
		t.Errorf("markdown = %q, want both reports", merged.Evidence.Markdown)
	}
}

func TestUncitedDimensionBoundToAnalyzedScopeDocumentsCost(t *testing.T) {
	prev := boundEvidenceVerdict(t,
		map[string]string{"internal/review/run.go": "digest-1"},
		store.ArchitectureDimension{Name: "Risks", Content: "No contract change was observed."},
	)
	if len(prev.Dimensions[0].Sources) != 1 || prev.Dimensions[0].Sources[0].Path != "internal/review/run.go" {
		t.Fatalf("uncited dimension sources = %+v, want the analyzed scope", prev.Dimensions[0].Sources)
	}

	inside := revalidateArchitectureEvidence(prev, evidenceTestRule,
		[]string{"internal/review/run.go"}, false,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-2"}))
	if !inside.NeedsFullReview || !inside.Verdict.Dimensions[0].Stale {
		t.Errorf("an edit inside the analyzed scope did not invalidate the prose-only dimension")
	}

	outside := revalidateArchitectureEvidence(prev, evidenceTestRule,
		[]string{"docs/readme.md"}, false,
		evidenceDigests(map[string]string{"internal/review/run.go": "digest-1"}))
	if outside.NeedsFullReview || outside.Verdict.Dimensions[0].Stale {
		t.Errorf("a push outside the analyzed scope invalidated the dimension")
	}
}

func TestArchitectureRuleVersionTracksRuleTables(t *testing.T) {
	policy := store.QAPolicy{ArchitectureAnalysisMode: store.ArchitectureAnalysisObservation}
	before := architectureRuleVersion(policy, nil)

	original := architectureInterfaceDirs
	architectureInterfaceDirs = append(append([]string(nil), original...), "custom-contracts")
	defer func() { architectureInterfaceDirs = original }()

	after := architectureRuleVersion(policy, nil)
	if before == after {
		t.Error("editing a detection table did not change the rule version")
	}
}
