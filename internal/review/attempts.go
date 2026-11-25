package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// fallbackMinTime is the smallest part of the run budget worth starting a
// backup attempt with: below it the clone is already done but the model cannot
// produce anything before the run deadline.
const fallbackMinTime = time.Minute

// attemptConfigRevision fingerprints the effective configuration of one
// attempt: the profile revision (provider, model, endpoint and credential) and
// the policy values that shape the request. Two attempts of the same run with
// different revisions show that the policy or the profile changed in between.
func attemptConfigRevision(cfg store.ReviewConfig, profile store.LLMProfile) string {
	payload := struct {
		Profile       string
		PolicyID      uint
		PolicyUpdated string
		Effort        string
		Budget        int64
		Ceiling       *int
		Excludes      string
		Instructions  string
		MaxFiles      int
		MaxDiffLines  int
		MaxPasses     int
	}{
		Profile:       store.ProfileConfigRevision(profile),
		PolicyID:      cfg.Policy.ID,
		PolicyUpdated: cfg.Policy.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Effort:        cfg.Policy.ReviewEffort,
		Budget:        cfg.Policy.ReviewTokenBudget,
		Ceiling:       cfg.Policy.ReviewContextCeiling,
		Excludes:      string(cfg.Policy.ExcludeGlobs),
		Instructions:  cfg.Policy.ExtraInstructions,
		MaxFiles:      cfg.Policy.MaxFilesPerPass,
		MaxDiffLines:  cfg.Policy.MaxDiffLinesPerPass,
		MaxPasses:     cfg.Policy.MaxPasses,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		// The payload is a flat struct of primitives.
		panic("review: encode attempt config revision: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// reviewProfiles returns the destinations the run may use, in order: the
// policy's profile first, then the team-authorized fallbacks. A fallback is
// eligible only when it is enabled, healthy and usable by the native engine,
// and, for a private repository, explicitly allowed to receive private code.
// An unauthorized or unsuitable profile is skipped with a log line; it never
// silently becomes a destination.
func (r *Runner) reviewProfiles(ctx context.Context, cfg store.ReviewConfig) []store.LLMProfile {
	candidates := []store.LLMProfile{cfg.Profile}
	seen := map[uint]bool{cfg.Profile.ID: true}
	for _, id := range store.FallbackProfileIDs(cfg.Policy) {
		if id == 0 || seen[id] {
			continue
		}
		profile, err := r.Store.LLMProfile(ctx, id)
		if err != nil {
			slog.Warn("fallback profile unavailable", "run_profile_id", cfg.Profile.ID, "profile_id", id, "error", err)
			continue
		}
		if !profile.Enabled || profile.HealthStatus != store.HealthOK {
			slog.Info("fallback profile not eligible", "profile_id", id,
				"enabled", profile.Enabled, "health", profile.HealthStatus)
			continue
		}
		if cfg.Repo.Private && !profile.AllowPrivateData {
			slog.Info("fallback profile not authorized for private code", "profile_id", id)
			continue
		}
		if strings.TrimSpace(profile.APIKeyEnc) == "" {
			slog.Info("fallback profile has no credential", "profile_id", id)
			continue
		}
		if _, _, err := nativeEndpoint(profile); err != nil {
			slog.Info("fallback profile is not usable by OCR", "profile_id", id, "error", err)
			continue
		}
		seen[id] = true
		candidates = append(candidates, profile)
	}
	return candidates
}

// nativeRecoverableSignal matches the provider failures worth retrying on a
// backup destination: quota, transient upstream errors and transport timeouts.
var nativeRecoverableSignal = regexp.MustCompile(
	`(?i)\b(408|409|425|429|500|502|503|504|529)\b|rate[ _-]?limit|quota|resource exhausted|` +
		`too many requests|overload|temporar|unavailable|timeout|timed out|deadline exceeded|` +
		`connection (reset|refused)|no such host|broken pipe|unexpected eof|network`)

// nativeTerminalSignal matches failures a different destination cannot fix:
// bad credentials, an unknown model or a local configuration error.
var nativeTerminalSignal = regexp.MustCompile(
	`(?i)\b(400|401|403|404)\b|unauthor|invalid api key|api key|authentication|` +
		`model not found|modelo .* no existe|does not exist|unsupported|invalid (request|model|config)|` +
		`configuration|config file`)

// recoverableNativeFailure reports whether an OCR invocation failed for a
// reason a backup profile might avoid. Unknown failures are not retried: a
// fallback must not spend the remaining run budget on an unexplained error.
func recoverableNativeFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	message := err.Error()
	if nativeTerminalSignal.MatchString(message) {
		return false
	}
	return nativeRecoverableSignal.MatchString(message)
}

// recoverableNativeResult reports whether a parsed OCR result describes a
// recoverable provider failure. Configuration, budget and coverage failures are
// not: another profile cannot fix them and the budget is already gone.
func recoverableNativeResult(code string) bool {
	switch code {
	case "llm_error", "timeout":
		return true
	default:
		return false
	}
}

// remainingReviewBudget is the token budget left for another attempt from the
// main pass's total. Zero or negative total means the cap is disabled.
func remainingReviewBudget(total int64, spent tokenUsage) int64 {
	if total <= 0 {
		return -1
	}
	remaining := total - int64(spent.Total)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// canStartFallback reports whether another attempt fits the main pass's
// remaining time and token budget. total is the main budget after the
// verification reservation, so a fallback never eats the verifier's share. A
// cancelled or expired run context never starts a fallback.
func canStartFallback(ctx context.Context, total int64, spent tokenUsage) bool {
	if ctx.Err() != nil {
		return false
	}
	if remaining := remainingReviewBudget(total, spent); remaining == 0 {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < fallbackMinTime {
		return false
	}
	return true
}

// attemptFallbackBudget is the OCR token cap of one attempt. A run without a
// budget keeps OCR's own default (zero disables the flag).
func attemptFallbackBudget(total int64, spent tokenUsage) int64 {
	remaining := remainingReviewBudget(total, spent)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// attemptUsage converts one parsed OCR result into the per-attempt store shape.
// A result without a summary has unknown usage and stays NULL.
func attemptUsage(res nativeReview) (*store.RunUsage, bool) {
	if !res.hasSummary {
		return nil, false
	}
	native := nativeTokenUsage(res)
	usage := &store.RunUsage{
		Input:      int64(native.Input),
		Output:     int64(native.Output),
		Reasoning:  int64(native.Reasoning),
		CacheRead:  int64(native.Cache.Read),
		CacheWrite: int64(native.Cache.Write),
		Total:      int64(native.Total),
	}
	if native.MaxContext > 0 {
		maxContext := int64(native.MaxContext)
		usage.MaxContext = &maxContext
	}
	return usage, true
}

// tokenDelta is the consumption one attempt added on top of a previous
// snapshot, used to record the successful attempt without copying the run's
// cumulative spend into it.
func tokenDelta(total, base tokenUsage) tokenUsage {
	delta := total
	delta.Input -= base.Input
	delta.Output -= base.Output
	delta.Reasoning -= base.Reasoning
	delta.Cache.Read -= base.Cache.Read
	delta.Cache.Write -= base.Cache.Write
	delta.Total -= base.Total
	delta.Steps -= base.Steps
	return delta
}

func runUsageFromTokens(t tokenUsage, known bool) (*store.RunUsage, bool) {
	if !known {
		return nil, false
	}
	usage := &store.RunUsage{
		Input:      int64(t.Input),
		Output:     int64(t.Output),
		Reasoning:  int64(t.Reasoning),
		CacheRead:  int64(t.Cache.Read),
		CacheWrite: int64(t.Cache.Write),
		Total:      int64(t.Total),
	}
	if t.MaxContext > 0 {
		maxContext := int64(t.MaxContext)
		usage.MaxContext = &maxContext
	}
	return usage, true
}
