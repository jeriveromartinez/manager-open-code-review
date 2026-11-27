package store_test

import (
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *fixture) proposeRule(repo store.Repository, decision, scope, category string, positive, negative []store.RuleExample) store.ReviewRule {
	f.t.Helper()

	rule, err := f.s.ProposeReviewRule(f.ctx, store.ReviewRule{
		RepositoryID:     repo.ID,
		Decision:         decision,
		Reason:           "decisión humana de prueba",
		PathScope:        scope,
		Category:         category,
		PositiveExamples: store.EncodeRuleExamples(positive),
		NegativeExamples: store.EncodeRuleExamples(negative),
		ProposedBy:       "lead",
	})
	if err != nil {
		f.t.Fatalf("ProposeReviewRule: %v", err)
	}
	return rule
}

func (f *fixture) observeRule(rule store.ReviewRule, now time.Time) store.ReviewRule {
	f.t.Helper()

	evaluation, err := store.EvaluateReviewRule(rule, now)
	if err != nil {
		f.t.Fatalf("EvaluateReviewRule: %v", err)
	}
	if _, err := f.s.RecordReviewRuleEvaluation(f.ctx, rule.ID, evaluation, now); err != nil {
		f.t.Fatalf("RecordReviewRuleEvaluation: %v", err)
	}
	observed, err := f.s.ObserveReviewRule(f.ctx, rule.ID, "lead", nil, now)
	if err != nil {
		f.t.Fatalf("ObserveReviewRule: %v", err)
	}
	return observed
}

func ruleScopeFixture(t *testing.T) (*fixture, store.Repository, store.Repository) {
	t.Helper()

	f := newFixture(t)
	installation := f.installation(f.nextID())
	team := f.team("core")
	first := f.repo(1, installation.ID, "acme/first", &team.ID, true)
	second := f.repo(2, installation.ID, "acme/second", &team.ID, true)
	return f, first, second
}

func validRuleExamples() ([]store.RuleExample, []store.RuleExample) {
	positive := []store.RuleExample{{
		Path: "internal/review/a.go", Category: "bug",
		Title: "nil pointer on optional field", Anchor: "if value == nil",
	}}
	negative := []store.RuleExample{{
		Path: "internal/review/a.go", Category: "bug",
		Title: "nil map write in cache", Anchor: "cache[key] = value",
	}}
	return positive, negative
}

func TestProposeReviewRuleVersionsAndScopes(t *testing.T) {
	f, first, second := ruleScopeFixture(t)
	positive, negative := validRuleExamples()

	v1 := f.proposeRule(first, store.RuleDecisionWithdraw, "internal/review/", "bug", positive, negative)
	if v1.Version != 1 || v1.Status != store.RuleStatusProposed {
		t.Fatalf("v1 = version %d status %q, want version 1 proposed", v1.Version, v1.Status)
	}
	if v1.RuleKey == "" {
		t.Fatal("rule key is empty")
	}

	v2 := f.proposeRule(first, store.RuleDecisionWithdraw, "internal/review/", "bug", positive, negative)
	if v2.Version != 2 {
		t.Fatalf("v2 version = %d, want 2", v2.Version)
	}
	old, err := f.s.ReviewRule(f.ctx, v1.ID)
	if err != nil {
		t.Fatalf("ReviewRule(v1): %v", err)
	}
	if old.Status != store.RuleStatusSuperseded || old.SupersededBy == nil || *old.SupersededBy != v2.ID {
		t.Errorf("v1 = status %q superseded_by %v, want superseded by v2", old.Status, old.SupersededBy)
	}

	firstRules, err := f.s.ReviewRulesForRepo(f.ctx, first.ID)
	if err != nil {
		t.Fatalf("ReviewRulesForRepo(first): %v", err)
	}
	if len(firstRules) != 2 {
		t.Fatalf("first repo rules = %d, want two versions", len(firstRules))
	}
	secondRules, err := f.s.ReviewRulesForRepo(f.ctx, second.ID)
	if err != nil {
		t.Fatalf("ReviewRulesForRepo(second): %v", err)
	}
	if len(secondRules) != 0 {
		t.Fatalf("second repo rules = %d, want none: a rule only applies in its repository", len(secondRules))
	}
}

