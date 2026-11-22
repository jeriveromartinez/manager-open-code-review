package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

// Trigger turns webhook deliveries and the periodic reconciliation into queued
// review runs. It notifies browsers only when it creates a run; the runner
// reports the claim and the terminal state.
type Trigger struct {
	Store  *store.Store
	Apps   github.AppProvider
	Jobs   Enqueuer
	Checks CheckStarter
	Box    *crypto.Box // QA_WORKER_KEY
	Events events.Publisher
}

// OnPullRequest queues a review for the current head of a pull request. A run
// for the same SHA (active or terminal) makes redeliveries harmless.
func (t *Trigger) OnPullRequest(ctx context.Context, prID uint) error {
	_, err := t.enqueue(ctx, prID, "webhook", false)
	return err
}

// OnChecksUpdated re-evaluates the CI gate of a pull request whose check state
// changed on GitHub. Only teams that gate reviews on CI are acted on: for
// everyone else a check delivery must not become a second review trigger, so
// the gate stays exactly where it was.
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
	_, err = t.enqueue(ctx, prID, "webhook", false)
	return err
}

// Retry queues a fresh run even when the current SHA already has a terminal
// run. Active runs of other SHAs are still superseded.
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

// enqueue implements both entry points. The boolean result reports whether a
// run was created, which lets EnqueuePending count only real work.
func (t *Trigger) enqueue(ctx context.Context, prID uint, triggerKind string, ignoreSameSHA bool) (bool, error) {
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

	runs, err := t.Store.ListReviewRuns(ctx, prID)
	if err != nil {
		return false, fmt.Errorf("list review runs of pull request %d: %w", prID, err)
	}
	if !ignoreSameSHA {
		blocked := false
		var stranded *store.ReviewRun
		for i := range runs {
			run := &runs[i]
			if run.HeadSHA != cfg.PR.HeadSHA || !blocksSameSHA(run.Status) {
				continue
			}
			blocked = true
			if run.Status == store.RunQueued && (stranded == nil || run.ID > stranded.ID) {
				stranded = run
			}
		}
		if blocked {
			// A queued run whose StartCheck or EnqueueReview failed on an
			// earlier delivery is stranded: heal it instead of skipping the
			// SHA forever. Terminal runs are simply left alone.
			if stranded != nil {
				if err := t.repair(ctx, *stranded); err != nil {
					return false, err
				}
			}
			return false, nil
		}
	}

	// The CI gate sits next to the draft/fork exclusions: it is re-evaluated on
	// every delivery, so the head is released by the very event that makes CI
	// green instead of waiting for a human retry.
	if cfg.Policy.RequireGreenCI {
		validations, err := t.Store.PRValidationsFor(ctx, prID, cfg.PR.HeadSHA)
		if err != nil {
			return false, fmt.Errorf("load validations of pull request %d: %w", prID, err)
		}
		if !validations.Green() {
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

	// Cancel in-flight checks of OTHER heads before superseding their runs.
	// The current SHA is never superseded by a trigger, so its check stays.
	for _, run := range runs {
		if run.HeadSHA == cfg.PR.HeadSHA ||
			(run.Status != store.RunQueued && run.Status != store.RunRunning) {
			continue
		}
		if err := t.Checks.CancelCheck(ctx, run.ID); err != nil {
			return false, fmt.Errorf("cancel check of review run %d: %w", run.ID, err)
		}
	}
	if err := t.Store.SupersedeOtherActiveRuns(ctx, prID, cfg.PR.HeadSHA); err != nil {
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
			if repairErr := t.repair(ctx, existing); repairErr != nil {
				return false, repairErr
			}
			return false, nil
		}
		return false, fmt.Errorf("create review run for pull request %d: %w", prID, err)
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
// ones that allow a fresh run at an otherwise identical head.
func blocksSameSHA(status string) bool {
	switch status {
	case store.RunQueued, store.RunRunning, store.RunSucceeded, store.RunInconclusive, store.RunFailed:
		return true
	default:
		return false
	}
}
