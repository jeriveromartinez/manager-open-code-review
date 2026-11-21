package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
	"gorm.io/datatypes"
)

// runTestEvents and runTestJobs record what the orchestrator publishes and
// enqueues after the terminal transaction.
type runTestEvents struct {
	mu    sync.Mutex
	items []events.Event
}

func (p *runTestEvents) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, ev)
	return nil
}

func (p *runTestEvents) has(ev events.Event) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, got := range p.items {
		if got == ev {
			return true
		}
	}
	return false
}

func (p *runTestEvents) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

func (p *runTestEvents) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.items...)
}

type runTestJobs struct {
	mu      sync.Mutex
	review  []uint
	publish []uint
	health  []uint
}

func (j *runTestJobs) EnqueueReview(_ context.Context, id uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.review = append(j.review, id)
	return nil
}

func (j *runTestJobs) EnqueuePublish(_ context.Context, id uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.publish = append(j.publish, id)
	return nil
}

func (j *runTestJobs) EnqueueLLMHealth(_ context.Context, id uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.health = append(j.health, id)
	return nil
}

func (j *runTestJobs) published() []uint {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]uint(nil), j.publish...)
}

func (j *runTestJobs) reviews() []uint {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]uint(nil), j.review...)
}

type runTestFixture struct {
	s        *store.Store
	box      *crypto.Box
	ctx      context.Context
	root     string
	bare     string
	baseSHA  string
	headSHA  string
	pr       store.PullRequest
	runID    uint
	ocrBin   string
	agentBin string
	calls    string
	events   *runTestEvents
	jobs     *runTestJobs
	policy   store.QAPolicy
}

func writeRunTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newRunTestFixture crea un repo bare con refs/pull/1/head, la configuración en
// Postgres y los dobles de ocr/opencode. El nombre del repo es "acme/origin"
// para que GitBaseURL + "/" + FullName + ".git" apunte al bare local y la
// publicación resuelva owner/repo.
func newRunTestFixture(t *testing.T) *runTestFixture {
	t.Helper()

	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "acme", "origin.git")
	runGit(t, root, "init", "-q", "-b", "main", work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")
	writeRunTestFile(t, filepath.Join(work, "main.go"), "package main\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "base")
	baseSHA := runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-qb", "feature")
	writeRunTestFile(t, filepath.Join(work, "auth.go"), "package main\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "feature")
	headSHA := runGit(t, work, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("create bare parent: %v", err)
	}
	runGit(t, root, "clone", "-q", "--bare", work, bare)
	runGit(t, bare, "update-ref", "refs/pull/1/head", headSHA)

	ctx := context.Background()
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	sealedKey, err := box.Seal([]byte("sk-test"))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	sealedToken, err := box.Seal([]byte("clone-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}

	team := store.Team{Name: "qa", Slug: "qa"}
	mustCreateRunTest(t, s, ctx, &team)
	profile := store.LLMProfile{
		Name: "reviewer", Provider: "anthropic", Model: "claude-sonnet",
		APIKeyEnc: sealedKey, Enabled: true, HealthStatus: store.HealthUnknown,
	}
	mustCreateRunTest(t, s, ctx, &profile)
	installation := store.Installation{ID: 77, AccountLogin: "acme", AccountType: "Organization"}
	mustCreateRunTest(t, s, ctx, &installation)
	teamID := team.ID
	repo := store.Repository{
		ID: 88, InstallationID: 77, FullName: "acme/origin", DefaultBranch: "main",
		Enabled: true, TeamID: &teamID,
	}
	mustCreateRunTest(t, s, ctx, &repo)
	policy := store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID, BlockingThreshold: "major",
		ExcludeGlobs:    datatypes.JSON([]byte(`[]`)),
		MaxFilesPerPass: 2, MaxDiffLinesPerPass: 1000, MaxPasses: 2,
	}
	mustCreateRunTest(t, s, ctx, &policy)
	pr := store.PullRequest{
		RepositoryID: 88, Number: 1, Title: "PR de prueba",
		HeadSHA: headSHA, BaseRef: "main", State: "open",
		QAStatus: store.StatusPending, GitHubUpdatedAt: time.Now(),
	}
	mustCreateRunTest(t, s, ctx, &pr)
	run, err := s.CreateReviewRun(ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: headSHA,
		BaseSHA: baseSHA, CloneTokenEnc: sealedToken,
	})
	if err != nil {
		t.Fatalf("create review run: %v", err)
	}

	f := &runTestFixture{
		s: s, box: box, ctx: ctx, root: root, bare: bare,
		baseSHA: baseSHA, headSHA: headSHA,
		pr: pr, runID: run.ID, events: &runTestEvents{}, jobs: &runTestJobs{},
		policy: policy, calls: filepath.Join(root, "agent-calls.txt"),
	}
	f.setOCR(t, runTestPreview(baseSHA, []string{"auth.go", "main.go"}),
		runTestRules([]string{"auth.go", "main.go"}), false)
	f.setAgent(t, runTestOutput("auth.go"), false)
	return f
}

func mustCreateRunTest(t *testing.T, s *store.Store, ctx context.Context, row any) {
	t.Helper()
	if err := s.DB.WithContext(ctx).Create(row).Error; err != nil {
		t.Fatalf("create %T: %v", row, err)
	}
}

func runTestPreview(baseSHA string, paths []string) string {
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		files = append(files, fmt.Sprintf(`{"path":%q,"status":"modified","insertions":3,"deletions":1}`, p))
	}
	return fmt.Sprintf(`{"mode":"delegate","merge_base":%q,"reviewable_files":[%s],"excluded_files":[]}`,
		baseSHA, strings.Join(files, ","))
}

func runTestRules(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, strconv.Quote(p))
	}
	return fmt.Sprintf(`{"groups":[{"rule":"revisar cambios","files":[%s]}]}`, strings.Join(quoted, ","))
}

