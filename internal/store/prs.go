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
// that head has been read from GitHub.
type PRView struct {
	ID          uint
	Repo        string
	Number      int
	Title       string
	AuthorLogin string
	HTMLURL     string
	QAStatus    string
	Group       string
	Validations PRValidations
	UpdatedAt   time.Time
}

// prViewRow mirrors the columns selected from the joined query. Keeping it
// separate from PRView avoids GORM trying to map the computed fields.
type prViewRow struct {
	ID                uint
	Repo              string
	Number            int
	Title             string
	AuthorLogin       string
	HTMLURL           string
	QAStatus          string
	ValidationsPassed int
	ValidationsFailed int
	ValidationsTotal  int
	UpdatedAt         time.Time
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
	q := s.DB.WithContext(ctx).Model(&PullRequest{}).Table("pull_requests AS pr").
		Joins("JOIN repositories r ON r.id = pr.repository_id").
		Where("pr.state = ?", stateOpen).
		Where("r.enabled = ?", true)

	if !u.IsAdmin {
		teams := s.DB.WithContext(ctx).Model(&TeamMember{}).
			Select("team_id").Where("user_id = ?", u.ID)
		q = q.Where("r.team_id IN (?)", teams)
	}

	if statuses, ok := groupStatuses(f.Group); ok {
		q = q.Where("pr.qa_status IN ?", statuses)
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
		Select(strings.Join([]string{
			"pr.id AS id",
			"r.full_name AS repo",
			"pr.number AS number",
			"pr.title AS title",
			"pr.author_login AS author_login",
			"pr.html_url AS html_url",
			"pr.qa_status AS qa_status",
			"pr.validations_passed AS validations_passed",
			"pr.validations_failed AS validations_failed",
			"pr.validations_total AS validations_total",
			"pr.updated_at AS updated_at",
		}, ", ")).
		Order("pr.updated_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list visible pull requests: %w", err)
	}

	views := make([]PRView, len(rows))
	for i, row := range rows {
		views[i] = PRView{
			ID:          row.ID,
			Repo:        row.Repo,
			Number:      row.Number,
			Title:       row.Title,
			AuthorLogin: row.AuthorLogin,
			HTMLURL:     row.HTMLURL,
			QAStatus:    row.QAStatus,
			Group:       GroupOf(row.QAStatus),
			Validations: PRValidations{
				Passed: row.ValidationsPassed,
				Failed: row.ValidationsFailed,
				Total:  row.ValidationsTotal,
			},
			UpdatedAt: row.UpdatedAt,
		}
	}
	return views, nil
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
