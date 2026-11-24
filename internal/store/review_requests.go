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
	ReviewRequestCauseFork         = "fork"
)

// ReviewRequestConfig is the configuration a review request needs for a pull
// request. Unlike ReviewConfig it also resolves forks and does not require an
// enabled LLM profile: a fork is requested for a human precisely because the
// engine never reviews it.
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

// ReviewRequestFor loads the request of one pull request revision.
func (s *Store) ReviewRequestFor(ctx context.Context, prID uint, headSHA string) (ReviewRequest, error) {
	var req ReviewRequest
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND head_sha = ?", prID, headSHA).
		Take(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRequest{}, fmt.Errorf("review request for pull request %d at %q: %w", prID, headSHA, ErrNotFound)
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("find review request for pull request %d at %q: %w", prID, headSHA, err)
	}
	return req, nil
}

// CreateReviewRequest inserts the request unless the pull request already has
// one for the same head. The boolean reports whether this call inserted it;
// false returns the stored row so the caller can reuse its state.
func (s *Store) CreateReviewRequest(ctx context.Context, req ReviewRequest) (created bool, stored ReviewRequest, err error) {
	req.ID = 0
	res := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "pull_request_id"}, {Name: "head_sha"}},
		DoNothing: true,
	}).Create(&req)
	if res.Error != nil {
		return false, ReviewRequest{}, fmt.Errorf("create review request for pull request %d: %w", req.PullRequestID, res.Error)
	}
	if res.RowsAffected == 1 {
		return true, req, nil
	}
	existing, err := s.ReviewRequestFor(ctx, req.PullRequestID, req.HeadSHA)
	if err != nil {
		return false, ReviewRequest{}, err
	}
	return false, existing, nil
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
