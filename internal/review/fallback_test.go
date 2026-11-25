package review

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func attemptsFor(t *testing.T, s *store.Store, ctx context.Context, runID uint) []store.ReviewAttempt {
	t.Helper()
	var attempts []store.ReviewAttempt
	if err := s.DB.WithContext(ctx).Where("run_id = ?", runID).Order("seq").Find(&attempts).Error; err != nil {
		t.Fatalf("load attempts of run %d: %v", runID, err)
	}
	return attempts
}

// runTestProviderFailure is an OCR result that failed for a provider reason
// while still reporting the tokens it spent.
func runTestProviderFailure(tokens int) string {
	return fmt.Sprintf(`{
  "status": "failed",
  "message": "el proveedor devolvió 429 rate limited",
  "retry_report": {"total_retries": 1, "failed_requests": 1},
  "summary": {"files_reviewed": 0, "comments": 0, "total_tokens": %d,
    "input_tokens": %d, "output_tokens": 0, "elapsed": "1s"},
  "comments": [],
  "manifest": {
    "schema_version": "ocr.run-manifest/v1", "terminal_state": "failed",
    "coverage": {
      "selected": [], "completed": [], "reused": [], "failed": [], "waived": [], "excluded": []
    }
  }
}`, tokens, tokens)
}

// reviewWithFallbackWindow runs the fixture with enough remaining time for a
// backup attempt; the default fixture timeout is shorter than the minimum.
func reviewWithFallbackWindow(t *testing.T, f *runTestFixture) error {
	t.Helper()
	runner := f.runner()
	runner.Cfg.Timeout = 5 * time.Minute
	return runner.Run(f.ctx, f.runID)
}

// A recoverable provider failure moves the run to the next authorized profile
// and records both destinations on their own attempt (#150).
func TestRunFallsBackToAuthorizedProfile(t *testing.T) {
	skipWithoutShellFake(t)
	f := newRunTestFixture(t)
	backup := f.addReviewProfile(t, "backup", store.HealthOK)
	f.setFallbackProfiles(t, backup.ID)

	failOnce := filepath.Join(f.root, "ocr-fail-once")
	t.Setenv("RUN_FAKE_OCR_FAIL_ONCE", failOnce)
	t.Setenv("RUN_FAKE_OCR_FAIL_ONCE_MESSAGE", "ocr temporal: 429 rate limited")

	if err := reviewWithFallbackWindow(t, f); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if run.EnqueuedProfileID != f.policy.LLMProfileID {
		t.Errorf("enqueued_profile_id = %d, want %d", run.EnqueuedProfileID, f.policy.LLMProfileID)
	}
	if run.ProfileID != backup.ID {
		t.Errorf("profile_id = %d, want the executing fallback %d", run.ProfileID, backup.ID)
	}

	attempts := attemptsFor(t, f.s, f.ctx, f.runID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(attempts), attempts)
	}
	if attempts[0].ProfileID != f.policy.LLMProfileID || attempts[0].Status != store.AttemptFailed {
		t.Errorf("first attempt = %+v, want the primary profile failed", attempts[0])
	}
	if attempts[1].ProfileID != backup.ID || attempts[1].Status != store.AttemptSucceeded {
		t.Errorf("second attempt = %+v, want the backup profile succeeded", attempts[1])
	}
	if attempts[0].ConfigRevision == "" || attempts[1].ConfigRevision == "" {
		t.Error("attempt config revisions are empty")
	}
	if attempts[0].Provider != "nvidia" || attempts[1].Provider != "anthropic" {
		t.Errorf("providers = %q/%q, want nvidia/anthropic", attempts[0].Provider, attempts[1].Provider)
	}
}

// A failure a different destination cannot fix must not spend the backup.
func TestRunDoesNotFallBackOnTerminalFailure(t *testing.T) {
	skipWithoutShellFake(t)
	f := newRunTestFixture(t)
	backup := f.addReviewProfile(t, "backup", store.HealthOK)
	f.setFallbackProfiles(t, backup.ID)

	failOnce := filepath.Join(f.root, "ocr-fail-once")
	t.Setenv("RUN_FAKE_OCR_FAIL_ONCE", failOnce)
	t.Setenv("RUN_FAKE_OCR_FAIL_ONCE_MESSAGE", "401 invalid api key")

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "ocr_error")
	if run.ProfileID != f.policy.LLMProfileID {
		t.Errorf("profile_id = %d, want the primary %d", run.ProfileID, f.policy.LLMProfileID)
	}
	if attempts := attemptsFor(t, f.s, f.ctx, f.runID); len(attempts) != 1 {
		t.Fatalf("attempts = %d, want no fallback attempt: %+v", len(attempts), attempts)
	}
}

