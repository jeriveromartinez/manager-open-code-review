package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	appgithub "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// PublishEnqueuer schedules the publish job of a run. The reconciler uses it to
// recover runs whose worker finished the review but never enqueued delivery.
type PublishEnqueuer interface {
	EnqueuePublish(ctx context.Context, runID uint) error
}

// publishProgress is the durable record of the delivery steps that already
// succeeded. Completed means the run owes nothing else; any later delivery is
// a no-op.
type publishProgress struct {
	Inline      bool `json:"inline,omitempty"`
	Summary     bool `json:"summary,omitempty"`
	Resolutions bool `json:"resolutions,omitempty"`
	Check       bool `json:"check,omitempty"`
	Status      bool `json:"status,omitempty"`
	Completed   bool `json:"completed,omitempty"`
	// CheckAnnotations counts the check annotations GitHub already accepted.
	// GitHub appends them on every PATCH, so a retry resumes after this count.
	CheckAnnotations int `json:"check_annotations,omitempty"`
}

func (p *publishContext) loadProgress() {
	if len(p.run.PublishState) == 0 {
		return
	}
	_ = json.Unmarshal(p.run.PublishState, &p.progress)
}

func (p *publishContext) stepDone(step string) bool {
	switch step {
	case store.PublishStepInline:
		return p.progress.Inline
	case store.PublishStepSummary:
		return p.progress.Summary
	case store.PublishStepResolutions:
		return p.progress.Resolutions
	case store.PublishStepCheck:
		return p.progress.Check
	case store.PublishStepStatus:
		return p.progress.Status
	default:
		return false
	}
}

// markStep persists that one step is done. It is written only after the step
// itself returned success, so a crash between the two can at worst repeat an
// operation that is already idempotent.
func (p *publishContext) markStep(ctx context.Context, step string) error {
	if err := p.w.Store.MarkPublishStep(ctx, p.run.ID, step); err != nil {
		return err
	}
	switch step {
	case store.PublishStepInline:
		p.progress.Inline = true
	case store.PublishStepSummary:
		p.progress.Summary = true
	case store.PublishStepResolutions:
		p.progress.Resolutions = true
	case store.PublishStepCheck:
		p.progress.Check = true
	case store.PublishStepStatus:
		p.progress.Status = true
	}
	return nil
}

// advanceCheckCursor persists how many check annotations GitHub has accepted.
// It only moves forward: a remote count below the local cursor means an older
// observation and must not make a retry resend an accepted page.
func (p *publishContext) advanceCheckCursor(ctx context.Context, accepted int) error {
	if accepted <= p.progress.CheckAnnotations {
		return nil
	}
	if err := p.w.Store.AdvancePublishCursor(ctx, p.run.ID,
		store.PublishCursorCheckAnnotations, accepted); err != nil {
		return err
	}
	p.progress.CheckAnnotations = accepted
	return nil
}

// settle clears the durable publish intent. A run whose comparison moved writes
// nothing shared, so it is settled immediately instead of being retried forever.
func (p *publishContext) settle(ctx context.Context) error {
	if err := p.w.Store.CompletePublish(ctx, p.run.ID); err != nil {
		return err
	}
	p.progress.Completed = true
	return nil
}

// publishCurrency is what a run's comparison is relative to the pull request
// right before a shared write.
type publishCurrency int

const (
	// publishCurrent: the run is the verdict owner of the current comparison.
	publishCurrent publishCurrency = iota
	// publishUnresolved: the head and base branch match but the current merge
	// base is not resolved yet. The trigger can adopt the run later, so the
	// delivery stays pending rather than settling.
	publishUnresolved
	// publishSuperseded: the head or base branch moved, the merge base disagrees,
	// or another run already owns the current comparison. Nothing may be written.
	publishSuperseded
)

