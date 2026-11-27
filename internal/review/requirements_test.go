package review

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func writeCriterionTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create dir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func criterionTestPR() store.PullRequest {
	return store.PullRequest{Number: 9, Title: "PR", HTMLURL: "https://github.com/acme/repo/pull/9"}
}

func criterionTestLinked(body string) linkedContext {
	return linkedContext{Issues: []linkedIssue{{Number: 7, Title: "Add auth", Body: body}}}
}

func findRequirement(t *testing.T, set RequirementSet, text string) Requirement {
	t.Helper()
	want := normalizeCriterionText(text)
	for _, req := range set.Requirements {
		if normalizeCriterionText(req.Text) == want {
			return req
		}
	}
	t.Fatalf("criterion %q not found in %+v", text, set.Requirements)
	return Requirement{}
}

func TestExtractRequirementsKeepsSourceVersionFingerprintAndLink(t *testing.T) {
	body := "## Acceptance criteria\n- [ ] login rejects insecure sessions\n- [ ] logout clears the token\n"
	pr := criterionTestPR()
	pr.Body = "## Acceptance criteria\n- [ ] login rejects insecure sessions\n"
	set := ExtractRequirements(pr, "acme/repo", criterionTestLinked(body))

	if len(set.Requirements) != 2 {
		t.Fatalf("requirements = %d, want 2 (the PR body duplicates one): %+v", len(set.Requirements), set.Requirements)
	}
	req := findRequirement(t, set, "login rejects insecure sessions")
	if req.Source.Kind != criterionSourceIssue || req.Source.Repo != "acme/repo" || req.Source.Number != 7 {
		t.Errorf("source = %+v, want the linked issue", req.Source)
	}
	if req.Source.Version == "" || req.Fingerprint == "" || req.ID == "" {
		t.Errorf("criterion misses version/fingerprint/id: %+v", req)
	}
	if req.Link != "https://github.com/acme/repo/issues/7" {
		t.Errorf("link = %q", req.Link)
	}

	again := ExtractRequirements(pr, "acme/repo", criterionTestLinked(body))
	if again.Requirements[0].ID != req.ID || again.Requirements[0].Fingerprint != req.Fingerprint {
		t.Errorf("identical criterion changed identity: %+v vs %+v", again.Requirements[0], req)
	}

	changed := ExtractRequirements(pr, "acme/repo", criterionTestLinked(body+"- [ ] audit logs the logout\n"))
	if changed.Requirements[0].Source.Version == req.Source.Version {
		t.Errorf("source version did not change with the issue")
	}
}

func TestExtractRequirementsFlagsInaccessibleSource(t *testing.T) {
	set := ExtractRequirements(criterionTestPR(), "acme/repo", linkedContext{Issues: []linkedIssue{{
		Number: 3, Err: "referencia inaccesible",
	}}})
	if len(set.Requirements) != 1 {
		t.Fatalf("requirements = %+v, want one inaccessible source", set.Requirements)
	}
	req := set.Requirements[0]
	if !req.Inaccessible || req.Uncertainty == "" {
		t.Errorf("inaccessible source = %+v, want explicit uncertainty", req)
	}
	plan := planRequirementEvaluation(set.Requirements, nil, "head", t.TempDir(), nil)
	if len(plan.Evaluate) != 0 || plan.Precomputed[req.ID].Status != store.CriterionUnverifiable {
		t.Errorf("plan = %+v, want a deterministic no verificable", plan)
	}
}

