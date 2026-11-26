package publish

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// humanVerdict returns the active human decision that belongs to this run's
// comparison. A verdict of another head or base is history and never reaches
// the gate, even if the row was not invalidated yet.
func (p *publishContext) humanVerdict(ctx context.Context) (store.HumanVerdict, bool) {
	verdict, ok, err := p.w.Store.CurrentHumanVerdict(ctx, p.pr)
	if err != nil {
		slog.Warn("read human verdict", "pull_request_id", p.pr.ID, "error", err)
		return store.HumanVerdict{}, false
	}
	if !ok || !strings.EqualFold(verdict.HeadSHA, p.run.HeadSHA) {
		return store.HumanVerdict{}, false
	}
	if p.run.BaseRef != "" && verdict.BaseRef != "" && verdict.BaseRef != p.run.BaseRef {
		return store.HumanVerdict{}, false
	}
	if p.run.BaseSHA != "" && verdict.MergeBaseSHA != "" &&
		!strings.EqualFold(verdict.MergeBaseSHA, p.run.BaseSHA) {
		return store.HumanVerdict{}, false
	}
	return verdict, true
}

// gateOutcome computes the check conclusion and the panel status from both
// results. The automated run keeps its own status: a human verdict never
// rewrites it, and the check text always shows the two apart. A partial human
// resolution leaves the case open instead of claiming the engine completed the
// coverage.
func (p *publishContext) gateOutcome(ctx context.Context, open []store.Finding) (conclusion, status string) {
	ci, err := p.ciVerdict(ctx)
	if err != nil {
		slog.Warn("read CI gate at publish", "pull_request_id", p.pr.ID, "run_id", p.run.ID, "error", err)
		ci = store.CIVerdict{State: store.CIUnknown}
	}

	if p.run.Status == store.RunSucceeded {
		switch {
		case p.hasBlocking(open):
			return "failure", store.StatusChangesRequested
		case p.hasMaterialUncertainty(open):
			// The verifier could not settle a finding that could change the
			// conclusion. The engine did not fail, but it did not conclude: the
			// case belongs to a person, not to a silent success.
			return "action_required", store.StatusInconclusive
		case ci.Green():
			return "success", store.StatusReadyForHuman
		default:
			// The review concluded but the required checks did not: the head is
			// still waiting for a CI verdict, not for a person.
			return "action_required", store.StatusWaitingCI
		}
	}

	verdict, ok := p.humanVerdict(ctx)
	if !ok {
		return "action_required", store.StatusInconclusive
	}
	switch verdict.Decision {
	case store.HumanVerdictChangesRequested:
		return "failure", store.StatusChangesRequested
	case store.HumanVerdictApproved:
		if !verdict.ScopeComplete {
			// A partial resolution is still a human request for more work, not a
			// claim that the automated coverage was completed.
			return "action_required", store.StatusInconclusive
		}
		if p.hasBlocking(open) {
			return "failure", store.StatusChangesRequested
		}
		if ci.Green() {
			return "success", store.StatusReadyForHuman
		}
		return "action_required", store.StatusWaitingCI
	default:
		return "action_required", store.StatusInconclusive
	}
}

// humanVerdictBlock renders the human decision as a section separate from the
// automated result. It states explicitly that the automation did not complete
// the coverage.
func (p *publishContext) humanVerdictBlock(ctx context.Context) string {
	verdict, ok := p.humanVerdict(ctx)
	if !ok {
		return ""
	}
	return p.renderHumanVerdict(verdict)
}

func (p *publishContext) renderHumanVerdict(verdict store.HumanVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Resultado humano:** %s (%s) · %s",
		humanDecisionLabel(verdict.Decision),
		humanScopeLabel(verdict.ScopeComplete),
		p.sanitize(verdict.ActorLogin))
	if !verdict.CreatedAt.IsZero() {
		fmt.Fprintf(&b, " · %s", verdict.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString("\n")

	scope := store.HumanVerdictScopeOf(verdict)
	if len(scope.Files) > 0 || len(scope.PendingFindings) > 0 {
		b.WriteString("\n**Alcance revisado:**\n")
		for _, file := range scope.Files {
			fmt.Fprintf(&b, "- `%s`\n", p.sanitize(humanRangeLabel(file)))
		}
		for _, id := range scope.PendingFindings {
			fmt.Fprintf(&b, "- hallazgo `%s`\n", p.sanitize(id))
		}
	}
	if conclusion := strings.TrimSpace(p.sanitize(verdict.Conclusion)); conclusion != "" {
		fmt.Fprintf(&b, "\n**Conclusión:** %s\n", conclusion)
	}
	if reason := strings.TrimSpace(p.sanitize(verdict.Reason)); reason != "" {
		fmt.Fprintf(&b, "**Motivo:** %s\n", reason)
	}
	b.WriteString("\n_La revisión automática no completó la cobertura; este resultado es humano._\n")
	return b.String()
}

func humanDecisionLabel(decision string) string {
	if decision == store.HumanVerdictChangesRequested {
		return "cambios requeridos"
	}
	return "aprobado"
}

func humanScopeLabel(complete bool) string {
	if complete {
		return "completo"
	}
	return "parcial"
}

func humanRangeLabel(file store.HumanVerdictRange) string {
	switch {
	case file.StartLine <= 0:
		return file.Path
	case file.EndLine > 0 && file.EndLine != file.StartLine:
		return fmt.Sprintf("%s:%d-%d", file.Path, file.StartLine, file.EndLine)
	default:
		return fmt.Sprintf("%s:%d", file.Path, file.StartLine)
	}
}
