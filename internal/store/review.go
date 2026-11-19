package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	// ErrNoReviewConfig is returned when no review configuration applies to a
	// pull request.
	ErrNoReviewConfig = errors.New("no review configuration")
	// ErrProfileInUse is returned when deleting an LLM profile still referenced
	// by a team's QA policy.
	ErrProfileInUse = errors.New("llm profile in use")
)

// Defaults applied by SaveQAPolicy when an administrator leaves a field at its
// zero value.
const (
	policyDefaultThreshold    = "major"
	policyDefaultMaxFiles     = 40
	policyDefaultMaxDiffLines = 3000
	policyDefaultMaxPasses    = 4
)

// ReviewConfig is the complete configuration that applies to one pull request.
type ReviewConfig struct {
	PR           PullRequest
	Repo         Repository
	Policy       QAPolicy
	Profile      LLMProfile
	Installation Installation
}

// quietDB silences the "record not found" log line for lookups where a miss is
// an expected outcome (policy resolution, artifact lookup, reload after upsert).
func quietDB(db *gorm.DB) *gorm.DB {
	return db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
}

// CreateLLMProfile inserts a profile whose APIKeyEnc is already encrypted with
// the worker key. The store never decrypts secrets.
func (s *Store) CreateLLMProfile(ctx context.Context, p LLMProfile) (LLMProfile, error) {
	if p.HealthStatus == "" {
		p.HealthStatus = HealthUnknown
	}
	if err := s.DB.WithContext(ctx).Create(&p).Error; err != nil {
		return LLMProfile{}, fmt.Errorf("create llm profile %q: %w", p.Name, err)
	}
	return p, nil
}

// UpdateLLMProfile replaces the editable fields of a profile, leaving its
// health state and timestamps alone. APIKeyEnc is the worker-sealed value; an
// empty value keeps the stored ciphertext instead of wiping it.
func (s *Store) UpdateLLMProfile(ctx context.Context, p LLMProfile) error {
	updates := map[string]any{
		"name":        p.Name,
		"provider":    p.Provider,
		"model":       p.Model,
		"variant":     p.Variant,
		"mode":        p.Mode,
		"base_url":    p.BaseURL,
		"concurrency": p.Concurrency,
		"enabled":     p.Enabled,
		"updated_at":  time.Now().UTC(),
	}
	if p.APIKeyEnc != "" {
		updates["api_key_enc"] = p.APIKeyEnc
	}

	res := s.DB.WithContext(ctx).Model(&LLMProfile{}).Where("id = ?", p.ID).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("update llm profile %d: %w", p.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update llm profile %d: %w", p.ID, ErrNotFound)
	}
	return nil
}

// DeleteLLMProfile removes a profile unless a QA policy still references it.
// The check and the delete run in one transaction so a policy cannot be added
// between them.
func (s *Store) DeleteLLMProfile(ctx context.Context, id uint) error {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var policies int64
		if err := tx.Model(&QAPolicy{}).Where("llm_profile_id = ?", id).Count(&policies).Error; err != nil {
			return fmt.Errorf("count qa policies: %w", err)
		}
		if policies > 0 {
			return ErrProfileInUse
		}

		res := tx.Where("id = ?", id).Delete(&LLMProfile{})
		if res.Error != nil {
			return fmt.Errorf("delete row: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete llm profile %d: %w", id, err)
	}
	return nil
}

// ListLLMProfiles returns all profiles ordered by name.
func (s *Store) ListLLMProfiles(ctx context.Context) ([]LLMProfile, error) {
	var profiles []LLMProfile
	if err := s.DB.WithContext(ctx).Order("name").Find(&profiles).Error; err != nil {
		return nil, fmt.Errorf("list llm profiles: %w", err)
	}
	return profiles, nil
}

