package publish

import (
	"context"
	"fmt"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// criteriaFieldBytes bounds one rendered criterion field. The stored values are
// already bounded at extraction, but the evaluator's prose is model output and
// must not be able to blow up the summary comment.
const criteriaFieldBytes = 1000

// buildCriteriaBlock renders the per-criterion verdicts of #232. It reads only
// the rows stored for this run's head, so a moved head or a failed evaluation
// publishes nothing instead of stale evidence (#227).
func (p *publishContext) buildCriteriaBlock(ctx context.Context) string {
	if p.w == nil || p.w.Store == nil || p.run.HeadSHA == "" {
		return ""
	}
	assessments, err := p.w.Store.CurrentCriterionAssessments(ctx, p.pr.ID, p.run.HeadSHA)
	if err != nil || len(assessments) == 0 {
		return ""
	}
	return renderCriteriaBlock(assessments, p.sanitize)
}

// renderCriteriaBlock is the pure renderer, testable without a database. Every
// user- and model-controlled value goes through sanitize before it reaches the
// GitHub comment.
func renderCriteriaBlock(assessments []store.CriterionAssessment, sanitize func(string) string) string {
	var b strings.Builder
	b.WriteString("\n### Cumplimiento de criterios\n\n")
	for _, assessment := range assessments {
		fmt.Fprintf(&b, "- **%s** · %s\n", criterionStatusLabel(assessment.Status),
			boundedCriterionField(assessment.Text, criteriaFieldBytes, sanitize))
		if source := criterionSourceLabel(assessment); source != "" {
			fmt.Fprintf(&b, "  - Fuente: %s\n", source)
		}
		if evidence := criterionEvidenceLabel(assessment, sanitize); evidence != "" {
			fmt.Fprintf(&b, "  - Evidencia: %s\n", evidence)
		}
		if observed := strings.TrimSpace(assessment.Observed); observed != "" {
			fmt.Fprintf(&b, "  - Observado: %s\n", boundedCriterionField(observed, criteriaFieldBytes, sanitize))
		}
		if required := strings.TrimSpace(assessment.Required); required != "" {
			fmt.Fprintf(&b, "  - Exigido: %s\n", boundedCriterionField(required, criteriaFieldBytes, sanitize))
		}
		if reason := strings.TrimSpace(assessment.Reason); reason != "" {
			fmt.Fprintf(&b, "  - Motivo: %s\n", boundedCriterionField(reason, criteriaFieldBytes, sanitize))
		}
		if uncertainty := strings.TrimSpace(assessment.Uncertainty); uncertainty != "" {
			fmt.Fprintf(&b, "  - Incertidumbre: %s\n", boundedCriterionField(uncertainty, criteriaFieldBytes, sanitize))
		}
	}
	b.WriteString("\n")
	return b.String()
}

func criterionStatusLabel(status string) string {
	switch status {
	case store.CriterionSatisfied:
		return "cumplido"
	case store.CriterionUnsatisfied:
		return "incumplido"
	case store.CriterionNotApplicable:
		return "no aplicable"
	default:
		return "no verificable"
	}
}

func criterionSourceLabel(assessment store.CriterionAssessment) string {
	var label string
	switch {
	case assessment.SourceURL != "" && assessment.SourceNumber > 0:
		label = fmt.Sprintf("[%s#%d](%s)", assessment.SourceRepo, assessment.SourceNumber, assessment.SourceURL)
	case assessment.SourceNumber > 0:
		label = fmt.Sprintf("%s#%d", assessment.SourceRepo, assessment.SourceNumber)
	case assessment.SourceKind == "pr_body":
		label = "descripción del PR"
	case assessment.SourceKind != "":
		label = assessment.SourceKind
	}
	if label == "" {
		return ""
	}
	if assessment.SourceVersion != "" {
		label += " · versión " + assessment.SourceVersion
	}
	return label
}

func criterionEvidenceLabel(assessment store.CriterionAssessment, sanitize func(string) string) string {
	evidence := store.DecodeCriterionEvidence(assessment.Evidence)
	if len(evidence) == 0 {
		return ""
	}
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		label := "`" + sanitize(item.Path)
		if item.StartLine > 0 {
			if item.EndLine > item.StartLine {
				label += fmt.Sprintf(":%d-%d", item.StartLine, item.EndLine)
			} else {
				label += fmt.Sprintf(":%d", item.StartLine)
			}
		}
		label += "`"
		if item.Test {
			label += " (prueba)"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

// boundedCriterionField sanitizes and cuts a value without splitting a UTF-8
// rune. GitHub counts characters, but a byte bound is the cheap upper bound.
func boundedCriterionField(value string, limit int, sanitize func(string) string) string {
	clean := sanitize(value)
	if len(clean) <= limit {
		return clean
	}
	cut := limit
	for cut > 0 && (clean[cut]&0xC0) == 0x80 {
		cut--
	}
	return clean[:cut] + "…"
}