func runTestOutput(path string) string {
	return fmt.Sprintf(`{"summary":"Revisión completada","coverage":{"reviewed_files":2,"skipped_files":0},"previous_findings":[],"findings":[{"path":%q,"severity":"major","category":"security","title":"Comparación insegura","body":"usar hash seguro","suggestion":"","confidence":"high","start_line":3,"end_line":3}]}`, path)
}

func (f *runTestFixture) setOCR(t *testing.T, preview, rules string, fail bool) {
	t.Helper()
	dir := filepath.Join(f.root, "ocr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	failLine := ""
	if fail {
		failLine = "echo 'ocr no disponible' >&2\nexit 1\n"
	}
	script := "#!/bin/sh\nset -eu\n" + failLine + `
if [ -n "${RUN_FAKE_OCR_ARGV:-}" ]; then
    printf '%s\n' "$@" >>"$RUN_FAKE_OCR_ARGV"
fi
dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "${2:-}" in
    preview) cat "$dir/preview.json" ;;
    rule) cat "$dir/rules.json" ;;
    *) echo "subcomando desconocido" >&2; exit 2 ;;
esac
`
	bin := filepath.Join(dir, "ocr.sh")
	writeRunTestFile(t, bin, script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunTestFile(t, filepath.Join(dir, "preview.json"), preview)
	writeRunTestFile(t, filepath.Join(dir, "rules.json"), rules)
	f.ocrBin = bin
}

func (f *runTestFixture) setAgent(t *testing.T, output string, fail bool) {
	t.Helper()
	dir := filepath.Join(f.root, "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "fakeopencode-review.sh"))
	if err != nil {
		t.Fatalf("read fake agent script: %v", err)
	}
	bin := filepath.Join(dir, "opencode.sh")
	writeRunTestFile(t, bin, string(src))
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRunTestFile(t, filepath.Join(dir, "output.json"), output)
	t.Setenv("RUN_FAKE_AGENT_CALLS", f.calls)
	f.agentBin = bin
	if fail {
		t.Setenv("RUN_FAKE_AGENT_FAIL", "1")
	}
}

func (f *runTestFixture) runner() *Runner {
	return &Runner{
		Store:  f.s,
		Box:    f.box,
		Events: f.events,
		Jobs:   f.jobs,
		Cfg: RunnerConfig{
			WorkspaceDir:  filepath.Join(f.root, "ws"),
			OCRBin:        f.ocrBin,
			OpencodeBin:   f.agentBin,
			GitBaseURL:    "file://" + f.root,
			FetchDepthMax: 10,
			Timeout:       30 * time.Second,
		},
	}
}

func (f *runTestFixture) loadRun(t *testing.T) store.ReviewRun {
	t.Helper()
	run, err := f.s.ReviewRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return run
}

func (f *runTestFixture) loadPR(t *testing.T) store.PullRequest {
	t.Helper()
	var pr store.PullRequest
	if err := f.s.DB.WithContext(f.ctx).First(&pr, f.pr.ID).Error; err != nil {
		t.Fatalf("reload pull request: %v", err)
	}
	return pr
}

func (f *runTestFixture) agentCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read agent calls: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func assertRunTerminal(t *testing.T, f *runTestFixture, status, code string) store.ReviewRun {
	t.Helper()
	run := f.loadRun(t)
	if run.Status != status {
		t.Errorf("run status = %q, want %q (error %q: %q)", run.Status, status, run.ErrorCode, run.ErrorMessage)
	}
	if run.ErrorCode != code {
		t.Errorf("error_code = %q, want %q (message: %q)", run.ErrorCode, code, run.ErrorMessage)
	}
	if run.FinishedAt == nil {
		t.Error("finished_at is nil")
	}
	return run
}

func assertPublishedAndQueued(t *testing.T, f *runTestFixture, wantRunID uint) {
	t.Helper()
	want := events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(wantRunID), 10)}
	if !f.events.has(want) {
		t.Errorf("missing event %+v; got %+v", want, f.events.items)
	}
	if got := f.jobs.published(); len(got) != 1 || got[0] != wantRunID {
		t.Errorf("EnqueuePublish calls = %v, want [%d]", got, wantRunID)
	}
}

func TestRunErrorClassification(t *testing.T) {
	ctx := context.Background()
	out := runOutcome{coverage: []byte(`{"reviewed_files":2,"skipped_files":0}`), usage: []byte(`{}`)}

	tests := []struct {
		name       string
		err        error
		wantStatus string
		wantCode   string
	}{
		// The original fixture "fatal: 401 Unauthorized" is not a message git
		// actually emits, so it could not tell a real remote 401 apart from a
		// bare number. It now carries the genuine git wording.
		{"401 del remote", errors.New("git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': The requested URL returned error: 401"), store.RunInconclusive, "token_expired"},
		{"403 del remote", errors.New("git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': The requested URL returned error: 403"), store.RunInconclusive, "token_expired"},
		{"HTTP 403", errors.New("git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': HTTP 403"), store.RunInconclusive, "token_expired"},
		{"autenticación", errors.New("fatal: Authentication failed for remote"), store.RunInconclusive, "token_expired"},
		{"usuario no legible", errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled"), store.RunInconclusive, "token_expired"},
		{"clave pública", errors.New("git@github.com: Permission denied (publickey)."), store.RunInconclusive, "token_expired"},
		{"clave pública y password", errors.New("git@github.com: Permission denied (publickey,password)."), store.RunInconclusive, "token_expired"},
		{"no autorizado", errors.New("remote: Unauthorized"), store.RunInconclusive, "token_expired"},
		{"token de instalación inválido", errors.New("remote: Invalid username or password."), store.RunInconclusive, "token_expired"},
		{"credenciales rechazadas", errors.New("remote: Bad credentials"), store.RunInconclusive, "token_expired"},
		{"red", errors.New("git fetch: EOF"), store.RunFailed, "git_error"},
		// A bare "401" inside a path, a SHA or an unrelated count is not an HTTP
		// status: telling the operator to reinstall the App for those is a
		// misleading and expensive action.
		{"401 en la ref", errors.New("git fetch: exit status 128: fatal: couldn't find remote ref refs/pull/1401/head"), store.RunFailed, "git_error"},
		{"401 en la ruta", errors.New("git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': Could not resolve host: git-run-401.internal"), store.RunFailed, "git_error"},
		{"401 en un SHA", errors.New("git fetch: exit status 128: fatal: reference is not a tree: 4f401a2"), store.RunFailed, "git_error"},
		{"401 en un contador", errors.New("git fetch: exit status 128: fatal: fetch-pack: protocol error after 401 packets"), store.RunFailed, "git_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, retryErr := classifyCloneError(ctx, out, tt.err)
			if retryErr != nil {
				t.Fatalf("classifyCloneError() retry error = %v, want nil", retryErr)
			}
			if got.status != tt.wantStatus || got.code != tt.wantCode {
				t.Errorf("status/code = %q/%q, want %q/%q", got.status, got.code, tt.wantStatus, tt.wantCode)
			}
			if string(got.coverage) != string(out.coverage) {
				t.Errorf("coverage = %q, want %q", got.coverage, out.coverage)
			}
		})
	}

	deadline, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if got, _ := classifyAgentError(deadline, out, context.DeadlineExceeded); got.status != store.RunInconclusive || got.code != "timeout" {
		t.Errorf("deadline classification = %q/%q, want inconclusive/timeout", got.status, got.code)
	}
	if got, _ := classifyOCR(ctx, out, errors.New("invalid JSON")); got.code != "ocr_error" {
		t.Errorf("OCR classification code = %q, want ocr_error", got.code)
	}
	if got, _ := classifyAgentError(ctx, out, errors.New("quota")); got.code != "llm_error" {
		t.Errorf("agent classification code = %q, want llm_error", got.code)
	}
	if got, retryErr := classifyAgentError(ctx, out, &ProfileCapacityError{ProfileID: 3}); retryErr == nil || got.status != "" {
		t.Errorf("capacity classification = %q/%v, want the error and no outcome", got.status, retryErr)
	}
	if got := truncateRunMessage(strings.Repeat("x", 2000)); len(got) != maxRunErrorMessage {
		t.Errorf("truncated message has %d chars, want %d", len(got), maxRunErrorMessage)
	}
}

func TestRunStopsWhenBudgetIsExhausted(t *testing.T) {
	f := newRunTestFixture(t)
	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	f.setOCR(t, runTestPreview(f.baseSHA, paths), runTestRules(paths), false)
	f.setAgent(t, runTestOutput("a.go"), false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Updates(map[string]any{
		"max_files_per_pass": 2, "max_passes": 3,
	}).Error; err != nil {
		t.Fatal(err)
	}

	runner := f.runner()
	// El presupuesto se agota antes de que arranque la primera pasada.
	runner.Cfg.Timeout = time.Nanosecond
	time.Sleep(2 * time.Millisecond)

	if err := runner.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "budget_exhausted")
	if !strings.Contains(run.ErrorMessage, "pasada 1 de 3") {
		t.Errorf("error_message = %q, want it to name the aborted pass", run.ErrorMessage)
	}
	if !strings.Contains(run.ErrorMessage, "6 archivos sin revisar") {
		t.Errorf("error_message = %q, want it to count the unreviewed files", run.ErrorMessage)
	}
	if !strings.Contains(run.ErrorMessage, "review_timeout en Ajustes → Revisión") {
		t.Errorf("error_message = %q, want it to point at the review_timeout panel setting", run.ErrorMessage)
	}
	if strings.Contains(run.ErrorMessage, "QA_REVIEW_TIMEOUT") {
		t.Errorf("error_message = %q, want no retired environment variable", run.ErrorMessage)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0 (no pass may start without budget)", got)
	}

	var coverage struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	}
	if err := json.Unmarshal(run.Coverage, &coverage); err != nil {
		t.Fatalf("decode coverage %q: %v", run.Coverage, err)
	}
	if coverage.ReviewedFiles != 0 || coverage.SkippedFiles != 6 {
		t.Errorf("coverage = %+v, want 0 reviewed / 6 skipped", coverage)
	}
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunLastPassTimeoutStaysTimeout(t *testing.T) {
	f := newRunTestFixture(t)
	paths := []string{"a.go", "b.go"}
	f.setOCR(t, runTestPreview(f.baseSHA, paths), runTestRules(paths), false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Updates(map[string]any{
		"max_files_per_pass": 1, "max_passes": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// La primera pasada responde y la última se cuelga: sin pasadas detrás, el
	// corte es un timeout de pasada y no agotamiento del run.
	f.setAgent(t, runTestOutput("a.go"), false)
	t.Setenv("RUN_FAKE_AGENT_SLEEP_ON_CALL", "2")

	runner := f.runner()
	runner.Cfg.Timeout = 300 * time.Millisecond

	if err := runner.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "timeout")
}

func TestRunBudgetShrinksWithEachPass(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	budget := newRunBudget(90*time.Second, clock)

	first, ok := budget.timeout()
	if !ok || first != 90*time.Second {
		t.Fatalf("first pass timeout = %v/%v, want 90s/true", first, ok)
	}
	now = now.Add(40 * time.Second)
	second, ok := budget.timeout()
	if !ok || second != 50*time.Second {
		t.Fatalf("second pass timeout = %v/%v, want 50s/true", second, ok)
	}
	now = now.Add(20 * time.Second)
	third, ok := budget.timeout()
	if !ok || third != 30*time.Second {
		t.Fatalf("third pass timeout = %v/%v, want 30s/true", third, ok)
	}
	if !(first > second && second > third) {
		t.Errorf("per-pass timeouts did not shrink: %v, %v, %v", first, second, third)
	}

	now = now.Add(45 * time.Second)
	if got := budget.remaining(); got != 0 {
		t.Errorf("remaining() past the deadline = %v, want 0", got)
	}
	if timeout, ok := budget.timeout(); ok {
		t.Errorf("timeout() with no budget left = %v/true, want not ok", timeout)
	}
}

func TestRunBudgetWithoutTimeoutIsUnlimited(t *testing.T) {
	budget := newRunBudget(0, nil)
	if timeout, ok := budget.timeout(); !ok || timeout != 0 {
		t.Fatalf("timeout() without a configured budget = %v/%v, want 0/true", timeout, ok)
	}
	if got := budget.remaining(); got != 0 {
		t.Errorf("remaining() = %v, want 0", got)
	}
}

func TestRunBudgetCutIsNotASinglePassTimeout(t *testing.T) {
	if !budgetCut(context.DeadlineExceeded, 2) {
		t.Error("deadline with passes left should be budget exhaustion")
	}
	if budgetCut(context.DeadlineExceeded, 0) {
		t.Error("deadline on the last pass should stay a plain timeout")
	}
	if budgetCut(errors.New("provider quota"), 2) {
		t.Error("a non-timeout failure must not be classified as budget exhaustion")
	}
}

func TestRunBlockingThreshold(t *testing.T) {
	findings := []store.Finding{
		{Status: store.FindingOpen, Severity: "minor"},
		{Status: store.FindingResolved, Severity: "blocker"},
	}
	if hasBlockingFinding(findings, "major") {
		t.Error("minor open finding should not block at major threshold")
	}
	findings[0].Severity = "major"
	if !hasBlockingFinding(findings, "major") {
		t.Error("major open finding should block at major threshold")
	}
	if hasBlockingFinding(findings, "blocker") {
		t.Error("resolved blocker should never block")
	}
}

func TestRunPipelineSucceeds(t *testing.T) {
	f := newRunTestFixture(t)
	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	run := assertRunTerminal(t, f, store.RunSucceeded, "")
	if run.Summary == "" {
		t.Error("summary is empty")
	}
	if pr := f.loadPR(t); pr.QAStatus != store.StatusChangesRequested {
		t.Errorf("qa_status = %q, want %q", pr.QAStatus, store.StatusChangesRequested)
	}

	findings, err := f.s.FindingsForPR(f.ctx, f.pr.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	got := findings[0]
	wantFP := Fingerprint(AgentFinding{
		Path: "auth.go", Category: "security", Title: "Comparación insegura",
		StartLine: 3, EndLine: 3,
	}, nil)
	if got.Fingerprint != wantFP {
		t.Errorf("fingerprint = %q, want %q", got.Fingerprint, wantFP)
	}
	if got.PullRequestID != f.pr.ID {
		t.Errorf("finding pull_request_id = %d, want %d", got.PullRequestID, f.pr.ID)
	}
	if got.Status != store.FindingOpen || got.Severity != "major" {
		t.Errorf("finding = %+v, want open major", got)
	}
	if got.LastSeenRunID != f.runID {
		t.Errorf("last_seen_run_id = %d, want %d", got.LastSeenRunID, f.runID)
	}

	assertPublishedAndQueued(t, f, f.runID)

	workspace := filepath.Join(f.root, "ws", fmt.Sprintf("run-%d", f.runID))
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("workspace %q still exists (err=%v)", workspace, err)
	}
}

func TestRunInconclusiveWhenOCRMissing(t *testing.T) {
	f := newRunTestFixture(t)
	f.ocrBin = "/bin/false"

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "ocr_error")
	if pr := f.loadPR(t); pr.QAStatus != store.StatusInconclusive {
		t.Errorf("qa_status = %q, want %q", pr.QAStatus, store.StatusInconclusive)
	}
	assertPublishedAndQueued(t, f, f.runID)
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0 (OCR failed first)", got)
	}
}

func TestRunInconclusiveLeavesFindingsOpen(t *testing.T) {
	f := newRunTestFixture(t)
	seed := store.Finding{
		ID: "seed-finding", PullRequestID: f.pr.ID, Fingerprint: "seed-fp",
		Path: "main.go", Severity: "minor", Status: store.FindingOpen,
		FirstSeenRunID: f.runID, LastSeenRunID: f.runID,
	}
	mustCreateRunTest(t, f.s, f.ctx, &seed)
	f.ocrBin = "/bin/false"

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "ocr_error")

	var got store.Finding
	if err := f.s.DB.WithContext(f.ctx).Where("id = ?", "seed-finding").Take(&got).Error; err != nil {
		t.Fatalf("reload seeded finding: %v", err)
	}
	if got.Status != store.FindingOpen {
		t.Errorf("seeded finding status = %q, want %q", got.Status, store.FindingOpen)
	}
	if got.LastSeenRunID != seed.LastSeenRunID {
		t.Errorf("seeded finding last_seen_run_id = %d, want %d", got.LastSeenRunID, seed.LastSeenRunID)
	}
}

