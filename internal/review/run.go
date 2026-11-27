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

	// cloneTokenWaitMessage is stored when a run is requeued waiting for its
	// clone token to be renewed.
	cloneTokenWaitMessage   = "el token de clonado caducó en la cola; esperando a que el control lo renueve"
	cloneTokenFailedMessage = "el token de clonado caducó y no se pudo renovar: revisa que la App siga instalada con acceso al repositorio y vuelve a encolar la revisión"

	// comparisonMovedMessage labels a run superseded because its base moved
	// while the model was reviewing.
	comparisonMovedMessage = "la comparación base–head cambió durante la revisión"

	// interruptedByShutdown is stored on a run a stopping worker put back in
	// the queue, so the next attempt is visible in the panel before it starts.
	interruptedByShutdown = "el worker se detuvo durante la revisión; el run vuelve a la cola"

	// terminalWriteTimeout bounds the writes done after River cancels the job
	// context on shutdown. They run detached from that cancellation so the
	// run never stays "running" until the stale watchdog.
	terminalWriteTimeout = 15 * time.Second

	// heartbeatWriteTimeout bounds the heartbeat write. It runs detached from
	// the pipeline cancellation so a stopped review can still record why.
	heartbeatWriteTimeout = 5 * time.Second
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
	WorkspaceDir, OCRBin, GitBaseURL string
	// GitHubAPIURL is the REST endpoint the worker uses to fetch a cited file for
	// an objection. Empty disables the fetch and escalates the objection.
	GitHubAPIURL  string
	FetchDepthMax int
	// ContextCeiling is the global review_context_ceiling. A team policy can
	// override it per run.
	ContextCeiling int
	// Timeout is the whole-run budget. It bounds the execution context and is
	// the global deadline OCR's groups share; when it approaches, the worker
	// asks OCR to close gracefully before the hard stop.
	Timeout time.Duration
}

// Enqueuer schedules the review, publication and health jobs the runner owes.
type Enqueuer interface {
	EnqueueReview(ctx context.Context, runID uint) error
	// EnqueueReviewRecovery schedules a re-delivery of a run whose lease
	// expired. The generation distinguishes the recovery delivery from the
	// abandoned job of an earlier attempt, which River would otherwise treat as
	// a live duplicate; the claim still lets only one worker recover it.
	EnqueueReviewRecovery(ctx context.Context, runID uint, generation int) error
	EnqueuePublish(ctx context.Context, runID uint) error
	EnqueueLLMHealth(ctx context.Context, profileID uint) error
}

// Runner executes one queued review run end to end.
type Runner struct {
	Store  *store.Store
	Box    *crypto.Box
	Events events.Publisher
	Jobs   Enqueuer
	Cfg    RunnerConfig

	// ObjectionVerifier overrides the bounded verifier used to reevaluate a
	// reply on a finding's thread. Nil builds the native OCR verifier.
	ObjectionVerifier ObjectionVerifier

	// CriteriaEvaluator overrides the focused pass that checks the linked
	// issue's criteria (#232). Nil builds the native OCR evaluator.
	CriteriaEvaluator CriterionEvaluator

	// heartbeatEvery overrides heartbeatInterval in tests. Zero uses the
	// production interval.
	heartbeatEvery time.Duration
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
	// baseSHA is the resolved reviewed base, persisted so the next push can be
	// reviewed incrementally when it still applies.
	baseSHA string
	// tokens accumulates every model call of the run; usage is its JSON form,
	// kept for panels that read the raw event stream.
	tokens tokenUsage
	// tokensKnown is set only when OCR reported a summary, so a cut run persists
	// NULL instead of a fake zero (#129).
	tokensKnown bool

	// decisions is the verifier's answer per finding. verificationSkipped names
	// why the second pass did not run, so the panel can explain the missing
	// triage instead of showing it as an empty answer.
	decisions           []Decision
	verifyUsage         []byte
	verifySessionID     string
	verificationSkipped string
	verificationError   string
	// executedProfile is the destination that actually produced the result,
	// which may be a fallback rather than the profile the run was enqueued
	// with. Verification and triage attribution use it (#150).
	executedProfile store.LLMProfile

	// architecture is the structured observational verdict of the base–head
	// comparison. It is JSON persisted on the run and never changes the review
	// outcome.
	architecture []byte

	// Context needed to reconcile findings from passes that already completed
	// when the run ends inconclusively.
	merged  AgentOutput
	open    []store.Finding
	runID   uint
	prID    uint
	anchors AnchorProvider
}

// tokenTotals flattens the aggregated usage into the columns the panel reads.
// The caller persists it only when tokensKnown is true; without an engine result
// the columns stay NULL and the averages skip the run instead of counting a fake
// zero. MaxContext stays nil when the engine reported no sample (native OCR), so
// the column is NULL and the averages skip it.
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

// setPhase persists the current step of a running run and tells panels. A
// terminal or superseded run rejects the write, which is expected when a phase
// update races the completion: the review must never fail for it, so only a
// real write error is logged.
func (r *Runner) setPhase(ctx context.Context, runID uint, owner, phase string, log *runLogSink) {
	if err := r.Store.SetReviewRunPhase(ctx, runID, owner, phase); err != nil {
		if !errors.Is(err, store.ErrRunNotActive) && !errors.Is(err, store.ErrRunLeaseLost) {
			log.Log(ctx, "worker", "", "warn", phase,
				fmt.Sprintf("no se pudo guardar la fase: %v", err))
		}
		return
	}
	if r.Events == nil {
		return
	}
	ev := events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)}
	if err := r.Events.Publish(ctx, ev); err != nil {
		slog.Warn("publish phase event", "run_id", runID, "phase", phase, "error", err)
	}
}

