package review

import (
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Verdict is what the second OCR pass answered about one finding.
type Verdict string

const (
	VerdictConfirm   Verdict = "confirm"
	VerdictRefute    Verdict = "refute"
	VerdictUncertain Verdict = "uncertain"
)

// Verification is the verifier's answer for one finding. It is stored on the
// finding for every decision, including the ones that change nothing, so a
// dry run or an unapplied refutation keeps its evidence.
type Verification struct {
	Verdict    Verdict   `json:"verdict"`
	Evidence   string    `json:"evidence"`
	HeadSHA    string    `json:"head_sha"`
	Model      string    `json:"model"`
	SessionID  string    `json:"session_id"`
	VerifiedAt time.Time `json:"verified_at"`
	// Reason explains why a verdict could not be applied. It is set when the
	// evidence does not validate at the reviewed head or when the policy
	// degrades the answer, so the panel and the summary can show why the
	// finding is still the human's call.
	Reason string `json:"reason,omitempty"`
	// DowngradeTo is an optional lower severity the verifier proposes for the
	// finding. Objection reevaluation records it but never applies it: the gate
	// severity only changes with a human decision.
	DowngradeTo string `json:"downgrade_to,omitempty"`
}

// VerifyPolicy is the deterministic policy applied to the verifier's answers.
type VerifyPolicy struct {
	Mode         string // store.AutoTriageOff/DryRun/Apply
	AutoWithdraw bool
	MaxFindings  int
	MaxFiles     int
	TokenShare   int
	TimeShare    int
	TokenMax     int64
}

// Decision is what the manager does with one finding. Apply is false for a dry
// run and for every answer the policy degrades to uncertain; Status is still
// the one that would be written, so the dry run shows the outcome.
type Decision struct {
	FindingID    string
	Verdict      Verdict
	Verification Verification
	Apply        bool
	Status       string
	Reason       string
}

// ParseVerdicts reads the machine-readable contract out of the verifier's
// comments. The first non-empty line must be
// `QA-VERIFY <finding_id> <CONFIRM|REFUTE|UNCERTAIN>`; the evidence is the
// first following line that starts with `EVIDENCE:`. Comments that do not
// follow the contract are ignored, and the first verdict for an id wins.
func ParseVerdicts(comments []nativeComment) map[string]Verification {
	verdicts := make(map[string]Verification, len(comments))
	for _, comment := range comments {
		lines := strings.Split(strings.TrimSpace(comment.Content), "\n")
		if len(lines) == 0 {
			continue
		}
		fields := strings.SplitN(strings.TrimSpace(lines[0]), " ", 3)
		if len(fields) != 3 || !strings.EqualFold(fields[0], "QA-VERIFY") {
			continue
		}
		id := strings.TrimSpace(fields[1])
		if id == "" {
			continue
		}
		verdict, ok := parseVerdict(fields[2])
		if !ok {
			continue
		}
		if _, seen := verdicts[id]; seen {
			continue
		}
		verification := Verification{Verdict: verdict}
		evidenceSet := false
		for _, line := range lines[1:] {
			trimmed := strings.TrimSpace(line)
			upper := strings.ToUpper(trimmed)
			switch {
			case !evidenceSet && strings.HasPrefix(upper, "EVIDENCE:"):
				verification.Evidence = strings.TrimSpace(trimmed[len("EVIDENCE:"):])
				evidenceSet = true
			case strings.HasPrefix(upper, "SEVERITY:"):
				verification.DowngradeTo = strings.ToLower(strings.TrimSpace(trimmed[len("SEVERITY:"):]))
			}
		}
		verdicts[id] = verification
	}
	return verdicts
}

func parseVerdict(raw string) (Verdict, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "CONFIRM":
		return VerdictConfirm, true
	case "REFUTE":
		return VerdictRefute, true
	case "UNCERTAIN":
		return VerdictUncertain, true
	default:
		return "", false
	}
}

