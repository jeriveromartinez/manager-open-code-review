package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// verifyMinBudget is the smallest verification worth starting. Below it the
	// pass is skipped and the findings stay untriaged (fail-open to the human).
	verifyMinBudget    = store.VerifyTokenMaxMin
	verifyMinTimeout   = time.Minute
	maxVerifyFileBytes = 4 << 20
)

// verifyPolicy resolves the team policy into the verifier's limits, filling the
// defaults for rows created before the columns existed.
func verifyPolicy(p store.QAPolicy) VerifyPolicy {
	policy := VerifyPolicy{
		Mode:         p.AutoTriageMode,
		AutoWithdraw: p.AutoWithdraw,
		MaxFindings:  p.VerifyMaxFindings,
		MaxFiles:     p.VerifyMaxFiles,
		TokenShare:   p.VerifyTokenShare,
		TimeShare:    p.VerifyTimeShare,
		TokenMax:     p.VerifyTokenMax,
	}
	if policy.Mode == "" {
		policy.Mode = store.AutoTriageOff
	}
	if policy.MaxFindings <= 0 {
		policy.MaxFindings = store.DefaultVerifyMaxFindings
	}
	if policy.MaxFiles <= 0 {
		policy.MaxFiles = store.DefaultVerifyMaxFiles
	}
	if policy.TokenShare <= 0 {
		policy.TokenShare = store.DefaultVerifyTokenShare
	}
	if policy.TokenShare >= 100 {
		// 100% would leave the main pass with no budget, which OCR reads as
		// "uncapped" and the verifier could then overspend the run.
		policy.TokenShare = 99
	}
	if policy.TimeShare <= 0 {
		policy.TimeShare = store.DefaultVerifyTimeShare
	}
	if policy.TimeShare >= 100 {
		// Likewise, a 100% share would leave the main pass without a timeout.
		policy.TimeShare = 99
	}
	if policy.TokenMax <= 0 {
		policy.TokenMax = store.DefaultVerifyTokenMax
	}
	return policy
}

// verifyReservation is the slice of the run's configured limits held back for
// the second pass before the first one starts, so the aggregate of the main
// pass, its chunks and the verifier cannot exceed them. A share too small to
// start a verification stays with the main pass under mainTokens/mainTime.
type verifyReservation struct {
	enabled    bool
	mainTokens int64
	mainTime   time.Duration
	tokens     int64
	timeout    time.Duration
}

// reserveVerification splits the run limits when auto-triage may need a second
// pass. The main pass, and the chunked retries that belong to it, may spend
// mainTokens; the verifier gets what was held back. A disabled run token budget
// leaves the main pass uncapped but still holds VerifyTokenMax for the verifier.
func reserveVerification(p store.QAPolicy, policy VerifyPolicy, timeout time.Duration) verifyReservation {
	if policy.Mode == store.AutoTriageOff {
		return verifyReservation{}
	}
	mainTokens, tokens := verifyAllowance(p, policy)
	reserved := verifyReservation{
		enabled:    true,
		mainTokens: mainTokens,
		mainTime:   timeout,
		tokens:     tokens,
	}
	if timeout > 0 {
		reserved.timeout = timeout * time.Duration(policy.TimeShare) / 100
	}
	// A share that cannot start the second pass is dead weight: holding it
	// back would only weaken the main pass, so both quotas stay available to
	// it. The verifier still reports insufficient_budget or insufficient_time.
	if !reservationViable(reserved.tokens, reserved.timeout) {
		if p.ReviewTokenBudget > 0 {
			reserved.mainTokens = p.ReviewTokenBudget
		}
		reserved.mainTime = timeout
	} else if timeout > 0 {
		reserved.mainTime = timeout - reserved.timeout
	}
	return reserved
}

// reservationViable reports whether the held-back quotas can actually start a
// verification: the token share must reach the minimum budget and the time
// share the minimum timeout (or there must be no deadline to cut).
func reservationViable(tokens int64, timeout time.Duration) bool {
	return tokens >= verifyMinBudget && (timeout <= 0 || timeout >= verifyMinTimeout)
}

// verifyAllowance returns the token cap the main pass may use and the share held
// back for verification. Without a run budget the main pass stays uncapped and
// verification keeps the fixed cap, the documented behaviour for teams that
// disabled the aggregate budget.
func verifyAllowance(p store.QAPolicy, policy VerifyPolicy) (mainTokens, verifyTokens int64) {
	if p.ReviewTokenBudget <= 0 {
		return 0, policy.TokenMax
	}
	verifyTokens = p.ReviewTokenBudget * int64(policy.TokenShare) / 100
	if verifyTokens > p.ReviewTokenBudget {
		verifyTokens = p.ReviewTokenBudget
	}
	mainTokens = p.ReviewTokenBudget - verifyTokens
	return mainTokens, verifyTokens
}

