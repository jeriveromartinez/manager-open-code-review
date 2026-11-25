package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// newChunkRunFixture parte de un repo real con big.go en la base y dos hunks
// separados en head. El techo de contexto bajo fuerza un chunk por hunk.
func newChunkRunFixture(t *testing.T) *runTestFixture {
	t.Helper()
	f := newRunTestFixture(t)
	work := filepath.Join(f.root, "work")

	runGit(t, work, "checkout", "-q", "main")
	base := numberedLines(40)
	writeRunTestFile(t, filepath.Join(work, "big.go"), textLines(base))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "big base")
	baseSHA := runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-q", "feature")
	runGit(t, work, "merge", "-q", "--no-edit", "main")
	head := append([]string(nil), base...)
	head[19] = "changed20"
	head[23] = "changed24"
	writeRunTestFile(t, filepath.Join(work, "big.go"), textLines(head))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "big head")
	headSHA := runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "push", "-q", f.bare, "main:main")
	runGit(t, work, "push", "-q", "-f", f.bare, "feature:refs/pull/1/head")

	f.updateRun(t, f.runID, map[string]any{"base_sha": baseSHA, "head_sha": headSHA})
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", f.pr.ID).Update("head_sha", headSHA).Error; err != nil {
		t.Fatalf("update pull request head: %v", err)
	}
	f.baseSHA, f.headSHA = baseSHA, headSHA
	f.pr.HeadSHA = headSHA
	return f
}

// chunkRunner usa un techo de 10 tokens: maxBytes = 10*3*30/100 = 9 bytes, así
// que cada hunk (cabecera incluida) forma su propio trozo.
func (f *runTestFixture) chunkRunner() *Runner {
	r := f.runner()
	r.Cfg.ContextCeiling = 10
	return r
}

// nativeChunkMainOutput simula el run principal: revisa auth.go y excluye
// big.go con el motivo indicado.
func nativeChunkMainOutput(excludeReason string) string {
	return fmt.Sprintf(`{
	  "status": "complete",
	  "summary": {"files_reviewed": 1, "comments": 1, "total_tokens": 100,
	    "input_tokens": 80, "output_tokens": 20},
	  "comments": [{
	    "path": "auth.go", "content": "Hallazgo principal\n\ncuerpo principal",
	    "start_line": 1, "end_line": 1, "category": "bug", "severity": "medium"
	  }],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1", "terminal_state": "complete",
	    "coverage": {
	      "selected": [{"path": "auth.go"}, {"path": "big.go"}],
	      "completed": [{"path": "auth.go"}],
	      "reused": [], "failed": [], "waived": [],
	      "excluded": [{"path": "big.go", "exclude_reason": %q}]
	    }
	  }
	}`, excludeReason)
}

// nativeChunkMainOutputWithoutSummary is the same review without a summary, so
// the run cannot know what the first pass spent.
func nativeChunkMainOutputWithoutSummary(excludeReason string) string {
	return fmt.Sprintf(`{
	  "status": "complete",
	  "comments": [{
	    "path": "auth.go", "content": "Hallazgo principal\n\ncuerpo principal",
	    "start_line": 1, "end_line": 1, "category": "bug", "severity": "medium"
	  }],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1", "terminal_state": "complete",
	    "coverage": {
	      "selected": [{"path": "auth.go"}, {"path": "big.go"}],
	      "completed": [{"path": "auth.go"}],
	      "reused": [], "failed": [], "waived": [],
	      "excluded": [{"path": "big.go", "exclude_reason": %q}]
	    }
	  }
	}`, excludeReason)
}

// nativeChunkOutput simula la revisión de un trozo: un hallazgo en big.go.
func nativeChunkOutput(path string, start, end int, title, body string, tokens int) string {
	return fmt.Sprintf(`{
	  "status": "complete",
	  "summary": {"files_reviewed": 1, "comments": 1, "total_tokens": %d,
	    "input_tokens": %d, "output_tokens": 20},
	  "comments": [{
	    "path": %q, "content": %q, "start_line": %d, "end_line": %d,
	    "category": "security", "severity": "high"
	  }],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1", "terminal_state": "complete",
	    "coverage": {
	      "selected": [{"path": %q}], "completed": [{"path": %q}],
	      "reused": [], "failed": [], "waived": [], "excluded": []
	    }
	  }
	}`, tokens, tokens-20, path, title+"\n\n"+body, start, end, path, path)
}

