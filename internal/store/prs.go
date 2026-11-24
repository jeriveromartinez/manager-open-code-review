package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// ErrNotFound is returned when a mutation targets a row that does not exist.
// It is shared by the store methods so callers can map it to a 404.
var ErrNotFound = errors.New("not found")

// stateOpen is GitHub's state value for a pull request that is still open.
// The QA workflow uses different constants for its own status column.
const stateOpen = "open"

// UpsertInstallation inserts or refreshes a GitHub App installation. Local
// state (there is none on installations yet) is never overwritten.
func (s *Store) UpsertInstallation(ctx context.Context, in Installation) error {
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"account_login", "account_type", "suspended_at", "updated_at",
		}),
	}).Create(&in).Error
	if err != nil {
		return fmt.Errorf("upsert installation %d: %w", in.ID, err)
	}
	return nil
}

// DeleteInstallation removes an installation. Repositories and their pull
// requests are removed by the database's ON DELETE CASCADE constraints.
func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	res := s.DB.WithContext(ctx).Where("id = ?", id).Delete(&Installation{})
	if res.Error != nil {
		return fmt.Errorf("delete installation %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete installation %d: %w", id, ErrNotFound)
	}
	return nil
}

// ListInstallations returns every installation known to the instance.
func (s *Store) ListInstallations(ctx context.Context) ([]Installation, error) {
	var rows []Installation
	if err := s.DB.WithContext(ctx).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	return rows, nil
}

// UpsertRepository inserts or refreshes the GitHub metadata of a repository.
// Enabled and TeamID are local settings and are deliberately left untouched on
// conflict so a GitHub sync cannot undo an administrator's configuration.
func (s *Store) UpsertRepository(ctx context.Context, r Repository) error {
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"full_name", "private", "default_branch", "installation_id",
		}),
	}).Create(&r).Error
	if err != nil {
		return fmt.Errorf("upsert repository %d: %w", r.ID, err)
	}
	return nil
}

// RemoveRepository removes a repository and, by cascade, its pull requests.
func (s *Store) RemoveRepository(ctx context.Context, id int64) error {
	res := s.DB.WithContext(ctx).Where("id = ?", id).Delete(&Repository{})
	if res.Error != nil {
		return fmt.Errorf("delete repository %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete repository %d: %w", id, ErrNotFound)
	}
	return nil
}

// EnabledRepositories lists the enabled repositories of an installation.
func (s *Store) EnabledRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	var rows []Repository
	err := s.DB.WithContext(ctx).
		Where("installation_id = ? AND enabled = ?", installationID, true).
		Order("id").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list enabled repositories for installation %d: %w", installationID, err)
	}
	return rows, nil
}

// PullRequest loads one pull request row.
func (s *Store) PullRequest(ctx context.Context, id uint) (PullRequest, error) {
	var pr PullRequest
	err := quietDB(s.DB.WithContext(ctx)).Take(&pr, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PullRequest{}, fmt.Errorf("pull request %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return PullRequest{}, fmt.Errorf("find pull request %d: %w", id, err)
	}
	return pr, nil
}

// SyncPullRequest applies a GitHub pull request event.
//
// Events are keyed by (repository_id, number) and locked with SELECT ... FOR
// UPDATE. A strictly older GitHubUpdatedAt is discarded, and replaying an
// identical payload is a no-op. changed is true only if a row was inserted or
// its persisted business fields actually changed.
func (s *Store) SyncPullRequest(ctx context.Context, in PullRequest) (pr PullRequest, changed bool, err error) {
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prev PullRequest
		loadErr := lockPullRequest(tx, in.RepositoryID, in.Number, &prev)
		switch {
		case loadErr == nil:
			return syncExisting(tx, &prev, in, &pr, &changed)
		case errors.Is(loadErr, gorm.ErrRecordNotFound):
			return insertPullRequest(tx, in, &pr, &changed)
		default:
			return loadErr
		}
	})
	if err != nil {
		return PullRequest{}, false, err
	}
	return pr, changed, nil
}

func lockPullRequest(tx *gorm.DB, repoID int64, number int, dest *PullRequest) error {
	// A missing row is the normal case for a brand-new PR; don't log it as an
	// error on every webhook delivery.
	quiet := tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	return quiet.Where("repository_id = ? AND number = ?", repoID, number).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Take(dest).Error
}

