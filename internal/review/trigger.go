package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// CheckStarter owns the GitHub check run of a review run. The publish worker
// implements it; the trigger only needs the start/cancel boundary.
type CheckStarter interface {
	StartCheck(ctx context.Context, runID uint) error
	CancelCheck(ctx context.Context, runID uint) error
}

// TriggerEnqueuer is the enqueuer a debounced trigger needs: the one the runner
// already uses plus the scheduled trigger insertion.
type TriggerEnqueuer interface {
	Enqueuer
	EnqueueReviewTrigger(ctx context.Context, prID uint, headSHA string, delay time.Duration) error
}

// MergeBaseResolver resolves the merge base of a comparison through GitHub's
// Compare API. A failure is not fatal: the trigger enqueues a conservative new
// run instead of reusing a result it could not verify.
type MergeBaseResolver interface {
	MergeBase(ctx context.Context, installationID int64, repoFullName, base, head string) (string, error)
}

// Republisher recomputes and republishes the verdict of a pull request when
// its CI state changed. It is the publisher worker, which owns the check run.
type Republisher interface {
	Republish(ctx context.Context, prID uint) error
}

// Trigger turns webhook deliveries and the periodic reconciliation into queued
// review runs. It notifies browsers only when it creates a run; the runner
// reports the claim and the terminal state.
type Trigger struct {
	Store   *store.Store
	Apps    github.AppProvider
	Jobs    TriggerEnqueuer
	Checks  CheckStarter
	Compare MergeBaseResolver
	// Republish corrects an existing verdict when the required checks change,
	// so red -> green -> red converges without repeating the analysis.
	Republish Republisher
	Box       *crypto.Box // QA_WORKER_KEY
	Events    events.Publisher
	// Debounce delays the webhook review until the head has been quiet for this
	// long. Zero keeps the immediate path.
	Debounce time.Duration
}

// OnPullRequest schedules a review for the current comparison of a pull
// request. With no debounce it queues the run immediately. A run for the same
// comparison (active or terminal) makes redeliveries harmless.
func (t *Trigger) OnPullRequest(ctx context.Context, prID uint) error {
	if t.Debounce <= 0 {
		_, err := t.enqueue(ctx, prID, "webhook", false)
		return err
	}
	cfg, err := t.reviewConfig(ctx, prID)
	if err != nil || cfg == nil {
		return err
	}
	if !cfg.PR.IsOpen() || cfg.PR.Draft || cfg.PR.IsFork {
		return nil
	}
	if t.Jobs == nil {
		return errors.New("review: trigger has no enqueuer")
	}
	return t.Jobs.EnqueueReviewTrigger(ctx, prID, cfg.PR.HeadSHA, t.Debounce)
}

// OnPullRequestIneligible reconciles a pull request that stopped being
// reviewable because it was closed, merged or converted to a draft. Active runs
// and their checks are discarded so no worker starts reviewing a pull request
// that is no longer eligible. Repeating the delivery is a no-op.
func (t *Trigger) OnPullRequestIneligible(ctx context.Context, prID uint) error {
	if t.Store == nil {
		return errors.New("review: trigger has no store")
	}
	unlock, err := t.Store.LockPR(ctx, prID)
	if err != nil {
		return fmt.Errorf("lock pull request %d: %w", prID, err)
	}
	defer unlock()
	_, err = t.reconcileIneligibleLocked(ctx, prID)
	return err
}

// FireDebounced runs a scheduled trigger. It no-ops when a newer push has moved
// the head (that push has its own job), when the PR was closed or merged, or
// when it stopped being eligible while the job waited. The check below is only
// a fast path; enqueueForHead repeats it under the PR lock.
func (t *Trigger) FireDebounced(ctx context.Context, prID uint, headSHA string) error {
	cfg, err := t.reviewConfig(ctx, prID)
	if err != nil || cfg == nil {
		return err
	}
	if cfg.PR.HeadSHA != headSHA || !cfg.PR.IsOpen() || cfg.PR.Draft || cfg.PR.IsFork {
		return nil
	}
	hint := t.comparisonHint(ctx, prID)
	_, err = t.enqueueForHead(ctx, prID, headSHA, "webhook", false, hint)
	return err
}

