package review

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func testReviewConfig() store.ReviewConfig {
	return store.ReviewConfig{
		PR: store.PullRequest{BaseRef: "main"},
		Policy: store.QAPolicy{
			BlockingThreshold: "major",
			ExcludeGlobs:      []byte(`["**/gen/**"]`),
			ExtraInstructions: "revisa seguridad",
			ReviewEffort:      store.ReviewEffortLow,
		},
		Profile: store.LLMProfile{
			ID: 7, Provider: "nvidia", Model: "nemotron", Mode: "api_key",
		},
	}
}

func TestUnitScopeInvalidatesOnComparisonPolicyProfileAndAnalyzer(t *testing.T) {
	cfg := testReviewConfig()
	base := newUnitScope(cfg, "base-1", "head-1", "low", 1000, 100)

	same := newUnitScope(cfg, "base-1", "head-1", "low", 1000, 100)
	if base.key() != same.key() {
		t.Fatalf("scope key changed for identical inputs")
	}

	cases := map[string]unitScope{
		"head":    newUnitScope(cfg, "base-1", "head-2", "low", 1000, 100),
		"base":    newUnitScope(cfg, "base-2", "head-1", "low", 1000, 100),
		"effort":  newUnitScope(cfg, "base-1", "head-1", "high", 1000, 100),
		"ceiling": newUnitScope(cfg, "base-1", "head-1", "low", 2000, 100),
		"budget":  newUnitScope(cfg, "base-1", "head-1", "low", 1000, 200),
		"analyzer": func() unitScope {
			s := base
			s.Analyzer = "ocr-9.9.9"
			return s
		}(),
	}
	policy := cfg
	policy.Policy.ExtraInstructions = "otra política"
	cases["policy"] = newUnitScope(policy, "base-1", "head-1", "low", 1000, 100)
	profile := cfg
	profile.Profile.Model = "otro-modelo"
	cases["profile"] = newUnitScope(profile, "base-1", "head-1", "low", 1000, 100)

	mainKey := base.unitKey("main", "", "")
	for name, scope := range cases {
		if got := scope.unitKey("main", "", ""); got == mainKey {
			t.Errorf("%s change did not invalidate the main unit key", name)
		}
	}
	if a, b := base.unitKey("chunk", "big.go", "commit-1"), base.unitKey("chunk", "big.go", "commit-2"); a == b {
		t.Errorf("different chunk commits share a unit key")
	}
	if a, b := base.unitKey("chunk", "a.go", "commit-1"), base.unitKey("chunk", "b.go", "commit-1"); a == b {
		t.Errorf("different chunk paths share a unit key")
	}
}

func TestResumeSessionRequiresMatchingScope(t *testing.T) {
	scope := newUnitScope(testReviewConfig(), "base", "head", "low", 1000, 100)
	run := store.ReviewRun{OCRSessionID: "sess-1", OCRSessionScope: scope.key()}
	if got := resumeSession(run, scope); got != "sess-1" {
		t.Fatalf("resumeSession = %q, want the stored session", got)
	}
	other := newUnitScope(testReviewConfig(), "base", "other-head", "low", 1000, 100)
	if got := resumeSession(run, other); got != "" {
		t.Fatalf("resumeSession with another comparison = %q, want no resume", got)
	}
}

func TestNewRunResumeSumsCompletedUsage(t *testing.T) {
	resume := newRunResume([]store.ReviewUnit{
		{UnitKey: "main", Status: store.ReviewUnitCompleted, Usage: []byte(`{"total":100}`)},
		{UnitKey: "chunk-1", Status: store.ReviewUnitCompleted, Usage: []byte(`{"total":50}`)},
		{UnitKey: "chunk-2", Status: "pending"},
	})

	if resume.spent != 150 {
		t.Fatalf("spent = %d, want 150 from completed units only", resume.spent)
	}
	if _, ok := resume.lookup("chunk-2"); ok {
		t.Fatal("pending unit was offered for reuse")
	}
}

func TestAnalyzerVersionMatchesDockerPin(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "Dockerfile"))
	if err != nil {
		t.Fatalf("read deploy/Dockerfile: %v", err)
	}
	pin := "@alibaba-group/open-code-review@" + strings.TrimPrefix(analyzerVersion, "ocr-")
	if !strings.Contains(string(raw), pin) {
		t.Fatalf("analyzerVersion = %q but deploy/Dockerfile does not contain %q", analyzerVersion, pin)
	}
}

