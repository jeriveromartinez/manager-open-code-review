package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// RuleApplication is the deterministic outcome of the review rules that match
// one finding. Status is empty when no rule changed it; Influences is the audit
// of every rule that matched, whether it applied or not.
type RuleApplication struct {
	Influences []store.RuleInfluence
	Status     string
	Reason     string
}

// ApplyReviewRules evaluates the applicable rules against one finding. The most
// specific rule decides; the rest are recorded as influence without acting.
// A rule never suppresses a severe finding: those stay visible until a person
// reviews the evidence, even in observation or with an active withdraw rule.
func ApplyReviewRules(rules []store.ReviewRule, finding store.Finding, now time.Time) RuleApplication {
	target := store.RuleTarget{
		Path:     finding.Path,
		Category: finding.Category,
		Title:    finding.Title,
		Anchor:   finding.ExistingCode,
	}
	matching := make([]store.ReviewRule, 0, len(rules))
	for _, rule := range rules {
		if !store.ReviewRuleApplicable(rule, now) {
			continue
		}
		if !store.ReviewRuleMatches(rule, target) {
			continue
		}
		matching = append(matching, rule)
	}
	sort.SliceStable(matching, func(i, j int) bool {
		if a, b := store.RuleSpecificity(matching[i]), store.RuleSpecificity(matching[j]); a != b {
			return a > b
		}
		return matching[i].Version > matching[j].Version
	})

	application := RuleApplication{}
	decided := false
	for _, rule := range matching {
		influence := store.RuleInfluence{
			RuleID:      rule.ID,
			RuleKey:     rule.RuleKey,
			Version:     rule.Version,
			Status:      rule.Status,
			Decision:    rule.Decision,
			Scope:       rule.PathScope,
			EvaluatedAt: store.DecodeRuleEvaluation(rule.Evaluation).EvaluatedAt,
		}
		switch {
		case rule.Status == store.RuleStatusObservation:
			influence.Reason = "regla en observación: no cambia el hallazgo"
		case rule.Decision == store.RuleDecisionConfirm:
			influence.Reason = "regla confirmada: el hallazgo se conserva"
			decided = true
		case severityRank(finding.Severity) >= severityRank(blockingSeverityKey):
			influence.Reason = "hallazgo grave: se conserva hasta que se revise la evidencia de la regla"
			decided = true
		case decided:
			influence.Reason = "otra regla ya decidió sobre este hallazgo"
		default:
			influence.Applied = true
			influence.Reason = fmt.Sprintf("regla v%d: %s", rule.Version, rule.Reason)
			application.Status = store.FindingWithdrawn
			application.Reason = influence.Reason
			decided = true
		}
		application.Influences = append(application.Influences, influence)
	}
	return application
}

// applyReviewRules records the rule influence of the run's findings and applies
// the withdraw decisions the approved rules accept. It runs after the findings
// are durable, so a crash here loses the annotation but never the finding, and
// after auto-triage, so a human decision always wins.
func (r *Runner) applyReviewRules(ctx context.Context, repositoryID int64, findings []store.Finding, log *runLogSink) {
	if repositoryID == 0 || len(findings) == 0 {
		return
	}
	now := time.Now().UTC()
	rules, err := r.Store.ApplicableReviewRules(ctx, repositoryID, now)
	if err != nil {
		log.Log(ctx, "worker", "", "warn", logPhaseFinish,
			fmt.Sprintf("no se pudieron cargar las reglas de revisión: %v", err))
		return
	}
	if len(rules) == 0 {
		return
	}

	influenced, applied := 0, 0
	for _, candidate := range findings {
		finding, err := r.Store.Finding(ctx, candidate.ID)
		if err != nil {
			continue
		}
		if finding.Status != store.FindingOpen || finding.Advisory ||
			finding.TriageSource == store.TriageSourceHuman {
			continue
		}
		application := ApplyReviewRules(rules, finding, now)
		if len(application.Influences) == 0 {
			// A finding that stopped matching a rule must stop claiming its
			// influence, or the panel and the summary would show stale evidence.
			if len(store.DecodeRuleInfluences(finding.RuleInfluences)) > 0 {
				if err := r.Store.RecordFindingRuleInfluences(ctx, finding.ID, nil); err != nil {
					log.Log(ctx, "worker", "", "warn", logPhaseFinish,
						fmt.Sprintf("no se pudieron limpiar las reglas del hallazgo %s: %v", finding.ID, err))
				}
			}
			continue
		}
		if err := r.Store.RecordFindingRuleInfluences(ctx, finding.ID, application.Influences); err != nil {
			log.Log(ctx, "worker", "", "warn", logPhaseFinish,
				fmt.Sprintf("no se pudieron guardar las reglas del hallazgo %s: %v", finding.ID, err))
			continue
		}
		influenced++
		if application.Status == "" || application.Status == finding.Status {
			continue
		}
		if _, err := r.Store.TriageFinding(ctx, finding.ID, application.Status, store.FindingAudit{
			Action: "finding.rule_triaged",
			Actor:  "review-rule",
			Source: store.TriageSourceRule,
			Reason: application.Reason,
		}); err != nil {
			if !errors.Is(err, store.ErrFindingHumanTriaged) {
				log.Log(ctx, "worker", "", "warn", logPhaseFinish,
					fmt.Sprintf("no se pudo aplicar la regla al hallazgo %s: %v", finding.ID, err))
			}
			continue
		}
		applied++
	}
	log.Log(ctx, "worker", "", "info", logPhaseFinish, fmt.Sprintf(
		"reglas de revisión: %d hallazgo(s) influido(s), %d retirado(s)", influenced, applied))
}

func ruleTimePtr(value time.Time) *time.Time { return &value }