// reviewConfig loads the configuration that applies to a pull request. A nil
// config without an error means the PR is not reviewable (draft, fork,
// disabled repository or missing policy); that is an expected outcome, not a
// failure that should retry the job.
func (t *Trigger) reviewConfig(ctx context.Context, prID uint) (*store.ReviewConfig, error) {
	if t.Store == nil {
		return nil, errors.New("review: trigger has no store")
	}
	cfg, err := t.Store.ReviewConfigForPR(ctx, prID)
	if errors.Is(err, store.ErrNoReviewConfig) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load review configuration of pull request %d: %w", prID, err)
	}
	return &cfg, nil
}

// OnChecksUpdated re-evaluates the CI gate of a pull request whose check state
// changed on GitHub. Only teams that gate reviews on CI are acted on: for
// everyone else a check delivery must not become a second review trigger, so
// the gate stays exactly where it was.
//
// With the gate active, a green required-check set may schedule a review; an
// existing terminal review of the current comparison is never repeated. In
// every case the publisher recomputes the stored verdict, so a head that turned
// red after publishing stops reporting success.
func (t *Trigger) OnChecksUpdated(ctx context.Context, prID uint) error {
	cfg, err := t.Store.ReviewConfigForPR(ctx, prID)
	if errors.Is(err, store.ErrNoReviewConfig) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load review configuration of pull request %d: %w", prID, err)
	}
	if !cfg.Policy.RequireGreenCI {
		return nil
	}
	verdict, err := t.Store.CIVerdictFor(ctx, prID, cfg.PR.HeadSHA, cfg.Policy.RequiredChecks)
	if err != nil {
		return fmt.Errorf("evaluate CI gate of pull request %d: %w", prID, err)
	}
	if verdict.Green() {
		if _, err := t.enqueue(ctx, prID, "webhook", false); err != nil {
			return err
		}
	}
	if t.Republish != nil {
		if err := t.Republish.Republish(ctx, prID); err != nil {
			return fmt.Errorf("republish verdict of pull request %d: %w", prID, err)
		}
	}
	return nil
}

// Retry queues a fresh run even when the current comparison already has a
// terminal run. Active runs of other comparisons are still superseded.
func (t *Trigger) Retry(ctx context.Context, prID uint) error {
	_, err := t.enqueue(ctx, prID, "retry", true)
	return err
}

// EnqueuePending queues reviews for every eligible pull request and returns
// how many runs were actually created. Per-PR errors are joined so one bad PR
// does not prevent the rest of the batch from being scheduled.
func (t *Trigger) EnqueuePending(ctx context.Context, limit int) (int, error) {
	if t.Store == nil {
		return 0, errors.New("review: trigger has no store")
	}
	prs, err := t.Store.PendingReviewPRs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list pending review pull requests: %w", err)
	}

	created := 0
	var errs []error
	for _, pr := range prs {
		ok, err := t.enqueue(ctx, pr.ID, "webhook", false)
		if err != nil {
			errs = append(errs, fmt.Errorf("pull request %d: %w", pr.ID, err))
			continue
		}
		if ok {
			created++
		}
	}
	return created, errors.Join(errs...)
}

// ReconcileQueuedRuns repairs the review queue. It supersedes active runs of
// pull requests that stopped being reviewable and gives queued runs back the
// job or check they lost. Every action is serialized per pull request by the
// same advisory lock the webhook uses, and only the missing piece is re-created,
// so a healthy queued run is left alone and a live job is never duplicated. A
// failure on one pull request does not stop the rest of the batch.
func (t *Trigger) ReconcileQueuedRuns(ctx context.Context, limit int) (int, error) {
	if t.Store == nil {
		return 0, errors.New("review: trigger has no store")
	}

	active, err := t.Store.ActiveRunsForReconcile(ctx, limit)
	if err != nil {
		return 0, err
	}
	done := 0
	var errs []error
	for _, prID := range distinctPRIDs(active) {
		unlock, err := t.Store.LockPR(ctx, prID)
		if err != nil {
			errs = append(errs, fmt.Errorf("lock pull request %d: %w", prID, err))
			continue
		}
		discarded, err := t.reconcileIneligibleLocked(ctx, prID)
		unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("discard runs of pull request %d: %w", prID, err))
			continue
		}
		if discarded {
			done++
		}
	}

	stranded, err := t.Store.StrandedQueuedRuns(ctx, limit)
	if err != nil {
		return done, errors.Join(append(errs, err)...)
	}
	for _, work := range stranded {
		unlock, err := t.Store.LockPR(ctx, work.PullRequestID)
		if err != nil {
			errs = append(errs, fmt.Errorf("lock pull request %d: %w", work.PullRequestID, err))
			continue
		}
		repaired, err := t.repairQueuedRunLocked(ctx, work)
		unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("repair review run %d: %w", work.RunID, err))
			continue
		}
		if repaired {
			done++
		}
	}
	return done, errors.Join(errs...)
}

