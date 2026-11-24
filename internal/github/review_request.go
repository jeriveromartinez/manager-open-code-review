package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// defaultReviewRequestAttempts is the bounded retry budget of a review
// request. A transient failure that survives it becomes an escalated queue
// item instead of a silent loss.
const defaultReviewRequestAttempts = 3

// ReviewRequestRetryDelay is the small pause between attempts. Callers set it
// on the service; GitHub's Retry-After wins when it asks for a longer wait.
const ReviewRequestRetryDelay = 500 * time.Millisecond

// maxReviewRequestRetryAfter bounds how long a delivery waits for GitHub, so a
// large Retry-After never blocks the publishing or webhook path for minutes.
const maxReviewRequestRetryAfter = 30 * time.Second

// ReviewRequestError is a failed review-request call with the GitHub status.
// A transport error (StatusCode 0), a rate limit and a server error are worth
// retrying; a local configuration error or any other 4xx is not.
type ReviewRequestError struct {
	StatusCode int
	Message    string
	// Local marks a failure produced before GitHub was called (invalid repo,
	// client configuration). It is never retried.
	Local bool
	// RetryAfter is GitHub's requested wait, when the response carried one.
	RetryAfter time.Duration
}

func (e *ReviewRequestError) Error() string {
	if e.StatusCode == 0 {
		return "request review: " + e.Message
	}
	return fmt.Sprintf("request review: GitHub API returned status %d: %s", e.StatusCode, e.Message)
}

// Retryable reports whether the failure can be transient. GitHub uses 403 for
// secondary rate limits, which carry Retry-After; a plain 403 is a permission
// rejection the caller cannot fix by retrying.
func (e *ReviewRequestError) Retryable() bool {
	if e.Local {
		return false
	}
	switch {
	case e.StatusCode == 0:
		return true
	case e.StatusCode == 429:
		return true
	case e.StatusCode == 403:
		return e.RetryAfter > 0
	default:
		return e.StatusCode >= 500
	}
}

// RequestReview asks GitHub to assign reviewers to a pull request through the
// official review-request API. Reviewers are individual logins, team reviewers
// are organisation team slugs.
func (a *App) RequestReview(ctx context.Context, installationID int64, repoFullName string, number int, reviewers, teams []string) error {
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || name == "" {
		return &ReviewRequestError{Local: true, Message: fmt.Sprintf("invalid repository full name %q", repoFullName)}
	}
	if len(reviewers) == 0 && len(teams) == 0 {
		return &ReviewRequestError{Local: true, Message: "no reviewer configured"}
	}
	client, err := a.InstallationClient(installationID)
	if err != nil {
		return &ReviewRequestError{Local: true, Message: err.Error()}
	}
	_, resp, err := client.PullRequests.RequestReviewers(ctx, owner, name, number, gh.ReviewersRequest{
		Reviewers:     reviewers,
		TeamReviewers: teams,
	})
	if err == nil {
		return nil
	}
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	var apiErr *gh.ErrorResponse
	if errors.As(err, &apiErr) {
		if apiErr.Response != nil {
			statusCode = apiErr.Response.StatusCode
		}
		return &ReviewRequestError{
			StatusCode: statusCode,
			Message:    apiErr.Message,
			RetryAfter: retryAfterDelay(apiErr.Response),
		}
	}
	return &ReviewRequestError{StatusCode: statusCode, Message: err.Error(), RetryAfter: retryAfterDelay(responseOf(resp))}
}

func responseOf(resp *gh.Response) *http.Response {
	if resp == nil {
		return nil
	}
	return resp.Response
}

func retryAfterDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

// ReviewRequestService requests a human review for a revision the engine cannot
// conclude or does not review. It deduplicates by pull request and head, so a
// redelivery, a retried publish or a second fork event never asks GitHub twice.
type ReviewRequestService struct {
	Store *store.Store
	Apps  AppProvider
	// MaxAttempts defaults to defaultReviewRequestAttempts.
	MaxAttempts int
	// RetryDelay is the pause between attempts. Zero retries immediately; the
	// bounded budget is what protects the API, not a long wait.
	RetryDelay time.Duration
}