// verifyBudgetFrom returns what the verifier may spend: the share reserved
// before the first pass, shrunk only when a known main usage already consumed
// the configured cap. Unknown usage never expands it, so the aggregate cannot
// exceed the budget.
func verifyBudgetFrom(totalBudget, reserved, used int64, usageKnown bool) int64 {
	if totalBudget <= 0 || !usageKnown {
		return reserved
	}
	if remaining := totalBudget - used; remaining < reserved {
		reserved = remaining
	}
	if reserved < 0 {
		return 0
	}
	return reserved
}

// verificationSkip names why the second pass cannot start. A zero timeout means
// the run has no configured deadline, which is not a reason to skip.
func verificationSkip(timeout time.Duration, budget int64) string {
	switch {
	case timeout > 0 && timeout < verifyMinTimeout:
		return "insufficient_time"
	case budget < verifyMinBudget:
		return "insufficient_budget"
	}
	return ""
}

// unverifiedReason explains on the finding itself why the verifier could not
// check it, so the panel does not show an empty answer.
func unverifiedReason(code string) string {
	switch code {
	case "insufficient_time":
		return "verificación omitida: tiempo insuficiente"
	case "insufficient_budget":
		return "verificación omitida: presupuesto insuficiente"
	default:
		return "verificación omitida: " + code
	}
}

// markUnverified records the skip reason on every finding the second pass did
// not check. Their status is left untouched: only the evidence is missing.
func markUnverified(verdicts map[string]Verification, findings []store.Finding, headSHA, reason string) {
	for _, f := range findings {
		if _, seen := verdicts[f.ID]; seen {
			continue
		}
		verdicts[f.ID] = Verification{Verdict: VerdictUncertain, HeadSHA: headSHA, Reason: reason}
	}
}

// verifyContext gives the verifier its own deadline so the OCR flags are not
// the only bound on the process. A zero timeout leaves the parent unchanged.
func verifyContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// verifyFindings runs the second OCR pass over the run's findings and fills
// out.decisions. It never fails the run: a verifier error or an exhausted
// budget leaves the findings untriaged.
func (r *Runner) verifyFindings(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig, apiKey []byte, workDir, headSHA string, out *runOutcome, reservation verifyReservation, log *runLogSink) {
	policy := verifyPolicy(cfg.Policy)
	if policy.Mode == store.AutoTriageOff || len(out.findings) == 0 {
		return
	}

	openByID := make(map[string]store.Finding, len(out.open))
	for _, f := range out.open {
		openByID[f.ID] = f
	}
	verdicts := make(map[string]Verification, len(out.findings))
	pending := make([]store.Finding, 0, len(out.findings))
	for _, f := range out.findings {
		if reused, ok := reusedVerdict(openByID[f.ID], f, headSHA); ok {
			verdicts[f.ID] = reused
			continue
		}
		pending = append(pending, f)
	}

	selected, paths := selectVerifyFindings(pending, policy)
	attempted := false
	if len(paths) > 0 {
		// The verifier spends the share reserved before the main pass, not a
		// fresh estimate: that is what keeps the aggregate inside the budget.
		budget := verifyBudgetFrom(cfg.Policy.ReviewTokenBudget, reservation.tokens, int64(out.tokens.Total), out.tokensKnown)
		timeout := reservation.timeout
		if deadline, ok := ctx.Deadline(); ok {
			if left := time.Until(deadline); left < timeout {
				timeout = left
			}
		}
		if code := verificationSkip(timeout, budget); code != "" {
			out.verificationSkipped = code
			markUnverified(verdicts, pending, headSHA, unverifiedReason(code))
			log.Log(ctx, "worker", "", "warn", logPhaseOCR, fmt.Sprintf(
				"verificación omitida (%s); %d hallazgo(s) conservan su estado", code, len(pending)))
		} else {
			r.setPhase(ctx, run.ID, store.RunPhaseVerifying, log)
			log.Log(ctx, "worker", "verify", "info", logPhaseOCR, fmt.Sprintf(
				"verificando %d hallazgo(s) en %d archivo(s)", len(selected), len(paths)))
			verifyCtx, cancel := verifyContext(ctx, timeout)
			// The OCR flags are not a process deadline: the verifier gets its own
			// context so a stalled scan cannot outlive the reserved time.
			defer cancel()
			verifier := &NativeVerifier{
				Bin:         r.Cfg.OCRBin,
				RepoDir:     workDir,
				Home:        filepath.Join(workDir, "home-ocr-verify"),
				Profile:     cfg.Profile,
				APIKey:      string(apiKey),
				Paths:       paths,
				Background:  buildVerifyContext(selected),
				MaxTokens:   r.Cfg.ContextCeiling,
				TokenBudget: budget,
				Timeout:     timeout,
				OnLog:       log.OCRLogger(ctx, "verify"),
			}
			if verifier.MaxTokens <= 0 {
				verifier.MaxTokens = reviewContextCeiling
			}
			started := time.Now()
			res, _, err := r.runNativeReview(verifyCtx, cfg.Profile.ID, verifier, log, "verify")
			if err != nil {
				out.verificationSkipped = "verifier_error"
				log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
					fmt.Sprintf("la verificación falló; los hallazgos quedan sin triage: %v", err))
			} else {
				attempted = true
				now := time.Now().UTC()
				for id, v := range ParseVerdicts(res.Comments) {
					if _, seen := verdicts[id]; seen {
						continue
					}
					v.HeadSHA = headSHA
					v.Model = cfg.Profile.Model
					v.SessionID = res.SessionID
					v.VerifiedAt = now
					verdicts[id] = v
				}
				out.verifySessionID = res.SessionID
				if res.hasSummary {
					usage := nativeTokenUsage(res)
					out.verifyUsage = usage.json()
					out.tokens.add(usage)
					out.tokensKnown = true
				}
				log.Log(ctx, "worker", "verify", "info", logPhaseOCR, fmt.Sprintf(
					"verificación terminada en %s: %d veredicto(s)",
					time.Since(started).Round(time.Second), len(verdicts)))
			}
		}
	}

	// A verifier that ran and answered nothing still leaves an explicit
	// uncertainty per finding, so stale evidence is not reused. A pass that
	// never ran keeps whatever the previous run stored, except for a skipped
	// one, which already recorded the reason on every pending finding.
	if len(verdicts) == 0 && !attempted {
		return
	}
	files := readCitedFiles(workDir, out.findings)
	out.decisions = Decide(out.findings, verdicts, files, headSHA, policy)
}

