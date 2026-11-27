package review

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func ruleFixture(decision, status, severity, title string, now time.Time) store.ReviewRule {
	return store.ReviewRule{
		ID: 7, RepositoryID: 1, RuleKey: "rule-key", Version: 2, Status: status,
		Decision: decision, Reason: "falso positivo repetido",
		PathScope: "internal/review/", Category: "bug",
		PositiveExamples: store.EncodeRuleExamples([]store.RuleExample{{
			Path: "internal/review/a.go", Category: "bug",
			Title: "nil pointer on optional field", Severity: severity,
		}}),
		NegativeExamples: store.EncodeRuleExamples([]store.RuleExample{{
			Path: "internal/review/a.go", Category: "bug",
			Title: "nil map write in cache", Severity: "major",
		}}),
		EvidenceExpiresAt: ruleTimePtr(now.Add(time.Hour)),
	}
}

func targetFinding(severity, title string) store.Finding {
	return store.Finding{
		ID: "f-1", Path: "internal/review/a.go", Category: "bug",
		Severity: severity, Title: title, Status: store.FindingOpen,
	}
}

func TestApplyReviewRulesSuppressesRepeatedFalsePositive(t *testing.T) {
	now := time.Now().UTC()
	rule := ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusActive, "minor", "", now)

	application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("minor", "Nil pointer on optional field"), now)
	if application.Status != store.FindingWithdrawn {
		t.Fatalf("status = %q, want withdrawn", application.Status)
	}
	if len(application.Influences) != 1 || !application.Influences[0].Applied ||
		application.Influences[0].RuleID != rule.ID || application.Influences[0].Version != rule.Version {
		t.Fatalf("influences = %+v, want one applied influence of the rule version", application.Influences)
	}
}

func TestApplyReviewRulesKeepsSimilarRealDefect(t *testing.T) {
	now := time.Now().UTC()
	rule := ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusActive, "minor", "", now)

	// Same repository, path and category, but the evidence is different: this is
	// a real defect of the same class, not the pattern a person refuted.
	application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("minor", "nil map write in cache"), now)
	if application.Status != "" || len(application.Influences) != 0 {
		t.Fatalf("application = %+v, want no influence on the real defect", application)
	}
}

func TestApplyReviewRulesNeverHidesSevereFindings(t *testing.T) {
	now := time.Now().UTC()
	rule := ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusActive, "major", "", now)

	application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("major", "nil pointer on optional field"), now)
	if application.Status != "" {
		t.Fatalf("status = %q, want the severe finding kept open", application.Status)
	}
	if len(application.Influences) != 1 || application.Influences[0].Applied {
		t.Fatalf("influences = %+v, want one unapplied influence", application.Influences)
	}
	if application.Influences[0].Reason == "" {
		t.Error("severe influence has no reason explaining why it stayed visible")
	}
}

func TestApplyReviewRulesObservedOrConfirmedNeverChangesStatus(t *testing.T) {
	now := time.Now().UTC()
	for _, rule := range []store.ReviewRule{
		ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusObservation, "minor", "", now),
		ruleFixture(store.RuleDecisionConfirm, store.RuleStatusActive, "minor", "", now),
	} {
		application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("minor", "nil pointer on optional field"), now)
		if application.Status != "" {
			t.Errorf("status = %q for %s %s, want no change", application.Status, rule.Status, rule.Decision)
		}
		if len(application.Influences) != 1 || application.Influences[0].Applied {
			t.Errorf("influences = %+v, want one unapplied audit entry", application.Influences)
		}
	}
}

func TestApplyReviewRulesIgnoresExpiredEvidence(t *testing.T) {
	now := time.Now().UTC()
	rule := ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusActive, "minor", "", now)
	expired := now.Add(store.RuleEvidenceValidity + time.Hour)

	application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("minor", "nil pointer on optional field"), expired)
	if application.Status != "" || len(application.Influences) != 0 {
		t.Fatalf("application = %+v, want an expired rule to stop applying", application)
	}
}

func TestApplyReviewRulesAppliesOnlyInItsApprovedScope(t *testing.T) {
	now := time.Now().UTC()
	rule := ruleFixture(store.RuleDecisionWithdraw, store.RuleStatusActive, "minor", "", now)
	rule.PathScope = "internal/publish/"

	application := ApplyReviewRules([]store.ReviewRule{rule}, targetFinding("minor", "nil pointer on optional field"), now)
	if application.Status != "" || len(application.Influences) != 0 {
		t.Fatalf("application = %+v, want no influence outside the approved path", application)
	}
}

