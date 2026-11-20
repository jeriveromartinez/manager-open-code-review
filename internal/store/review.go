package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	// ErrRunNotActive is returned when a run is completed after it stopped being
	// the running run (for example because a new head superseded it).
	ErrRunNotActive = errors.New("review run is no longer active")
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

// profileSlotNamespace occupies the high 16 bits of the 64-bit advisory keys
// used for profile slots. The old exclusive LockProfile used bare profile IDs
// (high bits zero)
// and LockPR uses the (4242, id) pair, so the three key spaces are disjoint.
// The remaining 48 bits are a 32-bit profile ID and a 16-bit slot index.
const (
	profileSlotNamespace = 9001
	profileSlotBits      = 16
	maxProfileSlots      = 1 << profileSlotBits
)

func profileSlotKey(profileID uint, slot int) int64 {
	return int64(profileSlotNamespace)<<48 |
		int64(uint64(profileID)&0xffffffff)<<profileSlotBits |
		int64(slot)
}

// AcquireProfileSlot takes one of the profile's concurrency slots as a
// session-level PostgreSQL advisory lock on a dedicated pool connection, so the
// limit holds across processes. The slot count is re-read on every call. It
// reports acquired=false when every slot is busy; that is not an error. Call
// release when the work is done; it is safe to call more than once.
func (s *Store) AcquireProfileSlot(ctx context.Context, profileID uint) (release func(), acquired bool, err error) {
	profile, err := s.LLMProfile(ctx, profileID)
	if err != nil {
		return nil, false, err
	}
	slots := profile.Concurrency
	if slots < 1 {
		// A zero or negative setting must fall back to the schema default
		// instead of leaving the profile unusable.
		slots = 1
	}
	if slots > maxProfileSlots {
		slots = maxProfileSlots
	}

	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire connection for llm profile %d slot: %w", profileID, err)
	}

	var key int64
	found := false
	for slot := 0; slot < slots && !found; slot++ {
		candidate := profileSlotKey(profileID, slot)
		var taken bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", candidate).Scan(&taken); err != nil {
			conn.Release()
			return nil, false, fmt.Errorf("try slot %d of llm profile %d: %w", slot, profileID, err)
		}
		if taken {
			key, found = candidate, true
		}
	}
	if !found {
		// No lock was taken, so the connection only goes back to the pool.
		conn.Release()
		return nil, false, nil
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// A session lock that could not be released keeps the slot busy
			// forever once the connection returns to the pool, so a failed
			// unlock retires the connection instead.
			if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", key); err != nil {
				_ = conn.Conn().Close(unlockCtx)
			}
			conn.Release()
		})
	}
	return release, true, nil
}

// prLockNamespace is a two-int advisory-lock namespace distinct from the
// profile-slot keys, so PR and profile locks never collide.
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
			"max_files_per_pass", "max_diff_lines_per_pass", "max_passes",
			"require_green_ci", "updated_at",
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

