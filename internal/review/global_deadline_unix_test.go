//go:build unix

package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// deadlinePartialReport is what the fake OCR writes when the worker asks it to
// close: a failed run whose manifest still describes the work it finished.
const deadlinePartialReport = `{
  "status": "failed",
  "message": "Review failed: deadline reached",
  "summary": {"files_reviewed": 1, "comments": 0, "total_tokens": 21,
    "input_tokens": 14, "output_tokens": 7},
  "manifest": {
    "schema_version": "ocr.run-manifest/v1",
    "terminal_state": "failed",
    "coverage": {
      "selected": [{"path": "a.go"}, {"path": "b.go"}],
      "completed": [{"path": "a.go"}],
      "reused": [],
      "failed": [{"path": "b.go", "classification": "timeout", "reason": "task deadline reached"}],
      "waived": [],
      "excluded": []
    }
  }
}`

func deadlineReviewer(t *testing.T, bin string, timeout, margin time.Duration) *NativeReviewer {
	t.Helper()
	base := "https://provider.example/v1"
	return &NativeReviewer{
		Bin:         bin,
		RepoDir:     t.TempDir(),
		Home:        filepath.Join(t.TempDir(), "home"),
		Profile:     store.LLMProfile{Provider: "custom", Model: "model-x", BaseURL: &base},
		BaseRef:     "base-sha",
		HeadRef:     "head-sha",
		MaxTokens:   reviewContextCeiling,
		Effort:      store.ReviewEffortLow,
		Timeout:     timeout,
		CloseMargin: margin,
	}
}

func writeDeadlineOCR(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ocr.sh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ocr: %v", err)
	}
	return bin
}