func TestExtractRequirementsFlagsContradictoryCriteria(t *testing.T) {
	body := "## Acceptance criteria\n- [ ] debe reintentar en 5xx\n- [ ] no debe reintentar en 5xx\n"
	set := ExtractRequirements(criterionTestPR(), "acme/repo", criterionTestLinked(body))
	if len(set.Requirements) != 2 {
		t.Fatalf("requirements = %+v", set.Requirements)
	}
	for _, req := range set.Requirements {
		if len(req.Contradicts) != 1 || req.Uncertainty == "" {
			t.Errorf("criterion %q = %+v, want a contradiction", req.Text, req)
		}
	}
	plan := planRequirementEvaluation(set.Requirements, nil, "head", t.TempDir(), nil)
	if len(plan.Evaluate) != 0 {
		t.Errorf("contradictory criteria must not be sent to the evaluator: %+v", plan.Evaluate)
	}
	for _, req := range set.Requirements {
		if plan.Precomputed[req.ID].Status != store.CriterionUnverifiable {
			t.Errorf("criterion %q was not downgraded to no verificable", req.Text)
		}
	}
}

func TestPlanRequirementEvaluationCarriesOnlyValidatedEvidence(t *testing.T) {
	dir := t.TempDir()
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n\nvar secure = true\n")
	writeCriterionTestFile(t, dir, "internal/auth_test.go", "package auth\n\nfunc TestSecure(t *testing.T) {}\n")
	writeCriterionTestFile(t, dir, "docs/readme.md", "docs\n")

	set := ExtractRequirements(criterionTestPR(), "acme/repo",
		criterionTestLinked("## Acceptance criteria\n- [ ] auth rejects insecure sessions\n"))
	req := set.Requirements[0]

	evidence := BindCriterionEvidence(dir, []store.CriterionEvidence{
		{Path: "internal/auth.go", StartLine: 3, EndLine: 3},
		{Path: "internal/auth_test.go", Test: true},
	})
	previous := store.CriterionAssessment{
		CriterionID:   req.ID,
		Fingerprint:   req.Fingerprint,
		SourceVersion: req.Source.Version,
		Status:        store.CriterionSatisfied,
		Evidence:      store.EncodeCriterionEvidence(evidence),
		HeadSHA:       "head-1",
	}

	plan := planRequirementEvaluation(set.Requirements, []store.CriterionAssessment{previous}, "head-1", dir, nil)
	if len(plan.Carried) != 1 || len(plan.Evaluate) != 0 {
		t.Fatalf("unchanged head did not carry the verdict: %+v", plan)
	}

	// A push that changes the source package invalidates the cited test even
	// though the test file itself did not move.
	plan = planRequirementEvaluation(set.Requirements, []store.CriterionAssessment{previous}, "head-2", dir, []string{"internal/auth.go"})
	if len(plan.Carried) != 0 || len(plan.Evaluate) != 1 {
		t.Errorf("changed package did not invalidate the test evidence: %+v", plan)
	}

	// A push that only touches another directory keeps the verdict.
	plan = planRequirementEvaluation(set.Requirements, []store.CriterionAssessment{previous}, "head-2", dir, []string{"docs/readme.md"})
	if len(plan.Carried) != 1 {
		t.Errorf("unrelated push invalidated the verdict: %+v", plan)
	}

	// Editing a cited file invalidates the digest even without a changed-path list.
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n\nvar secure = false\n")
	plan = planRequirementEvaluation(set.Requirements, []store.CriterionAssessment{previous}, "head-2", dir, nil)
	if len(plan.Carried) != 0 || len(plan.Evaluate) != 1 {
		t.Errorf("moved evidence digest did not invalidate the verdict: %+v", plan)
	}
}

func TestPlanRequirementEvaluationReevaluatesChangedIssueVersion(t *testing.T) {
	dir := t.TempDir()
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n")
	old := ExtractRequirements(criterionTestPR(), "acme/repo",
		criterionTestLinked("## Acceptance criteria\n- [ ] auth rejects insecure sessions\n"))
	req := old.Requirements[0]
	previous := store.CriterionAssessment{
		CriterionID:   req.ID,
		Fingerprint:   req.Fingerprint,
		SourceVersion: req.Source.Version,
		Status:        store.CriterionSatisfied,
		Evidence: store.EncodeCriterionEvidence(BindCriterionEvidence(dir,
			[]store.CriterionEvidence{{Path: "internal/auth.go"}})),
		HeadSHA: "head-1",
	}
	updated := ExtractRequirements(criterionTestPR(), "acme/repo",
		criterionTestLinked("## Acceptance criteria\n- [ ] auth rejects insecure sessions\n- [ ] logout clears the token\n"))
	plan := planRequirementEvaluation(updated.Requirements, []store.CriterionAssessment{previous}, "head-1", dir, nil)
	if len(plan.Evaluate) != 2 {
		t.Errorf("changed issue did not force re-evaluation: %+v", plan)
	}
}

