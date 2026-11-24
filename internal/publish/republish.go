package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Republish recomputes the gate of a pull request after a finding changed
// outside a review run: review threads resolved or reopened by hand, and
// findings withdrawn or confirmed with a /qa command. It updates the check run
// of the current head and the QA status, so GitHub and the panel stop
// disagreeing. A head without a finished run yields no verdict and is left
// alone.
func (w *Worker) Republish(ctx context.Context, prID uint) error {
	pr, err := loadPullRequest(ctx, w.Store, prID)
	if err != nil {
		return err
	}
	run, ok, err := w.currentRun(ctx, pr)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	p, err := w.loadRepublishContext(ctx, run, pr)
	if err != nil {
		return err
	}
	findings, err := w.Store.FindingsForPR(ctx, prID)
	if err != nil {
		return err
	}
	open := make([]store.Finding, 0, len(findings))
	for _, f := range findings {
		if f.Status == store.FindingOpen {
			open = append(open, f)
		}
	}

	conclusion := "action_required"
	status := store.StatusInconclusive
	if run.Status == store.RunSucceeded {
		conclusion = "success"
		status = store.StatusReadyForHuman
		if hasBlocking(open, p.policy.BlockingThreshold) {
			conclusion = "failure"
			status = store.StatusChangesRequested
		}
	}

	err = p.completeCheck(ctx, conclusion, open)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// ErrNotFound means this head never got a check run (a failure before
		// StartCheck), so GitHub has nothing to correct.
		return err
	}
	return p.setPRStatus(ctx, status)
}

// SetFindingThread closes or reopens the GitHub review thread of a finding and
// reports whether GitHub supports the operation for it. It returns false
// without error when the finding has no published thread to act on.
func (w *Worker) SetFindingThread(ctx context.Context, prID uint, finding store.Finding, resolved bool) (bool, error) {
	if finding.GithubCommentNodeID == "" && finding.GithubThreadID == "" {
		return false, nil
	}
	pr, err := loadPullRequest(ctx, w.Store, prID)
	if err != nil {
		return false, err
	}
	run, ok, err := w.currentRun(ctx, pr)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	p, err := w.loadRepublishContext(ctx, run, pr)
	if err != nil {
		return false, err
	}

	threadID := finding.GithubThreadID
	if threadID == "" {
		threadID, err = p.lookupThreadIn(ctx, finding.GithubCommentNodeID, true)
		if err != nil {
			return false, err
		}
	}
	if threadID == "" {
		return false, nil
	}

	mutation := resolveThreadMutation
	if !resolved {
		mutation = unresolveThreadMutation
	}
	var response map[string]threadMutationResult
	if err := p.app.GraphQL(ctx, p.inst.ID, mutation, map[string]any{"threadId": threadID}, &response); err != nil {
		return false, fmt.Errorf("set review thread %s resolved=%v: %w", threadID, resolved, err)
	}
	if err := w.Store.SetFindingThread(ctx, finding.ID, threadID); err != nil {
		return false, err
	}
	return true, nil
}

type threadMutationResult struct {
	Thread graphQLThread `json:"thread"`
}

// currentRun returns the latest run of the PR's current head, and false when
// that head has no run that produced a verdict.
func (w *Worker) currentRun(ctx context.Context, pr store.PullRequest) (store.ReviewRun, bool, error) {
	if pr.HeadSHA == "" {
		return store.ReviewRun{}, false, nil
	}
	runs, err := w.Store.ListReviewRuns(ctx, pr.ID)
	if err != nil {
		return store.ReviewRun{}, false, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].HeadSHA != pr.HeadSHA {
			continue
		}
		switch runs[i].Status {
		case store.RunSucceeded, store.RunInconclusive, store.RunFailed:
			// A verdict of another comparison must not drive the shared check
			// or the QA status (#141).
			if !pr.CoveredBy(runs[i]) {
				return store.ReviewRun{}, false, nil
			}
			return runs[i], true, nil
		}
		return store.ReviewRun{}, false, nil
	}
	return store.ReviewRun{}, false, nil
}

func (w *Worker) loadRepublishContext(ctx context.Context, run store.ReviewRun, pr store.PullRequest) (*publishContext, error) {
	repo, err := loadRepository(ctx, w.Store, pr.RepositoryID)
	if err != nil {
		return nil, err
	}
	inst, err := loadInstallation(ctx, w.Store, repo.InstallationID)
	if err != nil {
		return nil, err
	}

	policy := store.QAPolicy{BlockingThreshold: defaultBlockingThreshold}
	if repo.TeamID != nil {
		loaded, err := w.Store.QAPolicyForTeam(ctx, *repo.TeamID)
		switch {
		case err == nil:
			policy = loaded
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}

	app, err := w.Apps.App(ctx)
	if err != nil {
		return nil, fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("create installation client: %w", err)
	}
	owner, name, err := splitFullName(repo.FullName)
	if err != nil {
		return nil, err
	}

	runs, err := w.Store.ListReviewRuns(ctx, pr.ID)
	if err != nil {
		return nil, err
	}
	runNumber := 1
	for i, candidate := range runs {
		if candidate.ID == run.ID {
			runNumber = i + 1
			break
		}
	}

	return &publishContext{
		w: w, run: run, runNumber: runNumber, pr: pr, repo: repo,
		policy: policy, inst: inst, app: app, client: client,
		owner: owner, name: name, redactor: NewRedactor(),
	}, nil
}