func TestRunInconclusivePreservesFindingsFromCompletedPasses(t *testing.T) {
	f := newRunTestFixture(t)
	// Dos archivos con MaxFilesPerPass=1 producen dos pasadas: la primera
	// termina con un hallazgo válido y la segunda falla en el agente.
	f.setOCR(t, runTestPreview(f.baseSHA, []string{"auth.go", "main.go"}),
		runTestRules([]string{"auth.go", "main.go"}), false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Updates(map[string]any{
		"max_files_per_pass": 1, "max_passes": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}
	f.setAgent(t, runTestOutput("auth.go"), false)
	t.Setenv("RUN_FAKE_AGENT_FAIL_ON_CALL", "2")

	// Un hallazgo abierto preexistente no debe resolverse (complete=false).
	seed := store.Finding{
		ID: "seed-finding", PullRequestID: f.pr.ID, Fingerprint: "seed-fp",
		Path: "main.go", Severity: "minor", Status: store.FindingOpen,
		FirstSeenRunID: 1, LastSeenRunID: 1,
	}
	mustCreateRunTest(t, f.s, f.ctx, &seed)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "llm_error")
	if got := f.agentCalls(t); got != 2 {
		t.Errorf("agent calls = %d, want 2", got)
	}

	findings, err := f.s.FindingsForPR(f.ctx, f.pr.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2 (uno de la pasada 1 + el sembrado): %+v", len(findings), findings)
	}

	var fromPass, seeded store.Finding
	for _, finding := range findings {
		switch finding.ID {
		case seed.ID:
			seeded = finding
		default:
			fromPass = finding
		}
	}
	wantFP := Fingerprint(AgentFinding{
		Path: "auth.go", Category: "security", Title: "Comparación insegura",
		StartLine: 3, EndLine: 3,
	}, nil)
	if fromPass.Fingerprint != wantFP {
		t.Errorf("nuevo hallazgo fingerprint = %q, want %q", fromPass.Fingerprint, wantFP)
	}
	if fromPass.Status != store.FindingOpen || fromPass.Severity != "major" {
		t.Errorf("nuevo hallazgo = %+v, want open major", fromPass)
	}
	if fromPass.PullRequestID != f.pr.ID {
		t.Errorf("nuevo hallazgo pull_request_id = %d, want %d", fromPass.PullRequestID, f.pr.ID)
	}
	if fromPass.LastSeenRunID != f.runID {
		t.Errorf("nuevo hallazgo last_seen_run_id = %d, want %d", fromPass.LastSeenRunID, f.runID)
	}
	if seeded.Status != store.FindingOpen {
		t.Errorf("hallazgo sembrado status = %q, want %q", seeded.Status, store.FindingOpen)
	}
	if seeded.LastSeenRunID != seed.LastSeenRunID {
		t.Errorf("hallazgo sembrado last_seen_run_id = %d, want %d", seeded.LastSeenRunID, seed.LastSeenRunID)
	}
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunSupersededDoesNotPublish(t *testing.T) {
	f := newRunTestFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", f.runID).Update("status", store.RunSuperseded).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := f.loadRun(t)
	if run.Status != store.RunSuperseded {
		t.Errorf("run status = %q, want %q", run.Status, store.RunSuperseded)
	}
	if run.FinishedAt != nil {
		t.Error("superseded run was finished")
	}
	if got := f.jobs.published(); len(got) != 0 {
		t.Errorf("EnqueuePublish calls = %v, want none", got)
	}
	if got := f.events.len(); got != 0 {
		t.Errorf("published %d events, want none", got)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0", got)
	}
}

func TestRunHeadMovedBeforeCheckoutIsSuperseded(t *testing.T) {
	f := newRunTestFixture(t)

	work := filepath.Join(f.root, "work")
	writeRunTestFile(t, filepath.Join(work, "later.go"), "package main\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "later")
	runGit(t, work, "push", "-q", f.bare, "HEAD:refs/pull/1/head")

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSuperseded, "head_moved")
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0", got)
	}
	assertPublishedAndQueued(t, f, f.runID)
	if pr := f.loadPR(t); pr.QAStatus != store.StatusPending {
		t.Errorf("qa_status = %q, want unchanged %q", pr.QAStatus, store.StatusPending)
	}
}

func TestRunRetriesSchemaOnceThenSucceeds(t *testing.T) {
	f := newRunTestFixture(t)
	dir := filepath.Dir(f.agentBin)
	writeRunTestFile(t, filepath.Join(dir, "invalid.json"), `{"summary":""}`)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
	if got := f.agentCalls(t); got != 2 {
		t.Errorf("agent calls = %d, want 2 (one retry)", got)
	}
}

func TestRunSchemaInvalidTwiceIsInconclusive(t *testing.T) {
	f := newRunTestFixture(t)
	dir := filepath.Dir(f.agentBin)
	invalid := filepath.Join(dir, "always-invalid.json")
	writeRunTestFile(t, invalid, `{"summary":""}`)
	t.Setenv("RUN_FAKE_AGENT_OUTPUT", invalid)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "schema_invalid")
	if got := f.agentCalls(t); got != 2 {
		t.Errorf("agent calls = %d, want 2", got)
	}
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunAgentFailureIsLLMError(t *testing.T) {
	f := newRunTestFixture(t)
	f.setAgent(t, runTestOutput("auth.go"), true)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "llm_error")
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunAgentTimeoutIsTimeout(t *testing.T) {
	f := newRunTestFixture(t)
	slow := filepath.Join(filepath.Dir(f.agentBin), "slow-opencode.sh")
	writeRunTestFile(t, slow, "#!/bin/sh\nsleep 5\n")
	if err := os.Chmod(slow, 0o755); err != nil {
		t.Fatal(err)
	}
	f.agentBin = slow
	runner := f.runner()
	runner.Cfg.Timeout = 100 * time.Millisecond

	if err := runner.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "timeout")
}

