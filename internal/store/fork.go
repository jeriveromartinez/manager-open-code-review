package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Origin of a pull request relative to the repository it targets. The head
// repository's own `fork` flag is not used: a repository that is a fork of
// something else still owns its internal branches (#148).
const (
	OriginInternal  = "internal"
	OriginCrossRepo = "cross_repo"
	OriginUnknown   = "unknown"
)

// ClassifyOrigin compares the head and base repository identities. An empty
// side means GitHub did not report it: the origin is indeterminate and the
// caller must escalate instead of guessing.
func ClassifyOrigin(baseFullName, headFullName string) string {
	base := strings.TrimSpace(baseFullName)
	head := strings.TrimSpace(headFullName)
	if base == "" || head == "" {
		return OriginUnknown
	}
	if strings.EqualFold(base, head) {
		return OriginInternal
	}
	return OriginCrossRepo
}

// External reports whether the pull request crosses repositories or its origin
// could not be established. Rows written before the origin column existed fall
// back to the stored is_fork flag.
func (p PullRequest) External() bool {
	switch p.Origin {
	case OriginInternal:
		return false
	case OriginCrossRepo, OriginUnknown:
		return true
	default:
		return p.IsFork
	}
}

// ExternalUsage is the external contribution consumption of the last day that
// the policy limits are checked against.
type ExternalUsage struct {
	// RunsToday counts review runs of the same external author in the last 24
	// hours.
	RunsToday int
	// TokensToday sums the token usage of external runs in the repository in
	// the last 24 hours.
	TokensToday int64
}

// ExternalDecision is the outcome of applying the external-contribution policy
// to one pull request. Cause is the review-request cause recorded when the
// contribution is escalated; Reason is the human-readable explanation.
type ExternalDecision struct {
	Allow  bool
	Cause  string
	Reason string
}

// Defaults applied when a saved policy leaves the external limits at zero.
const (
	DefaultExternalDiffLines  = 3000
	DefaultExternalRunsPerDay = 10
)

// DefaultExternalAssociations are the author associations trusted by default
// when a policy does not list any.
var DefaultExternalAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// DecideExternalReview applies the team policy to an external pull request.
// Every denial maps to a review-request cause so the escalation keeps its
// reason instead of becoming an anonymous fork request.
func DecideExternalReview(pr PullRequest, policy QAPolicy, usage ExternalUsage) ExternalDecision {
	if !pr.External() {
		return ExternalDecision{Allow: true}
	}
	if pr.Origin == OriginUnknown {
		return ExternalDecision{
			Cause:  ReviewRequestCauseForkOriginUnknown,
			Reason: "no se pudo determinar el repositorio head del pull request",
		}
	}
	if !policy.AllowExternalReviews {
		return ExternalDecision{
			Cause:  ReviewRequestCauseFork,
			Reason: "el equipo no permite revisar aportes externos",
		}
	}
	if !trustedExternalAuthor(policy, pr.AuthorAssociation) {
		return ExternalDecision{
			Cause:  ReviewRequestCauseForkUnauthorized,
			Reason: fmt.Sprintf("la asociación del autor %q no está autorizada", pr.AuthorAssociation),
		}
	}

	limit := policy.MaxExternalDiffLines
	if limit <= 0 {
		limit = DefaultExternalDiffLines
	}
	if changed := pr.Additions + pr.Deletions; changed > limit {
		return ExternalDecision{
			Cause:  ReviewRequestCauseForkTooLarge,
			Reason: fmt.Sprintf("el aporte cambia %d líneas y el límite es %d", changed, limit),
		}
	}

	runs := policy.MaxExternalRunsPerDay
	if runs <= 0 {
		runs = DefaultExternalRunsPerDay
	}
	if usage.RunsToday >= runs {
		return ExternalDecision{
			Cause:  ReviewRequestCauseForkRateLimited,
			Reason: fmt.Sprintf("%d runs externos en 24h, límite %d", usage.RunsToday, runs),
		}
	}

	if policy.ExternalDailyTokenBudget > 0 && usage.TokensToday >= policy.ExternalDailyTokenBudget {
		return ExternalDecision{
			Cause: ReviewRequestCauseForkBudget,
			Reason: fmt.Sprintf("%d tokens externos en 24h, presupuesto %d",
				usage.TokensToday, policy.ExternalDailyTokenBudget),
		}
	}
	return ExternalDecision{Allow: true}
}

