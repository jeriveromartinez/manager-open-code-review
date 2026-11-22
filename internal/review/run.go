package review

import (
	"bytes"
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

	// cloneTokenWait is how long a run sleeps when its clone token expired in
	// the queue. The control process renews it on the next reconciliation.
	cloneTokenWait = time.Minute
	// maxCloneTokenWaits bounds the time spent waiting for that renewal. If it
	// never arrives the App lost access to the repository, and the run ends with
	// an actionable error instead of waiting forever.
	maxCloneTokenWaits = 12
	// cloneTokenClaimMargin is how much life a token needs when the worker
	// claims the run. The control process renews 30 minutes ahead, so anything
	// below this means the renewal is behind.
	cloneTokenClaimMargin = 10 * time.Minute

	cloneTokenWaitMessage   = "el token de clonado caducó en la cola; esperando a que el control lo renueve"
	cloneTokenFailedMessage = "el token de clonado caducó y no se pudo renovar: revisa que la App siga instalada con acceso al repositorio y vuelve a encolar la revisión"

	// interruptedByShutdown is stored on a run a stopping worker put back in
	// the queue, so the next attempt is visible in the panel before it starts.
	interruptedByShutdown = "el worker se detuvo durante la revisión; el run vuelve a la cola"

	// terminalWriteTimeout bounds the writes done after River cancels the job
	// context on shutdown. They run detached from that cancellation so the
	// run never stays "running" until the stale watchdog.
	terminalWriteTimeout = 15 * time.Second
)

// terminalContext returns a context that survives the job cancellation River
// applies when a soft stop exceeds its grace period.
func terminalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
}

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

// CloneTokenStaleError reports that the run was queued again because its clone
// token expired while it waited. The worker snoozes the job until the control
// process renews the token.
type CloneTokenStaleError struct {
	Wait time.Duration
}

func (e *CloneTokenStaleError) Error() string {
	return "token de clonado caducado; el run espera su renovación"
}

// RunnerConfig holds the worker-side settings of the review pipeline.
type RunnerConfig struct {
	WorkspaceDir, OCRBin, OpencodeBin, GitBaseURL string
	FetchDepthMax                                 int
	// ContextCeiling is the global review_context_ceiling. A team policy can
	// override it per run.
	ContextCeiling int
	// Timeout is the whole-run budget. It bounds the execution context and
	// derives OCR's own concurrent-task timeout.
	Timeout time.Duration
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
	// excludedFiles counts the files OCR dropped because of the team's policy,
	// reported in the coverage so the panel does not treat them as missing.
	excludedFiles int
	usage         []byte
	findings      []store.Finding
	resolved      []string
	// tokens accumulates every model call of the run; usage is its JSON form,
	// kept for panels that read the raw event stream.
	tokens tokenUsage

	// Context needed to reconcile findings from passes that already completed
	// when the run ends inconclusively.
	merged AgentOutput
	open   []store.Finding
	runID  uint
	prID   uint
}

// tokenTotals flattens the aggregated usage into the columns the panel reads.
// A run that never called the model writes zeros, which is accurate; runs that
// predate the columns stay NULL because they are never written again. MaxContext
// stays nil when the engine reported no sample (native OCR), so the column is
// NULL and the averages skip it instead of counting a fake zero.
func (o *runOutcome) tokenTotals() store.RunUsage {
	usage := store.RunUsage{
		Input:      int64(o.tokens.Input),
		Output:     int64(o.tokens.Output),
		Reasoning:  int64(o.tokens.Reasoning),
		CacheRead:  int64(o.tokens.Cache.Read),
		CacheWrite: int64(o.tokens.Cache.Write),
		Total:      int64(o.tokens.Total),
	}
	if o.tokens.MaxContext > 0 {
		maxContext := int64(o.tokens.MaxContext)
		usage.MaxContext = &maxContext
	}
	return usage
}

// announce tells open panels that the run's pull request changed state. The
// state is already stored, so a lost notification is only logged.
func (r *Runner) announce(ctx context.Context, run store.ReviewRun) {
	if r.Events == nil {
		return
	}
	ev := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(run.PullRequestID), 10)}
	if err := r.Events.Publish(ctx, ev); err != nil {
		slog.Warn("publish event", "type", ev.Type, "id", ev.ID, "run_id", run.ID, "error", err)
	}
}

// CheckProfile verifies an LLM profile with one minimal provider request. The
// review binary is only the fallback for providers outside the closed table.
func (r *Runner) CheckProfile(ctx context.Context, profileID uint) error {
	return (&ProfileChecker{
		Store:  r.Store,
		Box:    r.Box,
		Bin:    r.Cfg.OpencodeBin,
		Events: r.Events,
	}).Check(ctx, profileID)
}