// enqueue implements the immediate entry points. The boolean result reports
// whether a run was created, which lets EnqueuePending count only real work.
func (t *Trigger) enqueue(ctx context.Context, prID uint, triggerKind string, ignoreSameSHA bool) (bool, error) {
	var hint comparisonHint
	if !ignoreSameSHA {
		// Retry always creates a fresh run, so resolving Compare for it would
		// only add latency.
		hint = t.comparisonHint(ctx, prID)
	}
	return t.enqueueForHead(ctx, prID, "", triggerKind, ignoreSameSHA, hint)
}

// comparisonHint carries a Compare result resolved outside the PR lock. The
// network call is deliberately not made while the advisory lock is held; under
// the lock the hint is only trusted when the snapshot it was made for is still
// the current comparison.
type comparisonHint struct {
	headSHA   string
	baseRef   string
	baseSHA   string
	needed    bool
	mergeBase string
	failed    bool
}

// matches reports whether the hint still describes cfg's comparison.
func (h comparisonHint) matches(cfg store.ReviewConfig) bool {
	return h.needed && h.headSHA == cfg.PR.HeadSHA && h.baseRef == cfg.PR.BaseRef &&
		h.baseSHA == cfg.PR.BaseSHA && cfg.PR.MergeBaseSHA == ""
}

// comparisonHint decides whether a coverage check needs the Compare API: the
// head did not change, the base moved (the merge base was cleared) and a run
// of the same head and branch already resolved a merge base. Resolving it here,
// outside the lock, keeps a slow GitHub call from blocking every other delivery
// of the pull request. The hint also snapshots the base tip, so a force-push
// between the Compare call and the lock invalidates the result.
func (t *Trigger) comparisonHint(ctx context.Context, prID uint) comparisonHint {
	cfg, err := t.reviewConfig(ctx, prID)
	if err != nil || cfg == nil {
		return comparisonHint{}
	}
	runs, err := t.Store.ListReviewRuns(ctx, prID)
	if err != nil {
		return comparisonHint{}
	}

	hint := comparisonHint{headSHA: cfg.PR.HeadSHA, baseRef: cfg.PR.BaseRef, baseSHA: cfg.PR.BaseSHA}
	candidate := false
	for i := range runs {
		run := &runs[i]
		if !blocksSameSHA(run.Status) || !strings.EqualFold(run.HeadSHA, cfg.PR.HeadSHA) {
			continue
		}
		if run.BaseRef != "" && run.BaseRef != cfg.PR.BaseRef {
			continue
		}
		if cfg.PR.CoveredBy(*run) {
			return comparisonHint{}
		}
		if cfg.PR.MergeBaseSHA == "" && run.BaseSHA != "" {
			candidate = true
		}
	}
	if !candidate {
		return comparisonHint{}
	}

	hint.needed = true
	mergeBase, err := t.resolveMergeBase(ctx, cfg)
	if err != nil {
		hint.failed = true
		return hint
	}
	hint.mergeBase = mergeBase
	return hint
}

func (t *Trigger) resolveMergeBase(ctx context.Context, cfg *store.ReviewConfig) (string, error) {
	if t.Compare == nil {
		return "", errors.New("review: trigger has no comparator")
	}
	return t.Compare.MergeBase(ctx, cfg.Installation.ID, cfg.Repo.FullName, cfg.PR.BaseRef, cfg.PR.HeadSHA)
}

