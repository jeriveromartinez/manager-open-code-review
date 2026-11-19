package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// maxRunErrorMessage bounds the error text persisted on a terminal run.
const maxRunErrorMessage = 500

// RunnerConfig holds the worker-side settings of the review pipeline.
type RunnerConfig struct {
	WorkspaceDir, OCRBin, OpencodeBin, GitBaseURL string
	FetchDepthMax                                 int
	Timeout                                       time.Duration
}

// Enqueuer is the minimal queue interface used by the review worker. Task 8
// reuses this exact interface for the trigger and the health checks.
type Enqueuer interface {
	EnqueueReview(ctx context.Context, runID uint) error
	EnqueuePublish(ctx context.Context, runID uint) error
	EnqueueLLMHealth(ctx context.Context, profileID uint) error
}

// Runner executes one queued review run end to end.
type Runner struct {
	Store  *store.Store
	Apps   github.AppProvider
	Box    *crypto.Box
	Events events.Publisher
	Jobs   Enqueuer
	Cfg    RunnerConfig
}

// runOutcome is the complete terminal state written by CompleteReviewRun.
type runOutcome struct {
	status   string
	code     string
	message  string
	summary  string
	coverage []byte
	usage    []byte
	findings []store.Finding
	resolved []string

	// Context needed to reconcile findings from passes that already completed
	// when the run ends inconclusively.
	merged AgentOutput
	open   []store.Finding
	runID  uint
	prID   uint
}

// CheckProfile verifies an LLM profile with the same OpenCode binary and
// timeout as the review pipeline.
func (r *Runner) CheckProfile(ctx context.Context, profileID uint) error {
	return (&ProfileChecker{
		Store:   r.Store,
		Box:     r.Box,
		Bin:     r.Cfg.OpencodeBin,
		Timeout: r.Cfg.Timeout,
	}).Check(ctx, profileID)
}

// Run claims a queued run, executes the pipeline and records a terminal state.
// An unclaimed run (already taken, superseded or terminal) is a no-op: no
// event, no enqueue and no notification.
func (r *Runner) Run(ctx context.Context, runID uint) error {
	run, claimed, err := r.Store.ClaimReviewRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("claim review run %d: %w", runID, err)
	}
	if !claimed {
		return nil
	}

	var cfg store.ReviewConfig
	var outcome runOutcome
	cfg, err = r.Store.ReviewConfigForPR(ctx, run.PullRequestID)
	if err != nil {
		outcome = (&runOutcome{}).failed(fmt.Errorf("configuración de revisión: %w", err), "config")
	} else {
		outcome = r.execute(ctx, run, cfg)
	}

	if len(outcome.coverage) == 0 {
		outcome.coverage = []byte(`{}`)
	}
	if len(outcome.usage) == 0 {
		outcome.usage = []byte(`{}`)
	}
	if err := r.Store.CompleteReviewRun(ctx, runID, outcome.status, outcome.code,
		truncateRunMessage(outcome.message), outcome.summary, outcome.coverage,
		outcome.usage, outcome.findings, outcome.resolved); err != nil {
		return fmt.Errorf("complete review run %d: %w", runID, err)
	}

	prStatus := store.StatusInconclusive
	if outcome.status == store.RunSucceeded {
		prStatus = store.StatusReadyForHuman
		open, err := r.Store.OpenFindingsForPR(ctx, run.PullRequestID)
		if err != nil {
			return fmt.Errorf("list open findings of pull request %d: %w", run.PullRequestID, err)
		}
		if hasBlockingFinding(open, cfg.Policy.BlockingThreshold) {
			prStatus = store.StatusChangesRequested
		}
	}
	if err := r.Store.SetPRStatus(ctx, run.PullRequestID, prStatus); err != nil {
		return fmt.Errorf("set QA status of pull request %d: %w", run.PullRequestID, err)
	}

	// The terminal transaction is committed, so a notification failure must
	// not prevent the publish job from being enqueued (or vice versa).
	var notifyErr error
	if r.Events != nil {
		ev := events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)}
		if err := r.Events.Publish(ctx, ev); err != nil && notifyErr == nil {
			notifyErr = fmt.Errorf("publish run.updated for run %d: %w", runID, err)
		}
	}
	if r.Jobs != nil {
		if err := r.Jobs.EnqueuePublish(ctx, runID); err != nil && notifyErr == nil {
			notifyErr = fmt.Errorf("enqueue publish for run %d: %w", runID, err)
		}
	}
	return notifyErr
}