func TestBuildCriterionAssessmentsRequiresEvidenceAndObservedBehaviour(t *testing.T) {
	dir := t.TempDir()
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n")
	set := ExtractRequirements(criterionTestPR(), "acme/repo", criterionTestLinked(
		"## Acceptance criteria\n- [ ] auth rejects insecure sessions\n- [ ] logout clears the token\n"+
			"- [ ] audit writes one line\n"))
	plan := requirementEvaluationPlan{Carried: map[string]store.CriterionAssessment{}, Precomputed: map[string]store.CriterionAssessment{}}
	answers := map[string]RequirementAssessment{
		set.Requirements[0].ID: {CriterionID: set.Requirements[0].ID, Status: store.CriterionSatisfied},
		set.Requirements[1].ID: {CriterionID: set.Requirements[1].ID, Status: store.CriterionUnsatisfied,
			Evidence: []store.CriterionEvidence{{Path: "../outside.go"}}, Observed: "logout keeps the token"},
	}
	assessments := BuildCriterionAssessments(set.Requirements, plan, answers, dir, "head", "main", 1, 2, "fake", time.Now().UTC())

	if got := assessments[0].Status; got != store.CriterionUnverifiable {
		t.Errorf("satisfied without evidence = %q, want no verificable", got)
	}
	if got := assessments[1].Status; got != store.CriterionUnverifiable {
		t.Errorf("evidence outside the checkout = %q, want no verificable", got)
	}
	if assessments[1].Observed == "" {
		t.Errorf("observed behaviour was lost: %+v", assessments[1])
	}
	if got := assessments[2].Status; got != store.CriterionUnverifiable || assessments[2].Reason == "" {
		t.Errorf("silent evaluator = %+v, want no verificable with reason", assessments[2])
	}
}

func TestBuildCriterionAssessmentsExplainsMissingObservedBehaviour(t *testing.T) {
	dir := t.TempDir()
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n")
	set := ExtractRequirements(criterionTestPR(), "acme/repo",
		criterionTestLinked("## Acceptance criteria\n- [ ] auth rejects insecure sessions\n"))
	req := set.Requirements[0]
	plan := requirementEvaluationPlan{Carried: map[string]store.CriterionAssessment{}, Precomputed: map[string]store.CriterionAssessment{}}
	answers := map[string]RequirementAssessment{req.ID: {
		CriterionID: req.ID,
		Status:      store.CriterionSatisfied,
		Evidence:    []store.CriterionEvidence{{Path: "internal/auth.go"}},
		Required:    req.Text,
	}}
	assessment := BuildCriterionAssessments(set.Requirements, plan, answers, dir, "head", "main", 1, 2, "fake", time.Now().UTC())[0]
	if assessment.Status != store.CriterionSatisfied {
		t.Fatalf("status = %q, want cumplido", assessment.Status)
	}
	if assessment.Uncertainty == "" {
		t.Errorf("missing observed behaviour was not reported as uncertainty: %+v", assessment)
	}
}