// A slow group must be asked to close before the deadline and still hand back
// its partial report.
func TestReviewGlobalDeadlineKeepsPartialReport(t *testing.T) {
	output := filepath.Join(t.TempDir(), "partial.json")
	if err := os.WriteFile(output, []byte(deadlinePartialReport), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	bin := writeDeadlineOCR(t, fmt.Sprintf(`#!/bin/sh
trap 'cat %q; exit 1' INT
while :; do sleep 0.2; done
`, output))

	reviewer := deadlineReviewer(t, bin, 2*time.Second, 400*time.Millisecond)
	started := time.Now()
	res, _, err := reviewer.Review(context.Background())
	if err != nil {
		t.Fatalf("Review() error = %v, want the partial report", err)
	}
	elapsed := time.Since(started)
	if elapsed > 3*time.Second {
		t.Errorf("Review() took %s, want it closed before the deadline", elapsed)
	}
	if res.closeReason != "global" {
		t.Errorf("closeReason = %q, want global", res.closeReason)
	}
	if res.hardStopped {
		t.Error("hardStopped = true, want the cooperative close")
	}
	if res.closeElapsed <= 0 {
		t.Errorf("closeElapsed = %s, want the time between signal and exit", res.closeElapsed)
	}
	status, code := nativeOutcome(res)
	if status != store.RunInconclusive || code != "timeout_global" {
		t.Errorf("outcome = %q/%q, want inconclusive/timeout_global", status, code)
	}
	if reviewed, skipped, _ := nativeCoverageCounts(res); reviewed != 1 || skipped != 1 {
		t.Errorf("coverage = reviewed %d, skipped %d, want 1/1", reviewed, skipped)
	}
	if usage := nativeTokenUsage(res); usage.Total != 21 {
		t.Errorf("tokens = %d, want the 21 OCR reported", usage.Total)
	}
}

// A process that ignores the signal must be killed at the deadline, not left
// running past it.
func TestReviewGlobalDeadlineHardStopsUncooperativeProcess(t *testing.T) {
	bin := writeDeadlineOCR(t, `#!/bin/sh
trap '' INT
while :; do sleep 0.2; done
`)
	reviewer := deadlineReviewer(t, bin, 1200*time.Millisecond, 250*time.Millisecond)

	started := time.Now()
	_, _, err := reviewer.Review(context.Background())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Review() error = nil, want the hard stop")
	}
	var timeout *nativeTimeoutError
	if !errors.As(err, &timeout) || timeout.Kind != "global" {
		t.Fatalf("Review() error = %v, want a global timeout", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Review() took %s, want the process tree stopped at the deadline", elapsed)
	}
}

// analysisDeadlineReport is what the fake OCR writes when the analysis share,
// not the run, is what closes the invocation: the review is complete and the
// analysis section is present but no longer trustworthy.
const analysisDeadlineReport = `{
  "status": "complete",
  "summary": {"files_reviewed": 1, "comments": 0, "total_tokens": 12,
    "input_tokens": 8, "output_tokens": 4},
  "manifest": {
    "terminal_state": "complete",
    "coverage": {"completed": [{"path": "a.go"}]}
  },
  "review_status": "complete",
  "analysis_raw": "## Resumen\n- ok",
  "analysis_meta": {
    "schema_version": "ocr.review-analysis/v2",
    "execution_status": "completed"
  }
}`

// The analysis has its own bound. When it fires, the process is closed early
// and the analysis is inconclusive, but the review outcome the report carries
// stays untouched.
func TestReviewAnalysisDeadlineKeepsReviewOutcome(t *testing.T) {
	output := filepath.Join(t.TempDir(), "partial.json")
	if err := os.WriteFile(output, []byte(analysisDeadlineReport), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	bin := writeDeadlineOCR(t, fmt.Sprintf(`#!/bin/sh
trap 'cat %q; exit 1' INT
while :; do sleep 0.2; done
`, output))

	reviewer := deadlineReviewer(t, bin, 10*time.Second, 150*time.Millisecond)
	reviewer.Analysis = true
	reviewer.AnalysisTimeout = 400 * time.Millisecond

	started := time.Now()
	res, _, err := reviewer.Review(context.Background())
	if err != nil {
		t.Fatalf("Review() error = %v, want the partial report", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Review() took %s, want it closed by the analysis share", elapsed)
	}
	if !res.analysisTimedOut {
		t.Error("analysisTimedOut = false, want the analysis share to be recorded")
	}
	if res.closeReason != "analysis" || res.hardStopped {
		t.Errorf("closeReason/hardStopped = %q/%v, want the analysis close kind", res.closeReason, res.hardStopped)
	}
	if status, code := nativeOutcome(res); status != store.RunSucceeded || code != "" {
		t.Errorf("review outcome = %s/%q, want succeeded", status, code)
	}

	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
		store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
	verdict = applyNativeArchitectureAnalysis(verdict, res)
	verdict = markArchitectureTimedOut(verdict, "analysis deadline exceeded")
	if verdict.Status != store.ArchitectureInconclusive {
		t.Errorf("analysis status = %q, want inconclusive", verdict.Status)
	}
}

// A share shorter than OCR's default 20s close margin must still be closed
// gracefully; otherwise the hard stop arrives first and the review report is
// lost with the analysis it was carrying.
func TestReviewAnalysisDeadlineClosesGracefullyWhenShareIsShort(t *testing.T) {
	output := filepath.Join(t.TempDir(), "partial.json")
	if err := os.WriteFile(output, []byte(analysisDeadlineReport), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	bin := writeDeadlineOCR(t, fmt.Sprintf(`#!/bin/sh
trap 'cat %q; exit 1' INT
while :; do sleep 0.2; done
`, output))

	// Zero keeps the production margin of 20s, which is longer than the share.
	reviewer := deadlineReviewer(t, bin, 10*time.Second, 0)
	reviewer.Analysis = true
	reviewer.AnalysisTimeout = 400 * time.Millisecond

	res, _, err := reviewer.Review(context.Background())
	if err != nil {
		t.Fatalf("Review() error = %v, want the gracefully closed report", err)
	}
	if !res.analysisTimedOut {
		t.Error("analysisTimedOut = false, want the analysis share to be recorded")
	}
	if res.hardStopped {
		t.Error("hardStopped = true, want OCR to receive the close signal first")
	}
}

// A process that ignores the analysis close and writes nothing must end as an
// analysis timeout, never as the run's global timeout: the phase ran after the
// review report was persisted.
func TestReviewAnalysisPhaseWithoutReportIsAnalysisTimeout(t *testing.T) {
	bin := writeDeadlineOCR(t, `#!/bin/sh
trap '' INT
while :; do sleep 0.2; done
`)
	reviewer := deadlineReviewer(t, bin, 10*time.Second, 100*time.Millisecond)
	reviewer.Analysis = true
	reviewer.AnalysisTimeout = 350 * time.Millisecond

	started := time.Now()
	_, _, err := reviewer.Review(context.Background())
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Review() took %s, want the process stopped at the analysis share", elapsed)
	}
	var timeout *nativeTimeoutError
	if !errors.As(err, &timeout) || timeout.Kind != "analysis" {
		t.Fatalf("Review() error = %v, want an analysis timeout", err)
	}
	out, _ := classifyNativeError(context.Background(), runOutcome{}, err)
	if out.status != store.RunInconclusive || out.code != "timeout_analysis" {
		t.Errorf("outcome = %q/%q, want inconclusive/timeout_analysis", out.status, out.code)
	}
}

// A run that ends on its own keeps its result and reports no close.
func TestReviewNormalRunIsUnaffected(t *testing.T) {
	output := filepath.Join(t.TempDir(), "complete.json")
	if err := os.WriteFile(output, []byte(`{"status":"complete","summary":{"files_reviewed":1,"total_tokens":5},"manifest":{"terminal_state":"complete"}}`), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	bin := writeDeadlineOCR(t, fmt.Sprintf("#!/bin/sh\ncat %q\n", output))
	reviewer := deadlineReviewer(t, bin, 2*time.Second, 300*time.Millisecond)

	res, _, err := reviewer.Review(context.Background())
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if res.closeReason != "" || res.hardStopped {
		t.Errorf("closeReason/hardStopped = %q/%v, want a normal exit", res.closeReason, res.hardStopped)
	}
	if status, _ := nativeOutcome(res); status != store.RunSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
}

// Every group of one run disables OCR's per-task deadline and derives from the
// same run deadline instead of a rounded per-group minute.
func TestReviewGroupsShareTheGlobalDeadline(t *testing.T) {
	output := filepath.Join(t.TempDir(), "complete.json")
	if err := os.WriteFile(output, []byte(`{"status":"complete","summary":{"files_reviewed":1},"manifest":{"terminal_state":"complete"}}`), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	argsLog := filepath.Join(t.TempDir(), "args")
	bin := writeDeadlineOCR(t, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
cat %q
`, argsLog, output))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		reviewer := deadlineReviewer(t, bin, 0, 300*time.Millisecond)
		if _, _, err := reviewer.Review(ctx); err != nil {
			t.Fatalf("Review() group %d error = %v", i+1, err)
		}
	}

	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("recorded invocations = %d, want 2", len(lines))
	}
	for i, line := range lines {
		if !strings.Contains(line, "--timeout 0") {
			t.Errorf("group %d args = %q, want --timeout 0 so it shares the run deadline", i+1, line)
		}
	}
}