// reusedVerdict returns the stored verdict when it still refers to the same
// head and the same cited code.
func reusedVerdict(prev, current store.Finding, headSHA string) (Verification, bool) {
	if len(prev.Verification) == 0 {
		return Verification{}, false
	}
	var v Verification
	if err := json.Unmarshal(prev.Verification, &v); err != nil || v.Verdict == "" {
		return Verification{}, false
	}
	if v.HeadSHA != headSHA || prev.ExistingCode != current.ExistingCode {
		return Verification{}, false
	}
	return v, true
}

// selectVerifyFindings orders the findings minor-first, then nit, then the rest,
// and trims them to the policy's file and finding caps. It returns the selected
// findings and the unique paths in the same order.
func selectVerifyFindings(findings []store.Finding, p VerifyPolicy) ([]store.Finding, []string) {
	ordered := append([]store.Finding(nil), findings...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if a, b := verifyPriority(ordered[i].Severity), verifyPriority(ordered[j].Severity); a != b {
			return a < b
		}
		if ordered[i].Path != ordered[j].Path {
			return ordered[i].Path < ordered[j].Path
		}
		return ordered[i].StartLine < ordered[j].StartLine
	})

	seenPath := make(map[string]bool, p.MaxFiles)
	paths := make([]string, 0, p.MaxFiles)
	selected := make([]store.Finding, 0, p.MaxFindings)
	for _, f := range ordered {
		if len(selected) >= p.MaxFindings {
			break
		}
		if !seenPath[f.Path] {
			if len(paths) >= p.MaxFiles {
				continue
			}
			seenPath[f.Path] = true
			paths = append(paths, f.Path)
		}
		selected = append(selected, f)
	}
	return selected, paths
}

func verifyPriority(severity string) int {
	switch severity {
	case "minor":
		return 0
	case "nit":
		return 1
	default:
		return 2
	}
}

// readCitedFiles loads the files the findings point at. An invalid or oversized
// path is skipped, which leaves the evidence unverifiable and the verdict
// uncertain instead of trusting it.
func readCitedFiles(workDir string, findings []store.Finding) map[string][]byte {
	files := make(map[string][]byte, len(findings))
	for _, f := range findings {
		if _, done := files[f.Path]; done {
			continue
		}
		content, err := readRepoFile(workDir, f.Path)
		if err != nil {
			continue
		}
		files[f.Path] = content
	}
	return files
}

func readRepoFile(workDir, path string) ([]byte, error) {
	if path == "" || filepath.IsAbs(path) {
		return nil, fmt.Errorf("invalid finding path %q", path)
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("finding path escapes the checkout: %q", path)
	}
	full := filepath.Join(workDir, clean)
	rel, err := filepath.Rel(workDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("finding path escapes the checkout: %q", path)
	}
	// The PR can commit a symlink, so the lexical check is not enough: resolve
	// the real path and require it to stay inside the checkout too.
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, err
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("finding path escapes the checkout: %q", path)
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("finding path is a directory: %q", path)
	}
	if info.Size() > maxVerifyFileBytes {
		return nil, fmt.Errorf("finding file is too large: %q", path)
	}
	return os.ReadFile(real)
}