func TestRunMissingTokenIsTokenExpired(t *testing.T) {
	f := newRunTestFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", f.runID).Update("clone_token_enc", "").Error; err != nil {
		t.Fatal(err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunInconclusive, "token_expired")
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunGitFailureIsFailed(t *testing.T) {
	f := newRunTestFixture(t)
	runner := f.runner()
	runner.Cfg.GitBaseURL = "file://" + filepath.Join(f.root, "missing")

	if err := runner.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunFailed, "git_error")
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunCoverageIncomplete(t *testing.T) {
	f := newRunTestFixture(t)
	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	f.setOCR(t, runTestPreview(f.baseSHA, paths), runTestRules(paths), false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Updates(map[string]any{
		"max_files_per_pass": 2, "max_passes": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunInconclusive, "coverage_incomplete")
	var coverage struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	}
	if err := json.Unmarshal(run.Coverage, &coverage); err != nil {
		t.Fatalf("decode coverage: %v", err)
	}
	if coverage.ReviewedFiles != 2 || coverage.SkippedFiles != 3 {
		t.Errorf("coverage = %+v, want 2 reviewed / 3 skipped", coverage)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0", got)
	}
}

func TestRunInvalidConfigIsFailed(t *testing.T) {
	f := newRunTestFixture(t)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", f.policy.LLMProfileID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunFailed, "config")
	if pr := f.loadPR(t); pr.QAStatus != store.StatusInconclusive {
		t.Errorf("qa_status = %q, want %q", pr.QAStatus, store.StatusInconclusive)
	}
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunSucceededResolvesStaleFindings(t *testing.T) {
	f := newRunTestFixture(t)
	seed := store.Finding{
		ID: "stale-finding", PullRequestID: f.pr.ID, Fingerprint: "stale-fp",
		Path: "main.go", Severity: "major", Status: store.FindingOpen,
		FirstSeenRunID: 1, LastSeenRunID: 1,
	}
	mustCreateRunTest(t, f.s, f.ctx, &seed)
	f.setAgent(t, `{"summary":"Sin hallazgos","coverage":{"reviewed_files":2,"skipped_files":0},"previous_findings":[],"findings":[]}`, false)

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
	var got store.Finding
	if err := f.s.DB.WithContext(f.ctx).Where("id = ?", seed.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload seeded finding: %v", err)
	}
	if got.Status != store.FindingResolved {
		t.Errorf("seeded finding status = %q, want %q", got.Status, store.FindingResolved)
	}
	if got.LastSeenRunID != f.runID {
		t.Errorf("seeded finding last_seen_run_id = %d, want %d", got.LastSeenRunID, f.runID)
	}
	if pr := f.loadPR(t); pr.QAStatus != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", pr.QAStatus, store.StatusReadyForHuman)
	}
}

func TestRunSaturatedProfileIsRequeued(t *testing.T) {
	f := newRunTestFixture(t)

	release, acquired, err := f.s.AcquireProfileSlot(f.ctx, f.policy.LLMProfileID)
	if err != nil || !acquired {
		t.Fatalf("hold the profile slot = %v/%v", acquired, err)
	}
	defer release()

	err = f.runner().Run(f.ctx, f.runID)
	var capacity *ProfileCapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("Run() error = %v, want a ProfileCapacityError", err)
	}
	if capacity.ProfileID != f.policy.LLMProfileID {
		t.Errorf("capacity profile = %d, want %d", capacity.ProfileID, f.policy.LLMProfileID)
	}
	if capacity.Wait <= 0 {
		t.Errorf("capacity wait = %v, want a positive backoff", capacity.Wait)
	}

	run := f.loadRun(t)
	if run.Status != store.RunQueued {
		t.Errorf("run status = %q, want %q (error %q: %q)", run.Status, store.RunQueued, run.ErrorCode, run.ErrorMessage)
	}
	if run.FinishedAt != nil {
		t.Error("a run waiting for capacity was finished")
	}
	if run.Attempt != 2 {
		t.Errorf("run attempt = %d, want 2 (the wait is recorded)", run.Attempt)
	}
	if run.ErrorCode != capacityErrorCode {
		t.Errorf("run error_code = %q, want %q", run.ErrorCode, capacityErrorCode)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0", got)
	}
	if got := f.jobs.published(); len(got) != 0 {
		t.Errorf("EnqueuePublish calls = %v, want none for a requeued run", got)
	}
	// The claim and the return to the queue are both announced, so the panel
	// stops showing the run as started; there is no terminal event.
	prEvent := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)}
	want := []events.Event{prEvent, prEvent}
	if got := f.events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("events = %+v, want the claim and the requeue %+v", got, want)
	}

	// Releasing the slot lets the retried run go through.
	release()
	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("retried Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
}