// CheckProfile verifies an LLM profile with one minimal provider request over
// the same native endpoint OCR resolves for a review.
func (r *Runner) CheckProfile(ctx context.Context, profileID uint) error {
	return (&ProfileChecker{
		Store:  r.Store,
		Box:    r.Box,
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
		// The result may already be stored by a worker that died before
		// enqueueing its publication, or the recovery budget may have just
		// failed the run. Either way only the delivery is owed, and the
		// publication path settles it idempotently (#228).
		return r.resumePublication(ctx, run)
	}
	r.announce(ctx, run)
	log := newRunLogSink(r.Store, r.Events, run.ID, 0)
	if run.RecoveryCount > 0 {
		log.Log(ctx, "worker", "", "warn", logPhaseClaim, fmt.Sprintf(
			"run %d recuperado (intento de recuperación %d): %s",
			run.ID, run.RecoveryCount, run.ErrorMessage))
	} else {
		log.Log(ctx, "worker", "", "info", logPhaseClaim, fmt.Sprintf(
			"run %d reclamado: PR #%d, head %s, intento %d", run.ID, run.PullRequestID, run.HeadSHA, run.Attempt))
	}
	// The shutdown can cancel the job context between the claim and the first
	// database call. Do not start a pipeline that is already cancelled.
	if ctx.Err() != nil {
		return r.requeueInterrupted(ctx, run, log)
	}
	r.setPhase(ctx, run.ID, run.LeaseOwner, store.RunPhasePreparing, log)

	var cfg store.ReviewConfig
	var outcome runOutcome
	var retryErr error
	staleToken := staleCloneToken(run.CloneTokenExpiresAt, time.Now())
	if staleToken && run.Attempt <= maxCloneTokenWaits {
		log.Log(ctx, "worker", "", "warn", logPhasePrepare, cloneTokenWaitMessage)
		if err := r.Store.RequeueReviewRun(ctx, run.ID, run.LeaseOwner, "token_expired", cloneTokenWaitMessage); err != nil {
			if errors.Is(err, store.ErrRunNotActive) || errors.Is(err, store.ErrRunLeaseLost) {
				// A new head superseded the run, or another worker recovered
				// it, while it waited for a token.
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
			log.Log(ctx, "worker", "", "error", logPhasePrepare,
				fmt.Sprintf("configuración de revisión: %v", err))
			outcome = (&runOutcome{}).failed(fmt.Errorf("configuración de revisión: %w", err), "config")
		} else if policyErr := r.Store.SetReviewRunPolicy(ctx, run.ID, run.LeaseOwner, cfg.PolicyRevision, cfg.PolicyObservation); policyErr != nil {
			// The policy is re-resolved here because it can change while the run
			// waits. Recording it before executing keeps the revision on the run
			// describing what it actually enforced (#153).
			if errors.Is(policyErr, store.ErrRunNotActive) || errors.Is(policyErr, store.ErrRunLeaseLost) {
				return nil
			}
			log.Log(ctx, "worker", "", "error", logPhasePrepare,
				fmt.Sprintf("registrar política de revisión: %v", policyErr))
			outcome = (&runOutcome{}).failed(
				fmt.Errorf("registrar política de revisión: %w", policyErr), "config")
		} else {
			run.PolicyRevision = cfg.PolicyRevision
			run.PolicyObservation = cfg.PolicyObservation
			outcome, retryErr = r.execute(ctx, run, cfg, log)
		}
	}

	// A run that already succeeded is persisted below instead of requeued.
	if ctx.Err() != nil && outcome.status != store.RunSucceeded {
		return r.requeueInterrupted(ctx, run, log)
	}

	persistCtx, cancelPersist := terminalContext(ctx)
	defer cancelPersist()

	if retryErr != nil {
		var capacity *ProfileCapacityError
		if errors.As(retryErr, &capacity) && run.Attempt <= maxCapacityWaits {
			if err := r.Store.RequeueReviewRun(persistCtx, run.ID, run.LeaseOwner, capacityErrorCode, capacity.Error()); err != nil {
				if errors.Is(err, store.ErrRunNotActive) || errors.Is(err, store.ErrRunLeaseLost) {
					// A new head superseded the run, or another worker
					// recovered it, while it waited for a slot; there is
					// nothing left to retry.
					return nil
				}
				return fmt.Errorf("requeue review run %d: %w", runID, err)
			}
			// The claim showed the run as started; it is waiting again.
			r.announce(persistCtx, run)
			log.Log(persistCtx, "worker", "", "warn", logPhaseOCR,
				fmt.Sprintf("sin slots libres en el perfil %d; el run vuelve a la cola", capacity.ProfileID))
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

	// The base can move while the model is reviewing. Re-check the comparison
	// right before persisting so findings, resolutions and triage of a stale
	// base are never written, and the QA status keeps describing the run that
	// owns the current comparison.
	if outcome.status != store.RunSuperseded {
		moved, cmpErr := r.comparisonMovedAfterReview(persistCtx, run, outcome.baseSHA)
		switch {
		case cmpErr != nil:
			slog.Warn("comparison re-check failed; completing the run",
				"run_id", run.ID, "error", cmpErr)
		case moved:
			log.Log(persistCtx, "worker", "", "warn", logPhaseFinish, comparisonMovedMessage)
			outcome = (&runOutcome{}).superseded(errors.New(comparisonMovedMessage), "comparison_moved")
		}
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
		Status:               outcome.status,
		ErrorCode:            outcome.code,
		ErrorMessage:         truncateRunMessage(outcome.message),
		Summary:              outcome.summary,
		Coverage:             outcome.coverage,
		Usage:                outcome.usage,
		Tokens:               outcome.tokenTotals(),
		TokensKnown:          outcome.tokensKnown,
		Findings:             outcome.findings,
		Resolved:             outcome.resolved,
		BaseSHA:              outcome.baseSHA,
		VerifySessionID:      outcome.verifySessionID,
		VerifyUsage:          outcome.verifyUsage,
		ArchitectureAnalysis: outcome.architecture,
	}
	if err := r.Store.CompleteReviewRun(persistCtx, runID, run.LeaseOwner, completion); err != nil {
		if errors.Is(err, store.ErrRunLeaseLost) {
			// Another execution recovered the run after this lease expired.
			// Its state and units belong to the new owner, so this worker must
			// not delete them nor publish its stale result (#228).
			log.Log(persistCtx, "worker", "", "warn", logPhaseFinish,
				"otro worker recuperó la revisión; no se guarda este resultado")
			return nil
		}
		if errors.Is(err, store.ErrRunNotActive) {
			// A new head superseded this run while it was executing. The
			// trigger already cancelled its check; persisting or publishing
			// stale results would overwrite the state of the current run.
			r.removeRunState(runID)
			// The run can no longer be resumed, so its unit state must not
			// outlive the supersession.
			if err := r.Store.DeleteReviewUnits(persistCtx, runID); err != nil {
				slog.Warn("delete units of superseded run", "run_id", runID, "error", err)
			}
			return nil
		}
		log.Log(persistCtx, "worker", "", "error", logPhaseFinish,
			fmt.Sprintf("no se pudo guardar el resultado del run: %v", err))
		return fmt.Errorf("complete review run %d: %w", runID, err)
	}
	// The unit state is only useful while the run can be resumed; retention
	// ends with the terminal write.
	r.removeRunState(runID)
	log.Log(persistCtx, "worker", "", terminalLogLevel(outcome.status), logPhaseFinish,
		fmt.Sprintf("run %s%s: %s", outcome.status, terminalCodeSuffix(outcome.code), outcome.message))

	if outcome.status == store.RunSucceeded {
		r.applyAutoTriage(persistCtx, run, cfg, outcome, log)
	}

	if outcome.status != store.RunSuperseded {
		pr, err := r.Store.PullRequest(persistCtx, run.PullRequestID)
		if err != nil {
			return fmt.Errorf("load pull request %d: %w", run.PullRequestID, err)
		}
		// The QA status is shared by every run of the comparison. Only the run
		// that owns the current one may write it: a stale head, a moved base or
		// an unresolved comparison must be left to the run that does, or to the
		// publish path once the trigger adopts the comparison (#138).
		owner, current, err := r.Store.CurrentVerdictRun(persistCtx, pr)
		if err != nil {
			return fmt.Errorf("resolve current verdict of pull request %d: %w", run.PullRequestID, err)
		}
		if current && owner.ID == run.ID {
			prStatus := store.StatusInconclusive
			if outcome.status == store.RunSucceeded {
				prStatus = store.StatusReadyForHuman
				open, err := r.Store.OpenFindingsForPR(persistCtx, run.PullRequestID)
				if err != nil {
					return fmt.Errorf("list open findings of pull request %d: %w", run.PullRequestID, err)
				}
				if hasBlockingFinding(open, cfg.Policy.BlockingThreshold, run.HeadSHA, cfg.Policy.AutoTriageMode) {
					prStatus = store.StatusChangesRequested
				}
			}
			if err := r.Store.SetPRStatus(persistCtx, run.PullRequestID, prStatus); err != nil {
				return fmt.Errorf("set QA status of pull request %d: %w", run.PullRequestID, err)
			}
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

// resumePublication schedules the delivery of a run whose terminal result is
// already stored. It is the only work a re-delivery owes once the claim reports
// the run is no longer claimable. Any other state (another live owner, a queued
// run being handled elsewhere) needs nothing from this delivery.
func (r *Runner) resumePublication(ctx context.Context, run store.ReviewRun) error {
	if run.ID == 0 || !run.PublishPending || !isTerminalRunStatus(run.Status) || r.Jobs == nil {
		return nil
	}
	slog.Info("resuming owed publication of review run", "run_id", run.ID, "status", run.Status)
	if err := r.Jobs.EnqueuePublish(ctx, run.ID); err != nil {
		return fmt.Errorf("enqueue publish for review run %d: %w", run.ID, err)
	}
	return nil
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case store.RunSucceeded, store.RunInconclusive, store.RunFailed, store.RunSuperseded:
		return true
	}
	return false
}

// applyAutoTriage records every verifier answer and applies the decisions the
// policy accepted. It runs after the findings are durable, so a crash here
// loses the triage but never the review. A pull request whose head moved while
// the run was verifying keeps its findings untouched: the verdicts describe
// code that is no longer there.
func (r *Runner) applyAutoTriage(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig, out runOutcome, log *runLogSink) {
	if len(out.decisions) == 0 {
		if out.verificationSkipped != "" {
			log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
				"verificación omitida: "+out.verificationSkipped)
		}
		return
	}
	current, err := r.Store.ReviewConfigForPR(ctx, run.PullRequestID)
	if err != nil {
		log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
			fmt.Sprintf("no se pudo recargar el PR; no se aplica el triage: %v", err))
		return
	}
	if !strings.EqualFold(current.PR.HeadSHA, run.HeadSHA) {
		log.Log(ctx, "worker", "verify", "warn", logPhaseOCR, fmt.Sprintf(
			"el head del PR cambió de %s a %s; no se aplica el triage",
			run.HeadSHA, current.PR.HeadSHA))
		return
	}

	actor := "ocr-verify:" + out.executedProfile.Model
	confirmed, withdrawn, skipped := 0, 0, 0
	for _, decision := range out.decisions {
		// A finding the verifier never answered gets an explicit uncertain
		// record with its reason; a previous run's evidence is stale at this
		// head and must not stay published as a confirmation.
		if !decision.Verification.VerifiedAt.IsZero() || decision.Verification.Reason != "" {
			raw, err := json.Marshal(decision.Verification)
			if err != nil {
				log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
					fmt.Sprintf("no se pudo serializar la verificación de %s: %v", decision.FindingID, err))
			} else if err := r.Store.RecordFindingVerification(ctx, decision.FindingID, raw); err != nil {
				log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
					fmt.Sprintf("no se pudo guardar la verificación de %s: %v", decision.FindingID, err))
			}
		}
		if !decision.Apply {
			continue
		}
		if _, err := r.Store.TriageFinding(ctx, decision.FindingID, decision.Status, store.FindingAudit{
			Action: "finding.auto_triaged",
			Actor:  actor,
			Source: store.TriageSourceVerifier,
			Reason: decision.Reason,
		}); err != nil {
			if !errors.Is(err, store.ErrFindingHumanTriaged) {
				log.Log(ctx, "worker", "verify", "warn", logPhaseOCR,
					fmt.Sprintf("no se pudo aplicar el triage de %s: %v", decision.FindingID, err))
			}
			skipped++
			continue
		}
		if decision.Status == store.FindingWithdrawn {
			withdrawn++
		} else {
			confirmed++
		}
	}
	log.Log(ctx, "worker", "verify", "info", logPhaseOCR, fmt.Sprintf(
		"auto-triage: %d confirmados, %d descartados, %d omitidos", confirmed, withdrawn, skipped))
}

// requeueInterrupted returns a claimed run to the queue when River cancels the
// job context on shutdown. The claim already stored the run as running, so
// leaving it there would keep it stuck until the stale watchdog. The write runs
// detached from the cancellation, and returning ctx.Err() lets River treat a
// soft stop as an interruption and make the job available again without
// spending an attempt.
func (r *Runner) requeueInterrupted(ctx context.Context, run store.ReviewRun, log *runLogSink) error {
	persistCtx, cancel := terminalContext(ctx)
	defer cancel()
	log.Log(persistCtx, "worker", "", "warn", logPhaseFinish, interruptedByShutdown)
	if err := r.Store.RequeueReviewRun(persistCtx, run.ID, run.LeaseOwner, store.RunErrorInterrupted, interruptedByShutdown); err != nil {
		if errors.Is(err, store.ErrRunNotActive) || errors.Is(err, store.ErrRunLeaseLost) {
			// A new head superseded the run, or another worker already
			// recovered it, while this one stopped.
			return nil
		}
		return fmt.Errorf("requeue interrupted review run %d: %w", run.ID, err)
	}
	r.announce(persistCtx, run)
	slog.Info("review run requeued after worker shutdown", "run_id", run.ID)
	return ctx.Err()
}

// newNativeReviewer builds one OCR invocation bound to a concrete profile,
// isolated home and effective budget. The resume state key and the attempt row
// use the same candidate, so a unit is always attributed to the profile that
// produced it (#149, #150).
func newNativeReviewer(bin, repoDir, home string, profile store.LLMProfile, key []byte,
	excludes, background, baseRef, headRef, effort string, maxTokens int,
	tokenBudget int64, timeout time.Duration, onLog func(string)) *NativeReviewer {
	return &NativeReviewer{
		Bin:         bin,
		RepoDir:     repoDir,
		Home:        home,
		Profile:     profile,
		APIKey:      string(key),
		Excludes:    excludes,
		Background:  background,
		BaseRef:     baseRef,
		HeadRef:     headRef,
		Effort:      effort,
		MaxTokens:   maxTokens,
		TokenBudget: tokenBudget,
		Timeout:     timeout,
		OnLog:       onLog,
	}
}

// execute runs the pipeline and returns the state to persist. Every early exit
// returns a terminal outcome so the caller can always complete the run. The
// error is only set when the run must be retried instead of finished.
func (r *Runner) execute(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig, log *runLogSink) (runOutcome, error) {
	out := runOutcome{
		coverage: coverageJSON(0, 0, 0),
		usage:    []byte(`{}`),
		runID:    run.ID,
		prID:     run.PullRequestID,
	}
	log.Log(ctx, "worker", "", "info", logPhasePrepare, fmt.Sprintf(
		"PR #%d %q de %s en %s", cfg.PR.Number, cfg.PR.Title, cfg.PR.AuthorLogin, cfg.Repo.FullName))
	log.Log(ctx, "worker", "", "info", logPhasePolicy, fmt.Sprintf(
		"perfil %d (%s/%s), esfuerzo %s", cfg.Profile.ID, cfg.Profile.Provider, cfg.Profile.Model, cfg.Policy.ReviewEffort))

	// Every known finding, not only the open ones: a withdrawn finding keeps its
	// ID, triage and stored verdict across runs only if Reconcile can match it.
	// Its absence from the review does not resolve it either.
	known, err := r.Store.FindingsForPR(ctx, run.PullRequestID)
	if err != nil {
		log.Log(ctx, "worker", "", "error", logPhasePrepare,
			fmt.Sprintf("no se pudieron listar los hallazgos conocidos: %v", err))
		return out.failed(fmt.Errorf("listar hallazgos conocidos: %w", err), "config"), nil
	}
	out.open = known
	log.Log(ctx, "worker", "", "info", logPhasePrepare,
		fmt.Sprintf("hallazgos conocidos antes de revisar: %d", len(known)))

	token, err := r.boxOpen(run.CloneTokenEnc)
	if err != nil || len(strings.TrimSpace(string(token))) == 0 {
		if err == nil {
			err = errors.New("token de clonado ausente")
		}
		log.Log(ctx, "worker", "", "error", logPhaseClone,
			fmt.Sprintf("token de clonado: %v", err))
		return out.inconclusive(fmt.Errorf("token de clonado: %w", err), "token_expired"), nil
	}
	log.AddSecret(token)

	workDir := filepath.Join(r.Cfg.WorkspaceDir, fmt.Sprintf("run-%d", run.ID))
	defer os.RemoveAll(workDir)
	out.anchors = checkoutAnchors(workDir)

	baseRef := run.BaseSHA
	if baseRef == "" {
		baseRef = cfg.PR.BaseRef
	}
	headRef := pullHeadRef(cfg.PR.Number)
	remote := reviewRemote(r.Cfg.GitBaseURL, cfg.Repo.FullName)
	remote = withCloneToken(remote, token)

	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()
	runCtx := pipelineCtx
	// The aggregate time budget spans every attempt of the run. A requeue
	// resets started_at, so the origin is the first claim (#149).
	budgetTimeout := r.Cfg.Timeout
	if budgetTimeout > 0 && run.BudgetStartedAt != nil {
		budgetTimeout -= time.Since(*run.BudgetStartedAt)
	}
	if r.Cfg.Timeout > 0 {
		if budgetTimeout <= 0 {
			log.Log(runCtx, "worker", "", "warn", logPhaseFinish,
				"el tiempo agregado del run se agotó antes de reanudar")
			return out.inconclusive(errors.New(
				"el tiempo agregado del run se agotó antes de reanudar la revisión"), "timeout"), nil
		}
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(pipelineCtx, budgetTimeout)
		defer cancelTimeout()
	}
	if cfg.PR.External() {
		// The contribution is fetched from the base repository's pull ref, never
		// from the fork, and the checkout stays detached and read-only. The SHA
		// check below refuses a ref that moved after the trigger.
		log.Log(runCtx, "worker", "", "info", logPhaseClone,
			fmt.Sprintf("aporte externo: revisión de solo lectura desde %s %s", cfg.Repo.FullName, headRef))
	}
	// The heartbeat reports that the worker is alive, not that the pipeline
	// still has budget: it outlives the review deadline so a run that overran
	// it is classified as overrun and not as a lost worker (#130).
	stopHeartbeat := r.heartbeat(pipelineCtx, run.ID, run.LeaseOwner, log, cancelPipeline)
	defer stopHeartbeat()
	r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseCloning, log)
	log.Log(runCtx, "worker", "", "info", logPhaseClone,
		fmt.Sprintf("clonando %s (%s → %s)", cfg.Repo.FullName, baseRef, headRef))
	cloneStarted := time.Now()
	mergeBase, err := Clone(runCtx, workDir, remote, baseRef, headRef, r.Cfg.FetchDepthMax)
	if err != nil {
		log.Log(runCtx, "worker", "", "error", logPhaseClone, fmt.Sprintf("falló el clonado: %v", err))
		return classifyCloneError(runCtx, out, err)
	}
	headSHA, err := git(runCtx, workDir, "rev-parse", "--no-pager", "rev-parse", "HEAD")
	if err != nil {
		log.Log(runCtx, "worker", "", "error", logPhaseClone,
			fmt.Sprintf("no se pudo resolver el HEAD del checkout: %v", err))
		return out.failed(fmt.Errorf("resolver HEAD del checkout: %w", err), "git_error"), nil
	}
	log.Log(runCtx, "worker", "", "info", logPhaseClone,
		fmt.Sprintf("checkout listo en %s: head %s, merge-base %s",
			time.Since(cloneStarted).Round(time.Second), headSHA, mergeBase))
	if checkoutHeadMoved(run.HeadSHA, headSHA) {
		// refs/pull/<n>/head is a moving ref: a push can land between the
		// trigger and the clone. Reviewing that code and publishing it under
		// run.HeadSHA would approve a SHA that was never reviewed.
		log.Log(runCtx, "worker", "", "warn", logPhaseClone,
			fmt.Sprintf("la ref del PR avanzó de %s a %s antes del checkout", run.HeadSHA, headSHA))
		return out.superseded(fmt.Errorf("la ref del PR avanzó de %s a %s antes del checkout", run.HeadSHA, headSHA), "head_moved"), nil
	}

	// Re-check the comparison the run was created for: a retarget while it
	// waited in the queue must not review (or publish) code under the old key.
	current, cfgErr := r.Store.ReviewConfigForPR(runCtx, run.PullRequestID)
	if cfgErr != nil {
		log.Log(runCtx, "worker", "", "warn", logPhasePrepare,
			fmt.Sprintf("no se pudo recargar el PR; la publicación comprobará la clave: %v", cfgErr))
	} else {
		switch {
		case !strings.EqualFold(current.PR.HeadSHA, run.HeadSHA):
			return out.superseded(fmt.Errorf("el head del PR cambió de %s a %s",
				run.HeadSHA, current.PR.HeadSHA), "head_moved"), nil
		case run.BaseRef != "" && current.PR.BaseRef != run.BaseRef:
			return out.superseded(fmt.Errorf("la rama base cambió de %s a %s",
				run.BaseRef, current.PR.BaseRef), "comparison_moved"), nil
		case run.BaseSHA != "" && current.PR.MergeBaseSHA != "" &&
			!strings.EqualFold(run.BaseSHA, current.PR.MergeBaseSHA):
			return out.superseded(fmt.Errorf("el merge-base cambió de %s a %s",
				run.BaseSHA, current.PR.MergeBaseSHA), "comparison_moved"), nil
		case mergeBase != "" && current.PR.MergeBaseSHA != "" &&
			!strings.EqualFold(mergeBase, current.PR.MergeBaseSHA):
			// The checkout resolved a different merge base than the one the PR
			// now carries: the base moved while the run was queued.
			return out.superseded(fmt.Errorf("el merge-base del checkout %s no es el actual %s",
				mergeBase, current.PR.MergeBaseSHA), "comparison_moved"), nil
		}
		baseRef := run.BaseRef
		if baseRef == "" {
			baseRef = current.PR.BaseRef
		}
		if baseRef != "" && mergeBase != "" && !strings.EqualFold(current.PR.MergeBaseSHA, mergeBase) {
			if _, err := r.Store.SetPRMergeBase(runCtx, run.PullRequestID, run.HeadSHA, baseRef, current.PR.BaseSHA, mergeBase); err != nil {
				log.Log(runCtx, "worker", "", "warn", logPhasePrepare,
					fmt.Sprintf("no se pudo guardar el merge-base del PR: %v", err))
			}
		}
	}

	r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhasePreparing, log)
	excludePatterns, dropped, err := policyExcludePatterns(cfg.Policy.ExcludeGlobs)
	if err != nil {
		log.Log(runCtx, "worker", "", "error", logPhasePolicy,
			fmt.Sprintf("exclusiones de la política: %v", err))
		return out.failed(fmt.Errorf("leer exclusiones de la política: %w", err), "config"), nil
	}
	excludes := strings.Join(excludePatterns, ",")
	if len(dropped) > 0 {
		// OCR splits --exclude on commas without escaping, so these patterns
		// cannot reach it intact; warn instead of applying a mangled glob.
		slog.Warn("review excludes dropped: OCR cannot carry commas",
			"run_id", run.ID, "patterns", dropped)
		log.Log(runCtx, "worker", "", "warn", logPhasePolicy,
			fmt.Sprintf("exclusiones descartadas por contener comas: %s", strings.Join(dropped, ", ")))
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
			log.Log(runCtx, "worker", "", "error", logPhaseClone,
				fmt.Sprintf("no se pudo resolver la base del checkout: %v", err))
			return out.failed(fmt.Errorf("resolver base del checkout: %w", err), "git_error"), nil
		}
	}
	out.baseSHA = ocrBase
	log.Log(runCtx, "worker", "", "info", logPhaseClone, "base de la revisión: "+ocrBase)

	// A push is reviewed only from the last succeeded head when the base did
	// not move and the new head still descends from it. A missing, failed or
	// outdated prior run, a base change or a force-push all review the full
	// diff: resolving findings outside the range would be wrong.
	//
	// The previous architectural evidence is revalidated against the push
	// (#163), but it never widens this range: the incremental base and its
	// coverage are decided only by the review comparison. Evidence that stays
	// valid is carried; invalidated dimensions are marked stale.
	sourceDigests := checkoutSourceDigests(workDir)
	ruleVersion := architectureRuleVersion(cfg.Policy, excludePatterns)
	fromSHA := ocrBase
	incremental := false
	var carried store.ArchitectureVerdict
	carryEvidence := false
	prev, prevErr := r.Store.PreviousSucceededRun(runCtx, run.PullRequestID, run.ID)
	prevArch := store.ArchitectureVerdict{}
	comparisonChanged := false
	if prevErr == nil {
		prevArch = store.DecodeArchitectureVerdict(prev.ArchitectureAnalysis)
		comparisonChanged = prev.BaseSHA != "" && !strings.EqualFold(prev.BaseSHA, ocrBase)
		if run.BaseRef != "" && prev.BaseRef != "" && run.BaseRef != prev.BaseRef {
			comparisonChanged = true
		}
	}
	switch {
	case prevErr == nil:
		revalidation := revalidateArchitectureEvidence(prevArch, ruleVersion, nil, comparisonChanged, sourceDigests)
		if from, ok := incrementalSource(run, prev, ocrBase, headSHA); ok {
			// A shallow clone may lack the prior head: that is a failure of the
			// ancestry check and falls back to the full review.
			if _, err := git(runCtx, workDir, "merge-base --is-ancestor", "merge-base", "--is-ancestor", prev.HeadSHA, headSHA); err == nil {
				fromSHA = from
				incremental = true
				// The evidence is revalidated against the push, but its verdict
				// never decides how much of the pull request OCR reviews: the
				// incremental range and its coverage stay untouched (#163).
				if pushFiles, pushErr := gitChangedFiles(runCtx, workDir, prev.HeadSHA, headSHA); pushErr == nil {
					revalidation = revalidateArchitectureEvidence(prevArch, ruleVersion, architectureChangedPaths(pushFiles), false, sourceDigests)
				}
			} else {
				// A base change (or a force-push) makes the earlier comparison a
				// different context: its evidence cannot be reused.
				revalidation = revalidateArchitectureEvidence(prevArch, ruleVersion, nil, true, sourceDigests)
				log.Log(runCtx, "worker", "analysis", "warn", logPhasePrepare,
					"la comparación cambió; la evidencia arquitectónica queda pendiente de revalidación")
			}
		}
		carried = revalidation.Verdict
		carryEvidence = carried.Requested && len(carried.Dimensions) > 0
	case errors.Is(prevErr, store.ErrNotFound):
		// No prior complete run: the full diff is the only safe range.
		log.Log(runCtx, "worker", "", "info", logPhasePrepare,
			"sin run previo completado: se revisa el diff completo")
	default:
		slog.Warn("incremental review check failed; reviewing the full diff",
			"run_id", run.ID, "error", prevErr)
		log.Log(runCtx, "worker", "", "warn", logPhasePrepare,
			fmt.Sprintf("no se pudo comprobar el run previo; se revisa el diff completo: %v", prevErr))
	}
	if incremental {
		log.Log(runCtx, "worker", "", "info", logPhasePrepare,
			fmt.Sprintf("revisión incremental desde %s", fromSHA))
	} else {
		log.Log(runCtx, "worker", "", "info", logPhasePrepare,
			fmt.Sprintf("revisión completa desde %s hasta %s", fromSHA, headSHA))
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
	reviewBudget := cfg.Policy.ReviewTokenBudget
	if reviewBudget < 0 {
		reviewBudget = 0
	}

	// The architectural analysis runs as its own phase after the review report
	// is persisted, with its own budget and state. The diff inventory decides
	// whether an explicit signal fires; the same inventory is reused to confront
	// coverage after the review.
	inventory, invErr := gitChangedFiles(runCtx, workDir, fromSHA, headSHA)
	archStats, statsErr := gitArchitectureStats(runCtx, workDir, fromSHA, headSHA)
	if statsErr != nil {
		log.Log(runCtx, "worker", "analysis", "warn", logPhasePolicy,
			fmt.Sprintf("no se pudieron medir las líneas del diff para el análisis: %v", statsErr))
	}
	if invErr == nil {
		archStats = architectureStatsFromFiles(inventory, archStats)
	}
	analysisMode := architectureMode(cfg.Policy)
	signals := []string{}
	scope := store.ArchitectureScope{}
	// The contract comparison is local and deterministic: it reads the base
	// and head trees, never the model. A changed declared contract is an
	// explicit public-interface signal for the architectural analysis.
	contracts := store.ArchitectureContracts{Status: store.ContractsNotDeclared}
	if invErr == nil {
		signals = detectArchitectureSignals(inventory, archStats)
		if analysisMode == store.ArchitectureAnalysisObservation {
			contracts = compareVersionedContracts(runCtx, workDir, fromSHA, headSHA)
			if contractComparisonChanged(contracts) {
				signals = appendContractSignal(signals)
			}
		}
		sortArchitectureSignals(signals)
		scope = architectureScope(inventory, excludePatterns)
	}
	// The trusted baseline is deterministic and cheap, so it runs whenever the
	// architecture mode observes, even when no OCR signal fired. It resolves
	// rules from the reviewed base (ocrBase) and inventories its own full
	// ocrBase..headSHA range, so an incremental review does not shrink the set
	// of files the trusted rules are checked against.
	baselineEvaluated := analysisMode == store.ArchitectureAnalysisObservation
	var baseline store.ArchitectureBaseline
	if baselineEvaluated {
		baselineInventory, baselineInvErr := gitChangedFiles(runCtx, workDir, ocrBase, headSHA)
		baseline = checkArchitectureBaseline(
			gitBaselineTree{ctx: runCtx, dir: workDir, base: ocrBase, head: headSHA},
			cfg.Policy, baselineInventory)
		baseline.BaseSHA = ocrBase
		if baselineInvErr != nil {
			baseline.Status = store.ArchitectureBaselineUnknown
			baseline.Errors = append(baseline.Errors,
				fmt.Sprintf("no se pudo inventariar el cambio para la línea base: %v", baselineInvErr))
		}
	}
	// The control process resolved the authorized consumers with the App
	// credentials and stored a masked snapshot on the run (#160). The worker
	// only attaches it; a missing snapshot is an explicit limitation, never a
	// silent pass.
	consumerImpact := store.DecodeConsumerImpact(run.ConsumerImpact)
	if consumerImpact.Status == "" {
		if analysisMode == store.ArchitectureAnalysisObservation {
			consumerImpact = unavailableConsumerImpact(runCtx, workDir, fromSHA, cfg.Policy, cfg.Repo.Private)
		} else {
			consumerImpact = store.ArchitectureConsumers{Status: store.ConsumersNotConfigured}
		}
	}
	// Consumer evidence is rendered independently: it never forces the OCR
	// analysis on and never changes #157's explicit activation rule.
	consumerRequested := consumerLookupRelevant(consumerImpact)
	analysisRequested := architectureAnalysisRequested(analysisMode, invErr, signals)
	if !analysisRequested && carryEvidence && analysisMode == store.ArchitectureAnalysisObservation &&
		architectureEvidenceStale(carried) && invErr == nil {
		// Earlier evidence lost its source: the review must re-check it even
		// when this push would not have activated the analysis on its own.
		analysisRequested = true
		log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy,
			"evidencia arquitectónica previa pendiente de revalidación: se repite el análisis sobre el rango acumulado")
	}
	analysisBudget := architectureBudget(cfg.Policy, timeout, reviewBudget)
	// The analysis phase keeps the run's original remaining time as its outer
	// bound; the analysis share is enforced inside it. The verifier reserve
	// below shrinks the review pass, not the observation.
	analysisRunTimeout := timeout
	// The analysis phase spends from the same aggregate budget. OCR applies
	// --max-tokens-budget to the discarded re-review and the analysis cap
	// separately, so carve both shares out before the verifier's share; the
	// phase cannot exceed ReviewTokenBudget. Its time share bounds its own
	// phase; the run deadline still caps the total.
	if analysisRequested && reviewBudget > 0 {
		// A phase pass may not take more than half the configured budget: the
		// other half must remain a positive cap for the review itself.
		if half := reviewBudget / 2; analysisBudget.TokenBudget > half {
			analysisBudget.TokenBudget = half
		}
		if analysisBudget.TokenBudget < 1 {
			analysisBudget.TokenBudget = 1
		}
		reviewBudget -= 2 * analysisBudget.TokenBudget
		if reviewBudget < 1 {
			reviewBudget = 1
		}
	}

	// The verifier's share is carved out before the main pass starts, so the
	// first pass (and its chunks) cannot spend the budget reserved for it.
	triage := verifyPolicy(cfg.Policy)
	reservePolicy := cfg.Policy
	reservePolicy.ReviewTokenBudget = reviewBudget
	reserved := reserveVerification(reservePolicy, triage, timeout)
	budget := reviewBudget
	if reserved.enabled {
		budget = reserved.mainTokens
		timeout = reserved.mainTime
		if timeout < 0 {
			timeout = 0
		}
	}
	budgetText := "sin tope"
	if budget > 0 {
		budgetText = fmt.Sprint(budget)
	}
	log.Log(runCtx, "worker", "", "info", logPhasePolicy, fmt.Sprintf(
		"política: esfuerzo %s, max_tokens %d, presupuesto %s, timeout %s",
		effort, ceiling, budgetText, timeout.Round(time.Second)))
	if reserved.enabled {
		log.Log(runCtx, "worker", "", "info", logPhasePolicy, fmt.Sprintf(
			"verificación: cuota de %d tokens y %s; solo se reserva si puede iniciarse",
			reserved.tokens, reserved.timeout.Round(time.Second)))
	}

	// The plan is built before the first OCR group. It links every changed file
	// to its ranges and dependencies, records the evidence that makes a change
	// critical, and decides the order in which the budget-limited routes spend
	// their share (#231). The verification and persistence reserves were carved
	// out above, so a unit can never spend the time meant for checking findings.
	var plan reviewPlan
	if invErr == nil {
		verifyReserve := time.Duration(0)
		if reserved.enabled {
			verifyReserve = reserved.timeout
		}
		plan = buildReviewPlan(runCtx, planInputs{
			Dir:             workDir,
			BaseSHA:         fromSHA,
			HeadSHA:         headSHA,
			Changed:         inventory,
			ExcludePatterns: excludePatterns,
			CriticalGlobs:   planCriticalGlobs(cfg.Policy.CriticalGlobs),
			Contracts:       contracts,
			ContextBytes:    chunkByteBudget(ceiling),
			ReviewTimeout:   timeout,
			VerifyReserve:   verifyReserve,
		})
		log.Log(runCtx, "worker", "", "info", logPhasePolicy, fmt.Sprintf(
			"plan de cobertura: %d unidad(es), %d crítica(s), %d archivo(s); reserva verificación %ds, persistencia %ds",
			len(plan.Units), plan.CriticalTotal, len(inventory), plan.VerifyReserveSeconds, plan.PersistReserveSeconds))
	}

	architectureVerdict := newArchitectureVerdict(analysisMode, analysisRequested, signals, scope,
		store.ArchitectureComparison{
			Repository: cfg.Repo.FullName,
			Mode:       "range",
			BaseRef:    cfg.PR.BaseRef,
			HeadRef:    cfg.PR.HeadRef,
			BaseSHA:    fromSHA,
			HeadSHA:    headSHA,
		}, analysisBudget)
	architectureVerdict = attachContractComparison(architectureVerdict, contracts)
	// The operational dimensions are deterministic evidence from the change, so
	// they are evaluated even when the engine analysis has no signal. A failed
	// inventory yields an explicit unknown. They never enter the review outcome.
	opsEvidence, opsEvaluated := evaluateRunOpsEvidence(analysisMode, invErr, workDir, inventory)
	if opsEvaluated {
		architectureVerdict = applyOpsEvidence(architectureVerdict, opsEvidence)
		log.Log(runCtx, "worker", "ops", opsLogLevel(opsEvidence.Status), logPhasePolicy,
			fmt.Sprintf("evidencia operativa %s: %s", opsEvidence.Status, opsEvidenceSummary(opsEvidence)))
	}
	architectureVerdict.Consumers = consumerImpact
	if invErr != nil {
		architectureVerdict.Errors = append(architectureVerdict.Errors,
			fmt.Sprintf("no se pudo inventariar el cambio: %v", invErr))
	}
	if !analysisRequested && carryEvidence && analysisMode == store.ArchitectureAnalysisObservation {
		// The push did not touch the evidence: keep the previous verdict
		// instead of silently dropping it on the incremental path.
		architectureVerdict = carryArchitectureVerdict(carried, ocrBase, headSHA, analysisMode)
		architectureVerdict.Comparison.Repository = cfg.Repo.FullName
		architectureVerdict.Comparison.BaseRef = cfg.PR.BaseRef
		architectureVerdict.Comparison.HeadRef = cfg.PR.HeadRef
		log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy, fmt.Sprintf(
			"evidencia arquitectónica conservada desde %s: %s",
			architectureVerdict.CarriedFrom, architectureVerdict.Status))
	}
	if baselineEvaluated {
		architectureVerdict = applyArchitectureBaseline(architectureVerdict, baseline)
		baselineLevel := "info"
		if baseline.Status != store.ArchitectureBaselinePass {
			baselineLevel = "warn"
		}
		log.Log(runCtx, "worker", "analysis", baselineLevel, logPhasePolicy,
			fmt.Sprintf("línea base de arquitectura %s: %d regla(s), %d decisión(es), %d comprobación(es)",
				baseline.Status, len(baseline.Rules), len(baseline.Decisions), len(baseline.Checks)))
	}
	out.architecture = store.ArchitectureVerdictJSON(store.MaskRedactedConsumers(architectureVerdict))
	if len(contracts.Contracts) > 0 {
		log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy, fmt.Sprintf(
			"contratos: %s, %d comparado(s) de %d declarado(s)",
			contracts.Status, contracts.Compared, contracts.Declared))
	}
	switch {
	case analysisRequested:
		log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy, fmt.Sprintf(
			"análisis arquitectónico solicitado (%s): señales %s; presupuesto %d tokens, %ds",
			analysisMode, strings.Join(signals, ", "),
			analysisBudget.TokenBudget, analysisBudget.TimeBudgetSeconds))
	case analysisMode == store.ArchitectureAnalysisObservation:
		log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy,
			"análisis arquitectónico no solicitado: el cambio no activó ninguna señal")
		if consumerRequested {
			log.Log(runCtx, "worker", "analysis", "info", logPhasePolicy,
				fmt.Sprintf("consumidores evaluados sin análisis arquitectónico: estado %s", consumerImpact.Status))
		}
	}

	// The review context is profile-independent, so it is built once and reused
	// by every fallback attempt; each attempt still gets its own reviewer.
	linked := linkedContext{}
	if len(run.LinkedIssues) > 0 {
		if err := json.Unmarshal(run.LinkedIssues, &linked); err != nil {
			log.Log(runCtx, "worker", "", "warn", logPhasePrepare,
				fmt.Sprintf("contexto: no se pudo leer el contexto de issues enlazados: %v", err))
		}
	}
	if len(linked.Issues) > 0 {
		log.Log(runCtx, "worker", "", "info", logPhasePrepare,
			fmt.Sprintf("contexto: %d referencia(s) a issues enlazados", len(linked.Issues)))
	}
	changedFiles, err := changedPaths(runCtx, workDir, ocrBase, headSHA)
	if err != nil {
		log.Log(runCtx, "worker", "", "warn", logPhasePrepare,
			fmt.Sprintf("contexto: no se pudo listar el diff para contexto del repositorio: %v", err))
	}
	contextBytes := backgroundContextBudget(ceiling, budget)
	repoBudget := minInt(repositoryContextBudget(ceiling, budget), contextBytes/4)
	repoCtx := buildRepositoryContext(workDir, headSHA, changedFiles, err, repoBudget)
	for _, note := range repoCtx.Notes {
		log.Log(runCtx, "worker", "", "info", logPhasePrepare, "contexto: "+note)
	}
	background := buildBackground(cfg.PR, cfg.Policy.ExtraInstructions, backgroundExtras{
		linked:          linked,
		repo:            repoCtx,
		contextBytes:    contextBytes,
		contextBytesSet: true,
	})
	contextRev := reviewContextRevision(background)

	resume := r.loadResume(runCtx, run.ID)
	candidates := r.reviewProfiles(runCtx, cfg)
	var (
		res             nativeReview
		status, code    string
		activeProfile   store.LLMProfile
		activeKey       []byte
		activeScope     unitScope
		activeMainKey   string
		mainReused      bool
		mainRaw         []byte
		nativeErr       error
		activeAttemptID uint
		attemptBase     tokenUsage
		attemptKnown    bool
		attemptSeq      int
		attemptStarted  time.Time
		resultReady     bool
		reviewer        *NativeReviewer
		budgetBlocked   bool
		failedUsageLost bool
	)
	// A return after an attempt started closes it with the work it actually did.
	// Usage is the delta over the attempt's own snapshot, so the run total keeps
	// the cumulative spend while each destination is credited once.
	defer func() {
		if activeAttemptID == 0 {
			return
		}
		attemptStatus := store.AttemptFailed
		if out.status == store.RunSucceeded {
			attemptStatus = store.AttemptSucceeded
		}
		usage, known := runUsageFromTokens(tokenDelta(out.tokens, attemptBase), attemptKnown || out.tokensKnown)
		if err := r.Store.FinishReviewAttempt(context.WithoutCancel(ctx), activeAttemptID,
			attemptStatus, out.code, out.message, usage, known); err != nil {
			slog.Warn("finish review attempt", "run_id", run.ID, "attempt_id", activeAttemptID, "error", err)
		}
	}()

	for index, candidate := range candidates {
		scope := newUnitScopeFor(cfg, candidate, fromSHA, headSHA, effort, ceiling, budget)
		scope.Context = contextRev
		mainKey := scope.unitKey("main", "", "")
		reused, reusable := resume.reusableReview(mainKey)
		if !reusable && index > 0 {
			if !canStartFallback(runCtx, budget, out.tokens) {
				log.Log(runCtx, "worker", "", "warn", logPhaseOCR,
					"no queda presupuesto ni tiempo para un perfil de respaldo")
				break
			}
			log.Log(runCtx, "worker", "", "warn", logPhaseOCR, fmt.Sprintf(
				"reintentando con el perfil de respaldo %d (%s/%s)",
				candidate.ID, candidate.Provider, candidate.Model))
		}

		key, keyErr := r.boxOpen(candidate.APIKeyEnc)
		if keyErr != nil {
			if index == 0 {
				log.Log(runCtx, "worker", "", "error", logPhasePolicy,
					fmt.Sprintf("no se pudo descifrar la clave del perfil %d: %v", candidate.ID, keyErr))
				return out.inconclusive(fmt.Errorf("descifrar clave del perfil %d: %w", candidate.ID, keyErr), "llm_error"), nil
			}
			log.Log(runCtx, "worker", "", "warn", logPhaseOCR, fmt.Sprintf(
				"se omite el respaldo %d: no se pudo descifrar su clave: %v", candidate.ID, keyErr))
			continue
		}
		log.AddSecret(key)

		// A compatible unit keeps the profile that produced it: the key covers
		// the executing candidate and the policy snapshot (#149, #150).
		if reusable {
			activeProfile, activeKey = candidate, key
			activeScope, activeMainKey = scope, mainKey
			attemptBase = out.tokens
			attemptStarted = time.Now()
			res = reused
			mainReused = true
			status, code = nativeOutcome(res)
			resume.markUsage(res.hasSummary)
			// Chunks still need the checkout, the executing profile and its
			// isolated home, even though the main pass itself is reused.
			home := filepath.Join(runStateDir(r.Cfg.WorkspaceDir, run.ID), fmt.Sprintf("home-ocr-%d", candidate.ID))
			reviewer = newNativeReviewer(r.Cfg.OCRBin, workDir, home, candidate, key,
				excludes, background, fromSHA, headSHA,
				effort, ceiling, 0, timeout, log.OCRLogger(runCtx, "main"))
			log.Log(runCtx, "worker", "main", "info", logPhaseOCR, fmt.Sprintf(
				"se reutiliza el resultado principal del perfil %d de un intento anterior", candidate.ID))
			resultReady = true
			break
		}

		// The main pass shares the aggregate spend of the run with the chunks
		// and every prior attempt. Never pass 0: OCR reads it as "no cap".
		attemptBudget := int64(0)
		if budget > 0 {
			attemptBudget = chunkAllowance(budget, resume.spent, resume.usageKnown)
			if attemptBudget <= 0 {
				budgetBlocked = true
				log.Log(runCtx, "worker", "main", "warn", logPhaseOCR,
					"no se ejecuta la pasada principal: el presupuesto de tokens agregado está agotado")
				break
			}
		}

		attempt, startErr := r.Store.StartReviewAttempt(runCtx, run.ID, run.LeaseOwner, candidate, attemptConfigRevision(cfg, candidate))
		if startErr != nil {
			log.Log(runCtx, "worker", "", "error", logPhasePolicy,
				fmt.Sprintf("no se pudo registrar el intento con el perfil %d: %v", candidate.ID, startErr))
			return out.failed(fmt.Errorf("registrar el intento con el perfil %d: %w", candidate.ID, startErr), "config"), nil
		}
		activeAttemptID = attempt.ID
		attemptSeq = attempt.Seq
		activeProfile, activeKey = candidate, key
		activeScope, activeMainKey = scope, mainKey
		attemptBase = out.tokens
		attemptKnown = false

		home := filepath.Join(runStateDir(r.Cfg.WorkspaceDir, run.ID), fmt.Sprintf("home-ocr-%d", candidate.ID))
		reviewer = newNativeReviewer(r.Cfg.OCRBin, workDir, home, candidate, key,
			excludes, background, fromSHA, headSHA,
			effort, ceiling, attemptBudget, timeout, log.OCRLogger(runCtx, "main"))

		r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseReviewing, log)
		slog.Info("native OCR review started", "run_id", run.ID, "profile_id", candidate.ID,
			"attempt", attemptSeq, "from", fromSHA)
		log.Log(runCtx, "worker", "main", "info", logPhaseOCR,
			fmt.Sprintf("iniciando OCR sobre %s..%s", fromSHA, headSHA))
		attemptStarted = time.Now()
		sessionBefore := snapshotOCRSessions(reviewer.Home)
		if session := resumeSession(run, scope); session != "" {
			reviewer.ResumeSession = session
			log.Log(runCtx, "worker", "main", "info", logPhaseOCR,
				"reanudando la sesión de OCR de un intento anterior")
		}
		reviewer.OnSession = func(sessionID string) {
			r.saveSession(runCtx, run.ID, run.LeaseOwner, sessionID, scope.key())
		}
		res, mainRaw, nativeErr = r.runNativeReview(runCtx, candidate.ID, reviewer, log, "main")
		var capacity *ProfileCapacityError
		if nativeErr != nil && reviewer.ResumeSession != "" &&
			!errors.As(nativeErr, &capacity) && !truncatedContext(runCtx, nativeErr) {
			// A session can be gone or unusable (for example after OCR was
			// upgraded). Do not persist its failure: discard the session and
			// retry the pending main unit once without --resume.
			log.Log(runCtx, "worker", "main", "warn", logPhaseOCR,
				fmt.Sprintf("la sesión de OCR no se pudo reanudar; se reintenta la pasada completa: %v", nativeErr))
			r.clearSession(runCtx, run.ID, run.LeaseOwner)
			reviewer.ResumeSession = ""
			sessionBefore = snapshotOCRSessions(reviewer.Home)
			res, mainRaw, nativeErr = r.runNativeReview(runCtx, candidate.ID, reviewer, log, "main")
		}
		if nativeErr != nil {
			if sessionID := newestOCRSession(reviewer.Home, sessionBefore); sessionID != "" {
				r.saveSession(runCtx, run.ID, run.LeaseOwner, sessionID, scope.key())
			}
			log.Log(runCtx, "worker", "main", "error", logPhaseOCR, fmt.Sprintf("OCR falló: %v", nativeErr))
			if errors.As(nativeErr, &capacity) {
				if err := r.Store.FinishReviewAttempt(runCtx, activeAttemptID,
					store.AttemptFailed, "capacity", capacity.Error(), nil, false); err != nil {
					slog.Warn("finish review attempt", "run_id", run.ID, "error", err)
				}
				activeAttemptID = 0
				return out, capacity
			}
			if index+1 >= len(candidates) || !recoverableNativeFailure(nativeErr) ||
				!canStartFallback(runCtx, budget, out.tokens) {
				// The deferred close records the code the classifier assigns.
				if analysisRequested {
					// A failed re-check must not discard evidence that was still
					// valid; stale dimensions keep their stale marker.
					architectureVerdict = finalizeArchitectureVerdict(
						architectureVerdict, carried, carryEvidence, ruleVersion, sourceDigests)
					markArchitectureInconclusive(&out, &architectureVerdict, nativeErr)
				}
				out, _ = classifyNativeError(runCtx, out, nativeErr)
				return out, nil
			}
			if err := r.Store.FinishReviewAttempt(runCtx, activeAttemptID,
				store.AttemptFailed, "llm_error", truncateRunMessage(nativeErr.Error()), nil, false); err != nil {
				slog.Warn("finish review attempt", "run_id", run.ID, "error", err)
			}
			activeAttemptID = 0
			continue
		}
		sessionID := res.SessionID
		if sessionID == "" {
			sessionID = newestOCRSession(reviewer.Home, sessionBefore)
		}
		if sessionID != "" {
			r.saveSession(runCtx, run.ID, run.LeaseOwner, sessionID, scope.key())
		}

		status, code = nativeOutcome(res)
		if status != store.RunSucceeded && recoverableNativeResult(code) &&
			index+1 < len(candidates) && canStartFallback(runCtx, budget, out.tokens) {
			// The pass failed for a transient provider reason. Its spend is real
			// and gets its own attempt; its partial result is discarded so the
			// backup never inherits findings from a broken destination.
			usage, known := attemptUsage(res)
			if known {
				out.tokens.add(nativeTokenUsage(res))
			} else {
				failedUsageLost = true
			}
			if err := r.Store.FinishReviewAttempt(runCtx, activeAttemptID, store.AttemptFailed,
				code, truncateRunMessage(nativeStatusMessage(res, code)), usage, known); err != nil {
				slog.Warn("finish review attempt", "run_id", run.ID, "error", err)
			}
			activeAttemptID = 0
			continue
		}
		resultReady = true
		break
	}
	if !resultReady {
		if budgetBlocked && nativeErr == nil {
			nativeErr = errors.New("el presupuesto de tokens agregado se agotó antes de la pasada principal")
			if analysisRequested {
				architectureVerdict = finalizeArchitectureVerdict(
					architectureVerdict, carried, carryEvidence, ruleVersion, sourceDigests)
				markArchitectureInconclusive(&out, &architectureVerdict, nativeErr)
			}
			return out.inconclusive(nativeErr, "token_budget"), nil
		}
		if nativeErr == nil {
			nativeErr = errors.New("ningún perfil apto pudo ejecutar la revisión")
		}
		if analysisRequested {
			architectureVerdict = finalizeArchitectureVerdict(
				architectureVerdict, carried, carryEvidence, ruleVersion, sourceDigests)
			markArchitectureInconclusive(&out, &architectureVerdict, nativeErr)
		}
		out, _ = classifyNativeError(runCtx, out, nativeErr)
		return out, nil
	}
	if failedUsageLost {
		resume.markUsage(false)
	}
	if !mainReused {
		usage := nativeTokenUsage(res)
		resume.markSpent(usage)
		resume.markUsage(res.hasSummary)
		if status == store.RunSucceeded {
			r.persistUnit(runCtx, completedMainUnit(run.ID, activeMainKey, res, mainRaw))
		}
	}
	reviewed, skipped, excluded := nativeCoverageCounts(res)
	closeInfo := ""
	if res.closeReason != "" {
		reason := "cierre gradual"
		if res.hardStopped {
			reason = "cierre gradual sin respuesta; parada dura"
		}
		closeInfo = fmt.Sprintf(", %s en %s", reason, res.closeElapsed.Round(time.Millisecond))
	}
	log.Log(runCtx, "worker", "main", "info", logPhaseOCR, fmt.Sprintf(
		"OCR terminó en %s%s: estado %s%s, %d archivo(s) revisados, %d omitidos, %d excluidos, %d hallazgo(s)",
		time.Since(attemptStarted).Round(time.Second), closeInfo, status, terminalCodeSuffix(code),
		reviewed, skipped, excluded, len(res.Comments)))
	files := nativeCoverageFiles(res)
	out.tokens = attemptBase
	out.tokens.add(nativeTokenUsage(res))
	out.tokensKnown = res.hasSummary
	attemptKnown = res.hasSummary
	resume.adopt(out.tokens)
	out.executedProfile = activeProfile
	out.summary = nativeSummaryText(res, status)

	// The analysis is its own phase, run after the review report and its unit
	// were persisted. Its expiry can therefore only downgrade the observation:
	// the review findings, coverage and usage are already part of the outcome.
	// OCR 1.14.1 has no analysis-only mode, so the range is reviewed again and
	// that review output is deliberately ignored.
	var analysisRes nativeReview
	var analysisErr error
	if analysisRequested {
		analysisRes, analysisErr = r.runArchitecturePhase(runCtx, run, activeProfile, activeKey,
			workDir, excludes, background, fromSHA, headSHA, effort, ceiling,
			analysisRunTimeout, analysisBudget, log)
		if analysisRes.hasSummary {
			out.tokens.add(nativeTokenUsage(analysisRes))
		} else {
			log.Log(runCtx, "worker", "analysis", "warn", logPhaseOCR,
				"consumo del análisis desconocido: no se atribuye a la revisión")
		}
		resume.adopt(out.tokens)
	}
	if analysisErr != nil {
		reason := analysisErr.Error()
		var timeout *nativeTimeoutError
		if errors.As(analysisErr, &timeout) {
			reason = timeoutCodeFromKind(timeout.Kind) + ": " + reason
		}
		log.Log(runCtx, "worker", "analysis", "warn", logPhaseOCR, reason)
		architectureVerdict = markArchitectureTimedOut(architectureVerdict, reason)
	} else {
		architectureVerdict = applyNativeArchitectureAnalysis(architectureVerdict, analysisRes)
		var analysisDeadlineHit bool
		architectureVerdict, analysisDeadlineHit = applyAnalysisDeadline(architectureVerdict, analysisRes, analysisBudget)
		if analysisDeadlineHit {
			log.Log(runCtx, "worker", "analysis", "warn", logPhaseOCR, fmt.Sprintf(
				"análisis arquitectónico inconcluso (%s): agotó su plazo de %ds; la revisión conserva su resultado",
				timeoutCodeFromKind("analysis"), analysisBudget.TimeBudgetSeconds))
		}
	}
	if analysisRequested {
		// Bind every fresh section to the current rule version and sources,
		// including partial reports, and keep earlier evidence the re-check did
		// not cover.
		architectureVerdict = finalizeArchitectureVerdict(
			architectureVerdict, carried, carryEvidence, ruleVersion, sourceDigests)
	}
	// applyNativeArchitectureAnalysis rebuilds the dimensions and the risk and
	// uncertainty lists from the report, so the deterministic evidence is
	// integrated again: the trusted baseline, the contract comparison and the
	// operational dimensions. The baseline violation wins over the inconclusive
	// status a failed analysis leaves behind; neither the contract result nor
	// the operational evidence changes the status.
	if baselineEvaluated {
		architectureVerdict = applyArchitectureBaseline(architectureVerdict, baseline)
	}
	architectureVerdict = attachContractComparison(architectureVerdict, contracts)
	if opsEvaluated {
		architectureVerdict = applyOpsEvidence(architectureVerdict, opsEvidence)
	}
	architectureVerdict = applyConsumerImpact(architectureVerdict)
	architectureVerdict = store.MaskRedactedConsumers(architectureVerdict)
	out.architecture = store.ArchitectureVerdictJSON(architectureVerdict)
	if analysisRequested {
		log.Log(runCtx, "worker", "analysis", architectureLogLevel(architectureVerdict.Status), logPhaseOCR,
			fmt.Sprintf("análisis arquitectónico %s (%s): %d dimensión(es), %d token(s)",
				architectureVerdict.Status, architectureVerdict.ExecutionStatus,
				len(architectureVerdict.Dimensions), architectureVerdict.Usage.Total))
	}

	comments := append([]nativeComment(nil), res.Comments...)
	out.merged = mergeNativeFindings(run.PullRequestID, comments, out.anchors)
	scopePaths := reviewedPaths(res.Manifest.Coverage)

	// The inventory is needed twice: to route the text files OCR cannot see and
	// to confront the final per-file coverage with what the range changed. An
	// OCR-internal exclusion, failure or waiver leaves a required file pending;
	// only a policy exclusion takes it out of scope.
	var pending []coverageFile
	var coverageErr error
	if invErr != nil {
		coverageErr = fmt.Errorf("no se pudo inventariar el diff revisado: %w", invErr)
		log.Log(runCtx, "worker", "", "error", logPhaseFinish, coverageErr.Error())
	}

	// OCR excludes oversized files from the main run. Review each of them by
	// complete hunks in independent conversations instead of leaving the rest of
	// the pull request unreviewed.
	var chunked chunkSummary
	if paths := tooLargePaths(res); len(paths) > 0 && canChunkNativeRun(status, code) {
		// The plan decides the order: critical units first and related paths
		// together, so an exhausted budget leaves the least delicate work out.
		paths = plan.orderPaths(paths)
		r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseChunking, log)
		log.Log(runCtx, "worker", "", "info", logPhaseChunks, fmt.Sprintf(
			"%d archivo(s) demasiado grandes para el run principal; se revisan por partes", len(paths)))
		chunked = r.reviewTooLargeFiles(runCtx, run.ID, activeProfile.ID, reviewer, headSHA, paths, budget, log, resume, activeScope)
		out.tokens.add(chunked.tokens)
		if !chunked.usageKnown {
			out.tokensKnown = false
		}
		if len(chunked.comments) > 0 {
			comments = append(comments, chunked.comments...)
			out.merged = mergeNativeFindings(run.PullRequestID, comments, out.anchors)
		}
	}

	// Text files outside OCR's language list are routed through a synthetic,
	// bounded review instead of staying uncovered. A path a chunk already
	// attempted is not retried: the same engine would fail again.
	var textRouted chunkSummary
	if invErr == nil && canChunkNativeRun(status, code) {
		covered := make(map[string]bool)
		for _, file := range files {
			if fileCovered(file) {
				covered[file.Path] = true
			}
		}
		attempted := make(map[string]bool)
		for path, file := range chunked.statuses {
			attempted[path] = true
			if fileCovered(file) {
				covered[path] = true
			}
		}
		candidates := textRouteCandidates(inventory, excludePatterns, covered, attempted, res)
		candidates = plan.orderPaths(candidates)
		if len(candidates) > 0 {
			r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseChunking, log)
			log.Log(runCtx, "worker", "", "info", logPhaseChunks, fmt.Sprintf(
				"%d archivo(s) de texto fuera del reconocimiento de OCR; se revisan por la ruta de texto", len(candidates)))
			textRouted = r.reviewTextFiles(runCtx, run.ID, cfg.Profile.ID, reviewer, fromSHA, headSHA, candidates, budget, out.tokens, out.tokensKnown, log)
			out.tokens.add(textRouted.tokens)
			if !textRouted.usageKnown {
				out.tokensKnown = false
			}
			if len(textRouted.comments) > 0 {
				comments = append(comments, textRouted.comments...)
				out.merged = mergeNativeFindings(run.PullRequestID, comments, out.anchors)
			}
		}
	}

	routed := newChunkSummary()
	for path, file := range chunked.statuses {
		routed.statuses[path] = file
	}
	for path := range chunked.reused {
		routed.reused[path] = true
	}
	for path := range chunked.mixed {
		routed.mixed[path] = true
	}
	for path, file := range textRouted.statuses {
		routed.statuses[path] = file
	}
	if len(routed.statuses) > 0 {
		files = overlayCoverage(files, routed.statuses)
		status, code = chunkedRunStatus(status, code, routed)
		for path := range routed.reviewedPaths() {
			scopePaths[normalizeCoveragePath(path)] = true
		}
	}

	// The architectural verdict is evidence, but it still travels through the
	// same identity and publication path as any other finding (#162). Its
	// advisory flag keeps it out of the check and the inline comments.
	out.merged.Findings = append(out.merged.Findings, ArchitectureFindings(architectureVerdict)...)

	if invErr != nil {
		if status == store.RunSucceeded {
			status, code = store.RunInconclusive, "coverage_incomplete"
		}
	} else {
		files, pending = inventoryCoverage(files, inventory, excludePatterns, coverageSignalFrom(res))
	}
	reviewed, skipped, excluded = coverageCountsFromFiles(files)
	notRequired := coverageNotRequiredFromFiles(files)
	status, code = settledRunStatus(status, code, pending, routed, invErr == nil)
	// Reconcile the plan with the final coverage and the units a previous
	// attempt already paid for. An unverified critical unit makes the run
	// inconclusive, which the existing gate already treats as not approvable.
	reusedPaths := reusedCoveragePaths(res)
	if mainReused {
		// The whole main result came from a persisted unit, so every path it
		// covers was reused rather than reviewed in this attempt.
		for path := range reviewedPaths(res.Manifest.Coverage) {
			reusedPaths[path] = true
		}
	}
	for path := range routed.fullyReusedPaths() {
		reusedPaths[path] = true
	}
	plan.applyCoverage(files, reusedPaths)
	status, code = applyPlanGate(status, code, &plan)
	reviewFindings := countReviewFindings(out.merged.Findings)
	out.summary = coverageSummaryText(status, reviewed, reviewFindings)

	var planJSON *reviewPlan
	if len(plan.Units) > 0 {
		planJSON = &plan
	}
	out.coverage = coverageJSONWithPlan(reviewed, skipped, excluded, notRequired, files, planJSON)
	r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseFinishing, log)
	log.Log(runCtx, "worker", "", "info", logPhaseFinish, fmt.Sprintf(
		"cobertura final: %d revisados, %d omitidos, %d excluidos; %d hallazgo(s) tras deduplicar",
		reviewed, skipped, excluded, reviewFindings))

	switch status {
	case store.RunSucceeded:
		// Identity matching sees every known finding; resolution is limited to
		// the paths this run actually reviewed.
		upserts, resolved := ReconcileScoped(out.open, out.merged, run.ID, true, run.PullRequestID, out.anchors, scopePaths)
		if cfg.PR.External() {
			// Closing the contributor's review threads is a write based on
			// hostile content; an external run only reports findings, it never
			// resolves threads.
			resolved = nil
		}
		for i := range upserts {
			upserts[i].PullRequestID = run.PullRequestID
		}
		out.status = store.RunSucceeded
		out.findings = upserts
		out.resolved = resolved
		// Verification runs against the checkout before it is deleted. It only
		// annotates the findings; applying the decisions happens after they are
		// persisted, so a crash here loses verdicts but never corrupts state.
		// The verifier reads the same comparison the review saw, so a base-side
		// citation resolves against fromSHA (the merge base, or the previous
		// head on an incremental push), not against the persisted base.
		r.verifyFindings(runCtx, run, cfg, activeProfile, activeKey, workDir, headSHA, fromSHA, &out, reserved, log)
		// Criteria evaluation is independent of the findings pass: it checks the
		// linked issue's requirements against the same checkout, stores an answer
		// per criterion and never changes the check. The publication reads only
		// the rows stored for this head (#232, #227).
		r.evaluateRequirements(runCtx, run, cfg, activeProfile, activeKey, workDir, headSHA, fromSHA, changedFiles, &out, log)
		if cfg.Policy.AutoTriageMode == store.AutoTriageApply && out.verificationSkipped == "verifier_error" {
			// The policy promised that only confirmed findings drive the gate and
			// the publication. A failed verifier leaves them unconfirmed, so the
			// run cannot claim a conclusive verdict: it becomes inconclusive,
			// nothing is published, and the check asks for a person (#227).
			out = out.inconclusive(fmt.Errorf(
				"fase verificación: el verificador OCR falló: %s", out.verificationError), "verifier_error")
		}
		r.setPhase(runCtx, run.ID, run.LeaseOwner, store.RunPhaseFinishing, log)
		return out, nil
	case store.RunInconclusive:
		message := chunkedInconclusiveMessage(res, code, reviewed, skipped)
		if coverageErr != nil {
			message = coverageErr.Error()
		}
		message += plan.incompleteSuffix()
		out = out.inconclusive(errors.New(message), code)
		return out, nil
	default:
		out = out.failed(errors.New(nativeStatusMessage(res, code)), code)
		return out, nil
	}
}