// Request records and delivers one review request. A terminal row is returned
// as-is without calling GitHub. A GitHub rejection or an exhausted retry budget
// is recorded and returned without an error: the queue item is the outcome, not
// a failure that should make the publishing job retry forever.
func (s *ReviewRequestService) Request(ctx context.Context, cfg store.ReviewRequestConfig, cause string) (store.ReviewRequest, error) {
	if s == nil || s.Store == nil {
		return store.ReviewRequest{}, errors.New("request review: service has no store")
	}
	pr := cfg.PR
	if pr.HeadSHA == "" {
		return store.ReviewRequest{}, nil
	}
	reviewers, teams := reviewRecipients(cfg.Policy)
	req := store.ReviewRequest{
		PullRequestID: pr.ID,
		HeadSHA:       pr.HeadSHA,
		BaseRef:       pr.BaseRef,
		MergeBaseSHA:  pr.MergeBaseSHA,
		Cause:         cause,
		ReviewerLogin: strings.Join(reviewers, ","),
		ReviewerTeam:  strings.Join(teams, ","),
		Status:        store.ReviewRequestPending,
	}
	created, stored, err := s.Store.CreateReviewRequest(ctx, req)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	req = stored
	// A pending row can be resumed after a restart or a redelivery, so the
	// current target and comparison replace the stored ones before the retry.
	req.BaseRef = pr.BaseRef
	req.MergeBaseSHA = pr.MergeBaseSHA
	req.Cause = cause
	req.ReviewerLogin = strings.Join(reviewers, ",")
	req.ReviewerTeam = strings.Join(teams, ",")
	if !created {
		switch stored.Status {
		case store.ReviewRequestDelivered, store.ReviewRequestRejected, store.ReviewRequestEscalated:
			return stored, nil
		case store.ReviewRequestSending:
			// A fresh send belongs to another delivery; a stale one is reclaimed
			// below so a crash cannot strand the request.
			if time.Since(stored.UpdatedAt) < store.ReviewRequestClaimLease {
				return stored, nil
			}
		}
	}
	if len(reviewers) == 0 && len(teams) == 0 {
		return s.transition(ctx, stored.Status, req, store.ReviewRequestRejected, "no reviewer configured")
	}
	if s.Apps == nil {
		return s.transition(ctx, stored.Status, req, store.ReviewRequestRejected, "request review: no GitHub App provider")
	}

	maxAttempts := s.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultReviewRequestAttempts
	}
	for {
		claimed, ok, err := s.Store.ClaimReviewRequest(ctx, req.ID, maxAttempts)
		if err != nil {
			return store.ReviewRequest{}, err
		}
		if !ok {
			return s.outcomeWithoutClaim(ctx, req, maxAttempts)
		}
		req = claimed

		err = s.deliver(ctx, cfg, reviewers, teams)
		if err == nil {
			return s.transition(ctx, store.ReviewRequestSending, req, store.ReviewRequestDelivered, "")
		}
		req.LastError = err.Error()
		if !reviewErrorRetryable(err) {
			return s.transition(ctx, store.ReviewRequestSending, req, store.ReviewRequestRejected, req.LastError)
		}
		if req.Attempts >= maxAttempts {
			return s.transition(ctx, store.ReviewRequestSending, req, store.ReviewRequestEscalated, req.LastError)
		}
		pending, releaseErr := s.transition(ctx, store.ReviewRequestSending, req, store.ReviewRequestPending, req.LastError)
		if releaseErr != nil {
			return pending, releaseErr
		}
		req = pending
		if err := sleepRetry(ctx, reviewRetryDelay(err, s.RetryDelay)); err != nil {
			return req, err
		}
	}
}

// outcomeWithoutClaim reports what the request is doing when this delivery
// could not claim it: another delivery is sending it, it is terminal, or the
// retry budget ran out and the item is escalated.
func (s *ReviewRequestService) outcomeWithoutClaim(ctx context.Context, req store.ReviewRequest, maxAttempts int) (store.ReviewRequest, error) {
	current, err := s.Store.ReviewRequestFor(ctx, req.PullRequestID, req.HeadSHA)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	switch current.Status {
	case store.ReviewRequestSending:
		// A stale send whose retry budget is spent cannot be reclaimed, so it
		// must escalate instead of reporting "sending" forever.
		if time.Since(current.UpdatedAt) < store.ReviewRequestClaimLease || current.Attempts < maxAttempts {
			return current, nil
		}
		if current.LastError == "" {
			current.LastError = "retry budget exhausted"
		}
		return s.transition(ctx, store.ReviewRequestSending, current, store.ReviewRequestEscalated, current.LastError)
	case store.ReviewRequestDelivered, store.ReviewRequestRejected, store.ReviewRequestEscalated:
		return current, nil
	}
	if current.Attempts >= maxAttempts {
		if current.LastError == "" {
			current.LastError = "retry budget exhausted"
		}
		return s.transition(ctx, store.ReviewRequestPending, current, store.ReviewRequestEscalated, current.LastError)
	}
	return current, nil
}

func (s *ReviewRequestService) deliver(ctx context.Context, cfg store.ReviewRequestConfig, reviewers, teams []string) error {
	app, err := s.Apps.App(ctx)
	if err != nil {
		return err
	}
	return app.RequestReview(ctx, cfg.Installation.ID, cfg.Repo.FullName, cfg.PR.Number, reviewers, teams)
}

func (s *ReviewRequestService) transition(ctx context.Context, from string, req store.ReviewRequest, status, lastError string) (store.ReviewRequest, error) {
	req.Status = status
	req.LastError = lastError
	if status == store.ReviewRequestDelivered {
		now := time.Now().UTC()
		req.DeliveredAt = &now
	}
	if from == "" {
		return req, errors.New("request review: missing previous state")
	}
	if _, err := s.Store.TransitionReviewRequest(ctx, from, req); err != nil {
		return req, err
	}
	return req, nil
}

// reviewRetryDelay honors GitHub's Retry-After up to the cap, falling back to
// the configured delay otherwise.
func reviewRetryDelay(err error, fallback time.Duration) time.Duration {
	delay := fallback
	var reqErr *ReviewRequestError
	if errors.As(err, &reqErr) && reqErr.RetryAfter > delay {
		delay = reqErr.RetryAfter
	}
	if delay > maxReviewRequestRetryAfter {
		delay = maxReviewRequestRetryAfter
	}
	return delay
}

func sleepRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func reviewRecipients(policy store.QAPolicy) (reviewers, teams []string) {
	if login := strings.TrimSpace(policy.ReviewerLogin); login != "" {
		reviewers = []string{login}
	}
	if team := strings.TrimSpace(policy.ReviewerTeam); team != "" {
		teams = []string{team}
	}
	return reviewers, teams
}

func reviewErrorRetryable(err error) bool {
	var reqErr *ReviewRequestError
	if errors.As(err, &reqErr) {
		return reqErr.Retryable()
	}
	return false
}