// validateEvidence is the single gate for a verdict that would confirm or
// dismiss a finding. It requires the answer to describe the reviewed head, the
// cited path to exist at that head, and the quoted line to match exactly one of
// the lines the finding cited (or its own ExistingCode). Anything else is not
// evidence: the caller degrades the verdict to uncertain instead of trusting a
// quote from another commit, another file or another part of the file.
func validateEvidence(f store.Finding, v Verification, files map[string][]byte, headSHA string) string {
	if headSHA == "" || !strings.EqualFold(strings.TrimSpace(v.HeadSHA), headSHA) {
		return "la verificación no corresponde al head revisado"
	}
	content, ok := files[f.Path]
	if !ok {
		return "el archivo citado no existe en el head revisado"
	}
	evidence := strings.TrimSpace(v.Evidence)
	if evidence == "" {
		return "el verificador no citó evidencia"
	}
	if strings.ContainsAny(evidence, "\r\n") {
		return "la evidencia no es una sola línea del archivo"
	}
	start, end := f.StartLine, f.EndLine
	if end < start {
		end = start
	}
	if start <= 0 {
		return "el hallazgo no cita un rango de líneas"
	}
	lines := strings.Split(string(content), "\n")
	for line := start; line <= end && line <= len(lines); line++ {
		if strings.TrimSpace(lines[line-1]) == evidence {
			return ""
		}
	}
	for _, line := range strings.Split(f.ExistingCode, "\n") {
		if strings.TrimSpace(line) == evidence {
			return ""
		}
	}
	return "la evidencia no coincide con las líneas citadas del hallazgo"
}

// Decide applies the policy to every finding of the run. Human-triaged
// findings are not touched at all; the rest always get a decision, applied
// only in apply mode and only when the policy accepts it.
func Decide(findings []store.Finding, verdicts map[string]Verification, files map[string][]byte, headSHA string, p VerifyPolicy) []Decision {
	decisions := make([]Decision, 0, len(findings))
	for _, f := range findings {
		if f.TriageSource == store.TriageSourceHuman {
			continue
		}
		verdict, answered := verdicts[f.ID]
		if !answered {
			verdict = Verification{
				Verdict: VerdictUncertain,
				HeadSHA: headSHA,
				Reason:  "OCR no respondió por este hallazgo",
			}
		}
		decisions = append(decisions, decideFinding(f, verdict, files, headSHA, p))
	}
	return decisions
}

// degradeEvidence turns an answer whose evidence cannot be validated into an
// explicit uncertainty. The recorded verdict becomes uncertain too: the quote
// from another commit, file or line must never keep confirming or dismissing.
func degradeEvidence(decision Decision, f store.Finding, reason string) Decision {
	decision.Verdict = VerdictUncertain
	decision.Status = f.Status
	decision.Apply = false
	decision.Reason = reason
	decision.Verification.Verdict = VerdictUncertain
	decision.Verification.Reason = reason
	return decision
}

// degradePolicy keeps the verifier's raw answer and evidence, so a dry run
// still shows what OCR actually said, and only degrades the decision the policy
// refuses to apply. The reason is recorded for the human.
func degradePolicy(decision Decision, f store.Finding, reason string) Decision {
	decision.Verdict = VerdictUncertain
	decision.Status = f.Status
	decision.Apply = false
	decision.Reason = reason
	decision.Verification.Reason = reason
	return decision
}

func decideFinding(f store.Finding, verification Verification, files map[string][]byte, headSHA string, p VerifyPolicy) Decision {
	decision := Decision{
		FindingID:    f.ID,
		Verdict:      verification.Verdict,
		Verification: verification,
		Status:       f.Status,
	}
	switch verification.Verdict {
	case VerdictConfirm:
		if reason := validateEvidence(f, verification, files, headSHA); reason != "" {
			return degradeEvidence(decision, f, reason)
		}
		decision.Status = store.FindingOpen
		decision.Reason = "OCR confirmó el hallazgo"
		decision.Apply = p.Mode == store.AutoTriageApply
	case VerdictRefute:
		switch {
		case severityRank(f.Severity) >= severityRank("major"):
			return degradePolicy(decision, f, "no se descarta una severidad bloqueante sin humano")
		case !p.AutoWithdraw:
			return degradePolicy(decision, f, "el descarte automático está apagado")
		}
		if reason := validateEvidence(f, verification, files, headSHA); reason != "" {
			return degradeEvidence(decision, f, reason)
		}
		decision.Status = store.FindingWithdrawn
		decision.Reason = "OCR descartó el hallazgo"
		decision.Apply = p.Mode == store.AutoTriageApply
	default:
		decision.Verdict = VerdictUncertain
		if decision.Verification.Reason == "" {
			decision.Verification.Reason = "OCR no pudo verificar el hallazgo"
		}
		decision.Reason = decision.Verification.Reason
	}
	return decision
}