func TestRunSaturatedProfileFailsAfterWaitingBudget(t *testing.T) {
	f := newRunTestFixture(t)

	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", f.runID).Update("attempt", maxCapacityWaits+1).Error; err != nil {
		t.Fatal(err)
	}
	release, acquired, err := f.s.AcquireProfileSlot(f.ctx, f.policy.LLMProfileID)
	if err != nil || !acquired {
		t.Fatalf("hold the profile slot = %v/%v", acquired, err)
	}
	defer release()

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertRunTerminal(t, f, store.RunFailed, capacityErrorCode)
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0", got)
	}
	assertPublishedAndQueued(t, f, f.runID)
}

func TestRunCapacityWaitGrowsAndIsBounded(t *testing.T) {
	if first, second := CapacityWait(1), CapacityWait(2); second <= first {
		t.Errorf("CapacityWait(2) = %v, want more than CapacityWait(1) = %v", second, first)
	}
	if got := CapacityWait(1000); got != capacityWaitMax {
		t.Errorf("CapacityWait(1000) = %v, want the %v cap", got, capacityWaitMax)
	}
	if got := CapacityWait(0); got != capacityWaitBase {
		t.Errorf("CapacityWait(0) = %v, want the %v base", got, capacityWaitBase)
	}
}

func TestRunDuplicateDeliveryIsNoop(t *testing.T) {
	f := newRunTestFixture(t)
	r := f.runner()
	if err := r.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := r.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if got := f.jobs.published(); len(got) != 1 {
		t.Errorf("EnqueuePublish calls = %v, want one", got)
	}
	if got := f.events.len(); got != 3 {
		t.Errorf("events = %d, want the claim pr.updated, run.updated and pr.updated", got)
	}
	wantPR := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)}
	if !f.events.has(wantPR) {
		t.Errorf("missing event %+v; got %+v", wantPR, f.events.items)
	}
}

