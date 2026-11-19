package review

import (
	"context"
	"errors"
	"fmt"

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
// review runs. It never publishes browser events: the runner does that once the
// run reaches a terminal state.
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
		for _, run := range runs {
			if run.HeadSHA == cfg.PR.HeadSHA && blocksSameSHA(run.Status) {
				return false, nil
			}
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

	// Cancel every in-flight check before superseding its run. Only queued and
	// running rows can be active; terminal runs are kept for the audit trail.
	for _, run := range runs {
		if run.Status != store.RunQueued && run.Status != store.RunRunning {
			continue
		}
		if err := t.Checks.CancelCheck(ctx, run.ID); err != nil {
			return false, fmt.Errorf("cancel check of review run %d: %w", run.ID, err)
		}
	}
	if err := t.Store.SupersedeActiveRuns(ctx, prID); err != nil {
		return false, fmt.Errorf("supersede review runs of pull request %d: %w", prID, err)
	}

	app, err := t.Apps.App(ctx)
	if err != nil {
		return false, fmt.Errorf("load GitHub App: %w", err)
	}
	token, _, err := app.RepositoryReadToken(ctx, cfg.Installation.ID, cfg.Repo.FullName)
	if err != nil {
		return false, fmt.Errorf("mint read token for repository %s: %w", cfg.Repo.FullName, err)
	}
	sealed, err := t.Box.Seal([]byte(token))
	if err != nil {
		return false, fmt.Errorf("seal clone token: %w", err)
	}

	run, err := t.Store.CreateReviewRun(ctx, store.ReviewRun{
		PullRequestID: prID,
		ProfileID:     cfg.Profile.ID,
		HeadSHA:       cfg.PR.HeadSHA,
		Trigger:       triggerKind,
		Status:        store.RunQueued,
		CloneTokenEnc: sealed,
	})
	if err != nil {
		return false, fmt.Errorf("create review run for pull request %d: %w", prID, err)
	}
	if err := t.Checks.StartCheck(ctx, run.ID); err != nil {
		return false, fmt.Errorf("start check of review run %d: %w", run.ID, err)
	}
	if err := t.Jobs.EnqueueReview(ctx, run.ID); err != nil {
		return false, fmt.Errorf("enqueue review run %d: %w", run.ID, err)
	}
	return true, nil
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
