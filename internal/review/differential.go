package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Differential evidence runs one maintainer-approved test on the trusted base
// and on the reviewed head, in isolated worktrees and inside bubblewrap, and
// records what both sides produced. External contributions never execute, and a
// host without a usable bubblewrap keeps the feature disabled. It is observation
// only: the verdict that publishes a finding still comes from the
// confirmed-verdict path (#227).
const (
	differentialPolicyPath     = ".qa/differential-cases.json"
	differentialPolicyVersion  = "differential-cases/v1"
	differentialMaxPolicyBytes = 64 << 10
	differentialMaxPolicyCases = 10
	differentialMaxCases       = 5
	differentialAttempts       = 2
	differentialRunTimeout     = 2 * time.Minute
	differentialMaxCaptured    = 64 << 10
	differentialMaxEvidence    = 8 << 10
	differentialMaxLineBytes   = 300
	differentialMemoryLimit    = "512MiB"
)

// Differential outcome values. A regression is the only result that adds
// executable proof of the claim; the others document what was checked or why
// the comparison could not conclude.
const (
	DifferentialRegression   = "regression"
	DifferentialPreExisting  = "pre_existing"
	DifferentialPassing      = "passing"
	DifferentialImproved     = "improved"
	DifferentialInconclusive = "inconclusive"
)

// DifferentialCase is one test the trusted policy allows the worker to run. The
// path globs decide which findings the case is pertinent to; the package and
// test are the exact command, never assembled from model output.
type DifferentialCase struct {
	ID      string
	Paths   []string
	Package string
	Test    string
}

// DifferentialRunResult is one execution of a case on one side. Inconclusive
// names why the execution cannot be used as evidence (compilation, dependency,
// infrastructure, timeout, equivalence or case_not_found); empty means the exit
// code is the test outcome.
type DifferentialRunResult struct {
	Passed       bool
	ExitCode     int
	TimedOut     bool
	Inconclusive string
	Output       string
	Duration     time.Duration

	raw string
}

// DifferentialExecution is both sides of one case. Tests inject it through
// DifferentialExecutor; the real executor fills it from two git worktrees.
type DifferentialExecution struct {
	Environment string
	Isolation   string
	Base        []DifferentialRunResult
	Head        []DifferentialRunResult
}

// DifferentialEvidence is what a run stores on the finding's verification. It
// keeps the SHAs, the case, the environment, the achieved isolation, both side
// results and the output slice that shows the difference.
type DifferentialEvidence struct {
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	Language    string    `json:"language"`
	Case        string    `json:"case"`
	Package     string    `json:"package"`
	Test        string    `json:"test,omitempty"`
	BaseSHA     string    `json:"base_sha"`
	HeadSHA     string    `json:"head_sha"`
	Environment string    `json:"environment"`
	Isolation   string    `json:"isolation,omitempty"`
	BasePassed  bool      `json:"base_passed"`
	HeadPassed  bool      `json:"head_passed"`
	BaseOutput  string    `json:"base_output,omitempty"`
	HeadOutput  string    `json:"head_output,omitempty"`
	Attempts    int       `json:"attempts"`
	DurationMS  int64     `json:"duration_ms"`
	MeasuredAt  time.Time `json:"measured_at"`
}

// DifferentialExecutor runs one case against the two commits. The interface is
// deliberately narrow so tests use a fake and production stays opt-in through
// the trusted policy file.
type DifferentialExecutor interface {
	Run(ctx context.Context, testCase DifferentialCase, baseSHA, headSHA string) (DifferentialExecution, error)
}

// differentialPolicyFile is the maintainer-approved allow-list. It lives in the
// base tree, so a pull request cannot authorize its own commands.
type differentialPolicyFile struct {
	Version string                 `json:"version"`
	Cases   []differentialCaseSpec `json:"cases"`
}

type differentialCaseSpec struct {
	ID      string   `json:"id"`
	Paths   []string `json:"paths"`
	Package string   `json:"package"`
	Test    string   `json:"test,omitempty"`
}

var (
	differentialCaseIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	differentialPackagePattern  = regexp.MustCompile(`^\./[A-Za-z0-9_][A-Za-z0-9_./-]*$`)
	differentialTestNamePattern = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
)

// differentialSelection is one case plus every finding it is evidence for. A
// case runs once even when several findings cite the same module.
type differentialSelection struct {
	Case     DifferentialCase
	Findings []string
}