// Run claims a queued run, announces the claim, executes the pipeline and
// records a terminal state. An unclaimed run (already taken, superseded or
// terminal) is a no-op: no event, no enqueue and no notification. When no
// profile slot is free the run goes back to the queue and the returned
// ProfileCapacityError asks the caller to reschedule; once the waiter budget is
// spent it ends as a capacity failure.
func (r *Runner) Run(ctx context.Context, runID uint) error {
	run, claimed, err := r.Store.ClaimReviewRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("claim review run %d: %w", runID, err)
	}
	if !claimed {
		return nil
	}
	r.announce(ctx, run)
	// The shutdown can cancel the job context between the claim and the first
	// database call. Do not start a pipeline that is already cancelled.
	if ctx.Err() != nil {
		return r.requeueInterrupted(ctx, run)
	}

	var cfg store.ReviewConfig
	var outcome runOutcome
	var retryErr error
	staleToken := staleCloneToken(run.CloneTokenExpiresAt, time.Now())
	if staleToken && run.Attempt <= maxCloneTokenWaits {
		if err := r.Store.RequeueReviewRun(ctx, run.ID, "token_expired", cloneTokenWaitMessage); err != nil {
			if errors.Is(err, store.ErrRunNotActive) {
				// A new head superseded the run while it waited for a token.
				return nil
			}
			return fmt.Errorf("requeue review run %d for token refresh: %w", runID, err)
		}
		r.announce(ctx, run)
		slog.Info("review run waiting for clone token refresh", "run_id", run.ID, "attempt", run.Attempt)
		return &CloneTokenStaleError{Wait: cloneTokenWait}
	}
	if staleToken {
		outcome = (&runOutcome{}).inconclusive(errors.New(cloneTokenFailedMessage), "token_expired")
	} else {
		cfg, err = r.Store.ReviewConfigForPR(ctx, run.PullRequestID)
		if err != nil {
			outcome = (&runOutcome{}).failed(fmt.Errorf("configuración de revisión: %w", err), "config")
		} else {
			outcome, retryErr = r.execute(ctx, run, cfg)
		}
	}

	// A run that already succeeded is persisted below instead of requeued.
	if ctx.Err() != nil && outcome.status != store.RunSucceeded {
		return r.requeueInterrupted(ctx, run)
	}

	persistCtx, cancelPersist := terminalContext(ctx)
	defer cancelPersist()

	if retryErr != nil {
		var capacity *ProfileCapacityError
		if errors.As(retryErr, &capacity) && run.Attempt <= maxCapacityWaits {
			if err := r.Store.RequeueReviewRun(persistCtx, run.ID, capacityErrorCode, capacity.Error()); err != nil {
				if errors.Is(err, store.ErrRunNotActive) {
					// A new head superseded the run while it waited for a slot;
					// there is nothing left to retry.
					return nil
				}
				return fmt.Errorf("requeue review run %d: %w", runID, err)
			}
			// The claim showed the run as started; it is waiting again.
			r.announce(persistCtx, run)
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
	if outcome.tokens.Steps > 0 {
		outcome.usage = outcome.tokens.json()
	}
	if len(outcome.usage) == 0 {
		outcome.usage = []byte(`{}`)
	}
	completion := store.RunCompletion{
		Status:       outcome.status,
		ErrorCode:    outcome.code,
		ErrorMessage: truncateRunMessage(outcome.message),
		Summary:      outcome.summary,
		Coverage:     outcome.coverage,
		Usage:        outcome.usage,
		Tokens:       outcome.tokenTotals(),
		Findings:     outcome.findings,
		Resolved:     outcome.resolved,
	}
	if err := r.Store.CompleteReviewRun(persistCtx, runID, completion); err != nil {
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
			open, err := r.Store.OpenFindingsForPR(persistCtx, run.PullRequestID)
			if err != nil {
				return fmt.Errorf("list open findings of pull request %d: %w", run.PullRequestID, err)
			}
			if hasBlockingFinding(open, cfg.Policy.BlockingThreshold) {
				prStatus = store.StatusChangesRequested
			}
		}
		if err := r.Store.SetPRStatus(persistCtx, run.PullRequestID, prStatus); err != nil {
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
			if err := r.Events.Publish(persistCtx, ev); err != nil && notifyErr == nil {
				notifyErr = fmt.Errorf("publish %s for run %d: %w", ev.Type, runID, err)
			}
		}
	}
	if r.Jobs != nil {
		if err := r.Jobs.EnqueuePublish(persistCtx, runID); err != nil && notifyErr == nil {
			notifyErr = fmt.Errorf("enqueue publish for run %d: %w", runID, err)
		}
	}
	return notifyErr
}