// insertPullRequest creates a new row. DoNothing handles the race where two
// concurrent syncs both observe no row; the loser reloads and merges instead
// of surfacing a unique-violation error.
func insertPullRequest(tx *gorm.DB, in PullRequest, out *PullRequest, changed *bool) error {
	row := in
	row.ID = 0
	row.CreatedAt = time.Time{}
	row.UpdatedAt = time.Time{}
	row.QAStatus = OnSync(nil, in)

	res := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "repository_id"}, {Name: "number"},
		},
		DoNothing: true,
	}).Create(&row)
	if res.Error != nil {
		return fmt.Errorf("insert pull request #%d: %w", in.Number, res.Error)
	}
	if res.RowsAffected > 0 {
		*out = row
		*changed = true
		return nil
	}

	var existing PullRequest
	if err := lockPullRequest(tx, in.RepositoryID, in.Number, &existing); err != nil {
		return fmt.Errorf("reload concurrent pull request #%d: %w", in.Number, err)
	}
	return syncExisting(tx, &existing, in, out, changed)
}

func syncExisting(tx *gorm.DB, prev *PullRequest, in PullRequest, out *PullRequest, changed *bool) error {
	*out = *prev
	*changed = false

	if in.GitHubUpdatedAt.Before(prev.GitHubUpdatedAt) {
		return nil
	}

	status := OnSync(prev, in)
	if pullRequestPayloadEqual(*prev, in, status) {
		return nil
	}

	updates := map[string]any{
		"title":               in.Title,
		"author_login":        in.AuthorLogin,
		"author_association":  in.AuthorAssociation,
		"head_sha":            in.HeadSHA,
		"base_ref":            in.BaseRef,
		"base_sha":            in.BaseSHA,
		"head_ref":            in.HeadRef,
		"html_url":            in.HTMLURL,
		"draft":               in.Draft,
		"state":               in.State,
		"qa_status":           status,
		"github_updated_at":   in.GitHubUpdatedAt,
		"is_fork":             in.IsFork,
		"head_repo_full_name": in.HeadRepoFullName,
		"updated_at":          time.Now().UTC(),
	}
	if prev.HeadSHA != in.HeadSHA {
		// Counters belong to the head they were measured on; a new head starts
		// unknown instead of inheriting the verdict of the previous commit.
		updates["validations_passed"] = 0
		updates["validations_failed"] = 0
		updates["validations_total"] = 0
		updates["validations_detail"] = nil
	}
	if prev.HeadSHA != in.HeadSHA || prev.BaseRef != in.BaseRef || prev.BaseSHA != in.BaseSHA {
		// The merge base belongs to the comparison it was resolved for. Once
		// either side moves it is unknown until the trigger resolves Compare
		// again, and retaining it would let a stale result cover the delivery.
		updates["merge_base_sha"] = ""
	}
	if err := tx.Model(&PullRequest{}).Where("id = ?", prev.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update pull request %d: %w", prev.ID, err)
	}

	var updated PullRequest
	if err := tx.Take(&updated, prev.ID).Error; err != nil {
		return fmt.Errorf("reload pull request %d: %w", prev.ID, err)
	}
	*out = updated
	*changed = true
	return nil
}

// pullRequestPayloadEqual compares the fields owned by a GitHub sync. CreatedAt
// and UpdatedAt are local bookkeeping and must not make an event look new.
func pullRequestPayloadEqual(prev, in PullRequest, status string) bool {
	return prev.Title == in.Title &&
		prev.AuthorLogin == in.AuthorLogin &&
		prev.AuthorAssociation == in.AuthorAssociation &&
		prev.HeadSHA == in.HeadSHA &&
		prev.BaseRef == in.BaseRef &&
		prev.BaseSHA == in.BaseSHA &&
		prev.HeadRef == in.HeadRef &&
		prev.HTMLURL == in.HTMLURL &&
		prev.Draft == in.Draft &&
		prev.State == in.State &&
		prev.QAStatus == status &&
		prev.GitHubUpdatedAt.Equal(in.GitHubUpdatedAt) &&
		prev.IsFork == in.IsFork &&
		prev.HeadRepoFullName == in.HeadRepoFullName
}