// LLMProfile loads a profile by ID, including its encrypted API key.
func (s *Store) LLMProfile(ctx context.Context, id uint) (LLMProfile, error) {
	var profile LLMProfile
	err := s.DB.WithContext(ctx).Take(&profile, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return LLMProfile{}, fmt.Errorf("llm profile %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return LLMProfile{}, fmt.Errorf("find llm profile %d: %w", id, err)
	}
	return profile, nil
}

// SetLLMHealth records the outcome of a provider health check.
func (s *Store) SetLLMHealth(ctx context.Context, id uint, status, message string) error {
	res := s.DB.WithContext(ctx).Model(&LLMProfile{}).Where("id = ?", id).Updates(map[string]any{
		"health_status":     status,
		"health_checked_at": time.Now().UTC(),
		"health_error":      message,
	})
	if res.Error != nil {
		return fmt.Errorf("set llm profile %d health: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set llm profile %d health: %w", id, ErrNotFound)
	}
	return nil
}

// LockProfile takes a session-level PostgreSQL advisory lock keyed by profile
// ID on a dedicated pool connection. Call unlock when the critical section is
// done; it is safe to call more than once.
func (s *Store) LockProfile(ctx context.Context, profileID uint) (unlock func(), err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for llm profile %d lock: %w", profileID, err)
	}

	key := int64(profileID)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock llm profile %d: %w", profileID, err)
	}

	var once sync.Once
	unlock = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Closing the connection releases the session lock even when the
			// explicit unlock fails, so no error can leak into the func() API.
			_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", key)
			conn.Release()
		})
	}
	return unlock, nil
}

// prLockNamespace is a two-int advisory-lock namespace distinct from the
// single-int keys used by LockProfile, so PR and profile locks never collide.
const prLockNamespace = 4242

// LockPR takes a session-level PostgreSQL advisory lock keyed by pull request
// ID on a dedicated pool connection. It serializes the whole enqueue sequence
// of a PR (supersede, insert, check, job). Call unlock when done; it is safe to
// call more than once.
func (s *Store) LockPR(ctx context.Context, prID uint) (unlock func(), err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for pull request %d lock: %w", prID, err)
	}

	key := int64(prID)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1, $2)", prLockNamespace, key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock pull request %d: %w", prID, err)
	}

	var once sync.Once
	unlock = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Closing the connection releases the session lock even when the
			// explicit unlock fails, so no error can leak into the func() API.
			_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1, $2)", prLockNamespace, key)
			conn.Release()
		})
	}
	return unlock, nil
}

// QAPolicyForTeam loads the policy of a team. A missing policy is ErrNotFound.
func (s *Store) QAPolicyForTeam(ctx context.Context, teamID uint) (QAPolicy, error) {
	var policy QAPolicy
	err := quietDB(s.DB.WithContext(ctx)).Where("team_id = ?", teamID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return QAPolicy{}, fmt.Errorf("qa policy for team %d: %w", teamID, ErrNotFound)
	}
	if err != nil {
		return QAPolicy{}, fmt.Errorf("find qa policy for team %d: %w", teamID, err)
	}
	return policy, nil
}

// SaveQAPolicy inserts or updates a team's policy. The unique key is team_id,
// so saving twice never creates a second policy. Zero-valued settings are
// replaced by the instance defaults; an empty globs list is stored as [].
func (s *Store) SaveQAPolicy(ctx context.Context, p QAPolicy) (QAPolicy, error) {
	if p.BlockingThreshold == "" {
		p.BlockingThreshold = policyDefaultThreshold
	}
	if p.MaxFilesPerPass == 0 {
		p.MaxFilesPerPass = policyDefaultMaxFiles
	}
	if p.MaxDiffLinesPerPass == 0 {
		p.MaxDiffLinesPerPass = policyDefaultMaxDiffLines
	}
	if p.MaxPasses == 0 {
		p.MaxPasses = policyDefaultMaxPasses
	}
	globs := bytes.TrimSpace(p.ExcludeGlobs)
	if len(globs) == 0 || bytes.Equal(globs, []byte("null")) {
		p.ExcludeGlobs = datatypes.JSON([]byte(`[]`))
	}

	// The primary key and timestamps belong to the database: a caller may pass
	// a policy loaded earlier, and an upsert must not try to insert its ID.
	p.ID = 0
	p.CreatedAt = time.Time{}
	p.UpdatedAt = time.Time{}

	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "team_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"llm_profile_id", "blocking_threshold", "extra_instructions", "exclude_globs",
			"max_files_per_pass", "max_diff_lines_per_pass", "max_passes", "updated_at",
		}),
	}).Create(&p).Error
	if err != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, err)
	}
	return s.QAPolicyForTeam(ctx, p.TeamID)
}