func TestEvaluateReviewRuleMeasuresRepeatedFalsePositivesAndSevereOmissions(t *testing.T) {
	f, repo, _ := ruleScopeFixture(t)
	now := time.Now().UTC()

	rule := f.proposeRule(repo, store.RuleDecisionWithdraw, "internal/review/", "bug",
		[]store.RuleExample{
			{Path: "internal/review/a.go", Category: "bug", Title: "nil pointer on optional field"},
			{Path: "internal/review/b.go", Category: "bug", Title: "nil pointer on optional field"},
		},
		[]store.RuleExample{
			{Path: "internal/review/a.go", Category: "bug", Title: "real defect, same class", Severity: "major"},
			{Path: "internal/review/b.go", Category: "bug", Title: "other real defect", Severity: "minor"},
		})

	evaluation, err := store.EvaluateReviewRule(rule, now)
	if err != nil {
		t.Fatalf("EvaluateReviewRule: %v", err)
	}
	if evaluation.MatchedPositive != 2 || evaluation.FalsePositiveRepetition != 1 {
		t.Errorf("positives = %d repetition = %v, want 2 and 1",
			evaluation.MatchedPositive, evaluation.FalsePositiveRepetition)
	}
	if evaluation.MatchedNegative != 0 || evaluation.SevereOmissions != 0 {
		t.Errorf("negatives = %d severe = %d, want 0 and 0: the negative titles differ from the evidence",
			evaluation.MatchedNegative, evaluation.SevereOmissions)
	}

	// A negative example that shares the positive evidence is a severe omission:
	// the rule cannot tell the real defect from the false positive.
	rule.NegativeExamples = store.EncodeRuleExamples([]store.RuleExample{{
		Path: "internal/review/c.go", Category: "bug",
		Title: "NIL pointer on optional field", Severity: "major",
	}})
	evaluation, err = store.EvaluateReviewRule(rule, now)
	if err != nil {
		t.Fatalf("EvaluateReviewRule with severe negative: %v", err)
	}
	if evaluation.MatchedNegative != 1 || evaluation.SevereOmissions != 1 {
		t.Errorf("negatives = %d severe = %d, want 1 and 1", evaluation.MatchedNegative, evaluation.SevereOmissions)
	}
	if err := store.RulePromotionError(rule, evaluation); err == nil {
		t.Fatal("RulePromotionError = nil, want a rejection for a severe omission")
	}
}

func TestReviewRuleEvidenceExpiresAndIsReevaluated(t *testing.T) {
	f, repo, _ := ruleScopeFixture(t)
	positive, negative := validRuleExamples()
	now := time.Now().UTC().Truncate(time.Second)

	observed := f.observeRule(f.proposeRule(repo, store.RuleDecisionWithdraw, "internal/review/", "bug", positive, negative), now)
	if observed.Status != store.RuleStatusObservation || observed.EvidenceExpiresAt == nil {
		t.Fatalf("observed = status %q expires %v, want observation with an expiry", observed.Status, observed.EvidenceExpiresAt)
	}

	applicable, err := f.s.ApplicableReviewRules(f.ctx, repo.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ApplicableReviewRules(now): %v", err)
	}
	if len(applicable) != 1 {
		t.Fatalf("applicable = %d, want the observed rule", len(applicable))
	}

	expiredAt := now.Add(store.RuleEvidenceValidity + time.Hour)
	applicable, err = f.s.ApplicableReviewRules(f.ctx, repo.ID, expiredAt)
	if err != nil {
		t.Fatalf("ApplicableReviewRules(expired): %v", err)
	}
	if len(applicable) != 0 {
		t.Fatalf("applicable = %d after expiry, want none", len(applicable))
	}

	evaluation, err := store.EvaluateReviewRule(observed, expiredAt)
	if err != nil {
		t.Fatalf("re-evaluate: %v", err)
	}
	if _, err := f.s.RecordReviewRuleEvaluation(f.ctx, observed.ID, evaluation, expiredAt); err != nil {
		t.Fatalf("RecordReviewRuleEvaluation: %v", err)
	}
	applicable, err = f.s.ApplicableReviewRules(f.ctx, repo.ID, expiredAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("ApplicableReviewRules(re-evaluated): %v", err)
	}
	if len(applicable) != 1 {
		t.Fatalf("applicable = %d after re-evaluation, want the rule back", len(applicable))
	}
}