// CloseMissingPullRequests closes open pull requests of a repository that are
// no longer present in a GitHub listing. An empty list means GitHub reported
// no open pull requests at all, so every open row is closed.
func (s *Store) CloseMissingPullRequests(ctx context.Context, repoID int64, openNumbers []int) error {
	q := s.DB.WithContext(ctx).Model(&PullRequest{}).
		Where("repository_id = ? AND state = ?", repoID, stateOpen)
	if len(openNumbers) > 0 {
		q = q.Where("number NOT IN ?", openNumbers)
	}
	err := q.Updates(map[string]any{
		"state":      StatusClosed,
		"qa_status":  StatusClosed,
		"updated_at": time.Now().UTC(),
	}).Error
	if err != nil {
		return fmt.Errorf("close missing pull requests in repository %d: %w", repoID, err)
	}
	return nil
}

// PRFilter narrows the visible pull requests by QA workflow group. An empty or
// unknown group applies no filter.
type PRFilter struct{ Group string }

// PRView is the dashboard representation of a pull request. Validations are the
// stored counters of the current head; they stay 0/0 until the check state of
// that head has been read from GitHub. ReviewState tells the panel whether a
// review is actually running, can be scheduled, or never will be, and
// ReviewReason explains the blocked cases.
type PRView struct {
	ID           uint
	Repo         string
	Number       int
	Title        string
	AuthorLogin  string
	HTMLURL      string
	QAStatus     string
	Group        string
	ReviewState  string
	ReviewReason string
	Validations  PRValidations
	OpenFindings SeverityCounts
	StatusSince  time.Time
	TeamID       *uint
	UpdatedAt    time.Time
	// ReviewRequest is the latest human review request of the pull request, or
	// nil when none was ever recorded. It tells the panel whether the engine
	// handed the revision to a person and whether GitHub accepted it.
	ReviewRequest *ReviewRequestSummary
}

// ReviewRequestSummary is the panel-facing state of a human review request.
type ReviewRequestSummary struct {
	Status        string
	HeadSHA       string
	Cause         string
	ReviewerLogin string
	ReviewerTeam  string
	Attempts      int
	LastError     string
	DeliveredAt   *time.Time
}

// SeverityCounts counts findings by severity. Severities outside the known
// four are not counted: the panel has no column for them.
type SeverityCounts struct {
	Blocker int
	Major   int
	Minor   int
	Nit     int
}

// prViewRow mirrors the columns selected from the joined query. Keeping it
// separate from PRView avoids GORM trying to map the computed fields. The
// pointer fields are the ones a LEFT JOIN can leave NULL.
type prViewRow struct {
	ID                         uint
	Repo                       string
	Number                     int
	Title                      string
	AuthorLogin                string
	HTMLURL                    string
	QAStatus                   string
	TeamID                     *uint
	Draft                      bool
	IsFork                     bool
	PolicyID                   *uint
	ProfileID                  *uint
	ProfileEnabled             *bool
	ActiveRun                  string
	ValidationsPassed          int
	ValidationsFailed          int
	ValidationsTotal           int
	OpenBlockers               int
	OpenMajors                 int
	OpenMinors                 int
	OpenNits                   int
	QAStatusChangedAt          time.Time
	UpdatedAt                  time.Time
	ReviewRequestStatus        string
	ReviewRequestHeadSHA       string
	ReviewRequestCause         string
	ReviewRequestReviewerLogin string
	ReviewRequestReviewerTeam  string
	ReviewRequestAttempts      int
	ReviewRequestLastError     string
	ReviewRequestDeliveredAt   *time.Time
}

// Review states reported to the panel.
const (
	ReviewStateRunning   = "running"
	ReviewStateScheduled = "scheduled"
	ReviewStateBlocked   = "blocked"
)

// ReviewSignals are the facts that decide whether a pull request can be
// reviewed. ActiveRun means the current head already has a queued or running
// run.
type ReviewSignals struct {
	ActiveRun      bool
	Draft          bool
	Fork           bool
	TeamAssigned   bool
	HasPolicy      bool
	ProfileEnabled bool
}

// ReviewStateFor mirrors the eligibility rules of ReviewConfigForPR, which are
// the same ones the reconciler applies before enqueueing a run. Keeping the
// decision pure lets tests cover it without a database.
func ReviewStateFor(s ReviewSignals) (state, reason string) {
	// A run in flight is the truth, even if the configuration was removed or
	// disabled after it was queued.
	if s.ActiveRun {
		return ReviewStateRunning, ""
	}
	switch {
	case s.Draft:
		return ReviewStateBlocked, "Borrador: se revisará cuando pase a listo"
	case s.Fork:
		return ReviewStateBlocked, "PR de fork: no se revisa en esta fase"
	case !s.TeamAssigned:
		return ReviewStateBlocked, "Repositorio sin equipo asignado"
	case !s.HasPolicy:
		return ReviewStateBlocked, "El equipo no tiene política QA"
	case !s.ProfileEnabled:
		return ReviewStateBlocked, "La política apunta a un perfil LLM deshabilitado"
	default:
		return ReviewStateScheduled, ""
	}
}