// setOCRChunks instala un OCR falso que distingue el run principal (--from/--to)
// de los trozos (--commit) y sirve un JSON distinto por trozo.
func (f *runTestFixture) setOCRChunks(t *testing.T, mainOutput string, chunkOutputs []string) {
	t.Helper()
	dir := filepath.Join(f.root, "ocr-chunks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
if [ -n "${RUN_FAKE_OCR_CALLS:-}" ]; then printf 'call\n' >>"$RUN_FAKE_OCR_CALLS"; fi
if [ -n "${RUN_FAKE_OCR_ARGV:-}" ]; then printf '%s\n' "$*" >>"$RUN_FAKE_OCR_ARGV"; fi
if [ -n "${RUN_FAKE_OCR_ONCE_OUTPUT:-}" ] && [ ! -f "${RUN_FAKE_OCR_ONCE_OUTPUT}.used" ]; then
    : >"${RUN_FAKE_OCR_ONCE_OUTPUT}.used"
    cat "$RUN_FAKE_OCR_ONCE_OUTPUT"
    exit "${RUN_FAKE_OCR_EXIT:-0}"
fi
commit=""
prev=""
for arg in "$@"; do
    if [ "$prev" = "--background-file" ]; then cp "$arg" "${RUN_FAKE_OCR_BACKGROUND_OUT:-/dev/null}"; fi
    if [ "$prev" = "--commit" ]; then commit="$arg"; fi
    prev="$arg"
done
if [ -z "$commit" ]; then
    cat "$RUN_FAKE_OCR_MAIN_OUTPUT"
    exit "${RUN_FAKE_OCR_EXIT:-0}"
fi
n=0
if [ -f "$RUN_FAKE_OCR_COUNTER" ]; then n=$(cat "$RUN_FAKE_OCR_COUNTER"); fi
n=$((n + 1))
printf '%s' "$n" >"$RUN_FAKE_OCR_COUNTER"
if [ -n "${RUN_FAKE_OCR_FAIL_CHUNK:-}" ] && [ "$n" = "$RUN_FAKE_OCR_FAIL_CHUNK" ]; then
    printf 'ocr provider unavailable\n' >&2
    exit 1
fi
cat "$RUN_FAKE_OCR_DIR/chunk$n.json"
`
	bin := filepath.Join(dir, "ocr.sh")
	writeRunTestFile(t, bin, script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "main.json")
	writeRunTestFile(t, mainPath, mainOutput)
	for i, output := range chunkOutputs {
		writeRunTestFile(t, filepath.Join(dir, fmt.Sprintf("chunk%d.json", i+1)), output)
	}
	f.ocrBin = bin
	t.Setenv("RUN_FAKE_OCR_CALLS", f.ocrCallLog)
	t.Setenv("RUN_FAKE_OCR_ARGV", f.ocrArgv)
	t.Setenv("RUN_FAKE_OCR_BACKGROUND_OUT", f.ocrBack)
	t.Setenv("RUN_FAKE_OCR_MAIN_OUTPUT", mainPath)
	t.Setenv("RUN_FAKE_OCR_COUNTER", filepath.Join(dir, "counter"))
	t.Setenv("RUN_FAKE_OCR_DIR", dir)
}

type chunkCoverage struct {
	ReviewedFiles int            `json:"reviewed_files"`
	SkippedFiles  int            `json:"skipped_files"`
	ExcludedFiles int            `json:"excluded_files"`
	Files         []coverageFile `json:"files"`
}

func decodeChunkCoverage(t *testing.T, run store.ReviewRun) chunkCoverage {
	t.Helper()
	var coverage chunkCoverage
	if err := json.Unmarshal(run.Coverage, &coverage); err != nil {
		t.Fatalf("decode coverage %q: %v", run.Coverage, err)
	}
	return coverage
}

func coverageFileAt(t *testing.T, coverage chunkCoverage, path string) coverageFile {
	t.Helper()
	for _, file := range coverage.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("coverage has no entry for %q: %+v", path, coverage.Files)
	return coverageFile{}
}

func findingsForPath(t *testing.T, f *runTestFixture, path string) []store.Finding {
	t.Helper()
	findings, err := f.s.FindingsForPR(f.ctx, f.pr.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	var matched []store.Finding
	for _, finding := range findings {
		if finding.Path == path {
			matched = append(matched, finding)
		}
	}
	return matched
}

func TestFinishChunkMarksUsageUnknownWithoutSummary(t *testing.T) {
	summary := newChunkSummary()
	failures, reason := 0, ""
	finishChunk(context.Background(), &summary, "big.go",
		nativeReview{Status: "complete"}, nil, &failures, &reason)
	if summary.usageKnown {
		t.Error("usageKnown = true, want unknown after a chunk without a summary")
	}
	if failures != 0 || reason != "" || summary.tokens.Total != 0 {
		t.Errorf("summary = %+v/%d/%q, want a successful chunk with zero logged spend",
			summary.tokens, failures, reason)
	}

	// A failed chunk may have spent tokens before failing.
	failed := newChunkSummary()
	finishChunk(context.Background(), &failed, "big.go", nativeReview{},
		errors.New("provider down"), &failures, &reason)
	if failed.usageKnown {
		t.Error("usageKnown = true, want unknown after a failed chunk")
	}
}

func TestRunChunksTooLargeFile(t *testing.T) {
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "segundo cuerpo", 50),
	})

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")

	if got := f.ocrCalls(t); got != 3 {
		t.Errorf("ocr calls = %d, want 3 (main + two chunks)", got)
	}
	lines := f.ocrArgs(t)
	if !strings.Contains(lines[0], "--from") || !strings.Contains(lines[0], "--to") {
		t.Errorf("main invocation = %q, want --from/--to", lines[0])
	}
	for i, line := range lines[1:] {
		if !strings.Contains(line, "--commit") {
			t.Errorf("chunk invocation %d = %q, want --commit", i+1, line)
		}
	}

	coverage := decodeChunkCoverage(t, run)
	if coverage.ReviewedFiles != 2 || coverage.SkippedFiles != 0 || coverage.ExcludedFiles != 0 {
		t.Errorf("coverage counters = %+v, want 2 reviewed / 0 skipped / 0 excluded", coverage)
	}
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coverageReviewedByParts || big.Chunks != 2 || big.Method != coverageMethodChunked {
		t.Errorf("big.go coverage = %+v, want reviewed_by_parts/chunked with 2 chunks", big)
	}
	if auth := coverageFileAt(t, coverage, "auth.go"); auth.Status != coverageReviewed || auth.Method != coverageMethodOCR {
		t.Errorf("auth.go coverage = %+v, want reviewed/ocr", auth)
	}

	if run.TotalTokens == nil || *run.TotalTokens != 200 {
		t.Errorf("total_tokens = %v, want 200 (main + chunks)", run.TotalTokens)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 2 {
		t.Errorf("big.go findings = %d, want 2 (one per chunk)", got)
	}
	if got := len(findingsForPath(t, f, "auth.go")); got != 1 {
		t.Errorf("auth.go findings = %d, want 1", got)
	}
}

func TestRunChunksDedupeBoundaryFinding(t *testing.T) {
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Frontera compartida", "primer cuerpo", 50),
		nativeChunkOutput("big.go", 21, 21, "Frontera compartida", "segundo cuerpo", 50),
	})

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")

	findings := findingsForPath(t, f, "big.go")
	if len(findings) != 1 {
		t.Fatalf("big.go findings = %d, want 1 after the boundary dedupe: %+v", len(findings), findings)
	}
	got := findings[0]
	if got.StartLine != 20 || got.EndLine != 20 {
		t.Errorf("finding range = %d-%d, want the earliest 20-20", got.StartLine, got.EndLine)
	}
	if !strings.Contains(got.Body, "primer cuerpo") {
		t.Errorf("finding body = %q, want the first chunk body", got.Body)
	}
}

func TestRunChunkFailureKeepsRestOfRun(t *testing.T) {
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "cuerpo", 50),
	})
	t.Setenv("RUN_FAKE_OCR_FAIL_CHUNK", "2")

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")

	coverage := decodeChunkCoverage(t, run)
	if coverage.ReviewedFiles != 1 || coverage.SkippedFiles != 1 || coverage.ExcludedFiles != 0 {
		t.Errorf("coverage counters = %+v, want 1 reviewed / 1 skipped / 0 excluded", coverage)
	}
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coveragePending || big.Reason != "provider" {
		t.Errorf("big.go coverage = %+v, want pending/provider", big)
	}
	if got := f.ocrCalls(t); got != 3 {
		t.Errorf("ocr calls = %d, want 3", got)
	}

	// El trozo que sí terminó y el resto del PR conservan sus hallazgos.
	if got := len(findingsForPath(t, f, "big.go")); got != 1 {
		t.Errorf("big.go findings = %d, want the successful chunk finding", got)
	}
	if got := len(findingsForPath(t, f, "auth.go")); got != 1 {
		t.Errorf("auth.go findings = %d, want 1", got)
	}
}

// nativeTextRouteOutput is a valid answer for one text-route window: it
// reports the synthetic path completed and places its finding inside it.
func nativeTextRouteOutput(reviewPath string, line int, title string) string {
	return fmt.Sprintf(`{
	  "status": "complete",
	  "summary": {"files_reviewed": 1, "comments": 1, "total_tokens": 50,
	    "input_tokens": 40, "output_tokens": 10},
	  "comments": [{
	    "path": %q, "content": %q, "start_line": %d, "end_line": %d,
	    "category": "maintainability", "severity": "low"
	  }],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1", "terminal_state": "complete",
	    "coverage": {
	      "selected": [{"path": %q}], "completed": [{"path": %q}],
	      "reused": [], "failed": [], "waived": [], "excluded": []
	    }
	  }
	}`, reviewPath, title+"\n\ncuerpo", line, line, reviewPath, reviewPath)
}

func TestRunRoutesUnsupportedExtThroughTextRoute(t *testing.T) {
	f := newChunkRunFixture(t)
	reviewPath := textReviewPath("big.go")
	f.setOCRChunks(t, nativeChunkMainOutput("unsupported_ext"), []string{
		nativeTextRouteOutput(reviewPath, 20, "Hallazgo de la ruta de texto uno"),
		nativeTextRouteOutput(reviewPath, 24, "Hallazgo de la ruta de texto dos"),
	})

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")

	if got := f.ocrCalls(t); got != 3 {
		t.Errorf("ocr calls = %d, want 3 (main plus two text windows)", got)
	}
	args := f.ocrArgs(t)
	if len(args) == 0 {
		t.Fatal("no OCR invocations")
	}
	for i, line := range args[1:] {
		if !strings.Contains(line, "--commit") {
			t.Errorf("text route invocation %d = %q, want --commit", i+1, line)
		}
	}
	coverage := decodeChunkCoverage(t, run)
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coverageReviewedByParts || big.Method != coverageMethodText || big.Chunks != 2 {
		t.Errorf("big.go coverage = %+v, want reviewed_by_parts/text with 2 windows", big)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 2 {
		t.Errorf("big.go findings = %d, want the two text-route findings", got)
	}
}

func TestRunTextRouteInvalidStaysPending(t *testing.T) {
	f := newChunkRunFixture(t)
	reviewPath := textReviewPath("big.go")
	f.setOCRChunks(t, nativeChunkMainOutput("unsupported_ext"), []string{
		// wrong path: the engine did not review the synthetic file it was given.
		nativeTextRouteOutput("big.go", 20, "Hallazgo sin procedencia"),
		nativeTextRouteOutput(reviewPath, 24, "Hallazgo válido pero parcial"),
	})

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")
	coverage := decodeChunkCoverage(t, run)
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coveragePending || big.Reason != "text_route_invalid" {
		t.Errorf("big.go coverage = %+v, want pending/text_route_invalid", big)
	}
	if big.Method != coverageMethodText || !big.Human {
		t.Errorf("big.go coverage = %+v, want the text method and a human flag", big)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 0 {
		t.Errorf("big.go findings = %d, want none from an invalid text route", got)
	}
}

func TestRunChunkStopsWhenTokenBudgetExhausted(t *testing.T) {
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Fallo del trozo uno", "cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Fallo del trozo dos", "cuerpo", 50),
	})
	// 100 tokens del run principal + 50 del primer trozo superan el tope de 120.
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Update("review_token_budget", int64(120)).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")
	if got := f.ocrCalls(t); got != 2 {
		t.Fatalf("ocr calls = %d, want the main run and one chunk", got)
	}
	coverage := decodeChunkCoverage(t, run)
	big := coverageFileAt(t, coverage, "big.go")
	if big.Status != coveragePending || big.Reason != "token_budget" {
		t.Errorf("big.go coverage = %+v, want pending/token_budget", big)
	}
	if got := len(findingsForPath(t, f, "big.go")); got != 1 {
		t.Errorf("big.go findings = %d, want the first chunk finding", got)
	}
	if got := len(findingsForPath(t, f, "auth.go")); got != 1 {
		t.Errorf("auth.go findings = %d, want 1", got)
	}
}

func TestRunChunkReceivesTheRemainingMainBudget(t *testing.T) {
	skipWithoutShellFake(t)
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutput("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "Primer trozo", "cuerpo", 50),
		nativeChunkOutput("big.go", 24, 24, "Segundo trozo", "cuerpo", 50),
	})
	// 1000 for the run, 100 spent by the main pass and 50 by each chunk.
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Update("review_token_budget", int64(1000)).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	lines := f.ocrArgs(t)
	if len(lines) < 3 {
		t.Fatalf("ocr calls = %d, want the main run and two chunks", len(lines))
	}
	if got := ocrArgInLine(lines[0], "--max-tokens-budget"); got != "1000" {
		t.Errorf("main --max-tokens-budget = %q, want 1000", got)
	}
	if got := ocrArgInLine(lines[1], "--max-tokens-budget"); got != "900" {
		t.Errorf("first chunk --max-tokens-budget = %q, want 900 left of the main budget", got)
	}
	if got := ocrArgInLine(lines[2], "--max-tokens-budget"); got != "850" {
		t.Errorf("second chunk --max-tokens-budget = %q, want 850 after the first chunk", got)
	}
}

func TestRunDoesNotChunkWhenMainUsageIsUnknown(t *testing.T) {
	skipWithoutShellFake(t)
	f := newChunkRunFixture(t)
	f.setOCRChunks(t, nativeChunkMainOutputWithoutSummary("too_large"), []string{
		nativeChunkOutput("big.go", 20, 20, "No debería ejecutarse", "cuerpo", 50),
	})
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Update("review_token_budget", int64(100000)).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.chunkRunner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := f.ocrCalls(t); got != 1 {
		t.Errorf("ocr calls = %d, want only the main run: without usage data no chunk budget is proven", got)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")
	big := coverageFileAt(t, decodeChunkCoverage(t, run), "big.go")
	if big.Status != coveragePending || big.Reason != "token_budget" {
		t.Errorf("big.go coverage = %+v, want pending/token_budget", big)
	}
}

func TestDedupeNearbyFindings(t *testing.T) {
	base := AgentFinding{
		Path: "big.go", Category: "bug", Title: "Mismo problema",
		StartLine: 10, EndLine: 10, Body: "primer cuerpo",
	}
	tests := []struct {
		name  string
		other AgentFinding
		want  int
	}{
		{
			name:  "solapados",
			other: AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 9, EndLine: 12},
			want:  1,
		},
		{
			name:  "contiguos",
			other: AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 11, EndLine: 11},
			want:  1,
		},
		{
			name:  "separados por tres líneas",
			other: AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 14, EndLine: 14},
			want:  1,
		},
		{
			name:  "separados por cuatro líneas",
			other: AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 15, EndLine: 15},
			want:  2,
		},
		{
			name:  "otra categoría",
			other: AgentFinding{Path: "big.go", Category: "security", Title: "Mismo problema", StartLine: 10, EndLine: 10},
			want:  2,
		},
		{
			name:  "otro título normalizado",
			other: AgentFinding{Path: "big.go", Category: "bug", Title: "Otro problema", StartLine: 10, EndLine: 10},
			want:  2,
		},
		{
			name:  "otra ruta",
			other: AgentFinding{Path: "auth.go", Category: "bug", Title: "Mismo problema", StartLine: 10, EndLine: 10},
			want:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupeNearbyFindings([]AgentFinding{base, tt.other})
			if len(got) != tt.want {
				t.Fatalf("dedupeNearbyFindings() = %+v, want %d finding(s)", got, tt.want)
			}
		})
	}

	// El rango conservado empieza antes aunque la primera aparición venga después.
	late := AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 12, EndLine: 12, Body: "tarde"}
	early := AgentFinding{Path: "big.go", Category: "bug", Title: "Mismo problema", StartLine: 10, EndLine: 10, Body: "temprano"}
	got := dedupeNearbyFindings([]AgentFinding{late, early})
	if len(got) != 1 || got[0].StartLine != 10 || got[0].Body != "tarde" {
		t.Errorf("dedupeNearbyFindings() = %+v, want the earliest range and the first body", got)
	}
}

func TestNativeCoverageFilesAndCounts(t *testing.T) {
	res := nativeReview{Manifest: nativeManifest{Coverage: nativeCoverage{
		Completed: []nativeCoverageItem{{Path: "./a.go"}},
		Reused:    []nativeCoverageItem{{Path: "b.go"}},
		Failed: []nativeCoverageItem{
			{Path: "c.go", Classification: "provider"},
			{Path: "d.go"},
		},
		Waived: []nativeCoverageItem{{Path: "e.go", Reason: "policy"}},
		Excluded: []nativeCoverageItem{
			{Path: "f.go", ExcludeReason: "too_large"},
			{Path: "g.go", Reason: "user_exclude"},
		},
	}}}

	files := nativeCoverageFiles(res)
	want := map[string]coverageFile{
		"a.go": {Path: "a.go", Status: coverageReviewed, Method: coverageMethodOCR},
		"b.go": {Path: "b.go", Status: coverageReviewed, Method: coverageMethodOCR},
		"c.go": {Path: "c.go", Status: coverageNotReviewed, Method: coverageMethodOCR, Reason: "provider"},
		"d.go": {Path: "d.go", Status: coverageNotReviewed, Method: coverageMethodOCR, Reason: "failed"},
		"e.go": {Path: "e.go", Status: coverageNotReviewed, Method: coverageMethodOCR, Reason: "policy"},
		"f.go": {Path: "f.go", Status: coverageExcluded, Reason: "too_large"},
		"g.go": {Path: "g.go", Status: coverageExcluded, Reason: "user_exclude"},
	}
	if len(files) != len(want) {
		t.Fatalf("nativeCoverageFiles() = %+v, want %d entries", files, len(want))
	}
	for _, file := range files {
		if file != want[file.Path] {
			t.Errorf("entry %q = %+v, want %+v", file.Path, file, want[file.Path])
		}
	}
	if reviewed, skipped, excluded := coverageCountsFromFiles(files); reviewed != 2 || skipped != 3 || excluded != 2 {
		t.Errorf("counts = %d/%d/%d, want 2/3/2", reviewed, skipped, excluded)
	}
	if paths := tooLargePaths(res); len(paths) != 1 || paths[0] != "f.go" {
		t.Errorf("tooLargePaths() = %v, want [f.go]", paths)
	}

	files = overlayCoverage(files, map[string]coverageFile{
		"f.go": {Path: "f.go", Status: coverageReviewedByParts, Method: coverageMethodChunked, Chunks: 3},
	})
	if got := coverageFileAt(t, chunkCoverage{Files: files}, "f.go"); got.Status != coverageReviewedByParts || got.Chunks != 3 || got.Method != coverageMethodChunked {
		t.Errorf("overlay = %+v, want f.go reviewed_by_parts/chunked with 3 chunks", got)
	}
	if reviewed, skipped, excluded := coverageCountsFromFiles(files); reviewed != 3 || skipped != 3 || excluded != 1 {
		t.Errorf("counts after overlay = %d/%d/%d, want 3/3/1", reviewed, skipped, excluded)
	}
}

func TestChunkedRunStatus(t *testing.T) {
	reviewed := chunkSummary{statuses: map[string]coverageFile{
		"big.go": {Path: "big.go", Status: coverageReviewedByParts, Chunks: 2},
	}}
	failed := chunkSummary{statuses: map[string]coverageFile{
		"big.go": {Path: "big.go", Status: coverageNotReviewed, Reason: "provider"},
	}}
	mixed := chunkSummary{statuses: map[string]coverageFile{
		"a.go": {Path: "a.go", Status: coverageReviewedByParts, Chunks: 1},
		"b.go": {Path: "b.go", Status: coverageNotReviewed, Reason: "provider"},
	}}

	tests := []struct {
		name       string
		mainStatus string
		mainCode   string
		summary    chunkSummary
		wantStatus string
		wantCode   string
	}{
		{"completo y todo revisado", store.RunSucceeded, "", reviewed, store.RunSucceeded, ""},
		{"omitido y todo revisado", store.RunInconclusive, "no_coverage", reviewed, store.RunSucceeded, ""},
		{"incompleto y todo revisado sigue inconcluso", store.RunInconclusive, "coverage_incomplete", reviewed, store.RunInconclusive, "coverage_incomplete"},
		{"completo con fallo pasa a incompleto", store.RunSucceeded, "", failed, store.RunInconclusive, "coverage_incomplete"},
		{"sin cobertura y ningún trozo revisado", store.RunInconclusive, "no_coverage", failed, store.RunInconclusive, "no_coverage"},
		{"sin cobertura y algún trozo revisado", store.RunInconclusive, "no_coverage", mixed, store.RunInconclusive, "coverage_incomplete"},
		{"fallido no cambia", store.RunFailed, "llm_error", reviewed, store.RunFailed, "llm_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code := chunkedRunStatus(tt.mainStatus, tt.mainCode, tt.summary)
			if status != tt.wantStatus || code != tt.wantCode {
				t.Errorf("chunkedRunStatus() = %q/%q, want %q/%q", status, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestSettledRunStatus(t *testing.T) {
	reviewed := chunkSummary{statuses: map[string]coverageFile{
		"notes.md": {Path: "notes.md", Status: coverageReviewed, Method: coverageMethodText},
	}}
	failed := chunkSummary{statuses: map[string]coverageFile{
		"notes.md": {Path: "notes.md", Status: coverageNotReviewed, Method: coverageMethodText, Reason: "provider"},
	}}
	pending := []coverageFile{{Path: "c.go", Status: coveragePending, Reason: "provider"}}

	tests := []struct {
		name        string
		status      string
		code        string
		pending     []coverageFile
		routed      chunkSummary
		inventoryOK bool
		wantStatus  string
		wantCode    string
	}{
		{"ruta cubre todo el hueco", store.RunInconclusive, "coverage_incomplete", nil, reviewed, true, store.RunSucceeded, ""},
		{"no_coverage cubierto por la ruta", store.RunInconclusive, "no_coverage", nil, reviewed, true, store.RunSucceeded, ""},
		{"queda un archivo pendiente", store.RunInconclusive, "coverage_incomplete", pending, reviewed, true, store.RunInconclusive, "coverage_incomplete"},
		{"la ruta no terminó", store.RunInconclusive, "coverage_incomplete", nil, failed, true, store.RunInconclusive, "coverage_incomplete"},
		{"sin inventario no se concluye", store.RunInconclusive, "coverage_incomplete", nil, reviewed, false, store.RunInconclusive, "coverage_incomplete"},
		{"un fallo del proveedor no se convierte", store.RunInconclusive, "timeout", nil, reviewed, true, store.RunInconclusive, "timeout"},
		{"un run fallido no cambia", store.RunFailed, "llm_error", nil, reviewed, true, store.RunFailed, "llm_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code := settledRunStatus(tt.status, tt.code, tt.pending, tt.routed, tt.inventoryOK)
			if status != tt.wantStatus || code != tt.wantCode {
				t.Errorf("settledRunStatus() = %q/%q, want %q/%q", status, code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestCanChunkNativeRun(t *testing.T) {
	tests := []struct {
		status, code string
		want         bool
	}{
		{store.RunSucceeded, "", true},
		{store.RunInconclusive, "no_coverage", true},
		{store.RunInconclusive, "coverage_incomplete", true},
		{store.RunInconclusive, "token_budget", false},
		{store.RunInconclusive, "timeout", false},
		{store.RunInconclusive, "ocr_error", false},
		{store.RunFailed, "config", false},
		{store.RunFailed, "llm_error", false},
	}
	for _, tt := range tests {
		if got := canChunkNativeRun(tt.status, tt.code); got != tt.want {
			t.Errorf("canChunkNativeRun(%q, %q) = %v, want %v", tt.status, tt.code, got, tt.want)
		}
	}
	if got := chunkByteBudget(270000); got != 243000 {
		t.Errorf("chunkByteBudget(270000) = %d, want 243000", got)
	}
	if got := chunkByteBudget(10); got != 9 {
		t.Errorf("chunkByteBudget(10) = %d, want 9", got)
	}
	if got := chunkByteBudget(0); got != 0 {
		t.Errorf("chunkByteBudget(0) = %d, want 0", got)
	}
}
