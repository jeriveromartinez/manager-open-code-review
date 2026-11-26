package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Review request states. A request is pending while the engine tries it, being
// sent while one delivery owns it, delivered once GitHub accepted it, and
// rejected when GitHub or the local configuration made it impossible.
// Exhausted transient retries are escalated: the request stays visible as work
// for the team instead of silently vanishing.
const (
	ReviewRequestPending   = "pending"
	ReviewRequestSending   = "sending"
	ReviewRequestDelivered = "delivered"
	ReviewRequestRejected  = "rejected"
	ReviewRequestEscalated = "escalated"
)

// ReviewRequestClaimLease is how long a sending row is trusted before a later
// delivery may reclaim it, so a crash between the HTTP call and the update does
// not leave the request stuck.
const ReviewRequestClaimLease = 5 * time.Minute

// Causes recorded with a review request, so the queue says why it exists.
const (
	ReviewRequestCauseInconclusive = "inconclusive"
	ReviewRequestCauseFailed       = "failed"
	// ReviewRequestCauseFork is the generic external escalation: the team did
	// not enable external reviews, the configuration does not resolve, or the
	// origin could not be determined.
	ReviewRequestCauseFork              = "fork"
	ReviewRequestCauseForkUnauthorized  = "fork_unauthorized"
	ReviewRequestCauseForkTooLarge      = "fork_too_large"
	ReviewRequestCauseForkRateLimited   = "fork_rate_limited"
	ReviewRequestCauseForkBudget        = "fork_budget_exhausted"
	ReviewRequestCauseForkOriginUnknown = "fork_origin_unknown"
)

// ReviewRequestConfig is the configuration a review request needs for a pull
// request. Unlike ReviewConfig it also resolves external contributions and does
// not require an enabled LLM profile: an external request is made when the
// engine may not review the contribution.
type ReviewRequestConfig struct {
	PR           PullRequest
	Repo         Repository
	Policy       QAPolicy
	Installation Installation
}

// ReviewRequestConfigForPR loads everything needed to request a human review.
func (s *Store) ReviewRequestConfigForPR(ctx context.Context, prID uint) (ReviewRequestConfig, error) {
	pr, err := s.PullRequest(ctx, prID)
	if err != nil {
		return ReviewRequestConfig{}, err
	}
	db := quietDB(s.DB.WithContext(ctx))

	var repo Repository
	if err := db.Take(&repo, pr.RepositoryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReviewRequestConfig{}, fmt.Errorf("repository %d: %w", pr.RepositoryID, ErrNoReviewConfig)
		}
		return ReviewRequestConfig{}, fmt.Errorf("find repository %d: %w", pr.RepositoryID, err)
	}
	if !repo.Enabled || repo.TeamID == nil {
		return ReviewRequestConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
	}
	policy, err := s.QAPolicyForTeam(ctx, *repo.TeamID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ReviewRequestConfig{}, err
	}
	// A fork whose team has no policy is still requested: the missing target is
	// recorded as a rejected queue item, exactly like the publish path does.
	var installation Installation
	if err := db.Take(&installation, repo.InstallationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReviewRequestConfig{}, fmt.Errorf("installation %d: %w", repo.InstallationID, ErrNoReviewConfig)
		}
		return ReviewRequestConfig{}, fmt.Errorf("find installation %d: %w", repo.InstallationID, err)
	}
	return ReviewRequestConfig{PR: pr, Repo: repo, Policy: policy, Installation: installation}, nil
}

// ReviewRequestFor loads the newest request recorded for a pull request head.
// Use ReviewRequestForComparison when the base matters: a retarget keeps the
// head and can hold several requests.
func (s *Store) ReviewRequestFor(ctx context.Context, prID uint, headSHA string) (ReviewRequest, error) {
	var req ReviewRequest
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND head_sha = ?", prID, headSHA).
		Order("id DESC").
		Take(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRequest{}, fmt.Errorf("review request for pull request %d at %q: %w", prID, headSHA, ErrNotFound)
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("find review request for pull request %d at %q: %w", prID, headSHA, err)
	}
	return req, nil
}

// ReviewRequestForComparison loads the request of exactly one base-head
// comparison.
func (s *Store) ReviewRequestForComparison(ctx context.Context, prID uint, c Comparison) (ReviewRequest, error) {
	var req ReviewRequest
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND head_sha = ? AND base_ref = ? AND merge_base_sha = ?",
			prID, c.HeadSHA, c.BaseRef, c.MergeBaseSHA).
		Take(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRequest{}, fmt.Errorf("review request for pull request %d at %q on %q: %w",
			prID, c.HeadSHA, c.BaseRef, ErrNotFound)
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("find review request for pull request %d at %q on %q: %w",
			prID, c.HeadSHA, c.BaseRef, err)
	}
	return req, nil
}

// CreateReviewRequest inserts the request unless the pull request already has
// one for the same comparison. The boolean reports whether this call inserted
// it; false returns the stored row so the caller can reuse its state.
func (s *Store) CreateReviewRequest(ctx context.Context, req ReviewRequest) (created bool, stored ReviewRequest, err error) {
	req.ID = 0
	res := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "pull_request_id"}, {Name: "head_sha"},
			{Name: "base_ref"}, {Name: "merge_base_sha"},
		},
		DoNothing: true,
	}).Create(&req)
	if res.Error != nil {
		return false, ReviewRequest{}, fmt.Errorf("create review request for pull request %d: %w", req.PullRequestID, res.Error)
	}
	if res.RowsAffected == 1 {
		return true, req, nil
	}
	existing, err := s.ReviewRequestForComparison(ctx, req.PullRequestID, req.Comparison())
	if err != nil {
		return false, ReviewRequest{}, err
	}
	return false, existing, nil
}

