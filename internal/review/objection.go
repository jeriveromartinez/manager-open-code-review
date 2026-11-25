package review

import "github.com/jeriveromartinez/qa-control-center/internal/store"

// blockingSeverityKey is the lowest severity the verifier may not withdraw
// automatically, mirroring decideFinding.
const blockingSeverityKey = "major"

// ObjectionDecision is the bounded reevaluation's answer for one reply.
type ObjectionDecision struct {
	Action       string
	Status       string
	Apply        bool
	Reason       string
	Verification Verification
}

// DecideObjection applies the existing verifier rules to an objection. It keeps
// the finding unless the answer refutes it with evidence that validates at the
// reviewed head, and it never applies a refutation of a blocking finding: that
// case escalates to a human, whose decision is the only one that can downgrade
// or withdraw it. A human triage is never reevaluated.
func DecideObjection(f store.Finding, verification Verification, files map[string][]byte, headSHA string, p VerifyPolicy) ObjectionDecision {
	decision := ObjectionDecision{
		Action:       store.ObjectionEscalate,
		Status:       f.Status,
		Verification: verification,
	}
	if f.TriageSource == store.TriageSourceHuman {
		decision.Reason = "una decisión humana no se reevalúa"
		return decision
	}

	switch verification.Verdict {
	case VerdictConfirm:
		if reason := validateEvidence(f, verification, files, headSHA); reason != "" {
			return degradeObjectionEvidence(decision, f, reason)
		}
		decision.Action = store.ObjectionUphold
		decision.Reason = "la evidencia confirma el hallazgo"
		return decision

	case VerdictRefute:
		if reason := validateEvidence(f, verification, files, headSHA); reason != "" {
			return degradeObjectionEvidence(decision, f, reason)
		}
		// A blocking finding is never downgraded or withdrawn by the verifier,
		// even if the answer proposed a lower severity.
		if severityRank(f.Severity) >= severityRank(blockingSeverityKey) {
			decision.Reason = "una objeción a un hallazgo bloqueante requiere confirmación humana antes de rebajarlo"
			decision.Verification.Reason = decision.Reason
			return decision
		}
		if verification.DowngradeTo != "" {
			if severityRank(verification.DowngradeTo) >= severityRank(f.Severity) {
				decision.Reason = "la rebaja propuesta no es inferior a la severidad actual"
				decision.Verification.Reason = decision.Reason
				return decision
			}
			decision.Action = store.ObjectionDowngrade
			decision.Reason = "la evidencia propone rebajar el hallazgo; requiere confirmación humana"
			return decision
		}
		decision.Action = store.ObjectionWithdraw
		decision.Status = store.FindingWithdrawn
		decision.Reason = "la evidencia refuta el hallazgo"
		if p.Mode != store.AutoTriageApply || !p.AutoWithdraw {
			decision.Apply = false
			decision.Reason = "el descarte automático está apagado"
			return decision
		}
		decision.Apply = true
		return decision

	default:
		if decision.Verification.Reason == "" {
			decision.Verification.Reason = "la respuesta no es concluyente"
		}
		decision.Reason = decision.Verification.Reason
		return decision
	}
}

// degradeObjectionEvidence turns an answer whose quote cannot be validated at
// the reviewed head into an explicit uncertainty, mirroring degradeEvidence:
// a quote from another commit, file or line must never stay CONFIRM or REFUTE.
func degradeObjectionEvidence(decision ObjectionDecision, f store.Finding, reason string) ObjectionDecision {
	decision.Action = store.ObjectionEscalate
	decision.Status = f.Status
	decision.Apply = false
	decision.Reason = reason
	decision.Verification.Verdict = VerdictUncertain
	decision.Verification.Reason = reason
	return decision
}
