package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type differentialTree struct {
	base  map[string][]byte
	head  map[string][]byte
	reads []string
}

func (t *differentialTree) content(ref string) map[string][]byte {
	if ref == baselineRefHead {
		return t.head
	}
	return t.base
}

func (t *differentialTree) Read(ref, file string) ([]byte, bool, error) {
	t.reads = append(t.reads, ref)
	content, ok := t.content(ref)[file]
	return content, ok, nil
}

func (t *differentialTree) Blob(ref, file string) (string, bool, error) {
	if _, ok := t.content(ref)[file]; !ok {
		return "", false, nil
	}
	return "sha-" + file, true, nil
}

type fakeDifferentialExecutor struct {
	calls []DifferentialCase
	run   func(DifferentialCase) (DifferentialExecution, error)
}

func (f *fakeDifferentialExecutor) Run(_ context.Context, testCase DifferentialCase, _, _ string) (DifferentialExecution, error) {
	f.calls = append(f.calls, testCase)
	if f.run != nil {
		return f.run(testCase)
	}
	return DifferentialExecution{}, nil
}

// passthroughSandbox keeps the executor tests independent from the host: the
// OS sandbox itself is asserted by its command construction test.
type passthroughSandbox struct{}

func (passthroughSandbox) Available() bool { return true }
func (passthroughSandbox) Name() string    { return "test passthrough" }
func (passthroughSandbox) Wrap(ctx context.Context, _, _ string, command []string) (*exec.Cmd, bool) {
	return exec.CommandContext(ctx, command[0], command[1:]...), true
}

func validDifferentialPolicy() []byte {
	return []byte(`{
		"version": "differential-cases/v1",
		"cases": [{
			"id": "review-identity",
			"paths": ["internal/review/*.go"],
			"package": "./internal/review",
			"test": "TestReconcile"
		}]
	}`)
}

func TestParseDifferentialPolicyAcceptsBoundedGoCases(t *testing.T) {
	cases, err := parseDifferentialPolicy(validDifferentialPolicy())
	if err != nil {
		t.Fatalf("parseDifferentialPolicy() error = %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases = %+v, want one case", cases)
	}
	got := cases[0]
	if got.ID != "review-identity" || got.Package != "./internal/review" ||
		got.Test != "TestReconcile" || len(got.Paths) != 1 {
		t.Fatalf("case = %+v, want the declared case", got)
	}
}