// The panel must show a run as started as soon as a worker takes it, not only
// when it ends.
func TestClaimNotifiesThePR(t *testing.T) {
	f := newRunTestFixture(t)
	// Move the run id away from the PR id so an event carrying the run id
	// instead of the PR id fails.
	if err := f.s.DB.WithContext(f.ctx).Exec("UPDATE review_runs SET id = id + 1000 WHERE id = ?", f.runID).Error; err != nil {
		t.Fatalf("shift run id: %v", err)
	}
	f.runID += 1000
	r := f.runner()
	if err := r.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	prEvent := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)}
	runEvent := events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(f.runID), 10)}
	want := []events.Event{prEvent, runEvent, prEvent}
	if got := f.events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %+v, want the claim first: %+v", got, want)
	}

	// The run is terminal now, so a redelivery is not claimed and stays silent.
	if err := r.Run(f.ctx, f.runID); err != nil {
		t.Fatalf("unclaimed Run() error = %v", err)
	}
	if got := f.events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("events after an unclaimed run = %+v, want unchanged %+v", got, want)
	}
}

func TestClaimWithFailedNotificationStillRuns(t *testing.T) {
	f := newRunTestFixture(t)
	r := f.runner()
	r.Events = failingRunEvents{}
	if err := r.Run(f.ctx, f.runID); err == nil {
		t.Fatal("Run() error = nil, want the terminal notification failure reported")
	}
	assertRunTerminal(t, f, store.RunSucceeded, "")
}