// heartbeat keeps the run marked alive while the worker executes it. When the
// store reports the run is no longer running, the watchdog or a new head closed
// it, or that another execution recovered the expired lease, so abort cancels
// the pipeline instead of letting it finish blind.
func (r *Runner) heartbeat(ctx context.Context, runID uint, owner string, log *runLogSink, abort context.CancelFunc) func() {
	interval := r.heartbeatEvery
	if interval <= 0 {
		interval = heartbeatInterval
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), heartbeatWriteTimeout)
				err := r.Store.HeartbeatReviewRun(writeCtx, runID, owner)
				cancel()
				switch {
				case err == nil:
				case errors.Is(err, store.ErrRunLeaseLost):
					log.Log(context.WithoutCancel(ctx), "worker", "", "warn", logPhaseFinish,
						"otro worker recuperó el run tras expirar la concesión; se cancela la revisión en curso")
					abort()
					return
				case errors.Is(err, store.ErrRunNotActive):
					log.Log(context.WithoutCancel(ctx), "worker", "", "warn", logPhaseFinish,
						"el run ya no está activo; se cancela la revisión en curso")
					abort()
					return
				default:
					slog.Warn("review run heartbeat failed", "run_id", runID, "error", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// nativeCaller is one OCR invocation: a review or a focused verification.
type nativeCaller interface {
	Review(ctx context.Context) (nativeReview, []byte, error)
}

// runNativeReview holds one profile slot for exactly one OCR invocation.
func (r *Runner) runNativeReview(ctx context.Context, profileID uint, reviewer nativeCaller, log *runLogSink, invocation string) (nativeReview, []byte, error) {
	log.Log(ctx, "worker", invocation, "info", logPhaseOCR,
		fmt.Sprintf("esperando un slot del perfil %d", profileID))
	release, acquired, err := r.Store.AcquireProfileSlot(ctx, profileID)
	if err != nil {
		log.Log(ctx, "worker", invocation, "error", logPhaseOCR,
			fmt.Sprintf("no se pudo bloquear el perfil %d: %v", profileID, err))
		return nativeReview{}, nil, fmt.Errorf("bloquear perfil %d: %w", profileID, err)
	}
	if !acquired {
		log.Log(ctx, "worker", invocation, "warn", logPhaseOCR,
			fmt.Sprintf("el perfil %d no tiene slots libres", profileID))
		return nativeReview{}, nil, &ProfileCapacityError{ProfileID: profileID}
	}
	defer release()
	log.Log(ctx, "worker", invocation, "info", logPhaseOCR,
		fmt.Sprintf("slot del perfil %d adquirido; ejecutando OCR", profileID))
	return reviewer.Review(ctx)
}

// runArchitecturePhase runs the observational analysis in its own OCR
// invocation, after the review report is persisted. The analysis share bounds
// this invocation, so its expiry has its own close kind and cannot reach the
// review outcome or start a backup review. The aggregate token cap is the share
// already carved out of the run budget.
func (r *Runner) runArchitecturePhase(ctx context.Context, run store.ReviewRun, profile store.LLMProfile,
	key []byte, workDir, excludes, background, baseRef, headRef, effort string,
	maxTokens int, runTimeout time.Duration, budget store.ArchitectureBudget,
	log *runLogSink) (nativeReview, error) {
	home := filepath.Join(runStateDir(r.Cfg.WorkspaceDir, run.ID), "home-analysis")
	reviewer := newNativeReviewer(r.Cfg.OCRBin, workDir, home, profile, key,
		excludes, background, baseRef, headRef, effort, maxTokens,
		budget.TokenBudget, runTimeout, log.OCRLogger(ctx, "analysis"))
	reviewer.Analysis = true
	reviewer.AnalysisTokenBudget = budget.TokenBudget
	reviewer.AnalysisTimeout = time.Duration(budget.TimeBudgetSeconds) * time.Second
	log.Log(ctx, "worker", "analysis", "info", logPhaseOCR, fmt.Sprintf(
		"fase de análisis arquitectónico: presupuesto %d tokens, %ds",
		budget.TokenBudget, budget.TimeBudgetSeconds))
	res, _, err := r.runNativeReview(ctx, profile.ID, reviewer, log, "analysis")
	return res, err
}

func (r *Runner) boxOpen(sealed string) ([]byte, error) {
	if r.Box == nil {
		return nil, errors.New("crypto box no configurado")
	}
	return r.Box.Open(sealed)
}

// pullHeadRef is the pull request ref in the base repository. External
// contributions are always fetched from there, never from the fork.
func pullHeadRef(number int) string {
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// reviewRemote is the clone URL of the base repository.
func reviewRemote(gitBaseURL, repoFullName string) string {
	return strings.TrimSuffix(gitBaseURL, "/") + "/" + repoFullName + ".git"
}

// checkoutHeadMoved reports whether the checked-out ref no longer matches the
// head the run was created for. A moving pull ref must never be reviewed under
// the old SHA. An empty expected SHA is a legacy run without the comparison.
func checkoutHeadMoved(expected, actual string) bool {
	return expected != "" && !strings.EqualFold(expected, actual)
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
	return coverageJSONWithFiles(reviewed, skipped, excluded, 0, nil)
}

// policyExcludePatterns returns the patterns OCR can actually receive and the
// ones dropped for carrying a comma. The inventory uses the same kept list to
// decide which changed files the team excluded by policy.
func policyExcludePatterns(raw []byte) (kept, dropped []string, err error) {
	var patterns []string
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &patterns); err != nil {
			return nil, nil, fmt.Errorf("exclude_globs no es una lista: %w", err)
		}
	}
	kept = make([]string, 0, len(patterns))
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
	return kept, dropped, nil
}

// incrementalSource returns the previous head a push can be reviewed from. It
// requires the previous run to describe the same comparison: the same merge
// base and the same base branch. A rewritten base that happens to produce the
// same merge base is still a new context and reviews the full diff.
// comparisonMovedAfterReview re-reads the pull request immediately before the
// run is persisted. A head move is already caught before the review starts;
// this second check covers the base, which can move while the model works, and
// keeps a stale comparison from persisting findings, triage or QA status.
func (r *Runner) comparisonMovedAfterReview(ctx context.Context, run store.ReviewRun, reviewedBase string) (bool, error) {
	pr, err := r.Store.PullRequest(ctx, run.PullRequestID)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(pr.HeadSHA, run.HeadSHA) {
		// The run must not complete under a head it never reviewed, even when
		// the failure happened before the clone resolved a base.
		return true, nil
	}
	if run.BaseRef != "" && pr.BaseRef != run.BaseRef {
		return true, nil
	}
	// A base push is also visible in the base tip even when the sync already
	// cleared the merge base, so the two checks complement each other.
	if run.BaseTipSHA != "" && pr.BaseSHA != "" && !strings.EqualFold(run.BaseTipSHA, pr.BaseSHA) {
		return true, nil
	}
	base := run.BaseSHA
	if base == "" {
		base = reviewedBase
	}
	// An empty merge base means the comparison is still unresolved, not that it
	// moved: only two known merge bases can disagree.
	if base != "" && pr.MergeBaseSHA != "" && !strings.EqualFold(pr.MergeBaseSHA, base) {
		return true, nil
	}
	return false, nil
}

func incrementalSource(run, prev store.ReviewRun, reviewedBase, headSHA string) (string, bool) {
	if prev.BaseSHA == "" || !strings.EqualFold(prev.BaseSHA, reviewedBase) ||
		prev.HeadSHA == "" || strings.EqualFold(prev.HeadSHA, headSHA) {
		return "", false
	}
	if run.BaseRef != "" && prev.BaseRef != "" && prev.BaseRef != run.BaseRef {
		return "", false
	}
	return prev.HeadSHA, true
}

func hasBlockingFinding(findings []store.Finding, threshold, headSHA, mode string) bool {
	limit := severityRank(threshold)
	for _, f := range findings {
		if f.Status != store.FindingOpen || severityRank(f.Severity) < limit {
			continue
		}
		if findingBlocks(f, headSHA, mode) {
			return true
		}
	}
	return false
}

// findingBlocks mirrors the publisher's gate. In apply mode an uncertainty or a
// verdict from another head never blocks, and a confirmation needs its cited
// evidence. The observation modes (off and dry_run) never let a stored verdict
// suppress the block: they keep exactly the gate the pull request would have
// without verification.
func findingBlocks(f store.Finding, headSHA, mode string) bool {
	if f.TriageSource == store.TriageSourceHuman {
		return true
	}
	if mode != store.AutoTriageApply {
		return true
	}
	if len(f.Verification) == 0 {
		return true
	}
	var evidence struct {
		Verdict  string `json:"verdict"`
		Evidence string `json:"evidence"`
		HeadSHA  string `json:"head_sha"`
	}
	if err := json.Unmarshal(f.Verification, &evidence); err != nil || evidence.Verdict == "" {
		return true
	}
	if headSHA == "" || evidence.HeadSHA == "" || !strings.EqualFold(evidence.HeadSHA, headSHA) {
		return false
	}
	switch evidence.Verdict {
	case "confirm":
		return strings.TrimSpace(evidence.Evidence) != ""
	case "refute":
		return true
	default:
		return false
	}
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
		upserts, _ := Reconcile(out.open, out.merged, out.runID, false, out.prID, out.anchors)
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
		return out.inconclusive(phaseError("clonado", err), "timeout"), nil
	}
	message := strings.ToLower(err.Error())
	for _, signal := range cloneCredentialSignals {
		if signal.MatchString(message) {
			return out.inconclusive(phaseError("clonado", err), "token_expired"), nil
		}
	}
	for _, phrase := range cloneCredentialPhrases {
		if strings.Contains(message, phrase) {
			return out.inconclusive(phaseError("clonado", err), "token_expired"), nil
		}
	}
	if isTransientFetchError(err) {
		return out.inconclusive(phaseError("clonado", err), "git_transient"), nil
	}
	return out.failed(phaseError("clonado", err), "git_error"), nil
}