func TestParseDifferentialPolicyRejectsUnsafeCommands(t *testing.T) {
	for name, raw := range map[string]string{
		"absolute package":  `{"version":"differential-cases/v1","cases":[{"id":"a","paths":["a.go"],"package":"/tmp/x"}]}`,
		"escaping package":  `{"version":"differential-cases/v1","cases":[{"id":"a","paths":["a.go"],"package":"./../x"}]}`,
		"recursive package": `{"version":"differential-cases/v1","cases":[{"id":"a","paths":["a.go"],"package":"./..."}]}`,
		"shell in test":     `{"version":"differential-cases/v1","cases":[{"id":"a","paths":["a.go"],"package":"./x","test":"TestA; rm -rf /"}]}`,
		"escaping glob":     `{"version":"differential-cases/v1","cases":[{"id":"a","paths":["../*.go"],"package":"./x"}]}`,
		"bad version":       `{"version":"differential-cases/v2","cases":[]}`,
		"duplicate id": `{"version":"differential-cases/v1","cases":[
			{"id":"a","paths":["a.go"],"package":"./x"},
			{"id":"a","paths":["b.go"],"package":"./y"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDifferentialPolicy([]byte(raw)); err == nil {
				t.Fatalf("parseDifferentialPolicy(%s) accepted an unsafe case", name)
			}
		})
	}
}

func TestLoadDifferentialPolicyReadsTheTrustedBaseOnly(t *testing.T) {
	tree := &differentialTree{
		base: map[string][]byte{differentialPolicyPath: validDifferentialPolicy()},
		head: map[string][]byte{differentialPolicyPath: []byte(
			`{"version":"differential-cases/v1","cases":[{"id":"merged","paths":["web/*.ts"],"package":"./web","test":"TestInjected"}]}`)},
	}
	cases, reason := loadDifferentialPolicy(tree, "base-sha")
	if reason != "" {
		t.Fatalf("loadDifferentialPolicy() reason = %q, want none", reason)
	}
	if len(cases) != 1 || cases[0].ID != "review-identity" {
		t.Fatalf("cases = %+v, want the base policy", cases)
	}
	for _, ref := range tree.reads {
		if ref != baselineRefBase {
			t.Errorf("policy was read from %q, want only the trusted base", ref)
		}
	}

	missing := &differentialTree{}
	if cases, reason := loadDifferentialPolicy(missing, "base-sha"); len(cases) != 0 || reason != "" {
		t.Errorf("missing policy = %+v/%q, want a silent opt-out", cases, reason)
	}
	malformed := &differentialTree{base: map[string][]byte{differentialPolicyPath: []byte("{")}}
	if _, reason := loadDifferentialPolicy(malformed, "base-sha"); reason == "" {
		t.Error("malformed policy reported no reason")
	}
	if _, reason := loadDifferentialPolicy(tree, ""); reason == "" {
		t.Error("a comparison without base reported no reason")
	}
}

func TestSelectDifferentialCasesOnlySeverePertinentFindings(t *testing.T) {
	findings := []store.Finding{
		{ID: "major", Path: "internal/review/a.go", Severity: "major", StartLine: 2},
		{ID: "blocker", Path: "internal/review/b.go", Severity: "blocker", StartLine: 1},
		{ID: "minor", Path: "internal/review/c.go", Severity: "minor", StartLine: 3},
		{ID: "advisory", Path: "internal/review/d.go", Severity: "major", Advisory: true},
		{ID: "web", Path: "web/x.ts", Severity: "major"},
	}
	cases := []DifferentialCase{
		{ID: "review", Paths: []string{"internal/review/*.go"}, Package: "./internal/review", Test: "TestA"},
		{ID: "web", Paths: []string{"web/*.ts"}, Package: "./web", Test: "TestB"},
	}
	selections := selectDifferentialCases(findings, cases)
	if len(selections) != 2 {
		t.Fatalf("selections = %+v, want one per pertinent case", selections)
	}
	if selections[0].Case.ID != "review" || len(selections[0].Findings) != 2 {
		t.Errorf("review selection = %+v, want the blocker and the major", selections[0])
	}
	if selections[1].Case.ID != "web" || len(selections[1].Findings) != 1 || selections[1].Findings[0] != "web" {
		t.Errorf("web selection = %+v, want only the web finding", selections[1])
	}
}

func TestClassifyDifferentialOutcomes(t *testing.T) {
	pass := []DifferentialRunResult{{Passed: true, ExitCode: 0, Output: "ok"}}
	fail := []DifferentialRunResult{{ExitCode: 1, Output: "--- FAIL: TestX"}}
	compile := []DifferentialRunResult{{ExitCode: 1, Inconclusive: "compilation", Output: "[build failed]"}}
	timeout := []DifferentialRunResult{{ExitCode: -1, TimedOut: true, Inconclusive: "timeout"}}

	cases := []struct {
		name       string
		base, head []DifferentialRunResult
		status     string
		reason     string
	}{
		{"regression", pass, fail, DifferentialRegression, ""},
		{"pre-existing", fail, fail, DifferentialPreExisting, ""},
		{"both pass", pass, pass, DifferentialPassing, ""},
		{"improved", fail, pass, DifferentialImproved, ""},
		{"flaky base", []DifferentialRunResult{{Passed: true}, {ExitCode: 1}}, fail, DifferentialInconclusive, "base: flaky"},
		{"compilation", pass, compile, DifferentialInconclusive, "head: compilation"},
		{"timeout", timeout, fail, DifferentialInconclusive, "base: timeout"},
		{"not run", nil, fail, DifferentialInconclusive, "base: not_run"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _, _ := classifyDifferential(DifferentialExecution{Base: tt.base, Head: tt.head})
			if status != tt.status {
				t.Errorf("status = %q, want %q", status, tt.status)
			}
			if reason != tt.reason {
				t.Errorf("reason = %q, want %q", reason, tt.reason)
			}
		})
	}
}

func TestDifferentialOutputSliceKeepsTheFailure(t *testing.T) {
	output := strings.Join([]string{
		"=== RUN   TestAdd",
		"noise one", "noise two", "noise three",
		"--- FAIL: TestAdd (0.00s)",
		"    calc_test.go:6: Add returned the wrong value",
		"FAIL\texample.com/differential/calc\t0.001s",
	}, "\n")
	got := differentialOutputSlice(output)
	if !strings.Contains(got, "--- FAIL: TestAdd") || !strings.Contains(got, "wrong value") {
		t.Errorf("slice = %q, want the failure marker and its detail", got)
	}
}

func TestAttachDifferentialEvidenceAttachesRegression(t *testing.T) {
	out := &runOutcome{
		findings: []store.Finding{{
			ID: "f1", Path: "internal/review/a.go", Severity: "major", Status: store.FindingOpen,
		}},
		decisions: []Decision{{
			FindingID: "f1", Verdict: VerdictConfirm,
			Verification: Verification{Verdict: VerdictConfirm, HeadSHA: "head-sha"},
		}},
	}
	tree := &differentialTree{base: map[string][]byte{differentialPolicyPath: validDifferentialPolicy()}}
	executor := &fakeDifferentialExecutor{run: func(DifferentialCase) (DifferentialExecution, error) {
		return DifferentialExecution{
			Environment: "go version go1.27 linux/amd64",
			Base:        []DifferentialRunResult{{Passed: true}, {Passed: true}},
			Head: []DifferentialRunResult{
				{ExitCode: 1, Output: "--- FAIL: TestReconcile"},
				{ExitCode: 1, Output: "--- FAIL: TestReconcile"},
			},
		}, nil
	}}
	r := &Runner{Differential: executor}
	r.attachDifferentialEvidenceWith(context.Background(), tree, "/repo", "base-sha", "head-sha", true, out, nil)

	if len(executor.calls) != 1 || executor.calls[0].ID != "review-identity" {
		t.Fatalf("calls = %+v, want the policy case exactly once", executor.calls)
	}
	evidence := out.decisions[0].Verification.Differential
	if evidence == nil {
		t.Fatal("no differential evidence attached")
	}
	if evidence.Status != DifferentialRegression || evidence.BaseSHA != "base-sha" || evidence.HeadSHA != "head-sha" {
		t.Errorf("evidence = %+v, want the regression bound to both SHAs", evidence)
	}
	if evidence.Environment != "go version go1.27 linux/amd64" || evidence.Attempts != 2 {
		t.Errorf("evidence environment/attempts = %q/%d", evidence.Environment, evidence.Attempts)
	}
	if !evidence.BasePassed || evidence.HeadPassed {
		t.Errorf("side flags = %v/%v, want base pass and head failure", evidence.BasePassed, evidence.HeadPassed)
	}
	if out.decisions[0].Verdict != VerdictConfirm || out.decisions[0].Apply {
		t.Errorf("decision = %+v, want the verdict untouched", out.decisions[0])
	}
}

func TestDifferentialEvidenceNeverPromotesAnUncertainVerdict(t *testing.T) {
	out := &runOutcome{
		findings: []store.Finding{{
			ID: "f1", Path: "internal/review/a.go", Severity: "major", Status: store.FindingOpen,
		}},
		decisions: []Decision{{
			FindingID: "f1", Verdict: VerdictUncertain, Status: store.FindingOpen,
			Verification: Verification{Verdict: VerdictUncertain, Reason: "OCR no pudo verificar el hallazgo"},
		}},
	}
	tree := &differentialTree{base: map[string][]byte{differentialPolicyPath: validDifferentialPolicy()}}
	executor := &fakeDifferentialExecutor{run: func(DifferentialCase) (DifferentialExecution, error) {
		return DifferentialExecution{
			Base: []DifferentialRunResult{{Passed: true}, {Passed: true}},
			Head: []DifferentialRunResult{{ExitCode: 1}, {ExitCode: 1}},
		}, nil
	}}
	r := &Runner{Differential: executor}
	r.attachDifferentialEvidenceWith(context.Background(), tree, "/repo", "base-sha", "head-sha", true, out, nil)

	decision := out.decisions[0]
	if decision.Verdict != VerdictUncertain || decision.Status != store.FindingOpen || decision.Apply {
		t.Errorf("decision = %+v, want the uncertain verdict and status untouched", decision)
	}
	if decision.Verification.Verdict != VerdictUncertain {
		t.Errorf("stored verdict = %q, want uncertain", decision.Verification.Verdict)
	}
	if decision.Verification.Differential == nil ||
		decision.Verification.Differential.Status != DifferentialRegression {
		t.Errorf("differential = %+v, want the regression recorded as evidence", decision.Verification.Differential)
	}
}

func TestAttachDifferentialEvidenceWithoutPolicyRunsNothing(t *testing.T) {
	out := &runOutcome{
		findings:  []store.Finding{{ID: "f1", Path: "internal/review/a.go", Severity: "major"}},
		decisions: []Decision{{FindingID: "f1", Verification: Verification{Verdict: VerdictConfirm}}},
	}
	executor := &fakeDifferentialExecutor{}
	r := &Runner{Differential: executor}
	r.attachDifferentialEvidenceWith(context.Background(), &differentialTree{}, "/repo", "base-sha", "head-sha", true, out, nil)
	if len(executor.calls) != 0 {
		t.Fatalf("calls = %+v, want none without a trusted policy", executor.calls)
	}
	if out.decisions[0].Verification.Differential != nil {
		t.Error("a finding received evidence without a policy")
	}
}

func TestAttachDifferentialEvidenceKeepsFlakyInconclusive(t *testing.T) {
	out := &runOutcome{
		findings:  []store.Finding{{ID: "f1", Path: "internal/review/a.go", Severity: "major"}},
		decisions: []Decision{{FindingID: "f1", Verification: Verification{Verdict: VerdictConfirm}}},
	}
	tree := &differentialTree{base: map[string][]byte{differentialPolicyPath: validDifferentialPolicy()}}
	executor := &fakeDifferentialExecutor{run: func(DifferentialCase) (DifferentialExecution, error) {
		return DifferentialExecution{
			Base: []DifferentialRunResult{{Passed: true}, {ExitCode: 1}},
			Head: []DifferentialRunResult{{ExitCode: 1}, {ExitCode: 1}},
		}, nil
	}}
	r := &Runner{Differential: executor}
	r.attachDifferentialEvidenceWith(context.Background(), tree, "/repo", "base-sha", "head-sha", true, out, nil)

	evidence := out.decisions[0].Verification.Differential
	if evidence == nil || evidence.Status != DifferentialInconclusive {
		t.Fatalf("evidence = %+v, want inconclusive", evidence)
	}
	if !strings.Contains(evidence.Reason, "flaky") {
		t.Errorf("reason = %q, want the flaky side named", evidence.Reason)
	}
}

func TestCappedBufferDiscardsTheExcess(t *testing.T) {
	var buffer cappedBuffer
	buffer.max = 8
	if n, err := buffer.Write([]byte("0123456789abcdef")); err != nil || n != 16 {
		t.Fatalf("Write() = %d, %v; want 16 bytes consumed", n, err)
	}
	if got := buffer.String(); got != "01234567" {
		t.Errorf("buffer = %q, want the bounded prefix", got)
	}
	if n, _ := buffer.Write([]byte("xyz")); n != 3 {
		t.Errorf("second Write() = %d, want the full length reported", n)
	}
	if got := buffer.String(); got != "01234567" {
		t.Errorf("buffer grew beyond its cap: %q", got)
	}
}

// TestGitWorktreeDifferentialRunsBothSides proves the real executor on a
// throwaway Go module: the base passes and the head fails, so the comparison is
// a reproducible regression, and the temporary worktrees are removed.
func TestGitWorktreeDifferentialRunsBothSides(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the Go toolchain is not available")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "qa@example.invalid")
	runGit(t, dir, "config", "user.name", "QA")
	runGit(t, dir, "config", "core.autocrlf", "false")
	writeDifferentialFixture(t, dir, "func Add(a, b int) int { return a + b }\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	baseSHA := runGit(t, dir, "rev-parse", "HEAD")

	writeDifferentialFixture(t, dir, "func Add(a, b int) int { return a - b }\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "regression")
	headSHA := runGit(t, dir, "rev-parse", "HEAD")

	executor := &gitWorktreeDifferential{RepoDir: dir, Timeout: 90 * time.Second, Sandbox: passthroughSandbox{}}
	execution, err := executor.Run(context.Background(), DifferentialCase{
		ID: "calc-sum", Paths: []string{"calc/*.go"}, Package: "./calc", Test: "TestAdd",
	}, baseSHA, headSHA)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	evidence := buildDifferentialEvidence(DifferentialCase{ID: "calc-sum", Package: "./calc", Test: "TestAdd"},
		baseSHA, headSHA, execution)
	if evidence.Status != DifferentialRegression {
		t.Fatalf("evidence = %+v, want a regression", evidence)
	}
	if !evidence.BasePassed || evidence.HeadPassed {
		t.Errorf("side flags = %v/%v, want base pass and head failure", evidence.BasePassed, evidence.HeadPassed)
	}
	if !strings.Contains(evidence.HeadOutput, "--- FAIL") {
		t.Errorf("head output = %q, want the failure marker", evidence.HeadOutput)
	}
	if !strings.Contains(evidence.Environment, "go version") {
		t.Errorf("environment = %q, want the Go toolchain", evidence.Environment)
	}
	if evidence.Isolation != "test passthrough" {
		t.Errorf("isolation = %q, want the injected sandbox", evidence.Isolation)
	}

	worktrees := runGit(t, dir, "worktree", "list", "--porcelain")
	if lines := strings.Count(worktrees, "worktree "); lines != 1 {
		t.Errorf("worktree list has %d entries, want only the repository checkout:\n%s", lines, worktrees)
	}
}

func TestAttachDifferentialEvidenceSkipsExternalContribution(t *testing.T) {
	out := &runOutcome{
		findings:  []store.Finding{{ID: "f1", Path: "internal/review/a.go", Severity: "major"}},
		decisions: []Decision{{FindingID: "f1", Verification: Verification{Verdict: VerdictConfirm}}},
	}
	tree := &differentialTree{base: map[string][]byte{differentialPolicyPath: validDifferentialPolicy()}}
	executor := &fakeDifferentialExecutor{}
	r := &Runner{Differential: executor}
	r.attachDifferentialEvidenceWith(context.Background(), tree, "/repo", "base-sha", "head-sha", false, out, nil)

	if len(executor.calls) != 0 {
		t.Fatalf("calls = %+v, want no execution for an external contribution", executor.calls)
	}
	if len(tree.reads) != 0 {
		t.Errorf("reads = %v, want no policy read before the trust gate", tree.reads)
	}
	evidence := out.decisions[0].Verification.Differential
	if evidence == nil || evidence.Status != DifferentialInconclusive {
		t.Fatalf("evidence = %+v, want an explicit inconclusive record", evidence)
	}
	if !strings.Contains(evidence.Reason, "externo") || evidence.Isolation != "not_executed_external" {
		t.Errorf("evidence = %+v, want the external skip recorded", evidence)
	}
}

func TestDifferentialEnvironmentUsesPerRunCachesWithoutWorkerSecrets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("QA_WORKER_KEY", "super-secret")
	t.Setenv("DATABASE_URL", "postgres://user:pass@db")
	t.Setenv("GOCACHE", "/shared/gocache")
	t.Setenv("GOMODCACHE", "/shared/modcache")
	t.Setenv("GOPATH", "/shared/gopath")

	env, err := differentialEnvironment(root)
	if err != nil {
		t.Fatalf("differentialEnvironment() error = %v", err)
	}
	values := envMap(env)
	if _, ok := values["QA_WORKER_KEY"]; ok {
		t.Error("the test environment contains QA_WORKER_KEY")
	}
	if _, ok := values["DATABASE_URL"]; ok {
		t.Error("the test environment contains DATABASE_URL")
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		if !strings.HasPrefix(values[key], root) {
			t.Errorf("%s = %q, want a per-run path under %q", key, values[key], root)
		}
	}
	if values["GOPROXY"] != "off" || values["GOSUMDB"] != "off" {
		t.Errorf("GOPROXY/GOSUMDB = %q/%q, want the untrusted test offline", values["GOPROXY"], values["GOSUMDB"])
	}
	for key, value := range values {
		if strings.Contains(value, "super-secret") || strings.Contains(value, "/shared") ||
			strings.Contains(value, "user:pass") {
			t.Errorf("%s leaked a worker value %q", key, value)
		}
	}

	moduleEnv, err := differentialModuleEnvironment(root)
	if err != nil {
		t.Fatalf("differentialModuleEnvironment() error = %v", err)
	}
	moduleValues := envMap(moduleEnv)
	if moduleValues["GOPROXY"] != "https://proxy.golang.org" {
		t.Errorf("setup GOPROXY = %q, want the public proxy", moduleValues["GOPROXY"])
	}
	if !strings.HasPrefix(moduleValues["GOMODCACHE"], root) {
		t.Errorf("setup GOMODCACHE = %q, want the per-run cache", moduleValues["GOMODCACHE"])
	}
}

func envMap(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	return values
}

func TestBubblewrapSandboxIsolatesFilesystem(t *testing.T) {
	command := []string{"/usr/local/go/bin/go", "test", "./calc"}
	bwrap := (&bubblewrapSandbox{path: "bwrap"}).wrapArgs("/work", "/tmp/root", command)
	joined := strings.Join(bwrap, " ")
	for _, want := range []string{
		"--unshare-all", "--cap-drop ALL", "--ro-bind / /",
		"--tmpfs /tmp", "--tmpfs /home", "--tmpfs /root",
		"--bind /tmp/root /tmp/root", "--ro-bind /work /work",
		"--proc /proc", "--chdir /work",
		"-- /usr/local/go/bin/go test ./calc",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap args do not contain %q:\n%s", want, joined)
		}
	}
	// The worktree must stay read-only even when it sits inside the writable root.
	if strings.Index(joined, "--bind /tmp/root /tmp/root") > strings.Index(joined, "--ro-bind /work /work") {
		t.Errorf("writable root bind comes after the read-only worktree bind:\n%s", joined)
	}
}

func TestBubblewrapSandboxHidesWorkerHome(t *testing.T) {
	command := []string{"/home/worker/go/bin/go", "test", "./calc"}
	joined := strings.Join((&bubblewrapSandbox{path: "bwrap"}).wrapArgs(
		"/home/worker/project", "/home/worker/.qa/root", command), " ")
	for _, want := range []string{
		"--tmpfs /home", "--tmpfs /root",
		"--bind /home/worker/.qa/root /home/worker/.qa/root",
		"--ro-bind /home/worker/project /home/worker/project",
		"--ro-bind /home/worker/go /home/worker/go",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap args do not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--ro-bind /home /home") ||
		strings.Contains(joined, "--ro-bind /root /root") {
		t.Errorf("the sandbox re-exposed the worker home:\n%s", joined)
	}
}

func TestDetectDifferentialSandboxRequiresBubblewrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	differentialSandboxOnce = sync.Once{}
	differentialSandboxBest = nil
	t.Cleanup(func() {
		differentialSandboxOnce = sync.Once{}
		differentialSandboxBest = nil
	})

	sandbox := detectDifferentialSandbox()
	if sandbox.Available() {
		t.Fatal("detection reported an available sandbox without bubblewrap")
	}
	if _, ok := sandbox.(unavailableSandbox); !ok {
		t.Fatalf("sandbox = %T, want the fail-closed implementation", sandbox)
	}
}

func TestGitWorktreeDifferentialWithoutSandboxSkipsExecution(t *testing.T) {
	executor := &gitWorktreeDifferential{RepoDir: t.TempDir(), Sandbox: unavailableSandbox{}}
	execution, err := executor.Run(context.Background(), DifferentialCase{ID: "calc", Package: "./calc"}, "base-sha", "head-sha")
	if err != nil {
		t.Fatalf("Run() error = %v, want a disabled case instead of a failure", err)
	}
	if execution.Isolation != "unavailable" {
		t.Errorf("isolation = %q, want unavailable", execution.Isolation)
	}
	for side, runs := range map[string][]DifferentialRunResult{"base": execution.Base, "head": execution.Head} {
		if len(runs) != 1 || runs[0].Inconclusive != "isolation" {
			t.Errorf("%s runs = %+v, want one isolation-inconclusive result", side, runs)
		}
	}
}

func writeDifferentialFixture(t *testing.T, dir, addImplementation string) {
	t.Helper()
	files := map[string]string{
		"go.mod":       "module example.com/differential\n\ngo 1.21\n",
		"calc/calc.go": "package calc\n\n" + addImplementation,
		"calc/calc_test.go": `package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatalf("Add(1, 2) = %d, want 3", Add(1, 2))
	}
}
`,
	}
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