// execute runs the pipeline and returns the state to persist. Every early exit
// returns a terminal outcome so the caller can always complete the run.
func (r *Runner) execute(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig) runOutcome {
	out := runOutcome{
		coverage: coverageJSON(0, 0),
		usage:    []byte(`{}`),
		runID:    run.ID,
		prID:     run.PullRequestID,
	}

	open, err := r.Store.OpenFindingsForPR(ctx, run.PullRequestID)
	if err != nil {
		return out.failed(fmt.Errorf("listar hallazgos abiertos: %w", err), "config")
	}
	out.open = open

	token, err := r.boxOpen(run.CloneTokenEnc)
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		if err == nil {
			err = errors.New("token de clonado ausente")
		}
		return out.inconclusive(fmt.Errorf("token de clonado: %w", err), "token_expired")
	}

	workDir := filepath.Join(r.Cfg.WorkspaceDir, fmt.Sprintf("run-%d", run.ID))
	defer os.RemoveAll(workDir)

	baseRef := run.BaseSHA
	if baseRef == "" {
		baseRef = cfg.PR.BaseRef
	}
	headRef := fmt.Sprintf("refs/pull/%d/head", cfg.PR.Number)
	remote := strings.TrimSuffix(r.Cfg.GitBaseURL, "/") + "/" + cfg.Repo.FullName + ".git"
	remote = withCloneToken(remote, token)

	if _, err := Clone(ctx, workDir, remote, baseRef, headRef, r.Cfg.FetchDepthMax); err != nil {
		return classifyCloneError(ctx, out, err)
	}
	headSHA, err := git(ctx, workDir, "rev-parse", "--no-pager", "rev-parse", "HEAD")
	if err != nil {
		return out.failed(fmt.Errorf("resolver HEAD del checkout: %w", err), "git_error")
	}

	ocr := &OCR{Bin: r.Cfg.OCRBin, RepoDir: workDir}
	preview, err := ocr.Preview(ctx, baseRef, headSHA)
	if err != nil {
		return classifyOCR(ctx, out, err)
	}
	baseSHA := preview.MergeBase

	paths := make([]string, 0, len(preview.ReviewableFiles))
	for _, f := range preview.ReviewableFiles {
		paths = append(paths, f.Path)
	}
	groups, err := ocr.Rules(ctx, baseRef, headSHA, paths)
	if err != nil {
		return classifyOCR(ctx, out, err)
	}

	passes, uncovered, err := PlanPasses(PlanInput{
		Files:        preview.ReviewableFiles,
		Groups:       groups,
		MaxFiles:     cfg.Policy.MaxFilesPerPass,
		MaxDiffLines: cfg.Policy.MaxDiffLinesPerPass,
		MaxPasses:    cfg.Policy.MaxPasses,
	})
	if err != nil {
		return out.failed(fmt.Errorf("planificar pasadas: %w", err), "config")
	}

	reviewed := countCovered(passes)
	out.coverage = coverageJSON(reviewed, len(uncovered))
	if len(uncovered) > 0 {
		return out.inconclusive(
			fmt.Errorf("cobertura incompleta: %d archivos sin revisar", len(uncovered)),
			"coverage_incomplete")
	}

	findingIndex := make(map[string]int)
	for _, pass := range passes {
		diffPath, err := writePassDiff(ctx, workDir, baseSHA, headSHA, pass.Files)
		if err != nil {
			return out.failed(fmt.Errorf("generar diff de pasada: %w", err), "git_error")
		}

		key, err := r.boxOpen(cfg.Profile.APIKeyEnc)
		if err != nil {
			return out.inconclusive(fmt.Errorf("descifrar clave del perfil %d: %w", cfg.Profile.ID, err), "llm_error")
		}
		agent := &Agent{
			Bin:     r.Cfg.OpencodeBin,
			Dir:     workDir,
			Home:    filepath.Join(workDir, "home"),
			Profile: cfg.Profile,
			APIKey:  string(key),
			Timeout: r.Cfg.Timeout,
		}
		prompt := BuildPrompt(cfg.PR, pass.Rules, open, diffPath)

		text, usage, runErr := r.runAgent(ctx, agent, prompt, diffPath)
		if runErr != nil {
			return classifyAgentError(ctx, out, runErr)
		}
		parsed, err := ValidateOutput(text)
		if err != nil {
			retry := prompt + "\n\nLa salida anterior no validó el esquema: " + err.Error() +
				"\nCorrígela y responde solo con el JSON válido."
			text, usage, runErr = r.runAgent(ctx, agent, retry, diffPath)
			if runErr != nil {
				return classifyAgentError(ctx, out, runErr)
			}
			parsed, err = ValidateOutput(text)
			if err != nil {
				return out.inconclusive(fmt.Errorf("salida del agente inválida: %w", err), "schema_invalid")
			}
		}

		out.usage = normalizeJSON(usage)
		if parsed.Summary != "" {
			out.summary = parsed.Summary
		}
		for _, finding := range parsed.Findings {
			fp := Fingerprint(finding, nil)
			if i, seen := findingIndex[fp]; seen {
				out.merged.Findings[i] = finding // última pasada gana, posición estable
			} else {
				findingIndex[fp] = len(out.merged.Findings)
				out.merged.Findings = append(out.merged.Findings, finding)
			}
		}
	}

	upserts, resolved := Reconcile(open, out.merged, run.ID, true)
	for i := range upserts {
		upserts[i].PullRequestID = run.PullRequestID
	}
	out.status = store.RunSucceeded
	out.findings = upserts
	out.resolved = resolved
	return out
}