// ReopenReviewRequest returns a rejected request to pending with a fresh retry
// budget after its destination changed. The boolean reports whether this call
// moved it; false means another delivery changed the row first.
func (s *Store) ReopenReviewRequest(ctx context.Context, req ReviewRequest) (ReviewRequest, bool, error) {
	var reopened ReviewRequest
	err := s.DB.WithContext(ctx).Raw(`
UPDATE review_requests
SET status = ?, attempts = 0, last_error = '', delivered_at = NULL,
    base_ref = ?, merge_base_sha = ?, cause = ?,
    reviewer_login = ?, reviewer_team = ?, updated_at = now()
WHERE id = ? AND status = ?
RETURNING *`,
		ReviewRequestPending,
		req.BaseRef, req.MergeBaseSHA, req.Cause,
		req.ReviewerLogin, req.ReviewerTeam, req.ID, ReviewRequestRejected,
	).Scan(&reopened).Error
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("reopen review request %d: %w", req.ID, err)
	}
	return reopened, reopened.ID != 0, nil
}

// RecoverableReviewRequests returns the requests of the current comparison that
// a reconciliation can resume: pending after a crash, a sending row whose lease
// expired, or a rejected row whose configured destination changed. Delivered
// and escalated rows, rows of a previous comparison, rejected rows with an
// unchanged destination and rows whose repository cannot produce a request
// (disabled, no team or no installation) are left alone, so they cannot consume
// the batch and starve later work.
func (s *Store) RecoverableReviewRequests(ctx context.Context, limit int) ([]ReviewRequest, error) {
	var reqs []ReviewRequest
	err := quietDB(s.DB.WithContext(ctx)).Raw(`
SELECT r.*
FROM review_requests r
JOIN pull_requests pr ON pr.id = r.pull_request_id
JOIN repositories repo ON repo.id = pr.repository_id
JOIN installations inst ON inst.id = repo.installation_id
LEFT JOIN qa_policies pol ON pol.team_id = repo.team_id
WHERE repo.enabled
  AND repo.team_id IS NOT NULL
  AND r.head_sha = pr.head_sha
  AND r.base_ref = pr.base_ref
  AND r.merge_base_sha = pr.merge_base_sha
  AND (
       r.status = ?
    OR (r.status = ? AND r.updated_at < now() - make_interval(secs => ?))
    OR (r.status = ? AND (
           r.reviewer_login <> COALESCE(pol.reviewer_login, '')
        OR r.reviewer_team <> COALESCE(pol.reviewer_team, '')
    ))
  )
ORDER BY r.updated_at ASC, r.id ASC
LIMIT ?`,
		ReviewRequestPending, ReviewRequestSending, ReviewRequestClaimLease.Seconds(),
		ReviewRequestRejected, limit,
	).Scan(&reqs).Error
	if err != nil {
		return nil, fmt.Errorf("list recoverable review requests: %w", err)
	}
	return reqs, nil
}

// ClaimReviewRequest moves a pending (or stale sending) request to sending and
// increments its attempt count atomically. The boolean reports whether this
// caller owns the request; false means another delivery is already sending it,
// it reached a terminal state, or the retry budget is exhausted.
func (s *Store) ClaimReviewRequest(ctx context.Context, id uint, maxAttempts int) (ReviewRequest, bool, error) {
	var claimed ReviewRequest
	err := s.DB.WithContext(ctx).Raw(`
UPDATE review_requests
SET status = ?, attempts = attempts + 1, updated_at = now()
WHERE id = ?
  AND attempts < ?
  AND (status = ? OR (status = ? AND updated_at < now() - make_interval(secs => ?)))
RETURNING *`,
		ReviewRequestSending, id, maxAttempts,
		ReviewRequestPending, ReviewRequestSending, ReviewRequestClaimLease.Seconds(),
	).Scan(&claimed).Error
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("claim review request %d: %w", id, err)
	}
	return claimed, claimed.ID != 0, nil
}

// TransitionReviewRequest writes req only when the row is still in from, so a
// delivery that lost the claim cannot overwrite the winner's outcome.
func (s *Store) TransitionReviewRequest(ctx context.Context, from string, req ReviewRequest) (bool, error) {
	res := s.DB.WithContext(ctx).Model(&ReviewRequest{}).
		Where("id = ? AND status = ?", req.ID, from).
		Updates(reviewRequestUpdates(req))
	if res.Error != nil {
		return false, fmt.Errorf("transition review request %d from %q: %w", req.ID, from, res.Error)
	}
	return res.RowsAffected == 1, nil
}

func reviewRequestUpdates(req ReviewRequest) map[string]any {
	return map[string]any{
		"base_ref":       req.BaseRef,
		"merge_base_sha": req.MergeBaseSHA,
		"cause":          req.Cause,
		"reviewer_login": req.ReviewerLogin,
		"reviewer_team":  req.ReviewerTeam,
		"status":         req.Status,
		"attempts":       req.Attempts,
		"last_error":     req.LastError,
		"delivered_at":   req.DeliveredAt,
		"updated_at":     time.Now().UTC(),
	}
}

// ValidReviewRecipient reports whether a GitHub login or team slug is a single
// token. Empty is valid: it means "not configured", which the request path
// records as a rejected item instead of calling GitHub.
func ValidReviewRecipient(value string) bool {
	return !strings.ContainsAny(value, " \t\r\n/")
}