// RequeueReviewRun returns a claimed run to the queue without finishing it, so
// a worker that found no profile capacity can retry it later. The attempt
// counter doubles as the number of times the run had to wait: it bounds the
// backoff and it is what the worker compares against its budget.
func (s *Store) RequeueReviewRun(ctx context.Context, id uint, errCode, errMsg string) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning).
		Updates(map[string]any{
			"status":        RunQueued,
			"attempt":       gorm.Expr("attempt + 1"),
			"error_code":    errCode,
			"error_message": errMsg,
			"started_at":    nil,
			"updated_at":    time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("requeue review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("requeue review run %d: %w", id, ErrRunNotActive)
	}
	return nil
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
// The clone token is cleared because a superseded run never publishes.
func (s *Store) SupersedeActiveRuns(ctx context.Context, prID uint) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("pull_request_id = ? AND status IN ?", prID, []string{RunQueued, RunRunning}).
		Updates(map[string]any{
			"status":          RunSuperseded,
			"clone_token_enc": "",
			"updated_at":      time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// SupersedeOtherActiveRuns cancels queued and running runs of a pull request
// whose head SHA differs from headSHA. The run currently being (re)enqueued is
// left untouched, so a trigger can never discard its own freshly created run.
// Superseded runs lose their clone token: they will never publish.
func (s *Store) SupersedeOtherActiveRuns(ctx context.Context, prID uint, headSHA string) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("pull_request_id = ? AND head_sha <> ? AND status IN ?", prID, headSHA, []string{RunQueued, RunRunning}).
		Updates(map[string]any{
			"status":          RunSuperseded,
			"clone_token_enc": "",
			"updated_at":      time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede other runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// CompleteReviewRun stores a run's outcome in a single transaction: the run is
// finished only when it is still running, findings are upserted, resolved
// findings are marked, and the clone token is discarded. Any error rolls the
// whole set back. A run superseded while executing returns ErrRunNotActive and
// persists nothing, so stale results can never be published.
func (s *Store) CompleteReviewRun(ctx context.Context, id uint, status, errCode, errMsg, summary string, coverage, usage []byte, findings []Finding, resolved []string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(coverage) == 0 {
			coverage = []byte(`{}`)
		}
		if len(usage) == 0 {
			usage = []byte(`{}`)
		}
		now := time.Now().UTC()
		res := tx.Model(&ReviewRun{}).
			Where("id = ? AND status = ?", id, RunRunning).
			Updates(map[string]any{
				"status":          status,
				"error_code":      errCode,
				"error_message":   errMsg,
				"summary":         summary,
				"coverage":        datatypes.JSON(coverage),
				"usage":           datatypes.JSON(usage),
				"clone_token_enc": "",
				"finished_at":     now,
				"updated_at":      now,
			})
		if res.Error != nil {
			return fmt.Errorf("complete review run %d: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("complete review run %d: %w", id, ErrRunNotActive)
		}

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
		return nil
	})
}

// InterruptStaleRuns fails every running run whose start is older than cutoff
// and returns the runs it moved. Queued and terminal runs are never touched.
// A single UPDATE decides and persists the transition, so several instances can
// sweep the same database concurrently: row locks serialize them and the
// losers see zero rows, which keeps the count exact and each run interrupted
// once.
func (s *Store) InterruptStaleRuns(ctx context.Context, cutoff time.Time) ([]ReviewRun, error) {
	now := time.Now().UTC()
	// A running run always has started_at; the COALESCE keeps a row claimed
	// without one from staying stranded forever.
	const query = `
UPDATE review_runs
SET status = ?, error_code = ?, error_message = ?, clone_token_enc = '',
    finished_at = ?, updated_at = ?
WHERE status = ? AND COALESCE(started_at, created_at) < ?
RETURNING *`

	var runs []ReviewRun
	err := s.DB.WithContext(ctx).Raw(query,
		RunFailed, RunErrorInterrupted,
		"la revisión quedó interrumpida: el worker que la ejecutaba desapareció antes de terminar",
		now, now, RunRunning, cutoff,
	).Scan(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("interrupt review runs started before %s: %w", cutoff.UTC().Format(time.RFC3339), err)
	}
	return runs, nil
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

// Finding loads one finding by ID.
func (s *Store) Finding(ctx context.Context, id string) (Finding, error) {
	var f Finding
	err := quietDB(s.DB.WithContext(ctx)).Where("id = ?", id).Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Finding{}, fmt.Errorf("finding %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("find finding %q: %w", id, err)
	}
	return f, nil
}

// FindingForThread resolves the finding published in a review thread. commentIDs
// are the review comment node ids carried by the webhook payload; threadID is the
// thread node id already recorded by an earlier resolution.
func (s *Store) FindingForThread(ctx context.Context, prID uint, threadID string, commentIDs []string) (Finding, error) {
	q := quietDB(s.DB.WithContext(ctx)).Where("pull_request_id = ?", prID)
	switch {
	case threadID != "" && len(commentIDs) > 0:
		q = q.Where("github_thread_id = ? OR github_comment_node_id IN ?", threadID, commentIDs)
	case threadID != "":
		q = q.Where("github_thread_id = ?", threadID)
	case len(commentIDs) > 0:
		q = q.Where("github_comment_node_id IN ?", commentIDs)
	default:
		return Finding{}, fmt.Errorf("finding for review thread: %w", ErrNotFound)
	}

	var f Finding
	err := q.Order("id").Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Finding{}, fmt.Errorf("finding for review thread: %w", ErrNotFound)
	}
	if err != nil {
		return Finding{}, fmt.Errorf("find finding for review thread: %w", err)
	}
	return f, nil
}

// SetFindingStatusIfChanged transitions a finding and reports whether the row
// actually changed, so replaying the same webhook delivery is a no-op.
func (s *Store) SetFindingStatusIfChanged(ctx context.Context, id, status string) (bool, error) {
	res := s.DB.WithContext(ctx).Model(&Finding{}).
		Where("id = ? AND status <> ?", id, status).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return false, fmt.Errorf("set finding %q status: %w", id, res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}

	// Zero rows means either an unchanged status or a missing finding.
	if _, err := s.Finding(ctx, id); err != nil {
		return false, err
	}
	return false, nil
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

// SetFindingThread stores the resolved GitHub thread node id of a finding.
func (s *Store) SetFindingThread(ctx context.Context, id, threadID string) error {
	res := s.DB.WithContext(ctx).Model(&Finding{}).Where("id = ?", id).Updates(map[string]any{
		"github_thread_id": threadID,
		"updated_at":       time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("set finding %q thread: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set finding %q thread: %w", id, ErrNotFound)
	}
	return nil
}

// FindingAudit is the audit entry of a finding transition that happened outside
// a review run: a review-thread dispute or a /qa command.
type FindingAudit struct {
	Action    string
	Actor     string
	ActorID   *uint
	CommentID int64
	Reason    string
	ThreadID  string
}

// SetFindingStatusAudited transitions a finding and appends its audit row in a
// single transaction. It reports whether the status actually changed, which is
// what makes a replayed delivery neither re-audit nor flip state back.
func (s *Store) SetFindingStatusAudited(ctx context.Context, id, status string, entry FindingAudit) (bool, error) {
	changed := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		updates := map[string]any{"status": status, "updated_at": now}
		if entry.ThreadID != "" {
			updates["github_thread_id"] = entry.ThreadID
		}

		res := tx.Model(&Finding{}).Where("id = ? AND status <> ?", id, status).Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("update finding %q status: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			// An unchanged status still lets us record a thread ignored by an
			// earlier delivery, but it must not audit the transition twice.
			var existing Finding
			err := quietDB(tx).Where("id = ?", id).Take(&existing).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("update finding %q status: %w", id, ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("find finding %q: %w", id, err)
			}
			if entry.ThreadID != "" && existing.GithubThreadID == "" {
				if err := tx.Model(&Finding{}).Where("id = ?", id).
					Update("github_thread_id", entry.ThreadID).Error; err != nil {
					return fmt.Errorf("set finding %q thread: %w", id, err)
				}
			}
			return nil
		}

		data := map[string]any{"actor": entry.Actor}
		if entry.CommentID != 0 {
			data["comment_id"] = entry.CommentID
		}
		if entry.Reason != "" {
			data["reason"] = entry.Reason
		}
		if entry.ThreadID != "" {
			data["thread_id"] = entry.ThreadID
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshal audit data: %w", err)
		}
		row := AuditLog{
			ActorUserID: entry.ActorID,
			Action:      entry.Action,
			TargetType:  "finding",
			TargetID:    id,
			Data:        datatypes.JSON(raw),
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("write audit row: %w", err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// AuditCommand records a /qa command that is not a finding transition, such as a
// retry. Its comment_id is what makes a replayed delivery a no-op.
func (s *Store) AuditCommand(ctx context.Context, actorID *uint, action, targetType, targetID, actor string, commentID int64) error {
	raw, err := json.Marshal(map[string]any{"actor": actor, "comment_id": commentID})
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	row := AuditLog{
		ActorUserID: actorID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		Data:        datatypes.JSON(raw),
	}
	if err := s.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	return nil
}

// CommandHandled reports whether a /qa comment was already applied. GitHub
// redelivers webhooks, and commands are not naturally idempotent.
func (s *Store) CommandHandled(ctx context.Context, commentID int64) (bool, error) {
	var count int64
	err := s.DB.WithContext(ctx).Model(&AuditLog{}).
		Where("data->>'comment_id' = ?", strconv.FormatInt(commentID, 10)).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("look up command %d: %w", commentID, err)
	}
	return count > 0, nil
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