// phaseError names the pipeline phase in the message the panel shows. The run
// row no longer carries a phase once it is terminal, so this is what keeps the
// step queryable next to the technical detail.
func phaseError(phase string, err error) error {
	return fmt.Errorf("fase %s: %w", phase, err)
}

// invalidParameterPatterns recognise the argument errors OCR returns before it
// runs. The group names the flag so the panel can point at the exact setting
// instead of reporting a generic ocr_error.
var invalidParameterPatterns = []*regexp.Regexp{
	regexp.MustCompile(`invalid argument .* for "--([a-zA-Z0-9-]+)" flag`),
	regexp.MustCompile(`unknown flag: --?([a-zA-Z0-9-]+)`),
	regexp.MustCompile(`flag provided but not defined: --?([a-zA-Z0-9-]+)`),
	regexp.MustCompile(`flag needs an argument: --?([a-zA-Z0-9-]+)`),
}

// invalidOCRParameter returns the flag OCR rejected, or an empty string when the
// failure is not an argument error. OCR validates --effort in the template
// layer with its own wording, so that value maps to the flag explicitly.
func invalidOCRParameter(message string) string {
	for _, pattern := range invalidParameterPatterns {
		if match := pattern.FindStringSubmatch(message); len(match) > 1 {
			return "--" + match[1]
		}
	}
	if strings.Contains(strings.ToLower(message), `invalid effort "`) {
		return "--effort"
	}
	return ""
}