// enqueueForHead is enqueue with an optional head check evaluated under the PR
// lock. A scheduled trigger passes the SHA captured at scheduling time, so a
// push that lands between the fast-path check and this call cannot start a
// review of the newer head ahead of its own debounce.
func (t *Trigger) enqueueForHead(ctx context.Context, prID uint, expectedHead, triggerKind string, ignoreSameSHA bool, hint comparisonHint) (bool, error) {
	if t.Store == nil {
		return false, errors.New("review: trigger has no store")
	}
	unlock, err := t.Store.LockPR(ctx, prID)
	if err != nil {
		return false, fmt.Errorf("lock pull request %d: %w", prID, err)
	}
	defer unlock()

	cfg, err := t.Store.ReviewConfigForPR(ctx, prID)
	if errors.Is(err, store.ErrNoReviewConfig) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load review configuration of pull request %d: %w", prID, err)
	}
	// A merge or close during a debounce wait must not review a dead PR. The
	// check lives here so every entry point agrees on the same boundary.
	if expectedHead != "" && cfg.PR.HeadSHA != expectedHead {
		return false, nil
	}
	if !cfg.PR.IsOpen() {
		return false, nil
	}

	runs, err := t.Store.ListReviewRuns(ctx, prID)
	if err != nil {
		return false, fmt.Errorf("list review runs of pull request %d: %w", prID, err)
	}
	mergeBase := cfg.PR.MergeBaseSHA
	if !ignoreSameSHA {
		var stranded *store.ReviewRun
		covered := false
		for i := range runs {
			run := &runs[i]
			if !blocksSameSHA(run.Status) || !cfg.PR.CoveredBy(*run) {
				continue
			}
			covered = true
			if run.Status == store.RunQueued && (stranded == nil || run.ID > stranded.ID) {
				stranded = run
			}
		}
		if covered {
			// A queued run whose StartCheck or EnqueueReview failed on an
			// earlier delivery is stranded: heal it instead of skipping the
			// comparison forever. Terminal runs are simply left alone.
			if stranded != nil {
				if err := t.repair(ctx, *stranded); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		// The head is unchanged and the base moved, so only Compare can tell a
		// rewritten base (new run) from a base that advanced without changing
		// the merge base (reuse). A failed resolution is conservative.
		switch {
		case hint.matches(cfg) && !hint.failed:
			mergeBase = hint.mergeBase
			if adopted := runForMergeBase(runs, cfg.PR, mergeBase); adopted != nil {
				stored, err := t.Store.SetPRMergeBase(ctx, prID, cfg.PR.HeadSHA, cfg.PR.BaseRef, cfg.PR.BaseSHA, mergeBase)
				if err != nil {
					return false, err
				}
				if !stored {
					// The pull request moved while the comparison was being
					// adopted. The delivery that moved it owns the new run; this
					// one must not supersede it with a stale target.
					return false, nil
				}
				switch adopted.Status {
				case store.RunQueued:
					if t.Checks == nil || t.Jobs == nil {
						return false, errors.New("review: trigger cannot repair the adopted run")
					}
					if err := t.repair(ctx, *adopted); err != nil {
						return false, err
					}
				case store.RunSucceeded, store.RunInconclusive, store.RunFailed:
					// The comparison was unresolved when the run finished, so its
					// publication was skipped. Now that Compare proved it is
					// current, publish it; the publish job is idempotent.
					if t.Jobs == nil {
						return false, errors.New("review: trigger has no enqueuer")
					}
					if err := t.Jobs.EnqueuePublish(ctx, adopted.ID); err != nil {
						return false, fmt.Errorf("enqueue publish for review run %d: %w", adopted.ID, err)
					}
				}
				return false, nil
			}
		case hint.matches(cfg) && hint.failed:
			// Compare failed: never reuse a result we could not verify.
			mergeBase = ""
		}
	}

	target := store.Comparison{
		HeadSHA: cfg.PR.HeadSHA, BaseRef: cfg.PR.BaseRef, MergeBaseSHA: mergeBase,
	}

	// The CI gate sits next to the draft/fork exclusions: it is re-evaluated on
	// every delivery, so the head is released by the very event that makes CI
	// green instead of waiting for a human retry.
	if cfg.Policy.RequireGreenCI {
		verdict, err := t.Store.CIVerdictFor(ctx, prID, cfg.PR.HeadSHA, cfg.Policy.RequiredChecks)
		if err != nil {
			return false, fmt.Errorf("evaluate CI gate of pull request %d: %w", prID, err)
		}
		if !verdict.Green() {
			return false, nil
		}
	}

	if t.Apps == nil {
		return false, errors.New("review: trigger has no app provider")
	}
	if t.Box == nil {
		return false, errors.New("review: trigger has no worker key box")
	}
	if t.Checks == nil {
		return false, errors.New("review: trigger has no check starter")
	}
	if t.Jobs == nil {
		return false, errors.New("review: trigger has no enqueuer")
	}

	// Cancel in-flight checks of runs of another comparison before superseding
	// them. The comparison being queued is never superseded by its own trigger,
	// so its check stays.
	for _, run := range runs {
		if run.Status != store.RunQueued && run.Status != store.RunRunning {
			continue
		}
		if run.Comparison().Covers(target) {
			continue
		}
		if err := t.Checks.CancelCheck(ctx, run.ID); err != nil {
			return false, fmt.Errorf("cancel check of review run %d: %w", run.ID, err)
		}
	}
	if err := t.Store.SupersedeOtherActiveRuns(ctx, prID, target); err != nil {
		return false, fmt.Errorf("supersede review runs of pull request %d: %w", prID, err)
	}

	app, err := t.Apps.App(ctx)
	if err != nil {
		return false, fmt.Errorf("load GitHub App: %w", err)
	}
	token, expiresAt, err := app.RepositoryReadToken(ctx, cfg.Installation.ID, cfg.Repo.FullName)
	if err != nil {
		return false, fmt.Errorf("mint read token for repository %s: %w", cfg.Repo.FullName, err)
	}
	sealed, err := t.Box.Seal([]byte(token))
	if err != nil {
		return false, fmt.Errorf("seal clone token: %w", err)
	}

	run, err := t.Store.CreateReviewRun(ctx, store.ReviewRun{
		PullRequestID:       prID,
		ProfileID:           cfg.Profile.ID,
		HeadSHA:             cfg.PR.HeadSHA,
		BaseRef:             cfg.PR.BaseRef,
		BaseTipSHA:          cfg.PR.BaseSHA,
		BaseSHA:             mergeBase,
		Trigger:             triggerKind,
		Status:              store.RunQueued,
		CloneTokenEnc:       sealed,
		CloneTokenExpiresAt: &expiresAt,
	})
	if err != nil {
		// The partial unique index allows only one active run per PR. If a
		// concurrent (or retried) delivery won the race, heal its run instead
		// of reporting a failure that would be retried indefinitely.
		if isActiveRunUniqueViolation(err) {
			existing, readErr := t.Store.RunForSHA(ctx, prID, cfg.PR.HeadSHA)
			if readErr != nil {
				return false, fmt.Errorf("re-read review run for pull request %d at %q: %w", prID, cfg.PR.HeadSHA, readErr)
			}
			// The winner may belong to another comparison of the same head; it
			// already supersedes this delivery, so healing it would start the
			// wrong review.
			if !cfg.PR.CoveredBy(existing) {
				return false, nil
			}
			if repairErr := t.repair(ctx, existing); repairErr != nil {
				return false, repairErr
			}
			return false, nil
		}
		return false, fmt.Errorf("create review run for pull request %d: %w", prID, err)
	}
	// Publish the comparison the run belongs to. The write is conditional, so a
	// retarget landing here wins and the new run is superseded by the next
	// delivery instead of publishing under the wrong key.
	if mergeBase != "" && !strings.EqualFold(mergeBase, cfg.PR.MergeBaseSHA) {
		if _, err := t.Store.SetPRMergeBase(ctx, prID, cfg.PR.HeadSHA, cfg.PR.BaseRef, cfg.PR.BaseSHA, mergeBase); err != nil {
			return false, err
		}
	}
	// Notify before repair: the queued run is stored even if starting it fails,
	// and the redelivery that heals it stays silent.
	if t.Events != nil {
		ev := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(prID), 10)}
		if err := t.Events.Publish(ctx, ev); err != nil {
			slog.Warn("publish event", "type", ev.Type, "id", ev.ID, "error", err)
		}
	}
	if err := t.repair(ctx, run); err != nil {
		return false, err
	}
	return true, nil
}