// ReviewConfigForPR resolves every piece of configuration a worker needs to
// review a pull request. All reasons not to review it map to ErrNoReviewConfig;
// a missing pull request itself is ErrNotFound.
func (s *Store) ReviewConfigForPR(ctx context.Context, prID uint) (ReviewConfig, error) {
	db := quietDB(s.DB.WithContext(ctx))

	var pr PullRequest
	if err := db.Take(&pr, prID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNotFound)
		}
		return ReviewConfig{}, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if pr.Draft || pr.IsFork {
		return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
	}

	var repo Repository
	if err := db.Take(&repo, pr.RepositoryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReviewConfig{}, fmt.Errorf("repository %d: %w", pr.RepositoryID, ErrNoReviewConfig)
		}
		return ReviewConfig{}, fmt.Errorf("find repository %d: %w", pr.RepositoryID, err)
	}
	if !repo.Enabled || repo.TeamID == nil {
		return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
	}

	policy, err := s.QAPolicyForTeam(ctx, *repo.TeamID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
		}
		return ReviewConfig{}, err
	}
	profile, err := s.LLMProfile(ctx, policy.LLMProfileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
		}
		return ReviewConfig{}, err
	}
	if !profile.Enabled {
		return ReviewConfig{}, fmt.Errorf("pull request %d: %w", prID, ErrNoReviewConfig)
	}

	var installation Installation
	if err := db.Take(&installation, repo.InstallationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ReviewConfig{}, fmt.Errorf("installation %d: %w", repo.InstallationID, ErrNoReviewConfig)
		}
		return ReviewConfig{}, fmt.Errorf("find installation %d: %w", repo.InstallationID, err)
	}
	return ReviewConfig{
		PR: pr, Repo: repo, Policy: policy, Profile: profile, Installation: installation,
	}, nil
}

// PendingReviewPRs lists open, non-draft, non-fork PRs of enabled repositories
// with a team, a policy and an enabled profile, whose current head has no run in
// an active or terminal state. Superseded runs do not suppress a PR. Everything
// is filtered in SQL, including the limit and the ordering.
func (s *Store) PendingReviewPRs(ctx context.Context, limit int) ([]PullRequest, error) {
	const query = `
SELECT pr.* FROM pull_requests pr
JOIN repositories r ON r.id = pr.repository_id
JOIN qa_policies q ON q.team_id = r.team_id
JOIN llm_profiles p ON p.id = q.llm_profile_id AND p.enabled = true
WHERE pr.state = 'open' AND pr.draft = false AND pr.is_fork = false
  AND r.enabled = true AND r.team_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM review_runs rr
    WHERE rr.pull_request_id = pr.id
      AND rr.head_sha = pr.head_sha
      AND rr.status IN ('queued','running','succeeded','inconclusive','failed')
  )
ORDER BY pr.updated_at DESC
LIMIT ?`

	var prs []PullRequest
	if err := s.DB.WithContext(ctx).Raw(query, limit).Scan(&prs).Error; err != nil {
		return nil, fmt.Errorf("list pending review pull requests: %w", err)
	}
	return prs, nil
}

// CreateReviewRun inserts a run in the queued state unless the caller supplied
// another status.
func (s *Store) CreateReviewRun(ctx context.Context, in ReviewRun) (ReviewRun, error) {
	if in.Status == "" {
		in.Status = RunQueued
	}
	if in.Attempt == 0 {
		in.Attempt = 1
	}
	if in.Trigger == "" {
		in.Trigger = "webhook"
	}
	in.ID = 0
	in.CreatedAt = time.Time{}
	in.UpdatedAt = time.Time{}
	if err := s.DB.WithContext(ctx).Create(&in).Error; err != nil {
		return ReviewRun{}, fmt.Errorf("create review run for pull request %d: %w", in.PullRequestID, err)
	}
	return in, nil
}

// ClaimReviewRun atomically moves a queued run to running. A false result means
// another worker claimed it (or it is no longer queued) and is not an error.
func (s *Store) ClaimReviewRun(ctx context.Context, id uint) (ReviewRun, bool, error) {
	now := time.Now().UTC()
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunQueued).
		Updates(map[string]any{
			"status":     RunRunning,
			"started_at": now,
			"updated_at": now,
		})
	if res.Error != nil {
		return ReviewRun{}, false, fmt.Errorf("claim review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return ReviewRun{}, false, nil
	}
	run, err := s.ReviewRun(ctx, id)
	if err != nil {
		return ReviewRun{}, false, err
	}
	return run, true, nil
}

// ReviewRun loads one run by ID.
func (s *Store) ReviewRun(ctx context.Context, id uint) (ReviewRun, error) {
	var run ReviewRun
	err := quietDB(s.DB.WithContext(ctx)).Take(&run, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRun{}, fmt.Errorf("review run %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return ReviewRun{}, fmt.Errorf("find review run %d: %w", id, err)
	}
	return run, nil
}

// RunForSHA returns the latest run of a pull request for a given head SHA.
func (s *Store) RunForSHA(ctx context.Context, prID uint, headSHA string) (ReviewRun, error) {
	var run ReviewRun
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND head_sha = ?", prID, headSHA).
		Order("id DESC").Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRun{}, fmt.Errorf("review run for pull request %d at %q: %w", prID, headSHA, ErrNotFound)
	}
	if err != nil {
		return ReviewRun{}, fmt.Errorf("find review run for pull request %d at %q: %w", prID, headSHA, err)
	}
	return run, nil
}