// classifyNativeError handles failures that produced no parseable OCR result.
// A configuration failure is terminal; a deadline is classified by its kind so
// the panel can tell the run-wide deadline from OCR's own request or per-group
// timeout.
func classifyNativeError(ctx context.Context, out runOutcome, err error) (runOutcome, error) {
	var timeout *nativeTimeoutError
	if errors.As(err, &timeout) {
		return out.inconclusive(phaseError("revisión", err), timeoutCodeFromKind(timeout.Kind)), nil
	}
	if truncatedContext(ctx, err) {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out.inconclusive(phaseError("revisión", err), timeoutCodeFromKind("global")), nil
		}
		return out.inconclusive(phaseError("revisión", err), "timeout"), nil
	}
	message := strings.ToLower(err.Error())
	if param := invalidOCRParameter(err.Error()); param != "" {
		return out.failed(fmt.Errorf("fase revisión: parámetro %s inválido: %w", param, err), "invalid_parameter"), nil
	}
	for _, signal := range []string{
		"no valid llm endpoint", "configuration", "config file", "invalid config",
	} {
		if strings.Contains(message, signal) {
			return out.failed(phaseError("revisión", err), "config"), nil
		}
	}
	return out.inconclusive(phaseError("revisión", err), "ocr_error"), nil
}

// markArchitectureInconclusive records that a requested observation could not
// finish. It only touches the analysis field: the review outcome is never
// derived from it. A trusted-baseline violation is deterministic evidence and
// must not be downgraded by an unrelated OCR or budget failure; a stale verdict
// keeps its stale marker so the panel still shows the evidence as pending.
func markArchitectureInconclusive(out *runOutcome, verdict *store.ArchitectureVerdict, err error) {
	if err == nil {
		err = errors.New("analysis did not run")
	}
	if verdict.Status != store.ArchitectureViolation && verdict.Status != store.ArchitectureStale {
		verdict.Status = store.ArchitectureInconclusive
	}
	verdict.Errors = append(verdict.Errors, err.Error())
	final := store.MaskRedactedConsumers(applyConsumerImpact(*verdict))
	out.architecture = store.ArchitectureVerdictJSON(final)
}

func truncateRunMessage(message string) string {
	if len(message) <= maxRunErrorMessage {
		return message
	}
	return message[:maxRunErrorMessage]
}

func terminalLogLevel(status string) string {
	switch status {
	case store.RunSucceeded:
		return "info"
	case store.RunInconclusive:
		return "warn"
	default:
		return "error"
	}
}

func terminalCodeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " (" + code + ")"
}