func TestBuildReviewArgsCarriesResumeForMainPassOnly(t *testing.T) {
	profile := store.LLMProfile{Model: "nemotron"}
	in := nativeInvocation{
		RepoDir: "/repo", BaseRef: "base", HeadRef: "head",
		Profile: profile, Effort: "low", MaxTokens: 1000, ResumeSession: "sess-1",
	}
	args := strings.Join(buildReviewArgs(in), " ")
	if !strings.Contains(args, "--resume sess-1") {
		t.Fatalf("main arguments = %q, want --resume sess-1", args)
	}

	in.Commit = "synthetic"
	args = strings.Join(buildReviewArgs(in), " ")
	if strings.Contains(args, "--resume") {
		t.Fatalf("chunk arguments = %q, must not resume the main session", args)
	}
}

func TestOCRSessionLine(t *testing.T) {
	line := "[ocr] Session: 847045bd-7762-42db-b6bc-f48bd9ca61fa (retry with: --resume 847045bd-7762-42db-b6bc-f48bd9ca61fa)"
	if got := ocrSessionFromLine(line); got != "847045bd-7762-42db-b6bc-f48bd9ca61fa" {
		t.Fatalf("session = %q", got)
	}
	if got := ocrSessionFromLine("progress 50%"); got != "" {
		t.Fatalf("session from unrelated line = %q, want empty", got)
	}
}

var fakeOCRBuild struct {
	once sync.Once
	path string
	err  error
}

// fakeOCRBatch compiles the Go stand-in for the OCR binary. Unlike the shell
// fakes used elsewhere, it runs on every platform, so the resume acceptance
// tests also run on Windows.
func fakeOCRBatch(t *testing.T) string {
	t.Helper()
	fakeOCRBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "qa-fakeocr-")
		if err != nil {
			fakeOCRBuild.err = err
			return
		}
		bin := filepath.Join(dir, "fakeocr")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeocr")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeOCRBuild.err = fmt.Errorf("build fakeocr: %w: %s", err, out)
			return
		}
		fakeOCRBuild.path = bin
	})
	if fakeOCRBuild.err != nil {
		t.Fatalf("%v", fakeOCRBuild.err)
	}
	return fakeOCRBuild.path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// phaseDir prepares the fake OCR answers of one attempt.
func phaseDir(t *testing.T, root, phase string, chunks ...string) string {
	t.Helper()
	dir := filepath.Join(root, "phase"+phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, output := range chunks {
		writeRunTestFile(t, filepath.Join(dir, fmt.Sprintf("chunk%d.json", i+1)), output)
	}
	return dir
}

// startInterruptedAttempt runs the review with a fake OCR that blocks on the
// given chunk, waits until it is blocked, cancels the run and returns the
// pending call. Only the chunk before the blocked one has finished at that
// point unless the main pass was the only unit.
func (f *runTestFixture) startInterruptedAttempt(t *testing.T, root string, hangChunk int) {
	t.Helper()
	f.ocrBin = fakeOCRBatch(t)
	t.Setenv("RUN_FAKE_OCR_DIR", root)
	t.Setenv("RUN_FAKE_OCR_PHASE", "0")
	t.Setenv("RUN_FAKE_OCR_COUNTER", filepath.Join(root, "counter0"))
	t.Setenv("RUN_FAKE_OCR_CALLS", f.ocrCallLog)
	t.Setenv("RUN_FAKE_OCR_ARGV", f.ocrArgv)
	t.Setenv("RUN_FAKE_OCR_HANG_CHUNK", fmt.Sprint(hangChunk))
	ready := filepath.Join(root, "ready")
	t.Setenv("RUN_FAKE_OCR_READY", ready)

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.chunkRunner().Run(ctx, f.runID) }()
	waitForFile(t, ready)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("interrupted run did not return")
	}

	run := f.loadRun(t)
	if run.Status != store.RunQueued {
		t.Fatalf("interrupted run status = %q, want queued", run.Status)
	}
}

func TestRunResumeReusesCompletedUnits(t *testing.T) {
	f := newChunkRunFixture(t)
	root := t.TempDir()
	writeRunTestFile(t, filepath.Join(root, "main.json"), nativeChunkMainOutput("too_large"))
	phaseDir(t, root, "0",
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	)
	f.startInterruptedAttempt(t, root, 2)

	units, err := f.s.ReviewUnitsForRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("persisted units = %d, want main + first chunk", len(units))
	}

	// The next attempt must only execute the pending chunk.
	phaseDir(t, root, "1",
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	)
	t.Setenv("RUN_FAKE_OCR_PHASE", "1")
	t.Setenv("RUN_FAKE_OCR_COUNTER", filepath.Join(root, "counter1"))
	t.Setenv("RUN_FAKE_OCR_HANG_CHUNK", "")
	before := f.ocrCalls(t)
	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	if got := f.ocrCalls(t) - before; got != 1 {
		t.Fatalf("resumed OCR calls = %d, want only the pending chunk", got)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if run.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", run.Attempt)
	}
	// Two reused units plus the new one: 100 + 50 + 50, never counted twice.
	if run.TotalTokens == nil || *run.TotalTokens != 200 {
		t.Errorf("total_tokens = %v, want 200 without double counting", run.TotalTokens)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 2 {
		t.Errorf("big.go findings = %d, want 2", got)
	}
	if got := len(findingsForPath(t, f, "auth.go")); got != 1 {
		t.Errorf("auth.go findings = %d, want 1", got)
	}
	left, err := f.s.ReviewUnitsForRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("list units after terminal: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("terminal run kept %d units, want retention cleanup", len(left))
	}
}