// normalizeExternalAssociations validates the policy list and falls back to
// the defaults when it is empty. Associations are GitHub's uppercase enum
// values; anything with whitespace cannot be one.
func normalizeExternalAssociations(raw []byte) ([]string, error) {
	var configured []string
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &configured); err != nil {
			return nil, fmt.Errorf("invalid external trusted associations: %w", err)
		}
	}
	if len(configured) == 0 {
		return append([]string(nil), DefaultExternalAssociations...), nil
	}
	seen := make(map[string]struct{}, len(configured))
	normalized := make([]string, 0, len(configured))
	for _, association := range configured {
		association = strings.ToUpper(strings.TrimSpace(association))
		if association == "" || strings.ContainsAny(association, " \t\r\n") {
			return nil, fmt.Errorf("invalid external trusted association %q", association)
		}
		if _, ok := seen[association]; ok {
			continue
		}
		seen[association] = struct{}{}
		normalized = append(normalized, association)
	}
	return normalized, nil
}

// trustedExternalAuthor reports whether the author association is in the
// policy list. An empty or malformed list falls back to the defaults.
func trustedExternalAuthor(policy QAPolicy, association string) bool {
	association = strings.TrimSpace(association)
	if association == "" {
		return false
	}
	trusted := DefaultExternalAssociations
	var configured []string
	if len(policy.ExternalTrustedAssociations) > 0 {
		if err := json.Unmarshal(policy.ExternalTrustedAssociations, &configured); err == nil && len(configured) > 0 {
			trusted = configured
		}
	}
	for _, candidate := range trusted {
		if strings.EqualFold(strings.TrimSpace(candidate), association) {
			return true
		}
	}
	return false
}

// ExternalReviewUsage measures the author's runs and the team's token spend
// over the trailing 24 hours. Only external pull requests are counted, so
// internal reviews never consume the contribution budget. In-flight runs count
// their reservation until real usage is known, and spend is per team so a team
// with several repositories does not get one budget per repository.
func (s *Store) ExternalReviewUsage(ctx context.Context, pr PullRequest, teamID uint) (ExternalUsage, error) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	var runs int64
	if err := s.DB.WithContext(ctx).Raw(`
SELECT count(*)
FROM review_runs rr
JOIN pull_requests p ON p.id = rr.pull_request_id
WHERE p.author_login = ? AND p.is_fork = true AND rr.created_at >= ?`,
		pr.AuthorLogin, since,
	).Scan(&runs).Error; err != nil {
		return ExternalUsage{}, fmt.Errorf("count external review runs of author %q: %w", pr.AuthorLogin, err)
	}
	var tokens int64
	if err := s.DB.WithContext(ctx).Raw(`
SELECT COALESCE(sum(COALESCE(rr.total_tokens, rr.reserved_tokens)), 0)
FROM review_runs rr
JOIN pull_requests p ON p.id = rr.pull_request_id
JOIN repositories r ON r.id = p.repository_id
WHERE r.team_id = ? AND p.is_fork = true AND rr.created_at >= ?`,
		teamID, since,
	).Scan(&tokens).Error; err != nil {
		return ExternalUsage{}, fmt.Errorf("sum external tokens of team %d: %w", teamID, err)
	}
	return ExternalUsage{RunsToday: int(runs), TokensToday: tokens}, nil
}

// ExternalRunTokenReserve is the spend an external run holds from the moment it
// is queued. A policy without a total budget is unlimited, so it reserves
// nothing.
func ExternalRunTokenReserve(policy QAPolicy) int64 {
	if policy.ReviewTokenBudget > 0 {
		return policy.ReviewTokenBudget
	}
	return 0
}

// LockExternalAuthor serializes the external admission of one author. The
// caller holds it across the usage check and the run insert, so two concurrent
// deliveries cannot both observe room under MaxExternalRunsPerDay. It is a
// session advisory lock on a dedicated connection; the returned function
// releases it and returns the connection to the pool.
func (s *Store) LockExternalAuthor(ctx context.Context, login string) (func(), error) {
	login = strings.ToLower(strings.TrimSpace(login))
	db, err := s.DB.DB()
	if err != nil {
		return nil, fmt.Errorf("external author lock: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("external author lock: %w", err)
	}
	key := "qa-external-author:" + login
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("lock external author %q: %w", login, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
				slog.Error("release external author lock", "author", login, "error", err)
			}
			_ = conn.Close()
		})
	}, nil
}