// requeueInterrupted returns a claimed run to the queue when River cancels the
// job context on shutdown. The claim already stored the run as running, so
// leaving it there would keep it stuck until the stale watchdog. The write runs
// detached from the cancellation, and returning ctx.Err() lets River treat a
// soft stop as an interruption and make the job available again without
// spending an attempt.
func (r *Runner) requeueInterrupted(ctx context.Context, run store.ReviewRun) error {
	persistCtx, cancel := terminalContext(ctx)
	defer cancel()
	if err := r.Store.RequeueReviewRun(persistCtx, run.ID, store.RunErrorInterrupted, interruptedByShutdown); err != nil {
		if errors.Is(err, store.ErrRunNotActive) {
			// A new head superseded the run while the worker stopped.
			return nil
		}
		return fmt.Errorf("requeue interrupted review run %d: %w", run.ID, err)
	}
	r.announce(persistCtx, run)
	slog.Info("review run requeued after worker shutdown", "run_id", run.ID)
	return ctx.Err()
}

// execute runs the pipeline and returns the state to persist. Every early exit
// returns a terminal outcome so the caller can always complete the run. The
// error is only set when the run must be retried instead of finished.
func (r *Runner) execute(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig) (runOutcome, error) {
	out := runOutcome{
		coverage: coverageJSON(0, 0, 0),
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

	runCtx := ctx
	if r.Cfg.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, r.Cfg.Timeout)
		defer cancel()
	}
	mergeBase, err := Clone(runCtx, workDir, remote, baseRef, headRef, r.Cfg.FetchDepthMax)
	if err != nil {
		return classifyCloneError(runCtx, out, err)
	}
	headSHA, err := git(runCtx, workDir, "rev-parse", "--no-pager", "rev-parse", "HEAD")
	if err != nil {
		return out.failed(fmt.Errorf("resolver HEAD del checkout: %w", err), "git_error"), nil
	}
	if run.HeadSHA != "" && !strings.EqualFold(headSHA, run.HeadSHA) {
		// refs/pull/<n>/head is a moving ref: a push can land between the
		// trigger and the clone. Reviewing that code and publishing it under
		// run.HeadSHA would approve a SHA that was never reviewed.
		return out.superseded(fmt.Errorf("la ref del PR avanzó de %s a %s antes del checkout", run.HeadSHA, headSHA), "head_moved"), nil
	}

	excludes, dropped, err := policyExcludes(cfg.Policy.ExcludeGlobs)
	if err != nil {
		return out.failed(fmt.Errorf("leer exclusiones de la política: %w", err), "config"), nil
	}
	if len(dropped) > 0 {
		// OCR splits --exclude on commas without escaping, so these patterns
		// cannot reach it intact; warn instead of applying a mangled glob.
		slog.Warn("review excludes dropped: OCR cannot carry commas",
			"run_id", run.ID, "patterns", dropped)
	}
	key, err := r.boxOpen(cfg.Profile.APIKeyEnc)
	if err != nil {
		return out.inconclusive(fmt.Errorf("descifrar clave del perfil %d: %w", cfg.Profile.ID, err), "llm_error"), nil
	}

	// The first run has no BaseSHA, so Clone's merge-base is the reviewed base.
	// It is already computed from the fetched refs and works on shallow clones.
	ocrBase := run.BaseSHA
	if ocrBase == "" {
		ocrBase = mergeBase
	}
	if ocrBase == "" {
		ocrBase, err = fetchHeadBase(workDir)
		if err != nil {
			return out.failed(fmt.Errorf("resolver base del checkout: %w", err), "git_error"), nil
		}
	}

	timeout := r.Cfg.Timeout
	if deadline, ok := runCtx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	// The policy is read on every run, so a team change applies to the next
	// review without restarting the worker.
	ceiling := r.Cfg.ContextCeiling
	if cfg.Policy.ReviewContextCeiling != nil {
		ceiling = *cfg.Policy.ReviewContextCeiling
	}
	if ceiling <= 0 {
		ceiling = reviewContextCeiling
	}
	effort := strings.ToLower(strings.TrimSpace(cfg.Policy.ReviewEffort))
	if !store.ValidReviewEffort(effort) {
		effort = store.ReviewEffortLow
	}
	budget := cfg.Policy.ReviewTokenBudget
	if budget < 0 {
		budget = 0
	}
	reviewer := &NativeReviewer{
		Bin:         r.Cfg.OCRBin,
		RepoDir:     workDir,
		Home:        filepath.Join(workDir, "home-ocr"),
		Profile:     cfg.Profile,
		APIKey:      string(key),
		Excludes:    excludes,
		Background:  buildBackground(cfg.PR, cfg.Policy.ExtraInstructions),
		BaseRef:     ocrBase,
		HeadRef:     headSHA,
		Effort:      effort,
		MaxTokens:   ceiling,
		TokenBudget: budget,
		Timeout:     timeout,
	}

	slog.Info("native OCR review started", "run_id", run.ID, "profile_id", cfg.Profile.ID)
	res, _, err := r.runNativeReview(runCtx, cfg.Profile.ID, reviewer)
	if err != nil {
		var capacity *ProfileCapacityError
		if errors.As(err, &capacity) {
			return out, capacity
		}
		return classifyNativeError(runCtx, out, err)
	}

	status, code := nativeOutcome(res)
	reviewed, skipped, excluded := nativeCoverageCounts(res)
	out.coverage = coverageJSON(reviewed, skipped, excluded)
	out.excludedFiles = excluded
	out.tokens = nativeTokenUsage(res)
	out.summary = nativeSummaryText(res, status)
	out.merged = mergeNativeFindings(res.Comments)

	switch status {
	case store.RunSucceeded:
		upserts, resolved := Reconcile(open, out.merged, run.ID, true)
		for i := range upserts {
			upserts[i].PullRequestID = run.PullRequestID
		}
		out.status = store.RunSucceeded
		out.findings = upserts
		out.resolved = resolved
		return out, nil
	case store.RunInconclusive:
		return out.inconclusive(errors.New(nativeStatusMessage(res, code)), code), nil
	default:
		return out.failed(errors.New(nativeStatusMessage(res, code)), code), nil
	}
}