type failingRunEvents struct{}

func (failingRunEvents) Publish(context.Context, events.Event) error {
	return errors.New("notify unavailable")
}

func TestRunnerCheckProfileDelegates(t *testing.T) {
	f := newRunTestFixture(t)
	r := f.runner()
	if err := r.CheckProfile(f.ctx, f.policy.LLMProfileID); err != nil {
		t.Fatalf("CheckProfile() error = %v", err)
	}
	var profile store.LLMProfile
	if err := f.s.DB.WithContext(f.ctx).First(&profile, f.policy.LLMProfileID).Error; err != nil {
		t.Fatalf("reload profile: %v", err)
	}
	if profile.HealthStatus != store.HealthOK {
		t.Errorf("health_status = %q, want %q", profile.HealthStatus, store.HealthOK)
	}

	r.Cfg.OpencodeBin = "/bin/false"
	if err := r.CheckProfile(f.ctx, f.policy.LLMProfileID); err == nil {
		t.Error("CheckProfile with failing binary returned nil error")
	}
	if err := f.s.DB.WithContext(f.ctx).First(&profile, f.policy.LLMProfileID).Error; err != nil {
		t.Fatalf("reload profile after failure: %v", err)
	}
	if profile.HealthStatus != store.HealthError {
		t.Errorf("health_status = %q, want %q", profile.HealthStatus, store.HealthError)
	}
}