func TestRunResumeInvalidatesUnitsWhenPolicyChanges(t *testing.T) {
	f := newChunkRunFixture(t)
	root := t.TempDir()
	writeRunTestFile(t, filepath.Join(root, "main.json"), nativeChunkMainOutput("too_large"))
	phaseDir(t, root, "0",
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	)
	f.startInterruptedAttempt(t, root, 2)

	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).
		Update("extra_instructions", "la política cambió").Error; err != nil {
		t.Fatal(err)
	}
	phaseDir(t, root, "1",
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	)
	t.Setenv("RUN_FAKE_OCR_PHASE", "1")
	t.Setenv("RUN_FAKE_OCR_COUNTER", filepath.Join(root, "counter1"))
	t.Setenv("RUN_FAKE_OCR_HANG_CHUNK", "")
	before := f.ocrCalls(t)
	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	// The policy change invalidates every unit: main pass plus both chunks.
	if got := f.ocrCalls(t) - before; got != 3 {
		t.Fatalf("OCR calls after invalidation = %d, want 3", got)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
}

func TestRunResumeStopsWhenAggregateTimeIsSpent(t *testing.T) {
	f := newChunkRunFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Exec(
		`UPDATE review_runs SET budget_started_at = now() - interval '1 hour' WHERE id = ?`,
		f.runID).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "timeout")
	if got := f.ocrCalls(t); got != 0 {
		t.Errorf("ocr calls = %d, want 0 once the aggregate budget is spent", got)
	}
}

func TestRunResumeBudgetExhaustionKeepsRemainderPending(t *testing.T) {
	f := newChunkRunFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).
		Update("review_token_budget", int64(200)).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeRunTestFile(t, filepath.Join(root, "main.json"), nativeChunkMainOutput("too_large"))
	phaseDir(t, root, "0",
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 100),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 100),
	)
	// The first chunk blocks after the main pass, so only its 100 tokens are
	// persisted when the run is interrupted.
	f.startInterruptedAttempt(t, root, 1)

	phaseDir(t, root, "1",
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 100),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 100),
	)
	t.Setenv("RUN_FAKE_OCR_PHASE", "1")
	t.Setenv("RUN_FAKE_OCR_COUNTER", filepath.Join(root, "counter1"))
	t.Setenv("RUN_FAKE_OCR_HANG_CHUNK", "")
	before := f.ocrCalls(t)
	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	// Only the second chunk runs; the third is left pending because the
	// aggregate 200-token budget is already spent.
	if got := f.ocrCalls(t) - before; got != 1 {
		t.Fatalf("resumed OCR calls = %d, want one chunk before the budget stops", got)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")
	if run.TotalTokens == nil || *run.TotalTokens != 200 {
		t.Errorf("total_tokens = %v, want the aggregate 200", run.TotalTokens)
	}
	coverage := decodeChunkCoverage(t, run)
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coveragePending || big.Reason != "token_budget" {
		t.Errorf("big.go coverage = %+v, want pending/token_budget", big)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 1 {
		t.Errorf("big.go findings = %d, want the chunk that did run", got)
	}
}

func TestRunResumeMainPassDoesNotExceedAggregateBudget(t *testing.T) {
	f := newRunTestFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).
		Update("review_token_budget", int64(100)).Error; err != nil {
		t.Fatal(err)
	}
	// A unit from an earlier attempt already spent the whole aggregate budget,
	// but its key no longer matches the current scope, so the main pass is not
	// reusable.
	if err := f.s.UpsertReviewUnit(f.ctx, store.ReviewUnit{
		RunID: f.runID, UnitKey: "stale", Status: store.ReviewUnitCompleted,
		Usage: []byte(`{"total":100}`),
	}); err != nil {
		t.Fatalf("seed unit: %v", err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "token_budget")
	if got := f.ocrCalls(t); got != 0 {
		t.Errorf("ocr calls = %d, want 0 with the aggregate budget exhausted", got)
	}
}

func TestRunResumePersistsOCRSessionAndRetriesWithIt(t *testing.T) {
	f := newRunTestFixture(t)
	root := t.TempDir()
	mainOutput := filepath.Join(root, "main.json")
	writeRunTestFile(t, mainOutput, runTestNativeOutput("auth.go"))
	ready := filepath.Join(root, "ready")
	f.ocrBin = fakeOCRBatch(t)
	t.Setenv("RUN_FAKE_OCR_DIR", root)
	t.Setenv("RUN_FAKE_OCR_PHASE", "0")
	t.Setenv("RUN_FAKE_OCR_CALLS", f.ocrCallLog)
	t.Setenv("RUN_FAKE_OCR_ARGV", f.ocrArgv)
	t.Setenv("RUN_FAKE_OCR_MAIN_OUTPUT", mainOutput)
	t.Setenv("RUN_FAKE_OCR_EMIT_SESSION", "fake-session-1")
	t.Setenv("RUN_FAKE_OCR_HANG_MAIN", "1")
	t.Setenv("RUN_FAKE_OCR_READY", ready)

	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.runner().Run(ctx, f.runID) }()
	waitForFile(t, ready)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("interrupted main pass did not return")
	}
	run := f.loadRun(t)
	if run.Status != store.RunQueued {
		t.Fatalf("interrupted run status = %q, want queued", run.Status)
	}
	if run.OCRSessionID != "fake-session-1" || run.OCRSessionScope == "" {
		t.Fatalf("stored OCR session = %q/%q, want the announced id and scope",
			run.OCRSessionID, run.OCRSessionScope)
	}

	// The retry must resume OCR's own session instead of rerunning the pass.
	t.Setenv("RUN_FAKE_OCR_PHASE", "1")
	t.Setenv("RUN_FAKE_OCR_HANG_MAIN", "")
	t.Setenv("RUN_FAKE_OCR_EMIT_SESSION", "fake-session-2")
	before := len(f.ocrArgs(t))
	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
	args := f.ocrArgs(t)[before:]
	resumed := false
	for _, line := range args {
		if strings.Contains(line, "--resume fake-session-1") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("retry arguments %v, want --resume fake-session-1", args)
	}
	if got := f.loadRun(t).OCRSessionID; got != "fake-session-2" {
		t.Errorf("stored session = %q, want the successful attempt's id", got)
	}
}