// runNativeReview holds one profile slot for exactly one OCR invocation.
func (r *Runner) runNativeReview(ctx context.Context, profileID uint, reviewer *NativeReviewer) (nativeReview, []byte, error) {
	release, acquired, err := r.Store.AcquireProfileSlot(ctx, profileID)
	if err != nil {
		return nativeReview{}, nil, fmt.Errorf("bloquear perfil %d: %w", profileID, err)
	}
	if !acquired {
		return nativeReview{}, nil, &ProfileCapacityError{ProfileID: profileID}
	}
	defer release()
	return reviewer.Review(ctx)
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

func coverageJSON(reviewed, skipped, excluded int) []byte {
	raw, err := json.Marshal(struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
		ExcludedFiles int `json:"excluded_files"`
	}{reviewed, skipped, excluded})
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

// policyExcludes builds the value of OCR's --exclude flag. OCR splits the value
// on commas and offers no escaping, so a brace pattern or any other glob with a
// comma cannot survive the flag: those are returned as dropped so the caller
// warns instead of applying a mangled glob. Blank entries are ignored.
func policyExcludes(raw []byte) (string, []string, error) {
	var patterns []string
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &patterns); err != nil {
			return "", nil, fmt.Errorf("exclude_globs no es una lista: %w", err)
		}
	}
	kept := make([]string, 0, len(patterns))
	var dropped []string
	for _, pattern := range patterns {
		p := strings.TrimSpace(pattern)
		switch {
		case p == "":
			continue
		case strings.Contains(p, ","):
			dropped = append(dropped, p)
		default:
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ","), dropped, nil
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

// staleCloneToken reports whether the token expires before it can finish
// cloning. A nil expiry means the run predates the stored expiry: the clone is
// attempted with whatever token it carries.
func staleCloneToken(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && expiresAt.Before(now.Add(cloneTokenClaimMargin))
}

func truncatedContext(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) || ctx.Err() != nil
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

// classifyNativeError handles failures that produced no parseable OCR result.
// A configuration failure is terminal; anything else that is not our own
// deadline is reported as an OCR error and keeps the run inconclusive.
func classifyNativeError(ctx context.Context, out runOutcome, err error) (runOutcome, error) {
	if truncatedContext(ctx, err) {
		return out.inconclusive(err, "timeout"), nil
	}
	message := strings.ToLower(err.Error())
	for _, signal := range []string{
		"no valid llm endpoint", "configuration", "config file", "invalid config",
	} {
		if strings.Contains(message, signal) {
			return out.failed(err, "config"), nil
		}
	}
	return out.inconclusive(err, "ocr_error"), nil
}

func truncateRunMessage(message string) string {
	if len(message) <= maxRunErrorMessage {
		return message
	}
	return message[:maxRunErrorMessage]
}