func TestRunnerAppliesReviewRulesAndKeepsSevereFindingVisible(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	installation := store.Installation{ID: 992, AccountLogin: "acme", AccountType: "Organization"}
	if err := st.UpsertInstallation(ctx, installation); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	team := store.Team{Name: "QA", Slug: "qa"}
	if err := st.DB.Create(&team).Error; err != nil {
		t.Fatalf("seed team: %v", err)
	}
	repo := store.Repository{ID: 502, InstallationID: 992, FullName: "acme/rules", DefaultBranch: "main", Enabled: true, TeamID: &team.ID}
	if err := st.UpsertRepository(ctx, repo); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: repo.ID, Number: 9, Title: "PR", AuthorLogin: "octocat",
		HeadSHA: "head-1", BaseRef: "main", State: "open", QAStatus: store.StatusChangesRequested,
	}
	if err := st.DB.Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	for _, finding := range []store.Finding{
		{ID: "rule-minor", PullRequestID: pr.ID, Fingerprint: "fp-minor", IdentityVersion: 2,
			Path: "internal/review/a.go", StartLine: 3, EndLine: 3, Severity: "minor",
			Category: "bug", Title: "nil pointer on optional field", ExistingCode: "if value == nil"},
		{ID: "rule-major", PullRequestID: pr.ID, Fingerprint: "fp-major", IdentityVersion: 2,
			Path: "internal/review/a.go", StartLine: 9, EndLine: 9, Severity: "major",
			Category: "bug", Title: "nil pointer on optional field", ExistingCode: "if value == nil"},
	} {
		row := finding
		row.Status = store.FindingOpen
		if err := st.DB.Create(&row).Error; err != nil {
			t.Fatalf("seed finding %q: %v", row.ID, err)
		}
	}

	now := time.Now().UTC()
	rule, err := st.ProposeReviewRule(ctx, store.ReviewRule{
		RepositoryID: repo.ID, Decision: store.RuleDecisionWithdraw,
		Reason:    "el valor está validado antes de esta rama",
		PathScope: "internal/review/a.go", Category: "bug",
		PositiveExamples: store.EncodeRuleExamples([]store.RuleExample{{
			Path: "internal/review/a.go", Category: "bug", Title: "nil pointer on optional field",
		}}),
		NegativeExamples: store.EncodeRuleExamples([]store.RuleExample{{
			Path: "internal/review/a.go", Category: "bug",
			Title: "nil map write in cache", Severity: "major",
		}}),
		ProposedBy: "lead",
	})
	if err != nil {
		t.Fatalf("ProposeReviewRule: %v", err)
	}
	evaluation, err := store.EvaluateReviewRule(rule, now)
	if err != nil {
		t.Fatalf("EvaluateReviewRule: %v", err)
	}
	if _, err := st.RecordReviewRuleEvaluation(ctx, rule.ID, evaluation, now); err != nil {
		t.Fatalf("RecordReviewRuleEvaluation: %v", err)
	}
	if _, err := st.ActivateReviewRule(ctx, rule.ID, "lead", nil, now); err != nil {
		t.Fatalf("ActivateReviewRule: %v", err)
	}

	runner := &Runner{Store: st}
	runner.applyReviewRules(ctx, repo.ID, []store.Finding{{ID: "rule-minor"}, {ID: "rule-major"}}, nil)

	minor, err := st.Finding(ctx, "rule-minor")
	if err != nil {
		t.Fatalf("Finding(minor): %v", err)
	}
	if minor.Status != store.FindingWithdrawn || minor.TriageSource != store.TriageSourceRule {
		t.Errorf("minor = %+v, want a rule withdrawal", minor)
	}
	if influences := store.DecodeRuleInfluences(minor.RuleInfluences); len(influences) != 1 || !influences[0].Applied {
		t.Errorf("minor influences = %+v, want one applied rule", influences)
	}

	major, err := st.Finding(ctx, "rule-major")
	if err != nil {
		t.Fatalf("Finding(major): %v", err)
	}
	if major.Status != store.FindingOpen {
		t.Errorf("major status = %q, want it kept open", major.Status)
	}
	influences := store.DecodeRuleInfluences(major.RuleInfluences)
	if len(influences) != 1 || influences[0].Applied {
		t.Errorf("major influences = %+v, want one unapplied rule", influences)
	}
}