func TestRunResumeRetriesWithoutSessionWhenResumeFails(t *testing.T) {
	f := newRunTestFixture(t)
	root := t.TempDir()
	mainOutput := filepath.Join(root, "main.json")
	writeRunTestFile(t, mainOutput, runTestNativeOutput("auth.go"))
	ready := filepath.Join(root, "ready")
	f.ocrBin = fakeOCRBatch(t)
	t.Setenv("RUN_FAKE_OCR_DIR", root)
	t.Setenv("RUN_FAKE_OCR_PHASE", "0")
	t.Setenv("RUN_FAKE_OCR_CALLS", f.ocrCallLog)
	t.Setenv("RUN_FAKE_OCR_ARGV", f.ocrArgv)
	t.Setenv("RUN_FAKE_OCR_MAIN_OUTPUT", mainOutput)
	t.Setenv("RUN_FAKE_OCR_EMIT_SESSION", "stale")
	t.Setenv("RUN_FAKE_OCR_HANG_MAIN", "1")
	t.Setenv("RUN_FAKE_OCR_READY", ready)

	// The first attempt stores a real session and is interrupted mid-pass, so
	// the stored scope matches the attempt exactly.
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.runner().Run(ctx, f.runID) }()
	waitForFile(t, ready)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("interrupted main pass did not return")
	}
	if got := f.loadRun(t).OCRSessionID; got != "stale" {
		t.Fatalf("stored session = %q, want stale", got)
	}

	// The stored session is rejected on retry: OCR must rerun the pass without
	// --resume and finish successfully.
	t.Setenv("RUN_FAKE_OCR_PHASE", "1")
	t.Setenv("RUN_FAKE_OCR_HANG_MAIN", "")
	t.Setenv("RUN_FAKE_OCR_EMIT_SESSION", "")
	t.Setenv("RUN_FAKE_OCR_FAIL_RESUME", "1")
	beforeCalls := f.ocrCalls(t)
	beforeArgs := len(f.ocrArgs(t))
	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
	if got := f.ocrCalls(t) - beforeCalls; got != 2 {
		t.Fatalf("ocr calls = %d, want the failed resume plus the fallback", got)
	}
	args := f.ocrArgs(t)[beforeArgs:]
	if !strings.Contains(args[0], "--resume stale") {
		t.Errorf("first invocation = %q, want the stored session", args[0])
	}
	if strings.Contains(args[1], "--resume") {
		t.Errorf("fallback invocation = %q, must not resume the failed session", args[1])
	}
	if got := f.loadRun(t).OCRSessionID; got == "stale" {
		t.Errorf("failed session %q was persisted again", got)
	}
}
