package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"gorm.io/datatypes"
)

const (
	// analyzerVersion fingerprints the review engine. A different version may
	// produce different findings for the same diff, so its units are not
	// reusable. Bump it with the OCR pin in deploy/Dockerfile.
	analyzerVersion = "ocr-1.14.1"

	// unitWriteTimeout bounds the detached write of a completed unit. The job
	// context may already be cancelled when the unit is persisted, but losing
	// the result would pay for it twice on the next attempt.
	unitWriteTimeout = 5 * time.Second
)

// unitScope is the identity a persisted result must match to be reused: the
// comparison, the policy and profile that shaped the review, and the analyzer
// version. Any change invalidates the affected units.
type unitScope struct {
	BaseRef  string `json:"base_ref"`
	FromSHA  string `json:"from_sha"`
	HeadSHA  string `json:"head_sha"`
	Policy   string `json:"policy"`
	Profile  string `json:"profile"`
	Analyzer string `json:"analyzer"`
	// Context fingerprints the prompt context (description, criteria, linked
	// issues and repository excerpts) built once per run (#139, #149).
	Context string `json:"context,omitempty"`
}

type policyIdentity struct {
	Effort            string `json:"effort"`
	ContextCeiling    int    `json:"context_ceiling"`
	TokenBudget       int64  `json:"token_budget"`
	BlockingThreshold string `json:"blocking_threshold"`
	ExcludeGlobs      string `json:"exclude_globs"`
	ExtraInstructions string `json:"extra_instructions"`
}

