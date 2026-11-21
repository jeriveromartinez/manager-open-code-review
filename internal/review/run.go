package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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

// Capacity waits bound how long a run may keep looking for a free profile slot.
// A permanently saturated profile must end as a terminal failure instead of
// being rescheduled forever.
const (
	maxCapacityWaits = 12
	capacityWaitBase = 15 * time.Second
	capacityWaitMax  = 5 * time.Minute

	// capacityErrorCode labels a run that never got a profile slot.
	capacityErrorCode = "capacity"
)

// CapacityWait returns the delay before retrying a run that found no free
// profile slot, growing with the attempts already spent addressing the same
// saturation.
func CapacityWait(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := capacityWaitBase
	for i := 1; i < attempt && wait < capacityWaitMax; i++ {
		wait *= 2
	}
	if wait > capacityWaitMax {
		wait = capacityWaitMax
	}
	return wait
}

// ProfileCapacityError reports that every slot of the profile was busy. The
// worker turns it into a snooze: the run is queued again, not failed.
type ProfileCapacityError struct {
	ProfileID uint
	Wait      time.Duration
}

func (e *ProfileCapacityError) Error() string {
	return fmt.Sprintf("perfil %d sin capacidad disponible", e.ProfileID)
}

// RunnerConfig holds the worker-side settings of the review pipeline.
type RunnerConfig struct {
	WorkspaceDir, OCRBin, OpencodeBin, GitBaseURL string
	FetchDepthMax                                 int
	// Timeout is the whole-run budget, not a per-invocation watchdog: every pass
	// receives whatever is left of it and the run stops when nothing remains.
	Timeout time.Duration
	// Steps caps the agentic iterations of one pass. Zero uses the agent default.
	Steps int
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
// event, no enqueue and no notification. When no profile slot is free the run
// goes back to the queue and the returned ProfileCapacityError asks the caller
// to reschedule; once the waiter budget is spent it ends as a capacity failure.
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
	var retryErr error
	cfg, err = r.Store.ReviewConfigForPR(ctx, run.PullRequestID)
	if err != nil {
		outcome = (&runOutcome{}).failed(fmt.Errorf("configuración de revisión: %w", err), "config")
	} else {
		outcome, retryErr = r.execute(ctx, run, cfg)
	}

	if retryErr != nil {
		var capacity *ProfileCapacityError
		if errors.As(retryErr, &capacity) && run.Attempt <= maxCapacityWaits {
			if err := r.Store.RequeueReviewRun(ctx, run.ID, capacityErrorCode, capacity.Error()); err != nil {
				if errors.Is(err, store.ErrRunNotActive) {
					// A new head superseded the run while it waited for a slot;
					// there is nothing left to retry.
					return nil
				}
				return fmt.Errorf("requeue review run %d: %w", runID, err)
			}
			capacity.Wait = CapacityWait(run.Attempt)
			slog.Info("review run waiting for profile capacity",
				"run_id", run.ID, "profile_id", capacity.ProfileID,
				"attempt", run.Attempt, "wait", capacity.Wait)
			return capacity
		}
		// A saturated profile that outlived the waiter budget is reported as a
		// terminal failure: a queued run cannot stay pending forever.
		outcome = outcome.failed(retryErr, capacityErrorCode)
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
		if errors.Is(err, store.ErrRunNotActive) {
			// A new head superseded this run while it was executing. The
			// trigger already cancelled its check; persisting or publishing
			// stale results would overwrite the state of the current run.
			return nil
		}
		return fmt.Errorf("complete review run %d: %w", runID, err)
	}

	if outcome.status != store.RunSuperseded {
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
	}

	// The terminal transaction is committed, so a notification failure must
	// not prevent the publish job from being enqueued (or vice versa).
	var notifyErr error
	if r.Events != nil {
		notifications := []events.Event{
			{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)},
		}
		if outcome.status != store.RunSuperseded {
			notifications = append(notifications, events.Event{
				Type: "pr.updated",
				ID:   strconv.FormatUint(uint64(run.PullRequestID), 10),
			})
		}
		for _, ev := range notifications {
			if err := r.Events.Publish(ctx, ev); err != nil && notifyErr == nil {
				notifyErr = fmt.Errorf("publish %s for run %d: %w", ev.Type, runID, err)
			}
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
// returns a terminal outcome so the caller can always complete the run. The
// error is only set when the run must be retried instead of finished.
func (r *Runner) execute(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig) (runOutcome, error) {
	out := runOutcome{
		coverage: coverageJSON(0, 0),
		usage:    []byte(`{}`),
		runID:    run.ID,
		prID:     run.PullRequestID,
	}

	open, err := r.Store.OpenFindingsForPR(ctx, run.PullRequestID)
	if err != nil {
		return out.failed(fmt.Errorf("listar hallazgos abiertos: %w", err), "config"), nil
	}
	out.open = open

	token, err := r.boxOpen(run.CloneTokenEnc)
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		if err == nil {
			err = errors.New("token de clonado ausente")
		}
		return out.inconclusive(fmt.Errorf("token de clonado: %w", err), "token_expired"), nil
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
		return out.failed(fmt.Errorf("resolver HEAD del checkout: %w", err), "git_error"), nil
	}
	if run.HeadSHA != "" && !strings.EqualFold(headSHA, run.HeadSHA) {
		// refs/pull/<n>/head is a moving ref: a push can land between the
		// trigger and the clone. Reviewing that code and publishing it under
		// run.HeadSHA would approve a SHA that was never reviewed.
		return out.superseded(fmt.Errorf("la ref del PR avanzó de %s a %s antes del checkout", run.HeadSHA, headSHA), "head_moved"), nil
	}

	ocr := &OCR{Bin: r.Cfg.OCRBin, RepoDir: workDir}
	// El primer run no tiene BaseSHA: usar el nombre de rama no funciona en un
	// clon shallow porque no existe como ref local. FETCH_HEAD sí tiene el SHA
	// exacto que se acaba de pedir como base.
	ocrBase := run.BaseSHA
	if ocrBase == "" {
		ocrBase, err = fetchHeadBase(workDir)
		if err != nil {
			return out.failed(fmt.Errorf("resolver base del checkout: %w", err), "git_error"), nil
		}
	}
	preview, err := ocr.Preview(ctx, ocrBase, headSHA)
	if err != nil {
		return classifyOCR(ctx, out, err)
	}
	baseSHA := preview.MergeBase

	paths := make([]string, 0, len(preview.ReviewableFiles))
	for _, f := range preview.ReviewableFiles {
		paths = append(paths, f.Path)
	}
	groups, err := ocr.Rules(ctx, ocrBase, headSHA, paths)
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
		return out.failed(fmt.Errorf("planificar pasadas: %w", err), "config"), nil
	}

	reviewed := countCovered(passes)
	out.coverage = coverageJSON(reviewed, len(uncovered))
	if len(uncovered) > 0 {
		return out.inconclusive(
			fmt.Errorf("cobertura incompleta: %d archivos sin revisar", len(uncovered)),
			"coverage_incomplete"), nil
	}

	findingIndex := make(map[string]int)
	budget := newRunBudget(r.Cfg.Timeout, nil)
	for idx, pass := range passes {
		passesLeft := len(passes) - idx - 1
		timeout, ok := budget.timeout()
		if !ok {
			return out.budgetExhausted(idx+1, len(passes), passes, r.Cfg.Timeout), nil
		}
		slog.Info("review pass started",
			"run_id", run.ID, "pass", idx+1, "passes", len(passes),
			"files", len(pass.Files), "remaining", timeout)

		diffPath, err := writePassDiff(ctx, workDir, baseSHA, headSHA, pass.Files)
		if err != nil {
			return out.failed(fmt.Errorf("generar diff de pasada: %w", err), "git_error"), nil
		}

		key, err := r.boxOpen(cfg.Profile.APIKeyEnc)
		if err != nil {
			return out.inconclusive(fmt.Errorf("descifrar clave del perfil %d: %w", cfg.Profile.ID, err), "llm_error"), nil
		}
		agent := &Agent{
			Bin:     r.Cfg.OpencodeBin,
			Dir:     workDir,
			Home:    filepath.Join(workDir, "home"),
			Profile: cfg.Profile,
			APIKey:  string(key),
			Timeout: timeout,
			Steps:   r.Cfg.Steps,
		}
		prompt := BuildPrompt(cfg.PR, pass.Rules, open, diffPath)

		text, usage, runErr := r.runAgent(ctx, agent, prompt, diffPath)
		if runErr != nil {
			if budgetCut(runErr, passesLeft) {
				return out.budgetExhausted(idx+1, len(passes), passes, r.Cfg.Timeout), nil
			}
			return classifyAgentError(ctx, out, runErr)
		}
		parsed, err := ValidateOutput(text)
		if err != nil {
			retry := prompt + "\n\nLa salida anterior no validó el esquema: " + err.Error() +
				"\nCorrígela y responde solo con el JSON válido."
			agent.Timeout, ok = budget.timeout()
			if !ok {
				return out.budgetExhausted(idx+1, len(passes), passes, r.Cfg.Timeout), nil
			}
			text, usage, runErr = r.runAgent(ctx, agent, retry, diffPath)
			if runErr != nil {
				if budgetCut(runErr, passesLeft) {
					return out.budgetExhausted(idx+1, len(passes), passes, r.Cfg.Timeout), nil
				}
				return classifyAgentError(ctx, out, runErr)
			}
			parsed, err = ValidateOutput(text)
			if err != nil {
				return out.inconclusive(fmt.Errorf("salida del agente inválida: %w", err), "schema_invalid"), nil
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
	return out, nil
}

// runAgent holds one profile slot for exactly one agent invocation. Agent.Run
// adds the `-f` flag for the attached diff path.
func (r *Runner) runAgent(ctx context.Context, agent *Agent, prompt, diffPath string) (string, []byte, error) {
	release, acquired, err := r.Store.AcquireProfileSlot(ctx, agent.Profile.ID)
	if err != nil {
		return "", nil, fmt.Errorf("bloquear perfil %d: %w", agent.Profile.ID, err)
	}
	if !acquired {
		return "", nil, &ProfileCapacityError{ProfileID: agent.Profile.ID}
	}
	defer release()
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

// superseded discards the work when the PR ref moved away from the SHA the run
// was created for. Nothing is published and no finding is resolved; the publish
// worker only cancels the check run.
func (out runOutcome) superseded(err error, code string) runOutcome {
	out.status = store.RunSuperseded
	out.code = code
	out.message = err.Error()
	out.findings = nil
	out.resolved = nil
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

// budgetExhausted ends the run when the review_timeout budget ran out before
// every pass could run. The message has to name the aborted pass and the files
// left unreviewed: a bare DeadlineExceeded leaves the operator with nothing to
// act on. Inconclusive (not failed) so the findings of completed passes publish.
//
// pass is 1-based and is also the pass that could not run.
func (out runOutcome) budgetExhausted(pass, passes int, all []Pass, budget time.Duration) runOutcome {
	done := pass - 1
	out.coverage = coverageJSON(countCovered(all[:done]), countCovered(all[done:]))
	err := fmt.Errorf(
		"presupuesto agotado en la pasada %d de %d: %d archivos sin revisar; "+
			"aumenta review_timeout en Ajustes → Revisión (actual %s) para completar la revisión",
		pass, passes, countCovered(all[done:]), budget)
	return out.inconclusive(err, "budget_exhausted")
}

func truncatedContext(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) || ctx.Err() != nil
}

// runBudget is the whole-run time budget from the review_timeout setting. Every
// pass spends from it, so a slow pass leaves less for the passes after it
// instead of each invocation getting the full figure as an opaque watchdog.
type runBudget struct {
	total time.Duration
	now   func() time.Time
	start time.Time
}

func newRunBudget(total time.Duration, now func() time.Time) *runBudget {
	if now == nil {
		now = time.Now
	}
	return &runBudget{total: total, now: now, start: now()}
}

// remaining never goes negative. Without a configured budget it is 0.
func (b *runBudget) remaining() time.Duration {
	if b == nil || b.total <= 0 {
		return 0
	}
	left := b.total - b.now().Sub(b.start)
	if left < 0 {
		return 0
	}
	return left
}

// timeout is what a pass may consume: whatever is left of the whole-run budget.
// ok is false when nothing is left, so no further pass may start. Without a
// configured budget it means "no per-pass deadline".
func (b *runBudget) timeout() (time.Duration, bool) {
	if b == nil || b.total <= 0 {
		return 0, true
	}
	left := b.remaining()
	return left, left > 0
}

// budgetCut separates a run that ran out of budget from a single slow pass. The
// per-pass deadline is the whole remaining budget, so a pass cut short while
// passes remain leaves nothing for them: that is budget exhaustion, and the run
// must stop with an actionable message. On the last pass the pre-existing
// `timeout` code is kept.
func budgetCut(err error, passesLeft int) bool {
	return passesLeft > 0 && errors.Is(err, context.DeadlineExceeded)
}

// cloneCredentialSignals are the shapes git and the HTTP stack use to report a
// rejected credential. They are matched against the lowercased error as whole
// tokens or fixed phrases: a bare "401" is also a valid substring of a path, a
// SHA or a count, and acting on it tells the operator to reinstall the App for
// a failure that has nothing to do with credentials.
var cloneCredentialSignals = []*regexp.Regexp{
	// `fatal: unable to access '...': The requested URL returned error: 401`.
	regexp.MustCompile(`\breturned error: 40[13]\b`),
	// `HTTP 401` / `HTTP 403` from the transport.
	regexp.MustCompile(`\bhttp 40[13]\b`),
	// `401 Unauthorized` reported by a proxy in front of the remote.
	regexp.MustCompile(`\b40[13] unauthorized\b`),
	// `Permission denied (publickey).` and the `(publickey,password)` variant.
	regexp.MustCompile(`\bpermission denied \(publickey`),
}

var cloneCredentialPhrases = []string{
	"authentication failed",
	"could not read username",
	"unauthorized",
	// GitHub wording for a revoked or wrongly scoped installation token.
	"invalid username or password",
	"bad credentials",
	"support for password authentication was removed",
}

func classifyCloneError(ctx context.Context, out runOutcome, err error) (runOutcome, error) {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout"), nil
	}
	message := strings.ToLower(err.Error())
	for _, signal := range cloneCredentialSignals {
		if signal.MatchString(message) {
			return out.inconclusive(err, "token_expired"), nil
		}
	}
	for _, phrase := range cloneCredentialPhrases {
		if strings.Contains(message, phrase) {
			return out.inconclusive(err, "token_expired"), nil
		}
	}
	return out.failed(err, "git_error"), nil
}

func classifyOCR(ctx context.Context, out runOutcome, err error) (runOutcome, error) {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout"), nil
	}
	return out.inconclusive(err, "ocr_error"), nil
}

// classifyAgentError keeps a run waiting for a profile slot out of the LLM
// error path: it must be retried, not finished.
func classifyAgentError(ctx context.Context, out runOutcome, err error) (runOutcome, error) {
	var capacity *ProfileCapacityError
	if errors.As(err, &capacity) {
		return out, capacity
	}
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout"), nil
	}
	return out.inconclusive(err, "llm_error"), nil
}

func truncateRunMessage(message string) string {
	if len(message) <= maxRunErrorMessage {
		return message
	}
	return message[:maxRunErrorMessage]
}