// runForMergeBase returns the latest run of the pull request's head and base
// branch that resolved the given merge base. When it exists, Compare proved the
// comparison is unchanged and that run must be reused instead of duplicated.
func runForMergeBase(runs []store.ReviewRun, pr store.PullRequest, mergeBase string) *store.ReviewRun {
	if mergeBase == "" {
		return nil
	}
	var found *store.ReviewRun
	for i := range runs {
		run := &runs[i]
		if !blocksSameSHA(run.Status) || !strings.EqualFold(run.HeadSHA, pr.HeadSHA) {
			continue
		}
		if run.BaseRef != "" && run.BaseRef != pr.BaseRef {
			continue
		}
		if strings.EqualFold(run.BaseSHA, mergeBase) && (found == nil || run.ID > found.ID) {
			found = run
		}
	}
	return found
}

// repair restarts the check and (re)enqueues an existing run. Both operations
// are idempotent on the consumer side: StartCheck reopens the check run and a
// duplicate River job finds an already claimed or terminal run.
func (t *Trigger) repair(ctx context.Context, run store.ReviewRun) error {
	if err := t.Checks.StartCheck(ctx, run.ID); err != nil {
		return fmt.Errorf("start check of review run %d: %w", run.ID, err)
	}
	if err := t.Jobs.EnqueueReview(ctx, run.ID); err != nil {
		return fmt.Errorf("enqueue review run %d: %w", run.ID, err)
	}
	return nil
}

