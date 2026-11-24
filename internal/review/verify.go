package review

import (
	"bytes"
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
		for _, line := range lines[1:] {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(strings.ToUpper(trimmed), "EVIDENCE:") {
				continue
			}
			verification.Evidence = strings.TrimSpace(trimmed[len("EVIDENCE:"):])
			break
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

// EvidenceInFile reports whether the cited evidence appears literally in the
// head file. A refutation that cannot be found there is a hallucination or an
// injection, so it never withdraws a finding.
func EvidenceInFile(v Verification, content []byte) bool {
	evidence := strings.TrimSpace(v.Evidence)
	return evidence != "" && bytes.Contains(content, []byte(evidence))
}

// Decide applies the policy to every finding of the run. Human-triaged
// findings are not touched at all; the rest always get a decision, applied
// only in apply mode and only when the policy accepts it.
func Decide(findings []store.Finding, verdicts map[string]Verification, files map[string][]byte, p VerifyPolicy) []Decision {
	decisions := make([]Decision, 0, len(findings))
	for _, f := range findings {
		if f.TriageSource == store.TriageSourceHuman {
			continue
		}
		verdict, answered := verdicts[f.ID]
		if !answered {
			verdict = Verification{Verdict: VerdictUncertain}
		}
		decisions = append(decisions, decideFinding(f, verdict, files, p))
	}
	return decisions
}

func decideFinding(f store.Finding, verification Verification, files map[string][]byte, p VerifyPolicy) Decision {
	decision := Decision{
		FindingID:    f.ID,
		Verdict:      verification.Verdict,
		Verification: verification,
		Status:       f.Status,
	}
	switch verification.Verdict {
	case VerdictConfirm:
		decision.Status = store.FindingOpen
		decision.Reason = "OCR confirmó el hallazgo"
		decision.Apply = p.Mode == store.AutoTriageApply
	case VerdictRefute:
		decision.Status = store.FindingWithdrawn
		decision.Reason = "OCR descartó el hallazgo"
		switch {
		case severityRank(f.Severity) >= severityRank("major"):
			decision.Verdict = VerdictUncertain
			decision.Status = f.Status
			decision.Reason = "no se descarta una severidad bloqueante sin humano"
		case !p.AutoWithdraw:
			decision.Verdict = VerdictUncertain
			decision.Status = f.Status
			decision.Reason = "el descarte automático está apagado"
		case !EvidenceInFile(verification, files[f.Path]):
			decision.Verdict = VerdictUncertain
			decision.Status = f.Status
			decision.Reason = "la evidencia no aparece en el archivo del head"
		default:
			decision.Apply = p.Mode == store.AutoTriageApply
		}
	default:
		decision.Verdict = VerdictUncertain
		decision.Reason = "OCR no pudo verificar el hallazgo"
	}
	return decision
}