// comparisonStale reports whether the run's head or base branch no longer
// matches the pull request, or whether a resolved merge base disagrees. An
// unresolved current merge base is not staleness: the comparison may still be
// adopted, so the intent must stay pending until that is decided.
func (p *publishContext) comparisonStale() bool {
	if !strings.EqualFold(p.pr.HeadSHA, p.run.HeadSHA) {
		return true
	}
	if p.run.BaseRef != "" && p.run.BaseRef != p.pr.BaseRef {
		return true
	}
	if p.pr.MergeBaseSHA == "" {
		return false
	}
	if p.run.BaseSHA == "" {
		return true
	}
	return !strings.EqualFold(p.pr.MergeBaseSHA, p.run.BaseSHA)
}

// ensureCurrent reloads the pull request and reports whether the run still owns
// the shared GitHub state: same head and base–head comparison, and no newer run
// of that comparison. It runs immediately before every shared write, so a late
// or out-of-order publish cannot overwrite a newer summary, check or QA status
// (#138).
func (p *publishContext) ensureCurrent(ctx context.Context) (publishCurrency, error) {
	pr, err := loadPullRequest(ctx, p.w.Store, p.pr.ID)
	if err != nil {
		return publishSuperseded, err
	}
	p.pr = pr
	if !pr.CoveredBy(p.run) {
		if p.comparisonStale() {
			return publishSuperseded, nil
		}
		return publishUnresolved, nil
	}
	owner, ok, err := p.w.currentRun(ctx, pr)
	if err != nil {
		return publishSuperseded, err
	}
	if ok && owner.ID == p.run.ID {
		return publishCurrent, nil
	}
	return publishSuperseded, nil
}

// guard is the call-site form of ensureCurrent. It settles a genuinely stale
// delivery, leaves an unresolved comparison pending for adoption, and reports
// current=true when the caller may write.
func (p *publishContext) guard(ctx context.Context) (bool, error) {
	currency, err := p.ensureCurrent(ctx)
	if err != nil {
		return false, err
	}
	switch currency {
	case publishCurrent:
		return true, nil
	case publishUnresolved:
		return false, nil
	default:
		return false, p.settle(ctx)
	}
}

// ReconcilePendingPublishes enqueues the publish job of every terminal run
// whose delivery is still owed. A duplicate enqueue is harmless: the per-run
// lock serializes the workers and the recorded steps make the second delivery
// resume from where the first stopped.
func (w *Worker) ReconcilePendingPublishes(ctx context.Context, limit int) (int, error) {
	if w.Jobs == nil {
		return 0, nil
	}
	runs, err := w.Store.PendingPublishRuns(ctx, limit)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, run := range runs {
		if err := w.Jobs.EnqueuePublish(ctx, run.ID); err != nil {
			return enqueued, fmt.Errorf("enqueue publish of review run %d: %w", run.ID, err)
		}
		enqueued++
	}
	return enqueued, nil
}

// ReconcileReviewRequests resumes human review requests the delivery path left
// unfinished: a sending row whose lease expired, a pending row after a crash,
// or a rejected row whose destination was corrected. Only the pull request's
// current comparison is resumed, and an ambiguous POST is reconciled with
// GitHub before anything is sent again.
func (w *Worker) ReconcileReviewRequests(ctx context.Context, limit int) (int, error) {
	if w.Apps == nil {
		return 0, nil
	}
	rows, err := w.Store.RecoverableReviewRequests(ctx, limit)
	if err != nil {
		return 0, err
	}
	service := &appgithub.ReviewRequestService{
		Store: w.Store, Apps: w.Apps, RetryDelay: appgithub.ReviewRequestRetryDelay,
	}
	resumed := 0
	for _, row := range rows {
		cfg, err := w.Store.ReviewRequestConfigForPR(ctx, row.PullRequestID)
		if errors.Is(err, store.ErrNoReviewConfig) {
			continue
		}
		if err != nil {
			slog.Error("load review request config", "review_request_id", row.ID, "error", err)
			continue
		}
		if !cfg.PR.Comparison().Same(row.Comparison()) {
			// The pull request moved after the row was selected; its new
			// comparison is served by its own run.
			continue
		}
		if _, err := service.Request(ctx, cfg, row.Cause); err != nil {
			slog.Error("resume review request", "review_request_id", row.ID, "error", err)
			continue
		}
		resumed++
	}
	return resumed, nil
}