// reconcileIneligibleLocked discards the active runs of a pull request that is
// no longer reviewable and reports whether it did. The caller must hold the
// pull request lock.
func (t *Trigger) reconcileIneligibleLocked(ctx context.Context, prID uint) (bool, error) {
	cfg, err := t.reviewConfigLocked(ctx, prID)
	if err != nil {
		return false, err
	}
	if cfg != nil {
		return false, nil
	}
	return true, t.discardActiveLocked(ctx, prID)
}

// discardActiveLocked cancels the checks of the pull request's queued and
// running runs and supersedes them. Cancelling a check that was never created
// is a no-op, and the local supersede still happens when GitHub rejects the
// cancellation, so the worker cannot start a dead review.
func (t *Trigger) discardActiveLocked(ctx context.Context, prID uint) error {
	runs, err := t.Store.ListReviewRuns(ctx, prID)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range runs {
		if run.Status != store.RunQueued && run.Status != store.RunRunning {
			continue
		}
		if t.Checks == nil {
			errs = append(errs, fmt.Errorf("cancel check of review run %d: trigger has no check starter", run.ID))
			continue
		}
		if err := t.Checks.CancelCheck(ctx, run.ID); err != nil {
			errs = append(errs, fmt.Errorf("cancel check of review run %d: %w", run.ID, err))
		}
	}
	if err := t.Store.SupersedeActiveRuns(ctx, prID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reviewConfigLocked resolves the configuration of a reviewable pull request.
// A nil result means it must not be reviewed: closed or merged, draft, fork, a
// missing row, or configuration that no longer resolves (disabled repository,
// removed policy or profile, missing installation). The caller must hold the
// pull request lock.
func (t *Trigger) reviewConfigLocked(ctx context.Context, prID uint) (*store.ReviewConfig, error) {
	pr, err := t.Store.PullRequest(ctx, prID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !pr.IsOpen() || pr.Draft || pr.IsFork {
		return nil, nil
	}
	cfg, err := t.Store.ReviewConfigForPR(ctx, prID)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoReviewConfig) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// repairQueuedRunLocked heals one queued run under the pull request lock,
// recreating only the missing job or check. A run that stopped being queued,
// reviewable or part of the current comparison is superseded instead.
func (t *Trigger) repairQueuedRunLocked(ctx context.Context, work store.QueuedRunWork) (bool, error) {
	run, err := t.Store.ReviewRun(ctx, work.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if run.Status != store.RunQueued {
		return false, nil
	}

	cfg, err := t.reviewConfigLocked(ctx, run.PullRequestID)
	if err != nil {
		return false, err
	}
	if cfg == nil || !cfg.PR.CoveredBy(run) {
		// The pull request stopped being reviewable, or the queued run belongs
		// to an older head/base. Supersede it; EnqueuePending or the next
		// delivery schedules the current comparison.
		return true, t.discardActiveLocked(ctx, run.PullRequestID)
	}

	hasJob, hasCheck, err := t.Store.QueuedRunWork(ctx, run.ID)
	if err != nil {
		return false, err
	}
	repaired := false
	if !hasCheck {
		if t.Checks == nil {
			return false, errors.New("review: trigger has no check starter")
		}
		if err := t.Checks.StartCheck(ctx, run.ID); err != nil {
			return false, fmt.Errorf("start check of review run %d: %w", run.ID, err)
		}
		repaired = true
	}
	if !hasJob {
		if t.Jobs == nil {
			return false, errors.New("review: trigger has no enqueuer")
		}
		if err := t.Jobs.EnqueueReview(ctx, run.ID); err != nil {
			return false, fmt.Errorf("enqueue review run %d: %w", run.ID, err)
		}
		repaired = true
	}
	return repaired, nil
}

func distinctPRIDs(runs []store.ReviewRun) []uint {
	seen := make(map[uint]struct{}, len(runs))
	ids := make([]uint, 0, len(runs))
	for _, run := range runs {
		if _, ok := seen[run.PullRequestID]; ok {
			continue
		}
		seen[run.PullRequestID] = struct{}{}
		ids = append(ids, run.PullRequestID)
	}
	return ids
}

// cloneTokenRefreshWindow is how far ahead the control process renews the clone
// token of a queued run. It is larger than the 5-minute reconcile interval, so a
// token never reaches the worker close to expiring.
const cloneTokenRefreshWindow = 30 * time.Minute

// RefreshQueuedTokens renews the read-only clone tokens of queued runs that are
// close to expiring. The worker cannot mint tokens (it has no GitHub App
// credentials), so the control process does it on every reconciliation (#68).
// One repository failing to mint does not abort the sweep: the rest of the
// queue still gets a fresh token.
func (t *Trigger) RefreshQueuedTokens(ctx context.Context, limit int) (int, error) {
	if t.Apps == nil || t.Box == nil {
		return 0, nil
	}
	runs, err := t.Store.QueuedRunsForTokenRefresh(ctx, time.Now().Add(cloneTokenRefreshWindow), limit)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, nil
	}
	app, err := t.Apps.App(ctx)
	if err != nil {
		return 0, fmt.Errorf("load GitHub App: %w", err)
	}

	refreshed := 0
	for _, run := range runs {
		token, expiresAt, err := app.RepositoryReadToken(ctx, run.InstallationID, run.Repository)
		if err != nil {
			slog.Error("refresh clone token", "run_id", run.RunID, "repository", run.Repository, "error", err)
			continue
		}
		sealed, err := t.Box.Seal([]byte(token))
		if err != nil {
			slog.Error("seal refreshed clone token", "run_id", run.RunID, "error", err)
			continue
		}
		stored, err := t.Store.SetCloneToken(ctx, run.RunID, sealed, expiresAt)
		if err != nil {
			slog.Error("store refreshed clone token", "run_id", run.RunID, "error", err)
			continue
		}
		if stored {
			refreshed++
		}
	}
	return refreshed, nil
}

func isActiveRunUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "idx_review_runs_active"
}

// blocksSameSHA is the redelivery/scheduler guard: superseded runs are the only
// ones that allow a fresh run at an otherwise identical comparison.
func blocksSameSHA(status string) bool {
	switch status {
	case store.RunQueued, store.RunRunning, store.RunSucceeded, store.RunInconclusive, store.RunFailed:
		return true
	default:
		return false
	}
}