// Stats holds the dashboard KPIs for a user's visible pull requests.
type Stats struct {
	Open          int
	AwaitingHuman int
	InEvaluation  int
	WithProblems  int
}

// visiblePRs builds the shared scope: open pull requests of enabled repos,
// restricted to the caller's teams for non-admins. Group filters use the same
// status predicates as GroupOf.
func (s *Store) visiblePRs(ctx context.Context, u User, f PRFilter) *gorm.DB {
	q := s.teamScopedPRs(ctx, u).Where("pr.state = ?", stateOpen)
	if statuses, ok := groupStatuses(f.Group); ok {
		q = q.Where("pr.qa_status IN ?", statuses)
	}
	return q
}

// teamScopedPRs is every pull request, whatever its state, of the enabled
// repositories the user can see: all of them for admins, those of the user's
// teams otherwise. History-based metrics use it so a merged PR still counts.
func (s *Store) teamScopedPRs(ctx context.Context, u User) *gorm.DB {
	q := s.DB.WithContext(ctx).Model(&PullRequest{}).Table("pull_requests AS pr").
		Joins("JOIN repositories r ON r.id = pr.repository_id").
		Where("r.enabled = ?", true)

	if !u.IsAdmin {
		teams := s.DB.WithContext(ctx).Model(&TeamMember{}).
			Select("team_id").Where("user_id = ?", u.ID)
		q = q.Where("r.team_id IN (?)", teams)
	}
	return q
}

// groupStatuses mirrors GroupOf's predicates for use in SQL filters.
func groupStatuses(group string) ([]string, bool) {
	switch group {
	case "in_review":
		return []string{StatusPending, StatusReviewing, StatusWaitingCI}, true
	case "blocked":
		return []string{StatusChangesRequested, StatusInconclusive, StatusAwaitingApproval, StatusPaused}, true
	case "ready":
		return []string{StatusReadyForHuman}, true
	default:
		return nil, false
	}
}