// runAgent holds the profile lock for exactly one agent invocation. Agent.Run
// adds the `-f` flag for the attached diff path.
func (r *Runner) runAgent(ctx context.Context, agent *Agent, prompt, diffPath string) (string, []byte, error) {
	unlock, err := r.Store.LockProfile(ctx, agent.Profile.ID)
	if err != nil {
		return "", nil, fmt.Errorf("bloquear perfil %d: %w", agent.Profile.ID, err)
	}
	defer unlock()
	return agent.Run(ctx, prompt, []string{diffPath})
}

func (r *Runner) boxOpen(sealed string) ([]byte, error) {
	if r.Box == nil {
		return nil, errors.New("crypto box no configurado")
	}
	return r.Box.Open(sealed)
}

func withCloneToken(remote string, token []byte) string {
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || len(token) == 0 {
		return remote
	}
	u.User = url.UserPassword("x-access-token", string(token))
	return u.String()
}

// writePassDiff executes the git diff for one pass and writes its output to a
// temporary file inside the ephemeral workspace, attached to the agent prompt.
func writePassDiff(ctx context.Context, dir, baseSHA, headSHA string, files []string) (string, error) {
	args := []string{"-C", dir, "diff", "--unified=3", baseSHA, headSHA, "--"}
	args = append(args, files...)
	out, err := git(ctx, dir, "diff", args...)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "qa-diff-*.patch")
	if err != nil {
		return "", fmt.Errorf("crear archivo de diff: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(out); err != nil {
		return "", fmt.Errorf("escribir archivo de diff: %w", err)
	}
	return f.Name(), nil
}

func countCovered(passes []Pass) int {
	seen := make(map[string]bool)
	for _, pass := range passes {
		for _, file := range pass.Files {
			seen[file] = true
		}
	}
	return len(seen)
}

func coverageJSON(reviewed, skipped int) []byte {
	raw, err := json.Marshal(struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	}{reviewed, skipped})
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

func normalizeJSON(raw []byte) []byte {
	if len(raw) == 0 || !json.Valid(raw) || string(raw) == "null" {
		return []byte(`{}`)
	}
	return raw
}

func hasBlockingFinding(findings []store.Finding, threshold string) bool {
	limit := severityRank(threshold)
	for _, f := range findings {
		if f.Status == store.FindingOpen && severityRank(f.Severity) >= limit {
			return true
		}
	}
	return false
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "blocker", "critical":
		return 4
	case "major", "high":
		return 3
	case "minor", "medium":
		return 2
	case "nit", "low":
		return 1
	default:
		return 3
	}
}

// failed preserves coverage and usage from earlier passes, but never findings:
// design §8 requires failed runs not to touch them.
func (out runOutcome) failed(err error, code string) runOutcome {
	out.status = store.RunFailed
	out.code = code
	out.message = err.Error()
	return out
}

// inconclusive also preserves findings from passes that already completed
// (design §8): publish new comments but resolve nothing. Reconcile with
// complete=false builds the upserts without producing resolutions. Failed runs
// go through failed() and therefore persist no findings.
func (out runOutcome) inconclusive(err error, code string) runOutcome {
	out.status = store.RunInconclusive
	out.code = code
	out.message = err.Error()
	if len(out.merged.Findings) > 0 {
		upserts, _ := Reconcile(out.open, out.merged, out.runID, false)
		for i := range upserts {
			upserts[i].PullRequestID = out.prID
		}
		out.findings = upserts
		out.resolved = nil
	}
	return out
}

func truncatedContext(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) || ctx.Err() != nil
}

func classifyCloneError(ctx context.Context, out runOutcome, err error) runOutcome {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout")
	}
	message := strings.ToLower(err.Error())
	for _, needle := range []string{"401", "unauthorized", "authentication failed", "could not read username", "permission denied"} {
		if strings.Contains(message, needle) {
			return out.inconclusive(err, "token_expired")
		}
	}
	return out.failed(err, "git_error")
}

func classifyOCR(ctx context.Context, out runOutcome, err error) runOutcome {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout")
	}
	return out.inconclusive(err, "ocr_error")
}

func classifyAgentError(ctx context.Context, out runOutcome, err error) runOutcome {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout")
	}
	return out.inconclusive(err, "llm_error")
}

func truncateRunMessage(message string) string {
	if len(message) <= maxRunErrorMessage {
		return message
	}
	return message[:maxRunErrorMessage]
}