// parseDifferentialPolicy validates the allow-list. Anything the worker cannot
// run as a single bounded `go test` invocation is rejected instead of guessed.
func parseDifferentialPolicy(raw []byte) ([]DifferentialCase, error) {
	var file differentialPolicyFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("política diferencial inválida: %w", err)
	}
	if strings.TrimSpace(file.Version) != differentialPolicyVersion {
		return nil, fmt.Errorf("versión de política diferencial no soportada: %q", file.Version)
	}
	if len(file.Cases) > differentialMaxPolicyCases {
		return nil, fmt.Errorf("la política diferencial declara %d casos (máximo %d)",
			len(file.Cases), differentialMaxPolicyCases)
	}
	cases := make([]DifferentialCase, 0, len(file.Cases))
	seen := make(map[string]bool, len(file.Cases))
	for _, spec := range file.Cases {
		testCase, err := validateDifferentialCase(spec)
		if err != nil {
			return nil, err
		}
		if seen[testCase.ID] {
			return nil, fmt.Errorf("caso diferencial duplicado %q", testCase.ID)
		}
		seen[testCase.ID] = true
		cases = append(cases, testCase)
	}
	return cases, nil
}

func validateDifferentialCase(spec differentialCaseSpec) (DifferentialCase, error) {
	id := strings.TrimSpace(spec.ID)
	if !differentialCaseIDPattern.MatchString(id) {
		return DifferentialCase{}, fmt.Errorf("caso diferencial con id inválido %q", spec.ID)
	}
	pkg := strings.TrimSpace(spec.Package)
	if (pkg != "." && !differentialPackagePattern.MatchString(pkg)) || strings.Contains(pkg, "..") ||
		strings.HasSuffix(pkg, "/") || strings.Contains(pkg, "...") {
		return DifferentialCase{}, fmt.Errorf("caso diferencial %q con paquete no permitido %q", id, spec.Package)
	}
	test := strings.TrimSpace(spec.Test)
	if test != "" && !differentialTestNamePattern.MatchString(test) {
		return DifferentialCase{}, fmt.Errorf("caso diferencial %q con prueba no permitida %q", id, spec.Test)
	}
	if len(spec.Paths) == 0 || len(spec.Paths) > 10 {
		return DifferentialCase{}, fmt.Errorf("caso diferencial %q declara %d rutas (se requiere entre 1 y 10)",
			id, len(spec.Paths))
	}
	paths := make([]string, 0, len(spec.Paths))
	for _, rawPath := range spec.Paths {
		glob := strings.TrimSpace(strings.ReplaceAll(rawPath, `\`, "/"))
		if glob == "" || strings.HasPrefix(glob, "/") || strings.Contains(glob, "..") {
			return DifferentialCase{}, fmt.Errorf("caso diferencial %q con ruta no permitida %q", id, rawPath)
		}
		if _, err := path.Match(glob, "probe"); err != nil {
			return DifferentialCase{}, fmt.Errorf("caso diferencial %q con glob inválido %q", id, rawPath)
		}
		paths = append(paths, glob)
	}
	return DifferentialCase{ID: id, Paths: paths, Package: pkg, Test: test}, nil
}

// loadDifferentialPolicy reads the allow-list from the trusted base commit only.
// A missing file is an opt-out and reports no reason; a malformed file is
// reported so the panel explains why no evidence was produced.
func loadDifferentialPolicy(tree baselineTree, baseSHA string) ([]DifferentialCase, string) {
	if tree == nil || baseSHA == "" {
		return nil, "no hay base confiable para la política diferencial"
	}
	raw, ok, err := tree.Read(baselineRefBase, differentialPolicyPath)
	if err != nil {
		return nil, fmt.Sprintf("no se pudo leer %s: %v", differentialPolicyPath, err)
	}
	if !ok {
		return nil, ""
	}
	if len(raw) > differentialMaxPolicyBytes {
		return nil, fmt.Sprintf("%s supera el límite de %d bytes", differentialPolicyPath, differentialMaxPolicyBytes)
	}
	cases, err := parseDifferentialPolicy(raw)
	if err != nil {
		return nil, err.Error()
	}
	return cases, ""
}

// severeFindings returns the high-impact findings that can receive executable
// evidence: blockers and majors, never advisory observations. The order is
// deterministic so the case cap chooses the same cases across runs.
func severeFindings(findings []store.Finding) []store.Finding {
	severe := make([]store.Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Advisory || severityRank(finding.Severity) < severityRank("major") {
			continue
		}
		severe = append(severe, finding)
	}
	sort.SliceStable(severe, func(i, j int) bool {
		return differentialFindingLess(severe[i], severe[j])
	})
	return severe
}

func differentialFindingLess(a, b store.Finding) bool {
	if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
		return ra > rb
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	return a.ID < b.ID
}

// selectDifferentialCases assigns each severe finding to the first allowed case
// whose path globs cover it. Cases are capped; a case already selected keeps
// collecting findings without spending another slot.
func selectDifferentialCases(findings []store.Finding, cases []DifferentialCase) []differentialSelection {
	selections := make([]differentialSelection, 0, differentialMaxCases)
	index := make(map[string]int, differentialMaxCases)
	for _, finding := range severeFindings(findings) {
		for _, testCase := range cases {
			if !findingMatchesDifferentialCase(finding, testCase) {
				continue
			}
			if pos, ok := index[testCase.ID]; ok {
				selections[pos].Findings = append(selections[pos].Findings, finding.ID)
				continue
			}
			if len(selections) >= differentialMaxCases {
				continue
			}
			index[testCase.ID] = len(selections)
			selections = append(selections, differentialSelection{
				Case:     testCase,
				Findings: []string{finding.ID},
			})
		}
	}
	return selections
}

func findingMatchesDifferentialCase(finding store.Finding, testCase DifferentialCase) bool {
	findingPath := normalizeCoveragePath(finding.Path)
	if findingPath == "" {
		return false
	}
	for _, glob := range testCase.Paths {
		if matched, err := path.Match(glob, findingPath); err == nil && matched {
			return true
		}
	}
	return false
}

// buildDifferentialEvidence classifies an execution and records both side
// results. It never decides a verdict.
func buildDifferentialEvidence(testCase DifferentialCase, baseSHA, headSHA string, execution DifferentialExecution) DifferentialEvidence {
	status, reason, baseOutput, headOutput := classifyDifferential(execution)
	evidence := DifferentialEvidence{
		Status:      status,
		Reason:      reason,
		Language:    "go",
		Case:        testCase.ID,
		Package:     testCase.Package,
		Test:        testCase.Test,
		BaseSHA:     baseSHA,
		HeadSHA:     headSHA,
		Environment: execution.Environment,
		Isolation:   execution.Isolation,
		BaseOutput:  baseOutput,
		HeadOutput:  headOutput,
		Attempts:    len(execution.Base),
		MeasuredAt:  time.Now().UTC(),
	}
	if len(execution.Base) > 0 {
		evidence.BasePassed = execution.Base[0].Passed
	}
	if len(execution.Head) > 0 {
		evidence.HeadPassed = execution.Head[0].Passed
	}
	evidence.DurationMS = sideDurationMS(execution.Base) + sideDurationMS(execution.Head)
	return evidence
}

func sideDurationMS(runs []DifferentialRunResult) int64 {
	var total time.Duration
	for _, run := range runs {
		total += run.Duration
	}
	return total.Milliseconds()
}

// classifyDifferential turns the two side results into the documented cases.
// A side that disagrees with itself is flaky; compilation, dependency,
// infrastructure, timeout and equivalence failures are inconclusive.
func classifyDifferential(execution DifferentialExecution) (status, reason, baseOutput, headOutput string) {
	baseOutput = joinDifferentialOutput(execution.Base)
	headOutput = joinDifferentialOutput(execution.Head)
	baseState, baseReason := differentialSideState(execution.Base)
	headState, headReason := differentialSideState(execution.Head)
	if baseState == DifferentialInconclusive || headState == DifferentialInconclusive {
		reasons := make([]string, 0, 2)
		if baseReason != "" {
			reasons = append(reasons, "base: "+baseReason)
		}
		if headReason != "" {
			reasons = append(reasons, "head: "+headReason)
		}
		return DifferentialInconclusive, strings.Join(reasons, "; "), baseOutput, headOutput
	}
	switch {
	case baseState == DifferentialPassing && headState == "fail":
		return DifferentialRegression, "", baseOutput, headOutput
	case baseState == "fail" && headState == "fail":
		return DifferentialPreExisting, "", baseOutput, headOutput
	case baseState == DifferentialPassing && headState == DifferentialPassing:
		return DifferentialPassing, "", baseOutput, headOutput
	default:
		return DifferentialImproved, "", baseOutput, headOutput
	}
}

func differentialSideState(runs []DifferentialRunResult) (state, reason string) {
	if len(runs) == 0 {
		return DifferentialInconclusive, "not_run"
	}
	passed, failed := 0, 0
	for _, run := range runs {
		if run.TimedOut {
			return DifferentialInconclusive, "timeout"
		}
		if run.Inconclusive != "" {
			return DifferentialInconclusive, run.Inconclusive
		}
		if run.Passed {
			passed++
		} else {
			failed++
		}
	}
	if passed > 0 && failed > 0 {
		return DifferentialInconclusive, "flaky"
	}
	if passed > 0 {
		return DifferentialPassing, ""
	}
	return "fail", ""
}

func joinDifferentialOutput(runs []DifferentialRunResult) string {
	parts := make([]string, 0, len(runs))
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		output := strings.TrimSpace(run.Output)
		if output == "" || seen[output] {
			continue
		}
		seen[output] = true
		parts = append(parts, output)
	}
	joined := strings.Join(parts, "\n")
	if len(joined) <= differentialMaxEvidence {
		return joined
	}
	return joined[:differentialMaxEvidence]
}

func differentialDecisionIndex(out *runOutcome) map[string]int {
	index := make(map[string]int, len(out.decisions))
	for i, decision := range out.decisions {
		index[decision.FindingID] = i
	}
	return index
}

// differentialEligibleFindings keeps the findings that have a verification
// decision, so evidence is never attached to a human-triaged finding.
func differentialEligibleFindings(out *runOutcome, index map[string]int) []store.Finding {
	eligible := make([]store.Finding, 0, len(out.findings))
	for _, finding := range out.findings {
		if _, ok := index[finding.ID]; ok {
			eligible = append(eligible, finding)
		}
	}
	return eligible
}

// attachDifferentialEvidence attaches executable evidence to the decisions of
// the high-impact findings. Every failure is contained: no evidence, a
// malformed policy or a failed executor only leaves the finding without it.
// External contributions never execute: their code is untrusted and a test run
// is arbitrary code execution (#230).
func (r *Runner) attachDifferentialEvidence(ctx context.Context, workDir, baseSHA, headSHA string, trusted bool, out *runOutcome, log *runLogSink) {
	if out == nil || len(out.findings) == 0 || len(out.decisions) == 0 {
		return
	}
	tree := gitBaselineTree{ctx: ctx, dir: workDir, base: baseSHA, head: headSHA}
	r.attachDifferentialEvidenceWith(ctx, tree, workDir, baseSHA, headSHA, trusted, out, log)
}

func (r *Runner) attachDifferentialEvidenceWith(ctx context.Context, tree baselineTree, workDir, baseSHA, headSHA string, trusted bool, out *runOutcome, log *runLogSink) {
	decisionIndex := differentialDecisionIndex(out)
	eligible := differentialEligibleFindings(out, decisionIndex)
	if !trusted {
		reason := "aporte externo: no se ejecutan pruebas"
		evidence := DifferentialEvidence{
			Status:     DifferentialInconclusive,
			Reason:     reason,
			Language:   "go",
			BaseSHA:    baseSHA,
			HeadSHA:    headSHA,
			Isolation:  "not_executed_external",
			MeasuredAt: time.Now().UTC(),
		}
		for _, finding := range severeFindings(eligible) {
			pos := decisionIndex[finding.ID]
			copied := evidence
			out.decisions[pos].Verification.Differential = &copied
		}
		log.Log(ctx, "worker", "differential", "warn", logPhaseOCR,
			"evidencia diferencial omitida: "+reason)
		return
	}

	cases, policyReason := loadDifferentialPolicy(tree, baseSHA)
	if policyReason != "" {
		log.Log(ctx, "worker", "differential", "warn", logPhaseOCR,
			"evidencia diferencial omitida: "+policyReason)
		return
	}
	if len(cases) == 0 {
		return
	}

	selections := selectDifferentialCases(eligible, cases)
	if len(selections) == 0 {
		log.Log(ctx, "worker", "differential", "info", logPhaseOCR, fmt.Sprintf(
			"evidencia diferencial: ningún caso autorizado cubre %d hallazgo(s) grave(s)", len(severeFindings(eligible))))
		return
	}

	executor := r.Differential
	if executor == nil {
		executor = &gitWorktreeDifferential{RepoDir: workDir}
	}
	started := time.Now()
	for _, selection := range selections {
		execution, err := executor.Run(ctx, selection.Case, baseSHA, headSHA)
		evidence := DifferentialEvidence{
			Status:     DifferentialInconclusive,
			Language:   "go",
			Case:       selection.Case.ID,
			Package:    selection.Case.Package,
			Test:       selection.Case.Test,
			BaseSHA:    baseSHA,
			HeadSHA:    headSHA,
			MeasuredAt: time.Now().UTC(),
		}
		if err != nil {
			evidence.Reason = err.Error()
		} else {
			evidence = buildDifferentialEvidence(selection.Case, baseSHA, headSHA, execution)
		}
		level := "info"
		if evidence.Status == DifferentialInconclusive {
			level = "warn"
		}
		log.Log(ctx, "worker", "differential", level, logPhaseOCR, fmt.Sprintf(
			"caso diferencial %s (%s): %s%s", evidence.Case, evidence.Package,
			evidence.Status, differentialReasonSuffix(evidence.Reason)))
		for _, findingID := range selection.Findings {
			pos := decisionIndex[findingID]
			copied := evidence
			out.decisions[pos].Verification.Differential = &copied
		}
	}

	severe := severeFindings(out.findings)
	withEvidence := 0
	for _, finding := range severe {
		pos, ok := decisionIndex[finding.ID]
		if !ok {
			continue
		}
		if evidence := out.decisions[pos].Verification.Differential; evidence != nil &&
			evidence.Status != DifferentialInconclusive {
			withEvidence++
		}
	}
	elapsed := time.Since(started)
	share := 0
	if len(severe) > 0 {
		share = withEvidence * 100 / len(severe)
	}
	perFinding := time.Duration(0)
	if withEvidence > 0 {
		perFinding = elapsed / time.Duration(withEvidence)
	}
	log.Log(ctx, "worker", "differential", "info", logPhaseOCR, fmt.Sprintf(
		"evidencia diferencial: %d/%d hallazgos graves con evidencia ejecutable (%d%%); %s adicionales (%s por hallazgo)",
		withEvidence, len(severe), share, elapsed.Round(time.Millisecond), perFinding.Round(time.Millisecond)))
}

func differentialReasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return ": " + reason
}

// differentialSandbox wraps the untrusted test command in the strongest OS
// isolation the worker can provide. The feature is disabled unless bubblewrap
// is usable: a mount namespace without the mounts is not filesystem isolation.
type differentialSandbox interface {
	Available() bool
	Name() string
	Wrap(ctx context.Context, dir, writableRoot string, command []string) (*exec.Cmd, bool)
}

var (
	differentialSandboxOnce sync.Once
	differentialSandboxBest differentialSandbox
)

func detectDifferentialSandbox() differentialSandbox {
	differentialSandboxOnce.Do(func() {
		if path, err := exec.LookPath("bwrap"); err == nil {
			sandbox := &bubblewrapSandbox{path: path}
			if sandbox.Available() {
				differentialSandboxBest = sandbox
				return
			}
		}
		differentialSandboxBest = unavailableSandbox{}
	})
	return differentialSandboxBest
}

type bubblewrapSandbox struct {
	path string
	once sync.Once
	ok   bool
}

func (s *bubblewrapSandbox) Available() bool {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.path,
			"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
			"--ro-bind", "/", "/", "--tmpfs", "/tmp", "--tmpfs", "/home", "--tmpfs", "/root",
			"--proc", "/proc", "--dev", "/dev", "--", "true")
		s.ok = cmd.Run() == nil
	})
	return s.ok
}

func (s *bubblewrapSandbox) Name() string {
	return "bubblewrap: user/mount/pid/net namespaces, read-only root, hidden home"
}

func (s *bubblewrapSandbox) Wrap(ctx context.Context, dir, writableRoot string, command []string) (*exec.Cmd, bool) {
	if !s.Available() {
		return nil, false
	}
	return exec.CommandContext(ctx, s.path, s.wrapArgs(dir, writableRoot, command)...), true
}

func (s *bubblewrapSandbox) wrapArgs(dir, writableRoot string, command []string) []string {
	args := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp", "--tmpfs", "/home", "--tmpfs", "/root",
	}
	// Re-expose only what the test needs: the per-case cache root writable and
	// the worktree read-only on top of it. Everything else under the worker's
	// home stays hidden.
	args = append(args,
		"--dir", writableRoot,
		"--bind", writableRoot, writableRoot,
	)
	if !pathWithin(dir, writableRoot) {
		args = append(args, "--dir", dir)
	}
	args = append(args, "--ro-bind", dir, dir)
	// A Go toolchain installed under the hidden home would otherwise disappear;
	// bind its root back read-only.
	if goRoot := differentialGoRoot(command[0]); goRoot != "" &&
		(pathWithin(goRoot, "/home") || pathWithin(goRoot, "/root")) {
		args = append(args, "--dir", goRoot, "--ro-bind", goRoot, goRoot)
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", dir,
		"--",
	)
	return append(args, command...)
}

type unavailableSandbox struct{}

func (unavailableSandbox) Available() bool { return false }
func (unavailableSandbox) Name() string    { return "unavailable (bubblewrap required)" }
func (unavailableSandbox) Wrap(context.Context, string, string, []string) (*exec.Cmd, bool) {
	return nil, false
}

// differentialGoRoot returns the toolchain root of a go binary so a toolchain
// installed under a hidden home can be bound back read-only.
func differentialGoRoot(goBin string) string {
	binDir := filepath.Dir(goBin)
	if filepath.Base(binDir) == "bin" {
		return filepath.Dir(binDir)
	}
	return binDir
}

func pathWithin(value, parent string) bool {
	rel, err := filepath.Rel(parent, value)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// gitWorktreeDifferential is the production executor. It creates one detached
// worktree per side, prepares a per-case module cache, runs the test inside
// bubblewrap, bounds time, memory and output, and removes the worktrees before
// returning.
type gitWorktreeDifferential struct {
	RepoDir string
	Timeout time.Duration
	Runs    int
	// Sandbox overrides detection in tests. Nil uses the best sandbox the host
	// provides; when none exists the feature stays disabled and the case is
	// reported as inconclusive.
	Sandbox differentialSandbox
}

func (e *gitWorktreeDifferential) Run(ctx context.Context, testCase DifferentialCase, baseSHA, headSHA string) (DifferentialExecution, error) {
	if e.RepoDir == "" {
		return DifferentialExecution{}, errors.New("repositorio diferencial no configurado")
	}
	if baseSHA == "" || headSHA == "" {
		return DifferentialExecution{}, errors.New("la comparación diferencial requiere base y head")
	}
	sandbox := e.Sandbox
	if sandbox == nil {
		sandbox = detectDifferentialSandbox()
	}
	if !sandbox.Available() {
		// Executable evidence must run inside an OS sandbox. Without bubblewrap
		// the feature stays disabled and the case is explicitly inconclusive.
		return DifferentialExecution{
			Isolation: "unavailable",
			Base:      []DifferentialRunResult{{ExitCode: -1, Inconclusive: "isolation", Output: "bubblewrap no está disponible"}},
			Head:      []DifferentialRunResult{{ExitCode: -1, Inconclusive: "isolation", Output: "bubblewrap no está disponible"}},
		}, nil
	}
	root, err := os.MkdirTemp("", "qa-differential-")
	if err != nil {
		return DifferentialExecution{}, fmt.Errorf("crear entorno diferencial: %w", err)
	}
	defer os.RemoveAll(root)

	env, err := differentialEnvironment(root)
	if err != nil {
		return DifferentialExecution{}, err
	}
	moduleEnv, err := differentialModuleEnvironment(root)
	if err != nil {
		return DifferentialExecution{}, err
	}
	runs := e.Runs
	if runs <= 0 {
		runs = differentialAttempts
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = differentialRunTimeout
	}

	execution := DifferentialExecution{
		Environment: differentialGoVersion(ctx, e.RepoDir, env),
		Isolation:   sandbox.Name(),
	}
	sides := []struct {
		name string
		sha  string
		dst  *[]DifferentialRunResult
	}{
		{"base", baseSHA, &execution.Base},
		{"head", headSHA, &execution.Head},
	}
	for _, side := range sides {
		if ctx.Err() != nil {
			break
		}
		results := e.runSide(ctx, root, env, moduleEnv, sandbox, side.name, side.sha, testCase, runs, timeout)
		*side.dst = results
	}
	return execution, nil
}

func (e *gitWorktreeDifferential) runSide(ctx context.Context, root string, env, moduleEnv []string, sandbox differentialSandbox, side, sha string, testCase DifferentialCase, runs int, timeout time.Duration) []DifferentialRunResult {
	if ctx.Err() != nil {
		return []DifferentialRunResult{{ExitCode: -1, TimedOut: true, Inconclusive: "timeout"}}
	}
	dir := filepath.Join(root, side)
	if err := e.addWorktree(ctx, dir, sha); err != nil {
		return []DifferentialRunResult{{ExitCode: -1, Inconclusive: "equivalence", Output: err.Error()}}
	}
	defer e.removeWorktree(dir)

	head, err := git(ctx, dir, "rev-parse HEAD", "rev-parse", "HEAD")
	if err != nil || !strings.EqualFold(head, sha) {
		return []DifferentialRunResult{{ExitCode: -1, Inconclusive: "equivalence",
			Output: fmt.Sprintf("el worktree %s no quedó en %s", side, sha)}}
	}
	if reason := prepareDifferentialSide(dir, testCase); reason != "" {
		return []DifferentialRunResult{{ExitCode: -1, Inconclusive: "equivalence", Output: reason}}
	}
	// Dependencies are downloaded into the per-case cache before any untrusted
	// code runs. The download itself never executes the repository's tests.
	if failed := downloadDifferentialModules(ctx, dir, moduleEnv, timeout); failed != nil {
		return []DifferentialRunResult{*failed}
	}

	results := make([]DifferentialRunResult, 0, runs)
	for i := 0; i < runs; i++ {
		if ctx.Err() != nil {
			results = append(results, DifferentialRunResult{ExitCode: -1, TimedOut: true, Inconclusive: "timeout"})
			break
		}
		results = append(results, runDifferentialTest(ctx, sandbox, dir, root, testCase, env, timeout))
	}
	return results
}

func (e *gitWorktreeDifferential) addWorktree(ctx context.Context, dir, sha string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", "--force", dir, sha)
	cmd.Dir = e.RepoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("preparar worktree temporal: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *gitWorktreeDifferential) removeWorktree(dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", dir)
	cmd.Dir = e.RepoDir
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		prune := exec.CommandContext(ctx, "git", "worktree", "prune")
		prune.Dir = e.RepoDir
		_ = prune.Run()
	}
}

// prepareDifferentialSide rejects a side where the declared package or its test
// files do not exist. That is a failed equivalence, not a regression.
func prepareDifferentialSide(dir string, testCase DifferentialCase) string {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "el repositorio no tiene go.mod"
	}
	relative := strings.TrimPrefix(testCase.Package, "./")
	pkgDir := filepath.Join(dir, filepath.FromSlash(relative))
	rel, err := filepath.Rel(dir, pkgDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "el paquete diferencial escapa del worktree"
	}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return "el paquete diferencial no existe en este lado"
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			return ""
		}
	}
	return "el paquete diferencial no tiene pruebas"
}

func runDifferentialTest(parent context.Context, sandbox differentialSandbox, dir, root string, testCase DifferentialCase, env []string, timeout time.Duration) DifferentialRunResult {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	goBin, err := exec.LookPath("go")
	if err != nil {
		return DifferentialRunResult{ExitCode: -1, Inconclusive: "infrastructure", Output: "el binario go no está disponible"}
	}
	args := []string{"test", "-count=1", "-timeout", fmt.Sprintf("%ds", int(timeout.Seconds()))}
	if testCase.Test != "" {
		args = append(args, "-run", "^"+testCase.Test+"$")
	}
	args = append(args, testCase.Package)
	command := append([]string{goBin}, args...)

	cmd, ok := sandbox.Wrap(ctx, dir, root, command)
	if !ok {
		return DifferentialRunResult{ExitCode: -1, Inconclusive: "isolation", Output: "bubblewrap no está disponible"}
	}
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	isolateProcessGroup(cmd)
	var captured cappedBuffer
	captured.max = differentialMaxCaptured
	cmd.Stdout = &captured
	cmd.Stderr = &captured

	started := time.Now()
	err = cmd.Run()
	result := DifferentialRunResult{Duration: time.Since(started), raw: captured.String()}
	if ctx.Err() != nil {
		result.TimedOut = true
		result.ExitCode = -1
		result.Inconclusive = "timeout"
		result.Output = differentialOutputSlice(result.raw)
		return result
	}
	if err == nil {
		result.Inconclusive = classifyDifferentialFailure(0, result.raw)
		if result.Inconclusive == "" {
			result.Passed = true
		}
		result.Output = differentialOutputSlice(result.raw)
		return result
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		result.ExitCode = -1
		result.Inconclusive = "infrastructure"
		result.Output = differentialOutputSlice(err.Error())
		return result
	}
	result.ExitCode = exitErr.ExitCode()
	result.Inconclusive = classifyDifferentialFailure(result.ExitCode, result.raw)
	result.Output = differentialOutputSlice(result.raw)
	return result
}

// downloadDifferentialModules prepares the per-case module cache before any
// untrusted test runs. It only runs the Go tool on go.mod/go.sum; the
// repository's test code is never executed here.
func downloadDifferentialModules(parent context.Context, dir string, env []string, timeout time.Duration) *DifferentialRunResult {
	goBin, err := exec.LookPath("go")
	if err != nil {
		result := DifferentialRunResult{ExitCode: -1, Inconclusive: "infrastructure", Output: "el binario go no está disponible"}
		return &result
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, goBin, "mod", "download")
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	isolateProcessGroup(cmd)
	var captured cappedBuffer
	captured.max = differentialMaxCaptured
	cmd.Stdout = &captured
	cmd.Stderr = &captured

	started := time.Now()
	err = cmd.Run()
	if ctx.Err() != nil {
		result := DifferentialRunResult{Duration: time.Since(started), ExitCode: -1, TimedOut: true,
			Inconclusive: "timeout", raw: captured.String()}
		result.Output = differentialOutputSlice(result.raw)
		return &result
	}
	if err == nil {
		return nil
	}
	result := DifferentialRunResult{Duration: time.Since(started), raw: captured.String(), Inconclusive: "dependency"}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if reason := classifyDifferentialFailure(result.ExitCode, result.raw); reason != "" {
			result.Inconclusive = reason
		}
	} else {
		result.ExitCode = -1
		result.Inconclusive = "infrastructure"
		result.Output = differentialOutputSlice(err.Error())
		return &result
	}
	result.Output = differentialOutputSlice(result.raw)
	return &result
}

func classifyDifferentialFailure(exitCode int, output string) string {
	lower := strings.ToLower(output)
	if exitCode == 0 {
		if strings.Contains(lower, "no tests to run") || strings.Contains(lower, "no test files") {
			return "case_not_found"
		}
		return ""
	}
	// 137 is SIGKILL, commonly the container or OS memory limit, never a test
	// assertion.
	if exitCode == 137 {
		return "infrastructure"
	}
	for _, signal := range []string{
		"signal: killed", "no space left on device", "cannot allocate memory",
		"resource temporarily unavailable", "fork/exec", "exec: \"go\"",
		"operation not permitted",
	} {
		if strings.Contains(lower, signal) {
			return "infrastructure"
		}
	}
	for _, signal := range []string{
		"no required module provides", "missing go.sum entry", "cannot find module providing",
		"module lookup disabled", "go: downloading", "proxy.golang.org",
		"dial tcp", "no such host", "connection refused", "i/o timeout",
	} {
		if strings.Contains(lower, signal) {
			return "dependency"
		}
	}
	for _, signal := range []string{"[build failed]", "build failed", "[setup failed]"} {
		if strings.Contains(lower, signal) {
			return "compilation"
		}
	}
	if strings.Contains(lower, "--- fail:") || strings.Contains(lower, "\nfail\t") ||
		strings.HasPrefix(lower, "fail\t") || strings.Contains(lower, "\nfail ") {
		return ""
	}
	return "unknown_failure"
}

// differentialOutputSlice keeps the lines that prove the outcome, never the
// whole stream. A failing test contributes its failure markers; any other
// failure contributes its tail, where the compiler or setup error lives.
func differentialOutputSlice(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "--- FAIL:") || strings.HasPrefix(line, "panic:") ||
			strings.Contains(line, "[build failed]") || strings.Contains(line, "[setup failed]") {
			start = i
			break
		}
	}
	if start < 0 {
		start = len(lines) - 12
		if start < 0 {
			start = 0
		}
	} else if start > 2 {
		start -= 2
	}
	end := start + 14
	if end > len(lines) {
		end = len(lines)
	}
	kept := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		if len(line) > differentialMaxLineBytes {
			line = line[:differentialMaxLineBytes] + "…"
		}
		kept = append(kept, line)
	}
	joined := strings.Join(kept, "\n")
	if len(joined) > differentialMaxEvidence {
		return joined[:differentialMaxEvidence]
	}
	return joined
}

// differentialCommonEnvironment builds the explicit environment shared by the
// setup and test phases. Only PATH is inherited; credentials, proxy settings
// and the user's git configuration are never passed.
func differentialCommonEnvironment(root string) ([]string, error) {
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	gocache := filepath.Join(root, "gocache")
	gopath := filepath.Join(root, "gopath")
	gomodcache := filepath.Join(root, "gomodcache")
	for _, dir := range []string{home, tmp, gocache, gopath, gomodcache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("crear entorno diferencial %s: %w", dir, err)
		}
	}
	gitConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitConfig, nil, 0o600); err != nil {
		return nil, fmt.Errorf("crear gitconfig diferencial: %w", err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"GOCACHE=" + gocache,
		"GOMODCACHE=" + gomodcache,
		"GOPATH=" + gopath,
		"GOFLAGS=-buildvcs=false -mod=readonly",
		"GOTOOLCHAIN=local",
		"CGO_ENABLED=0",
		"GOMEMLIMIT=" + differentialMemoryLimit,
		"GIT_CONFIG_GLOBAL=" + gitConfig,
		"GIT_CONFIG_SYSTEM=" + gitConfig,
		"GIT_TERMINAL_PROMPT=0",
	}, nil
}

// differentialEnvironment is the untrusted test environment. Its caches live
// under the per-case root and it has no network: any download is a failed
// dependency, never a side effect of the pull request.
func differentialEnvironment(root string) ([]string, error) {
	env, err := differentialCommonEnvironment(root)
	if err != nil {
		return nil, err
	}
	return append(env, "GOPROXY=off", "GOSUMDB=off"), nil
}

// differentialModuleEnvironment is the trusted setup environment. It may reach
// the public Go module proxy to fill the per-case cache, but it still carries
// no credentials and never executes the repository's tests.
func differentialModuleEnvironment(root string) ([]string, error) {
	env, err := differentialCommonEnvironment(root)
	if err != nil {
		return nil, err
	}
	return append(env, "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org"), nil
}

func differentialGoVersion(ctx context.Context, dir string, env []string) string {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "go", "version")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// cappedBuffer stores at most max bytes and discards the rest, so a chatty test
// can never grow the worker's memory or block on a full pipe.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := b.max - b.buf.Len(); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