// The remaining token budget bounds the retry: a failed pass that already spent
// the cap leaves no room for the backup.
func TestRunFallbackRespectsBudget(t *testing.T) {
	skipWithoutShellFake(t)
	f := newRunTestFixture(t)
	backup := f.addReviewProfile(t, "backup", store.HealthOK)
	f.setFallbackProfiles(t, backup.ID)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("team_id = ?", f.policy.TeamID).
		Update("review_token_budget", 100).Error; err != nil {
		t.Fatalf("set budget: %v", err)
	}

	once := filepath.Join(f.root, "ocr-once-output.json")
	writeRunTestFile(t, once, runTestProviderFailure(100))
	t.Setenv("RUN_FAKE_OCR_ONCE_OUTPUT", once)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunFailed, "llm_error")
	if run.TotalTokens == nil || *run.TotalTokens != 100 {
		t.Fatalf("total tokens = %v, want the 100 spent before the budget ran out", run.TotalTokens)
	}
	if got := f.ocrCalls(t); got != 1 {
		t.Fatalf("ocr invocations = %d, want no retry after the budget is gone", got)
	}
	if attempts := attemptsFor(t, f.s, f.ctx, f.runID); len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1: %+v", len(attempts), attempts)
	}
}

// A fallback after a partial pass keeps both consumptions once: the failed
// attempt keeps its own spend and the successful one only its delta.
func TestRunFallbackAfterPartialWorkDoesNotDoubleCount(t *testing.T) {
	skipWithoutShellFake(t)
	f := newRunTestFixture(t)
	backup := f.addReviewProfile(t, "backup", store.HealthOK)
	f.setFallbackProfiles(t, backup.ID)

	once := filepath.Join(f.root, "ocr-once-output.json")
	writeRunTestFile(t, once, runTestProviderFailure(100))
	t.Setenv("RUN_FAKE_OCR_ONCE_OUTPUT", once)

	if err := reviewWithFallbackWindow(t, f); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if run.TotalTokens == nil || *run.TotalTokens != 250 {
		t.Fatalf("run total tokens = %v, want 100+150", run.TotalTokens)
	}
	attempts := attemptsFor(t, f.s, f.ctx, f.runID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2: %+v", len(attempts), attempts)
	}
	if attempts[0].TotalTokens == nil || *attempts[0].TotalTokens != 100 {
		t.Errorf("failed attempt tokens = %v, want 100", attempts[0].TotalTokens)
	}
	if attempts[1].TotalTokens == nil || *attempts[1].TotalTokens != 150 {
		t.Errorf("successful attempt tokens = %v, want 150", attempts[1].TotalTokens)
	}
}

// A policy change while the run waits is visible: the enqueued profile stays on
// the run and the executed profile is recorded on the attempt (#150).
// A fallback must leave the chunk pass a correctly sized budget: the chunks get
// the policy cap minus every token already spent, not a remaining value that is
// charged twice.
func TestRunFallbackThenChunkBudgetIsNotDoubleCharged(t *testing.T) {
	skipWithoutShellFake(t)
	f := newChunkRunFixture(t)
	backup := f.addReviewProfile(t, "backup", store.HealthOK)
	f.setFallbackProfiles(t, backup.ID)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	})
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("team_id = ?", f.policy.TeamID).
		Update("review_token_budget", int64(300)).Error; err != nil {
		t.Fatalf("set budget: %v", err)
	}

	once := filepath.Join(f.root, "ocr-once-output.json")
	writeRunTestFile(t, once, runTestProviderFailure(100))
	t.Setenv("RUN_FAKE_OCR_ONCE_OUTPUT", once)

	runner := f.chunkRunner()
	runner.Cfg.Timeout = 5 * time.Minute
	if err := runner.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if got := f.ocrCalls(t); got != 4 {
		t.Fatalf("ocr calls = %d, want failed main + fallback main + two chunks", got)
	}
	if run.TotalTokens == nil || *run.TotalTokens != 300 {
		t.Fatalf("run total tokens = %v, want the 300 budget spent once", run.TotalTokens)
	}
	if attempts := attemptsFor(t, f.s, f.ctx, f.runID); len(attempts) != 2 {
		t.Fatalf("attempts = %d, want primary failed and backup succeeded", len(attempts))
	}
}

func TestRunRecordsProfileChangeWhileQueued(t *testing.T) {
	skipWithoutShellFake(t)
	f := newRunTestFixture(t)
	next := f.addReviewProfile(t, "policy-next", store.HealthOK)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("team_id = ?", f.policy.TeamID).
		Update("llm_profile_id", next.ID).Error; err != nil {
		t.Fatalf("change policy profile: %v", err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if run.EnqueuedProfileID != f.policy.LLMProfileID {
		t.Errorf("enqueued_profile_id = %d, want %d", run.EnqueuedProfileID, f.policy.LLMProfileID)
	}
	if run.ProfileID != next.ID {
		t.Errorf("profile_id = %d, want the profile that executed %d", run.ProfileID, next.ID)
	}
	attempts := attemptsFor(t, f.s, f.ctx, f.runID)
	if len(attempts) != 1 || attempts[0].ProfileID != next.ID {
		t.Fatalf("attempts = %+v, want one executed by %d", attempts, next.ID)
	}
}