func TestRunCompleteCoversAllFiles(t *testing.T) {
	f := newRunTestFixture(t)
	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go"}
	f.setOCR(t, runTestPreview(f.baseSHA, paths), runTestRules(paths), false)
	f.setAgent(t, runTestOutput("a.go"), false)
	f.policy.MaxFilesPerPass = 3
	f.policy.MaxPasses = 2
	if err := f.s.DB.WithContext(f.ctx).Model(&store.QAPolicy{}).
		Where("id = ?", f.policy.ID).Updates(map[string]any{
		"max_files_per_pass": 3, "max_passes": 2,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	run := assertRunTerminal(t, f, store.RunSucceeded, "")

	var coverage struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	}
	if err := json.Unmarshal(run.Coverage, &coverage); err != nil {
		t.Fatalf("decode coverage %q: %v", run.Coverage, err)
	}
	if coverage.ReviewedFiles != 5 || coverage.SkippedFiles != 0 {
		t.Errorf("coverage = %+v, want 5 reviewed / 0 skipped", coverage)
	}
	if got := f.agentCalls(t); got != 2 {
		t.Errorf("agent calls = %d, want 2 passes", got)
	}

	findings, err := f.s.FindingsForPR(f.ctx, f.pr.ID)
	if err != nil {
		t.Fatalf("findings: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("findings = %d, want 1 after merging duplicate passes: %+v", len(findings), findings)
	}
	assertPublishedAndQueued(t, f, f.runID)
}
