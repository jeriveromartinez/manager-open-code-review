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
	if policy.TimeShare <= 0 {
		policy.TimeShare = store.DefaultVerifyTimeShare
	}
	if policy.TokenMax <= 0 {
		policy.TokenMax = store.DefaultVerifyTokenMax
	}
	return policy
}

// verifyFindings runs the second OCR pass over the run's findings and fills
// out.decisions. It never fails the run: a verifier error or an exhausted
// budget leaves the findings untriaged.
func (r *Runner) verifyFindings(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig, apiKey []byte, workDir, headSHA string, out *runOutcome, log *runLogSink) {
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
	if len(paths) > 0 {
		timeout, budget := verifyBudget(ctx, cfg.Policy, policy, int64(out.tokens.Total))
		switch {
		case timeout > 0 && timeout < verifyMinTimeout:
			out.verificationSkipped = "insufficient_time"
		case budget < verifyMinBudget:
			out.verificationSkipped = "insufficient_budget"
		default:
			r.setPhase(ctx, run.ID, store.RunPhaseVerifying, log)
			log.Log(ctx, "worker", "verify", "info", logPhaseOCR, fmt.Sprintf(
				"verificando %d hallazgo(s) en %d archivo(s)", len(selected), len(paths)))
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
			res, _, err := r.runNativeReview(ctx, cfg.Profile.ID, verifier, log, "verify")
			if err != nil {
				out.verificationSkipped = "verifier_error"
				log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
					fmt.Sprintf("la verificación falló; los hallazgos quedan sin triage: %v", err))
			} else {
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

	if len(verdicts) == 0 {
		return
	}
	if out.verificationSkipped != "" {
		log.Log(ctx, "worker", "", "warn", logPhaseOCR,
			"verificación omitida ("+out.verificationSkipped+"); se reutilizan los veredictos guardados")
	}
	files := readCitedFiles(workDir, out.findings)
	out.decisions = Decide(out.findings, verdicts, files, policy)
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

// verifyBudget slices the run's remaining time and token budget for the second
// pass. The share is a reservation, never extra: it cannot exceed what the main
// pass left of the configured cap. Without a run budget it uses the fixed cap,
// so teams that disabled the budget still get verification.
func verifyBudget(ctx context.Context, p store.QAPolicy, policy VerifyPolicy, used int64) (time.Duration, int64) {
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline) * time.Duration(policy.TimeShare) / 100
	}
	if p.ReviewTokenBudget == 0 {
		return timeout, policy.TokenMax
	}
	budget := p.ReviewTokenBudget * int64(policy.TokenShare) / 100
	if remaining := p.ReviewTokenBudget - used; remaining < budget {
		if remaining < 0 {
			remaining = 0
		}
		budget = remaining
	}
	return timeout, budget
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