// SupersedeActiveRuns cancels queued and running runs of a pull request so a
// new head can be queued. Terminal runs are left untouched for the audit trail.
func (s *Store) SupersedeActiveRuns(ctx context.Context, prID uint) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("pull_request_id = ? AND status IN ?", prID, []string{RunQueued, RunRunning}).
		Updates(map[string]any{
			"status":     RunSuperseded,
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// SupersedeOtherActiveRuns cancels queued and running runs of a pull request
// whose head SHA differs from headSHA. The run currently being (re)enqueued is
// left untouched, so a trigger can never discard its own freshly created run.
func (s *Store) SupersedeOtherActiveRuns(ctx context.Context, prID uint, headSHA string) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("pull_request_id = ? AND head_sha <> ? AND status IN ?", prID, headSHA, []string{RunQueued, RunRunning}).
		Updates(map[string]any{
			"status":     RunSuperseded,
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede other runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// CompleteReviewRun stores a run's outcome in a single transaction: findings
// are upserted, resolved findings are marked, and only then is the run row
// finished. Any error rolls the whole set back.
func (s *Store) CompleteReviewRun(ctx context.Context, id uint, status, errCode, errMsg, summary string, coverage, usage []byte, findings []Finding, resolved []string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, f := range findings {
			if _, err := upsertFinding(tx, f); err != nil {
				return err
			}
		}
		for _, findingID := range resolved {
			if err := updateFindingStatus(tx, findingID, FindingResolved, id); err != nil {
				return err
			}
		}

		if len(coverage) == 0 {
			coverage = []byte(`{}`)
		}
		if len(usage) == 0 {
			usage = []byte(`{}`)
		}
		now := time.Now().UTC()
		res := tx.Model(&ReviewRun{}).Where("id = ?", id).Updates(map[string]any{
			"status":        status,
			"error_code":    errCode,
			"error_message": errMsg,
			"summary":       summary,
			"coverage":      datatypes.JSON(coverage),
			"usage":         datatypes.JSON(usage),
			"finished_at":   now,
			"updated_at":    now,
		})
		if res.Error != nil {
			return fmt.Errorf("complete review run %d: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("complete review run %d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// ListReviewRuns returns the runs of a pull request oldest first.
func (s *Store) ListReviewRuns(ctx context.Context, prID uint) ([]ReviewRun, error) {
	var runs []ReviewRun
	err := s.DB.WithContext(ctx).Where("pull_request_id = ?", prID).Order("id").Find(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("list review runs of pull request %d: %w", prID, err)
	}
	return runs, nil
}

// SetPRStatus changes only the QA workflow status of a pull request.
func (s *Store) SetPRStatus(ctx context.Context, prID uint, status string) error {
	res := s.DB.WithContext(ctx).Model(&PullRequest{}).Where("id = ?", prID).
		Update("qa_status", status)
	if res.Error != nil {
		return fmt.Errorf("set QA status of pull request %d: %w", prID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set QA status of pull request %d: %w", prID, ErrNotFound)
	}
	return nil
}

// UpsertFinding inserts a finding or refreshes an existing one with the same
// (pull request, fingerprint) pair. The original ID, first seen run and GitHub
// publication coordinates are preserved.
func (s *Store) UpsertFinding(ctx context.Context, f Finding) (Finding, error) {
	return upsertFinding(s.DB.WithContext(ctx), f)
}

func upsertFinding(db *gorm.DB, f Finding) (Finding, error) {
	res := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"path", "start_line", "end_line", "severity", "category", "title",
			"body", "suggestion", "confidence", "status", "last_seen_run_id", "updated_at",
		}),
	}).Create(&f)
	if res.Error != nil {
		return Finding{}, fmt.Errorf("upsert finding %q: %w", f.ID, res.Error)
	}

	var saved Finding
	err := quietDB(db).Where("pull_request_id = ? AND fingerprint = ?", f.PullRequestID, f.Fingerprint).
		Take(&saved).Error
	if err != nil {
		return Finding{}, fmt.Errorf("reload finding %q: %w", f.ID, err)
	}
	return saved, nil
}

// FindingsForPR lists every finding of a pull request, oldest first.
func (s *Store) FindingsForPR(ctx context.Context, prID uint) ([]Finding, error) {
	var findings []Finding
	err := s.DB.WithContext(ctx).Where("pull_request_id = ?", prID).Order("id").Find(&findings).Error
	if err != nil {
		return nil, fmt.Errorf("list findings of pull request %d: %w", prID, err)
	}
	return findings, nil
}

// OpenFindingsForPR lists the findings that still need attention.
func (s *Store) OpenFindingsForPR(ctx context.Context, prID uint) ([]Finding, error) {
	var findings []Finding
	err := s.DB.WithContext(ctx).
		Where("pull_request_id = ? AND status = ?", prID, FindingOpen).
		Order("id").Find(&findings).Error
	if err != nil {
		return nil, fmt.Errorf("list open findings of pull request %d: %w", prID, err)
	}
	return findings, nil
}

// UpdateFindingStatus records a finding's lifecycle transition and the run that
// last observed it.
func (s *Store) UpdateFindingStatus(ctx context.Context, id, status string, runID uint) error {
	return updateFindingStatus(s.DB.WithContext(ctx), id, status, runID)
}

func updateFindingStatus(db *gorm.DB, id, status string, runID uint) error {
	res := db.Model(&Finding{}).Where("id = ?", id).Updates(map[string]any{
		"status":           status,
		"last_seen_run_id": runID,
		"updated_at":       time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("update finding %q status: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update finding %q status: %w", id, ErrNotFound)
	}
	return nil
}

// SetFindingPublished stores the GitHub coordinates of a posted finding.
func (s *Store) SetFindingPublished(ctx context.Context, id string, commentID int64, nodeID, threadID string) error {
	res := s.DB.WithContext(ctx).Model(&Finding{}).Where("id = ?", id).Updates(map[string]any{
		"github_comment_id":      commentID,
		"github_comment_node_id": nodeID,
		"github_thread_id":       threadID,
		"updated_at":             time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("set finding %q published: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set finding %q published: %w", id, ErrNotFound)
	}
	return nil
}

// UpsertArtifact records a published GitHub artifact. Summary comments are one
// per pull request; other kinds are unique per (pull request, kind, head SHA),
// matching the partial unique indexes.
func (s *Store) UpsertArtifact(ctx context.Context, a GitHubArtifact) (GitHubArtifact, error) {
	return upsertArtifact(s.DB.WithContext(ctx), a)
}

func upsertArtifact(db *gorm.DB, a GitHubArtifact) (GitHubArtifact, error) {
	conflict := "ON CONFLICT (pull_request_id, kind, head_sha) WHERE kind <> 'summary_comment'"
	if a.Kind == ArtifactSummary {
		conflict = "ON CONFLICT (pull_request_id) WHERE kind = 'summary_comment'"
	}
	const query = `INSERT INTO github_artifacts
		(pull_request_id, kind, head_sha, external_id, url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		%s
		DO UPDATE SET external_id = EXCLUDED.external_id,
		              url = EXCLUDED.url,
		              updated_at = EXCLUDED.updated_at
		RETURNING *`

	now := time.Now().UTC()
	var saved GitHubArtifact
	err := db.Raw(fmt.Sprintf(query, conflict),
		a.PullRequestID, a.Kind, a.HeadSHA, a.ExternalID, a.URL, now, now,
	).Scan(&saved).Error
	if err != nil {
		return GitHubArtifact{}, fmt.Errorf("upsert %s artifact for pull request %d: %w", a.Kind, a.PullRequestID, err)
	}
	return saved, nil
}

// ArtifactFor loads a published artifact. A summary has no meaningful head SHA
// (there is only one per pull request), so its lookup ignores headSHA.
func (s *Store) ArtifactFor(ctx context.Context, prID uint, kind, headSHA string) (GitHubArtifact, error) {
	q := quietDB(s.DB.WithContext(ctx)).Where("pull_request_id = ? AND kind = ?", prID, kind)
	if kind != ArtifactSummary {
		q = q.Where("head_sha = ?", headSHA)
	}

	var artifact GitHubArtifact
	err := q.Order("id DESC").Take(&artifact).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return GitHubArtifact{}, fmt.Errorf("%s artifact for pull request %d: %w", kind, prID, ErrNotFound)
	}
	if err != nil {
		return GitHubArtifact{}, fmt.Errorf("find %s artifact for pull request %d: %w", kind, prID, err)
	}
	return artifact, nil
}