type profileIdentity struct {
	ID       uint    `json:"id"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Variant  *string `json:"variant,omitempty"`
	Mode     string  `json:"mode"`
	BaseURL  *string `json:"base_url,omitempty"`
}

// newUnitScope fingerprints the inputs that decide whether a stored unit still
// describes the review that is about to run.
func newUnitScope(cfg store.ReviewConfig, fromSHA, headSHA, effort string, ceiling int, budget int64) unitScope {
	return newUnitScopeFor(cfg, cfg.Profile, fromSHA, headSHA, effort, ceiling, budget)
}

// newUnitScopeFor is newUnitScope for the profile that actually executes the
// unit: the primary or an authorized fallback (#150).
func newUnitScopeFor(cfg store.ReviewConfig, profile store.LLMProfile, fromSHA, headSHA, effort string, ceiling int, budget int64) unitScope {
	policy, _ := json.Marshal(policyIdentity{
		Effort:            effort,
		ContextCeiling:    ceiling,
		TokenBudget:       budget,
		BlockingThreshold: cfg.Policy.BlockingThreshold,
		ExcludeGlobs:      string(cfg.Policy.ExcludeGlobs),
		ExtraInstructions: cfg.Policy.ExtraInstructions,
	})
	profileID, _ := json.Marshal(profileIdentity{
		ID:       profile.ID,
		Provider: profile.Provider,
		Model:    profile.Model,
		Variant:  profile.Variant,
		Mode:     profile.Mode,
		BaseURL:  profile.BaseURL,
	})
	return unitScope{
		BaseRef:  cfg.PR.BaseRef,
		FromSHA:  fromSHA,
		HeadSHA:  headSHA,
		Policy:   hex.EncodeToString(policy),
		Profile:  hex.EncodeToString(profileID),
		Analyzer: analyzerVersion,
	}
}

// key is the stable fingerprint persisted alongside an OCR session.
func (s unitScope) key() string {
	raw, _ := json.Marshal(s)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// reviewContextRevision fingerprints the prompt context so a unit produced
// under different PR/issue/repository context is not reused.
func reviewContextRevision(background string) string {
	sum := sha256.Sum256([]byte(background))
	return hex.EncodeToString(sum[:])
}

// unitKey identifies one reusable unit inside the scope. Chunk units also carry
// the synthetic commit, which is deterministic for a given diff, so a changed
// hunk gets a different key.
func (s unitScope) unitKey(kind, path, diffFingerprint string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		s.key(), kind, normalizeCoveragePath(path), diffFingerprint,
	}, "\x00")))
	return kind + ":" + hex.EncodeToString(sum[:])
}

// runResume is the per-attempt view of the state persisted by earlier attempts.
type runResume struct {
	units map[string]store.ReviewUnit
	// spent is every token recorded by this run across attempts, including
	// units whose scope changed and will not be reused. The aggregate spend
	// limit must not forget what a failed attempt already cost (#149).
	spent int64
	// usageKnown is false when a persisted unit cannot prove its consumption.
	// Without that evidence no budget is assumed left, matching the chunk rule.
	usageKnown bool
}

func newRunResume(units []store.ReviewUnit) *runResume {
	r := &runResume{units: make(map[string]store.ReviewUnit, len(units)), usageKnown: true}
	for _, unit := range units {
		r.units[unit.UnitKey] = unit
		known := false
		if res, err := parseNativeReview(unit.Result); err == nil {
			known = res.hasSummary
		}
		if !known {
			r.usageKnown = false
		}
		if len(unit.Usage) > 0 {
			var usage tokenUsage
			if err := json.Unmarshal(unit.Usage, &usage); err == nil {
				r.spent += int64(usage.Total)
			}
		}
	}
	return r
}

// lookup returns the completed unit for key, if one is reusable.
func (r *runResume) lookup(key string) (store.ReviewUnit, bool) {
	if r == nil {
		return store.ReviewUnit{}, false
	}
	unit, ok := r.units[key]
	if !ok || unit.Status != store.ReviewUnitCompleted {
		return store.ReviewUnit{}, false
	}
	return unit, true
}

// markSpent accounts for a unit executed in this attempt. Reused units are
// already counted when the resume state is loaded and must not be added again.
func (r *runResume) markSpent(usage tokenUsage) {
	if r != nil {
		r.spent += int64(usage.Total)
	}
}

// adopt keeps the aggregate spend at least the run's cumulative total, so
// attempts that failed without producing a unit still bound the next spend.
func (r *runResume) adopt(total tokenUsage) {
	if r != nil && int64(total.Total) > r.spent {
		r.spent = int64(total.Total)
	}
}

// markUsage records whether this attempt's consumption is known.
func (r *runResume) markUsage(known bool) {
	if r != nil && !known {
		r.usageKnown = false
	}
}

// loadResume reads the units of previous attempts. A read failure leaves the
// run without reuse instead of failing the review.
func (r *Runner) loadResume(ctx context.Context, runID uint) *runResume {
	units, err := r.Store.ReviewUnitsForRun(ctx, runID)
	if err != nil {
		slog.Warn("review resume state unavailable; running every unit",
			"run_id", runID, "error", err)
		return newRunResume(nil)
	}
	return newRunResume(units)
}

// persistUnit stores one completed unit detached from the pipeline context: an
// interruption right after a unit finished must not throw its result away.
func (r *Runner) persistUnit(ctx context.Context, unit store.ReviewUnit) bool {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unitWriteTimeout)
	defer cancel()
	if err := r.Store.UpsertReviewUnit(writeCtx, unit); err != nil {
		slog.Warn("save review unit", "run_id", unit.RunID, "unit_key", unit.UnitKey, "error", err)
		return false
	}
	return true
}

// saveSession records OCR's session id for the main pass. It is best effort: a
// lost id only means the next attempt reruns the main unit. Only the current
// lease owner may write it.
func (r *Runner) saveSession(ctx context.Context, runID uint, owner, sessionID, scope string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unitWriteTimeout)
	defer cancel()
	if err := r.Store.SetReviewRunSession(writeCtx, runID, owner, sessionID, scope); err != nil &&
		!errors.Is(err, store.ErrRunNotActive) && !errors.Is(err, store.ErrRunLeaseLost) {
		slog.Warn("save OCR session", "run_id", runID, "error", err)
	}
}

// clearSession forgets the stored OCR session before retrying without it.
func (r *Runner) clearSession(ctx context.Context, runID uint, owner string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unitWriteTimeout)
	defer cancel()
	if err := r.Store.SetReviewRunSession(writeCtx, runID, owner, "", ""); err != nil &&
		!errors.Is(err, store.ErrRunNotActive) && !errors.Is(err, store.ErrRunLeaseLost) {
		slog.Warn("clear OCR session", "run_id", runID, "error", err)
	}
}

// resumeSession returns the OCR session id of a previous attempt when it is
// still valid for the current scope.
func resumeSession(run store.ReviewRun, scope unitScope) string {
	if run.OCRSessionID == "" || run.OCRSessionScope != scope.key() {
		return ""
	}
	return run.OCRSessionID
}

// runStateDir is the per-run durable directory. It survives requeues (and so
// the OCR sessions inside it) and is removed when the run reaches a terminal
// state. A lost-worker run leaves it behind because no worker runs that
// cleanup; reclaiming those directories is out of scope here.
func runStateDir(workspaceDir string, runID uint) string {
	return filepath.Join(workspaceDir, fmt.Sprintf("run-%d-state", runID))
}

func (r *Runner) removeRunState(runID uint) {
	if r.Cfg.WorkspaceDir == "" {
		return
	}
	if err := os.RemoveAll(runStateDir(r.Cfg.WorkspaceDir, runID)); err != nil {
		slog.Warn("remove review run state", "run_id", runID, "error", err)
	}
}

// reusableReview parses a persisted unit and returns its result only when it
// can stand in for a fresh OCR invocation.
func (r *runResume) reusableReview(key string) (nativeReview, bool) {
	unit, ok := r.lookup(key)
	if !ok || len(unit.Result) == 0 {
		return nativeReview{}, false
	}
	res, err := parseNativeReview(unit.Result)
	if err != nil {
		slog.Warn("ignoring unreadable review unit", "unit_key", key, "error", err)
		return nativeReview{}, false
	}
	status, _ := nativeOutcome(res)
	if status != store.RunSucceeded {
		return nativeReview{}, false
	}
	return res, true
}

// completedMainUnit builds the row persisted for a finished main pass.
func completedMainUnit(runID uint, key string, res nativeReview, raw []byte) store.ReviewUnit {
	usage := nativeTokenUsage(res)
	return store.ReviewUnit{
		RunID:   runID,
		UnitKey: key,
		Kind:    "main",
		Status:  store.ReviewUnitCompleted,
		Result:  datatypes.JSON(raw),
		Usage:   datatypes.JSON(usage.json()),
	}
}

// completedChunkUnit builds the row persisted for a finished chunk.
func completedChunkUnit(runID uint, key, path string, res nativeReview, raw []byte) store.ReviewUnit {
	usage := nativeTokenUsage(res)
	return store.ReviewUnit{
		RunID:   runID,
		UnitKey: key,
		Kind:    "chunk",
		Path:    path,
		Status:  store.ReviewUnitCompleted,
		Result:  datatypes.JSON(raw),
		Usage:   datatypes.JSON(usage.json()),
	}
}
