package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type criterionFakeEvaluator struct {
	calls   int
	answers map[string]RequirementAssessment
}

func (f *criterionFakeEvaluator) Evaluate(_ context.Context, _ CriterionEvaluationInput) (CriterionEvaluation, error) {
	f.calls++
	return CriterionEvaluation{Assessments: f.answers, Model: "fake"}, nil
}

func TestEvaluateRequirementsStoresCarriesAndReevaluates(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "internal"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	authPath := filepath.Join(workDir, "internal", "auth.go")
	if err := os.WriteFile(authPath, []byte("package auth\n"), 0o644); err != nil {
		t.Fatalf("write auth.go: %v", err)
	}

	s := testutil.NewStore(t)
	ctx := context.Background()
	if err := s.UpsertInstallation(ctx, store.Installation{
		ID: 1, AccountLogin: "acme", AccountType: "Organization",
	}); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpsertRepository(ctx, store.Repository{
		ID: 1, InstallationID: 1, FullName: "acme/repo", DefaultBranch: "main", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertRepository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: 1, Number: 1, Title: "PR", HeadSHA: "head-1", BaseRef: "main",
		State: "open", QAStatus: store.StatusPending,
	}
	if err := s.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	run := store.ReviewRun{PullRequestID: pr.ID, HeadSHA: "head-1", Status: store.RunSucceeded}
	if err := s.DB.WithContext(ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}

	linked := linkedContext{Issues: []linkedIssue{{
		Number: 7, Title: "Add auth", Body: "## Acceptance criteria\n- [ ] auth rejects insecure sessions\n",
	}}}
	encodedLinked, err := json.Marshal(linked)
	if err != nil {
		t.Fatalf("marshal linked context: %v", err)
	}
	set := ExtractRequirements(pr, "acme/repo", linked)
	req := set.Requirements[0]
	fake := &criterionFakeEvaluator{answers: map[string]RequirementAssessment{
		req.ID: {
			CriterionID: req.ID,
			Status:      store.CriterionUnsatisfied,
			Evidence:    []store.CriterionEvidence{{Path: "internal/auth.go", StartLine: 1, EndLine: 1}},
			Observed:    "auth accepts insecure sessions",
			Required:    "auth rejects insecure sessions",
		},
	}}
	runner := &Runner{Store: s, CriteriaEvaluator: fake, Cfg: RunnerConfig{ContextCeiling: 1000}}
	cfg := store.ReviewConfig{PR: pr, Repo: store.Repository{FullName: "acme/repo"}}
	log := newRunLogSink(s, nil, run.ID, 0)
	out := &runOutcome{}

	run.LinkedIssues = encodedLinked
	runner.evaluateRequirements(ctx, run, cfg, store.LLMProfile{}, nil, workDir,
		"head-1", "base", []string{"internal/auth.go"}, out, log)
	if fake.calls != 1 {
		t.Fatalf("evaluator calls = %d, want 1", fake.calls)
	}
	current, err := s.CurrentCriterionAssessments(ctx, pr.ID, "head-1")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 1 || current[0].Status != store.CriterionUnsatisfied || current[0].HeadSHA != "head-1" {
		t.Fatalf("stored verdict = %+v", current)
	}
	if evidence := store.DecodeCriterionEvidence(current[0].Evidence); len(evidence) != 1 || evidence[0].Digest == "" {
		t.Errorf("stored evidence = %+v, want a bound digest", evidence)
	}

	// A push that does not touch the cited file keeps the answer without a new
	// model call, but the stored verdict is re-stamped at the new head.
	run.HeadSHA = "head-2"
	runner.evaluateRequirements(ctx, run, cfg, store.LLMProfile{}, nil, workDir,
		"head-2", "base", []string{"docs/readme.md"}, out, log)
	if fake.calls != 1 {
		t.Errorf("evaluator was called for an unchanged verdict: %d", fake.calls)
	}
	current, err = s.CurrentCriterionAssessments(ctx, pr.ID, "head-2")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 1 || current[0].HeadSHA != "head-2" {
		t.Fatalf("carried verdict = %+v", current)
	}

	// Editing the cited file invalidates the evidence and forces a new answer.
	if err := os.WriteFile(authPath, []byte("package auth\n\nvar insecure = false\n"), 0o644); err != nil {
		t.Fatalf("rewrite auth.go: %v", err)
	}
	run.HeadSHA = "head-3"
	runner.evaluateRequirements(ctx, run, cfg, store.LLMProfile{}, nil, workDir,
		"head-3", "base", []string{"internal/auth.go"}, out, log)
	if fake.calls != 2 {
		t.Errorf("evaluator calls = %d, want a re-evaluation", fake.calls)
	}
	current, err = s.CurrentCriterionAssessments(ctx, pr.ID, "head-3")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 1 || current[0].HeadSHA != "head-3" {
		t.Fatalf("re-evaluated verdict = %+v", current)
	}
}

func TestEvaluateRequirementsNeedsNoEvaluatorForContradictions(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	s := testutil.NewStore(t)
	ctx := context.Background()
	if err := s.UpsertInstallation(ctx, store.Installation{ID: 1, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("UpsertInstallation: %v", err)
	}
	if err := s.UpsertRepository(ctx, store.Repository{
		ID: 1, InstallationID: 1, FullName: "acme/repo", DefaultBranch: "main", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertRepository: %v", err)
	}
	pr := store.PullRequest{RepositoryID: 1, Number: 2, Title: "PR", HeadSHA: "head", BaseRef: "main",
		State: "open", QAStatus: store.StatusPending}
	if err := s.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	run := store.ReviewRun{PullRequestID: pr.ID, HeadSHA: "head", Status: store.RunSucceeded}
	if err := s.DB.WithContext(ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	linked := linkedContext{Issues: []linkedIssue{{Number: 7, Title: "Issue",
		Body: "## Acceptance criteria\n- [ ] debe reintentar en 5xx\n- [ ] no debe reintentar en 5xx\n"}}}
	encoded, _ := json.Marshal(linked)
	run.LinkedIssues = encoded
	fake := &criterionFakeEvaluator{}
	runner := &Runner{Store: s, CriteriaEvaluator: fake}
	cfg := store.ReviewConfig{PR: pr, Repo: store.Repository{FullName: "acme/repo"}}
	out := &runOutcome{}
	runner.evaluateRequirements(ctx, run, cfg, store.LLMProfile{}, nil, workDir, "head", "base", []string{"a.go"}, out,
		newRunLogSink(s, nil, run.ID, 0))

	if fake.calls != 0 {
		t.Errorf("contradictory criteria reached the evaluator %d time(s)", fake.calls)
	}
	current, err := s.CurrentCriterionAssessments(ctx, pr.ID, "head")
	if err != nil {
		t.Fatalf("CurrentCriterionAssessments: %v", err)
	}
	if len(current) != 2 {
		t.Fatalf("verdicts = %+v, want 2", current)
	}
	for _, assessment := range current {
		if assessment.Status != store.CriterionUnverifiable || assessment.Uncertainty == "" {
			t.Errorf("contradiction was not recorded as uncertainty: %+v", assessment)
		}
	}
}