// ListVisiblePullRequests returns the PRs the user is allowed to see, newest
// first, with the QA group computed from the stored status.
func (s *Store) ListVisiblePullRequests(ctx context.Context, u User, f PRFilter) ([]PRView, error) {
	var rows []prViewRow
	err := s.visiblePRs(ctx, u, f).
		Joins("LEFT JOIN qa_policies pol ON pol.team_id = r.team_id").
		Joins("LEFT JOIN llm_profiles prof ON prof.id = pol.llm_profile_id").
		Joins(`LEFT JOIN LATERAL (
			SELECT rq.status, rq.head_sha, rq.cause, rq.reviewer_login, rq.reviewer_team,
			       rq.attempts, rq.last_error, rq.delivered_at
			FROM review_requests rq WHERE rq.pull_request_id = pr.id
			ORDER BY rq.id DESC LIMIT 1) rr ON true`).
		Select(strings.Join([]string{
			"pr.id AS id",
			"r.full_name AS repo",
			"pr.number AS number",
			"pr.title AS title",
			"pr.author_login AS author_login",
			"pr.html_url AS html_url",
			"pr.qa_status AS qa_status",
			"r.team_id AS team_id",
			"pr.draft AS draft",
			"pr.is_fork AS is_fork",
			"pol.id AS policy_id",
			"prof.id AS profile_id",
			"prof.enabled AS profile_enabled",
			"COALESCE((SELECT rr.status FROM review_runs rr WHERE rr.pull_request_id = pr.id AND rr.head_sha = pr.head_sha AND rr.status IN ('queued','running') ORDER BY rr.id DESC LIMIT 1), '') AS active_run",
			"pr.validations_passed AS validations_passed",
			"pr.validations_failed AS validations_failed",
			"pr.validations_total AS validations_total",
			openFindingsColumn("blocker", "open_blockers"),
			openFindingsColumn("major", "open_majors"),
			openFindingsColumn("minor", "open_minors"),
			openFindingsColumn("nit", "open_nits"),
			"pr.qa_status_changed_at AS qa_status_changed_at",
			"pr.updated_at AS updated_at",
			"COALESCE(rr.status, '') AS review_request_status",
			"COALESCE(rr.head_sha, '') AS review_request_head_sha",
			"COALESCE(rr.cause, '') AS review_request_cause",
			"COALESCE(rr.reviewer_login, '') AS review_request_reviewer_login",
			"COALESCE(rr.reviewer_team, '') AS review_request_reviewer_team",
			"COALESCE(rr.attempts, 0) AS review_request_attempts",
			"COALESCE(rr.last_error, '') AS review_request_last_error",
			"rr.delivered_at AS review_request_delivered_at",
		}, ", ")).
		Order("pr.updated_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list visible pull requests: %w", err)
	}

	views := make([]PRView, len(rows))
	for i, row := range rows {
		state, reason := ReviewStateFor(ReviewSignals{
			ActiveRun:      row.ActiveRun != "",
			Draft:          row.Draft,
			Fork:           row.IsFork,
			TeamAssigned:   row.TeamID != nil,
			HasPolicy:      row.PolicyID != nil,
			ProfileEnabled: row.ProfileID != nil && row.ProfileEnabled != nil && *row.ProfileEnabled,
		})
		views[i] = PRView{
			ID:           row.ID,
			Repo:         row.Repo,
			Number:       row.Number,
			Title:        row.Title,
			AuthorLogin:  row.AuthorLogin,
			HTMLURL:      row.HTMLURL,
			QAStatus:     row.QAStatus,
			Group:        GroupOf(row.QAStatus),
			ReviewState:  state,
			ReviewReason: reason,
			Validations: PRValidations{
				Passed: row.ValidationsPassed,
				Failed: row.ValidationsFailed,
				Total:  row.ValidationsTotal,
			},
			OpenFindings: SeverityCounts{
				Blocker: row.OpenBlockers,
				Major:   row.OpenMajors,
				Minor:   row.OpenMinors,
				Nit:     row.OpenNits,
			},
			StatusSince: row.QAStatusChangedAt,
			TeamID:      row.TeamID,
			UpdatedAt:   row.UpdatedAt,
		}
		if row.ReviewRequestStatus != "" {
			views[i].ReviewRequest = &ReviewRequestSummary{
				Status:        row.ReviewRequestStatus,
				HeadSHA:       row.ReviewRequestHeadSHA,
				Cause:         row.ReviewRequestCause,
				ReviewerLogin: row.ReviewRequestReviewerLogin,
				ReviewerTeam:  row.ReviewRequestReviewerTeam,
				Attempts:      row.ReviewRequestAttempts,
				LastError:     row.ReviewRequestLastError,
				DeliveredAt:   row.ReviewRequestDeliveredAt,
			}
		}
	}
	return views, nil
}

// openFindingsColumn selects how many open findings of one severity a pull
// request has. Severities are fixed literals, never user input.
func openFindingsColumn(severity, alias string) string {
	return fmt.Sprintf(
		"(SELECT COUNT(*) FROM findings f WHERE f.pull_request_id = pr.id AND f.status = '%s' AND f.severity = '%s') AS %s",
		FindingOpen, severity, alias)
}

// Stats computes the dashboard KPIs over the same visible set as
// ListVisiblePullRequests.
func (s *Store) Stats(ctx context.Context, u User) (Stats, error) {
	count := func(q *gorm.DB) (int, error) {
		var n int64
		if err := q.Count(&n).Error; err != nil {
			return 0, err
		}
		return int(n), nil
	}

	inReview, _ := groupStatuses("in_review")
	blocked, _ := groupStatuses("blocked")

	var stats Stats
	var err error
	if stats.Open, err = count(s.visiblePRs(ctx, u, PRFilter{})); err != nil {
		return Stats{}, fmt.Errorf("count open pull requests: %w", err)
	}
	if stats.AwaitingHuman, err = count(s.visiblePRs(ctx, u, PRFilter{}).
		Where("pr.qa_status = ?", StatusReadyForHuman)); err != nil {
		return Stats{}, fmt.Errorf("count pull requests awaiting human: %w", err)
	}
	if stats.InEvaluation, err = count(s.visiblePRs(ctx, u, PRFilter{}).
		Where("pr.qa_status IN ?", inReview)); err != nil {
		return Stats{}, fmt.Errorf("count pull requests in evaluation: %w", err)
	}
	if stats.WithProblems, err = count(s.visiblePRs(ctx, u, PRFilter{}).
		Where("pr.qa_status IN ?", blocked)); err != nil {
		return Stats{}, fmt.Errorf("count pull requests with problems: %w", err)
	}
	return stats, nil
}