func TestReevaluatingActiveRuleWithSevereOmissionStopsApplying(t *testing.T) {
	f, repo, _ := ruleScopeFixture(t)
	positive, negative := validRuleExamples()
	now := time.Now().UTC().Truncate(time.Second)

	rule := f.observeRule(f.proposeRule(repo, store.RuleDecisionWithdraw, "internal/review/", "bug", positive, negative), now)
	active, err := f.s.ActivateReviewRule(f.ctx, rule.ID, "lead", nil, now)
	if err != nil {
		t.Fatalf("ActivateReviewRule: %v", err)
	}

	// A re-evaluation that reports a real defect would hide it, so the rule must
	// stop applying instead of refreshing its evidence window.
	evaluation := store.RuleEvaluation{
		EvaluatedAt: now, SampleSize: 2, MatchedPositive: 1,
		MatchedNegative: 1, SevereOmissions: 1,
	}
	if _, err := f.s.RecordReviewRuleEvaluation(f.ctx, active.ID, evaluation, now); err != nil {
		t.Fatalf("RecordReviewRuleEvaluation: %v", err)
	}

	applicable, err := f.s.ApplicableReviewRules(f.ctx, repo.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ApplicableReviewRules: %v", err)
	}
	if len(applicable) != 0 {
		t.Fatalf("applicable = %+v, want the rule to stop applying after a severe omission", applicable)
	}
}

func TestRevertReviewRuleRestoresPreviousVersion(t *testing.T) {
	f, repo, _ := ruleScopeFixture(t)
	positive, negative := validRuleExamples()
	now := time.Now().UTC().Truncate(time.Second)

	v1 := f.observeRule(f.proposeRule(repo, store.RuleDecisionWithdraw, "internal/review/", "bug", positive, negative), now)
	if _, err := f.s.ActivateReviewRule(f.ctx, v1.ID, "lead", nil, now); err != nil {
		t.Fatalf("ActivateReviewRule(v1): %v", err)
	}

	v2 := f.proposeRule(repo, store.RuleDecisionConfirm, "internal/review/", "bug", positive, negative)
	v2 = f.observeRule(v2, now)
	if _, err := f.s.ActivateReviewRule(f.ctx, v2.ID, "lead", nil, now); err != nil {
		t.Fatalf("ActivateReviewRule(v2): %v", err)
	}

	reverted, err := f.s.RevertReviewRule(f.ctx, v2.ID, "lead", nil, now)
	if err != nil {
		t.Fatalf("RevertReviewRule: %v", err)
	}
	if reverted.Version != 3 || reverted.Decision != store.RuleDecisionWithdraw ||
		reverted.Status != store.RuleStatusObservation {
		t.Fatalf("reverted = v%d %q %q, want v3 withdraw observation",
			reverted.Version, reverted.Decision, reverted.Status)
	}
	previous, err := f.s.ReviewRule(f.ctx, v2.ID)
	if err != nil {
		t.Fatalf("ReviewRule(v2): %v", err)
	}
	if previous.Status != store.RuleStatusSuperseded {
		t.Errorf("v2 status = %q, want superseded", previous.Status)
	}

	applicable, err := f.s.ApplicableReviewRules(f.ctx, repo.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ApplicableReviewRules: %v", err)
	}
	if len(applicable) != 1 || applicable[0].ID != reverted.ID {
		t.Fatalf("applicable = %+v, want only the reverted version", applicable)
	}
}