func TestCriterionScenarios(t *testing.T) {
	dir := t.TempDir()
	writeCriterionTestFile(t, dir, "internal/auth.go", "package auth\n")
	writeCriterionTestFile(t, dir, "internal/logout.go", "package auth\n")
	body := "## Acceptance criteria\n- [ ] auth rejects insecure sessions\n- [ ] logout clears the token\n"

	// All criteria met.
	set := ExtractRequirements(criterionTestPR(), "acme/repo", criterionTestLinked(body))
	plan := requirementEvaluationPlan{Carried: map[string]store.CriterionAssessment{}, Precomputed: map[string]store.CriterionAssessment{}}
	answers := map[string]RequirementAssessment{}
	for _, req := range set.Requirements {
		answers[req.ID] = RequirementAssessment{CriterionID: req.ID, Status: store.CriterionSatisfied,
			Evidence: []store.CriterionEvidence{{Path: "internal/auth.go"}}, Observed: "behaves as required"}
	}
	assessments := BuildCriterionAssessments(set.Requirements, plan, answers, dir, "head", "main", 1, 2, "fake", time.Now().UTC())
	metrics := MeasureRequirements(assessments, 0, len(answers))
	if metrics.Satisfied != 2 || metrics.ValidNonCompliance != 0 || metrics.UnnecessaryEscalation != 0 {
		t.Errorf("all-met metrics = %+v", metrics)
	}

	// Partial compliance: one criterion is a valid non-compliance with evidence.
	answers[set.Requirements[0].ID] = RequirementAssessment{CriterionID: set.Requirements[0].ID,
		Status: store.CriterionUnsatisfied, Evidence: []store.CriterionEvidence{{Path: "internal/auth.go"}},
		Observed: "accepts insecure sessions", Required: set.Requirements[0].Text}
	assessments = BuildCriterionAssessments(set.Requirements, plan, answers, dir, "head", "main", 1, 2, "fake", time.Now().UTC())
	metrics = MeasureRequirements(assessments, 0, len(answers))
	if metrics.Satisfied != 1 || metrics.Unsatisfied != 1 || metrics.ValidNonCompliance != 1 {
		t.Errorf("partial metrics = %+v", metrics)
	}

	// Ambiguous requirement: a contradiction is never sent to the evaluator.
	ambiguous := ExtractRequirements(criterionTestPR(), "acme/repo", criterionTestLinked(
		"## Acceptance criteria\n- [ ] debe reintentar en 5xx\n- [ ] no debe reintentar en 5xx\n"))
	ambiguousPlan := planRequirementEvaluation(ambiguous.Requirements, nil, "head", dir, nil)
	ambiguousAssessments := BuildCriterionAssessments(ambiguous.Requirements, ambiguousPlan, nil, dir, "head", "main", 1, 2, "fake", time.Now().UTC())
	ambiguousMetrics := MeasureRequirements(ambiguousAssessments, 0, len(ambiguousPlan.Evaluate))
	if ambiguousMetrics.Unverifiable != 2 || ambiguousMetrics.ReEvaluated != 0 {
		t.Errorf("ambiguous metrics = %+v", ambiguousMetrics)
	}
}

func TestMeasureRequirementsCountsSignals(t *testing.T) {
	satisfied := store.CriterionAssessment{Status: store.CriterionSatisfied,
		Evidence: store.EncodeCriterionEvidence([]store.CriterionEvidence{{Path: "a.go"}})}
	missing := store.CriterionAssessment{Status: store.CriterionUnsatisfied,
		Evidence: store.EncodeCriterionEvidence([]store.CriterionEvidence{{Path: "b.go"}})}
	uncertain := store.CriterionAssessment{Status: store.CriterionUnverifiable,
		Evidence: store.EncodeCriterionEvidence([]store.CriterionEvidence{{Path: "c.go"}})}
	notApplicable := store.CriterionAssessment{Status: store.CriterionNotApplicable}
	metrics := MeasureRequirements([]store.CriterionAssessment{satisfied, missing, uncertain, notApplicable}, 2, 3)
	if metrics.Satisfied != 1 || metrics.Unsatisfied != 1 || metrics.Unverifiable != 1 || metrics.NotApplicable != 1 {
		t.Errorf("status counts = %+v", metrics)
	}
	if metrics.ValidNonCompliance != 1 || metrics.UnnecessaryEscalation != 1 {
		t.Errorf("observation counts = %+v", metrics)
	}
	if metrics.Carried != 2 || metrics.ReEvaluated != 3 {
		t.Errorf("plan counts = %+v", metrics)
	}
}
