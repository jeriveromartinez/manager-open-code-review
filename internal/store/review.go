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
	// ErrFindingResolved is returned when triaging a finding the code already
	// fixed: confirming it would reopen it and re-block the gate.
	ErrFindingResolved = errors.New("finding is resolved")
	// ErrFindingHumanTriaged is returned when the verifier tries to change a
	// finding a person already decided. A human never loses to the second pass.
	ErrFindingHumanTriaged = errors.New("finding already triaged by a human")
	// ErrFindingThreadAmbiguous is returned when a review thread delivery
	// matches more than one finding. Picking one would change a finding the
	// event did not target.
	ErrFindingThreadAmbiguous = errors.New("review thread matches several findings")
)

// Defaults applied by SaveQAPolicy when an administrator leaves a field at its
// zero value.
const (
	policyDefaultThreshold    = "major"
	policyDefaultMaxFiles     = 40
	policyDefaultMaxDiffLines = 3000
	policyDefaultMaxPasses    = 4
	policyDefaultReviewEffort = ReviewEffortLow

	DefaultVerifyMaxFindings = 50
	DefaultVerifyMaxFiles    = 20
	DefaultVerifyTokenShare  = 20
	DefaultVerifyTimeShare   = 15
	DefaultVerifyTokenMax    = 100000
)

// VerifyTokenMaxMin is the smallest per-run cap the second OCR pass accepts:
// below it the scan cannot answer anything useful.
const VerifyTokenMaxMin = 10000

// Review effort values and policy bounds shared by validation and the API. The
// context ceiling is the per-conversation input limit, not the run's total
// spend; the token budget is the optional total cap (#72).
const (
	ReviewEffortLow    = "low"
	ReviewEffortMedium = "medium"
	ReviewEffortHigh   = "high"

	ReviewContextCeilingMin = 4096
	ReviewContextCeilingMax = 2000000
	ReviewTokenBudgetMax    = 1000000000
)

// ValidReviewEffort reports whether effort is one of the three values the
// engine understands.
func ValidReviewEffort(effort string) bool {
	switch effort {
	case ReviewEffortLow, ReviewEffortMedium, ReviewEffortHigh:
		return true
	default:
		return false
	}
}

// ValidAutoTriageMode reports whether mode is one the verifier understands.
func ValidAutoTriageMode(mode string) bool {
	switch mode {
	case AutoTriageOff, AutoTriageDryRun, AutoTriageApply:
		return true
	default:
		return false
	}
}

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

// HealthUsage is the token accounting of one health check. A nil *HealthUsage
// means the check recorded none: the probe failed or the provider did not
// report usage, which must not count as zero in an average.
type HealthUsage struct {
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Total      int64
	MaxContext int64
}

// RecordLLMHealth stores the verdict on the profile and appends the check to
// the history in one transaction. Usage is stored as NULL when nil, so the
// check is visible but excluded from token averages.
func (s *Store) RecordLLMHealth(ctx context.Context, id uint, status, message string, usage *HealthUsage) error {
	checkedAt := time.Now().UTC()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&LLMProfile{}).Where("id = ?", id).Updates(map[string]any{
			"health_status":     status,
			"health_checked_at": checkedAt,
			"health_error":      message,
		})
		if res.Error != nil {
			return fmt.Errorf("set llm profile %d health: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("set llm profile %d health: %w", id, ErrNotFound)
		}

		check := LLMHealthCheck{
			ProfileID: id, Status: status, ErrorMessage: message, CheckedAt: checkedAt,
		}
		if usage != nil {
			input, output, reasoning := usage.Input, usage.Output, usage.Reasoning
			cacheRead, cacheWrite := usage.CacheRead, usage.CacheWrite
			total, maxContext := usage.Total, usage.MaxContext
			check.InputTokens, check.OutputTokens, check.ReasoningTokens = &input, &output, &reasoning
			check.CacheReadTokens, check.CacheWriteTokens = &cacheRead, &cacheWrite
			check.TotalTokens, check.MaxContextTokens = &total, &maxContext
		}
		if err := tx.Create(&check).Error; err != nil {
			return fmt.Errorf("record llm profile %d health check: %w", id, err)
		}
		return nil
	})
	return err
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
	if p.ReviewEffort == "" {
		p.ReviewEffort = policyDefaultReviewEffort
	}
	if !ValidReviewEffort(p.ReviewEffort) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid review effort %q", p.TeamID, p.ReviewEffort)
	}
	if p.ReviewContextCeiling != nil &&
		(*p.ReviewContextCeiling < ReviewContextCeilingMin || *p.ReviewContextCeiling > ReviewContextCeilingMax) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid review context ceiling %d",
			p.TeamID, *p.ReviewContextCeiling)
	}
	if p.ReviewTokenBudget < 0 || p.ReviewTokenBudget > ReviewTokenBudgetMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid review token budget %d",
			p.TeamID, p.ReviewTokenBudget)
	}
	if p.AutoTriageMode == "" {
		p.AutoTriageMode = AutoTriageOff
	}
	if !ValidAutoTriageMode(p.AutoTriageMode) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid auto-triage mode %q",
			p.TeamID, p.AutoTriageMode)
	}
	if p.VerifyMaxFindings == 0 {
		p.VerifyMaxFindings = DefaultVerifyMaxFindings
	}
	if p.VerifyMaxFiles == 0 {
		p.VerifyMaxFiles = DefaultVerifyMaxFiles
	}
	if p.VerifyTokenShare == 0 {
		p.VerifyTokenShare = DefaultVerifyTokenShare
	}
	if p.VerifyTimeShare == 0 {
		p.VerifyTimeShare = DefaultVerifyTimeShare
	}
	if p.VerifyTokenMax == 0 {
		p.VerifyTokenMax = DefaultVerifyTokenMax
	}
	if p.VerifyMaxFindings < 0 || p.VerifyMaxFiles < 0 {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid verify limits %d/%d",
			p.TeamID, p.VerifyMaxFindings, p.VerifyMaxFiles)
	}
	if p.VerifyTokenShare < 0 || p.VerifyTokenShare > 100 ||
		p.VerifyTimeShare < 0 || p.VerifyTimeShare > 100 {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid verify shares %d/%d",
			p.TeamID, p.VerifyTokenShare, p.VerifyTimeShare)
	}
	if p.VerifyTokenMax < VerifyTokenMaxMin || p.VerifyTokenMax > ReviewTokenBudgetMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid verify token max %d",
			p.TeamID, p.VerifyTokenMax)
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
			"require_green_ci", "review_context_ceiling", "review_effort",
			"review_token_budget", "auto_triage_mode", "auto_withdraw",
			"verify_max_findings", "verify_max_files", "verify_token_share",
			"verify_time_share", "verify_token_max", "updated_at",
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
// with a team, a policy and an enabled profile, whose current comparison has no
// run in an active or terminal state. A run covers the pull request only when
// its head, base branch and resolved merge base match the current ones; legacy
// runs without a key still cover it once. Superseded runs never suppress a PR.
// Everything is filtered in SQL, including the limit and the ordering.
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
      AND (
        (rr.base_ref = '' AND rr.base_sha = '')
        OR ((rr.base_ref = '' OR rr.base_ref = pr.base_ref)
          AND rr.base_sha <> '' AND pr.merge_base_sha <> '' AND rr.base_sha = pr.merge_base_sha)
        OR (rr.base_ref <> '' AND rr.base_ref = pr.base_ref AND rr.base_sha = '' AND pr.merge_base_sha = '')
      )
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
			"status":       RunRunning,
			"started_at":   now,
			"heartbeat_at": now,
			"updated_at":   now,
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

// HeartbeatReviewRun records that the worker executing the run is alive. Only a
// running run accepts the write: ErrRunNotActive tells the worker that the
// watchdog or a new head already closed it, so it must stop the pipeline.
func (s *Store) HeartbeatReviewRun(ctx context.Context, id uint) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning).
		Update("heartbeat_at", time.Now().UTC())
	if res.Error != nil {
		return fmt.Errorf("heartbeat review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("heartbeat review run %d: %w", id, ErrRunNotActive)
	}
	return nil
}

// RequeueReviewRun returns a claimed run to the queue without finishing it, so
// a worker that found no profile capacity can retry it later. The attempt
// counter doubles as the number of times the run had to wait: it bounds the
// backoff and it is what the worker compares against its budget.
func (s *Store) RequeueReviewRun(ctx context.Context, id uint, errCode, errMsg string) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning).
		Updates(map[string]any{
			"status":           RunQueued,
			"attempt":          gorm.Expr("attempt + 1"),
			"error_code":       errCode,
			"error_message":    errMsg,
			"started_at":       nil,
			"phase":            "",
			"phase_started_at": nil,
			"updated_at":       time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("requeue review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("requeue review run %d: %w", id, ErrRunNotActive)
	}
	return nil
}

// QueuedCloneToken identifies a queued run whose clone token the control
// process must renew.
type QueuedCloneToken struct {
	RunID          uint   `gorm:"column:run_id"`
	InstallationID int64  `gorm:"column:installation_id"`
	Repository     string `gorm:"column:repository"`
}

// QueuedRunsForTokenRefresh returns queued runs whose clone token expires before
// deadline. Runs created before the expiry was stored have a nil expiry and are
// refreshed too: minting a token again is harmless and records the expiry.
func (s *Store) QueuedRunsForTokenRefresh(ctx context.Context, deadline time.Time, limit int) ([]QueuedCloneToken, error) {
	var runs []QueuedCloneToken
	err := s.DB.WithContext(ctx).Raw(`
		SELECT rr.id AS run_id, repo.installation_id, repo.full_name AS repository
		FROM review_runs rr
		JOIN pull_requests pr ON pr.id = rr.pull_request_id
		JOIN repositories repo ON repo.id = pr.repository_id
		WHERE rr.status = ?
		  AND (rr.clone_token_expires_at IS NULL OR rr.clone_token_expires_at < ?)
		ORDER BY rr.id
		LIMIT ?`, RunQueued, deadline.UTC(), limit).Scan(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("list queued runs needing a clone token: %w", err)
	}
	return runs, nil
}

// SetCloneToken replaces the sealed clone token of a queued run. It reports
// false when the run is no longer queued: the worker claimed it or a trigger
// superseded it, and that run's token must not be overwritten.
func (s *Store) SetCloneToken(ctx context.Context, id uint, sealed string, expiresAt time.Time) (bool, error) {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunQueued).
		Updates(map[string]any{
			"clone_token_enc":        sealed,
			"clone_token_expires_at": expiresAt.UTC(),
			"updated_at":             time.Now().UTC(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("set clone token of review run %d: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
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

// PreviousSucceededRun returns the latest succeeded run of a pull request
// before beforeRunID. It is the run whose head can seed an incremental review.
func (s *Store) PreviousSucceededRun(ctx context.Context, prID, beforeRunID uint) (ReviewRun, error) {
	var run ReviewRun
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND status = ? AND id < ?", prID, RunSucceeded, beforeRunID).
		Order("id DESC").Take(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRun{}, fmt.Errorf("previous succeeded review run of pull request %d before %d: %w", prID, beforeRunID, ErrNotFound)
	}
	if err != nil {
		return ReviewRun{}, fmt.Errorf("find previous succeeded review run of pull request %d before %d: %w", prID, beforeRunID, err)
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
			"status":           RunSuperseded,
			"clone_token_enc":  "",
			"phase":            "",
			"phase_started_at": nil,
			"updated_at":       time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// SupersedeOtherActiveRuns cancels queued and running runs of a pull request
// whose comparison differs from the given one, even when they share the head.
// The run currently being (re)enqueued is left untouched, so a trigger can
// never discard its own freshly created run. Legacy runs with empty key fields
// match any comparison. Superseded runs lose their clone token and never
// publish.
func (s *Store) SupersedeOtherActiveRuns(ctx context.Context, prID uint, comparison Comparison) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("pull_request_id = ? AND status IN ?", prID, []string{RunQueued, RunRunning}).
		Where(`NOT (
			head_sha = ? AND (
				(base_ref = '' AND base_sha = '')
				OR ((base_ref = '' OR base_ref = ?) AND base_sha <> '' AND ? <> '' AND base_sha = ?)
				OR (base_ref <> '' AND base_ref = ? AND base_sha = '' AND ? = '')
			)
		)`, comparison.HeadSHA, comparison.BaseRef, comparison.MergeBaseSHA,
			comparison.MergeBaseSHA, comparison.BaseRef, comparison.MergeBaseSHA).
		Updates(map[string]any{
			"status":           RunSuperseded,
			"clone_token_enc":  "",
			"phase":            "",
			"phase_started_at": nil,
			"updated_at":       time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("supersede other runs of pull request %d: %w", prID, res.Error)
	}
	return nil
}

// SetPRMergeBase records the resolved merge base of the pull request's current
// comparison. The update is conditional on the head, base branch and base tip
// still being the ones the resolution was made for, so a retarget or a
// force-push landing between the Compare call and this write cannot overwrite
// the newer comparison. It reports whether the row was updated; false means the
// comparison moved.
func (s *Store) SetPRMergeBase(ctx context.Context, prID uint, headSHA, baseRef, baseTipSHA, mergeBase string) (bool, error) {
	if mergeBase == "" {
		return false, nil
	}
	res := s.DB.WithContext(ctx).Model(&PullRequest{}).
		Where("id = ? AND head_sha = ? AND base_ref = ? AND base_sha = ?",
			prID, headSHA, baseRef, baseTipSHA).
		Where("merge_base_sha = '' OR merge_base_sha = ?", mergeBase).
		Updates(map[string]any{
			"merge_base_sha": mergeBase,
			"updated_at":     time.Now().UTC(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("set merge base of pull request %d: %w", prID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// RunUsage is the structured token consumption of one review run, summed over
// every model call. MaxContext is the largest prompt of the run; nil means the
// engine reported no sample, which is stored as NULL rather than a fake zero.
type RunUsage struct {
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Total      int64
	MaxContext *int64
}

// RunCompletion is the terminal state of a review run, persisted together with
// its findings in one transaction.
type RunCompletion struct {
	Status       string
	ErrorCode    string
	ErrorMessage string
	Summary      string
	Coverage     []byte
	Usage        []byte
	Tokens       RunUsage
	// TokensKnown reports whether an engine measured consumption. When false the
	// token columns are stored as NULL, not as a fake zero (#129).
	TokensKnown bool
	Findings    []Finding
	Resolved    []string
	// BaseSHA is the reviewed base of the run, used to decide whether the next
	// push can be reviewed incrementally. An empty value never overwrites the
	// base a run already carried.
	BaseSHA string
	// VerifySessionID and VerifyUsage are the second OCR pass's session and
	// consumption. Empty values mean no verification ran.
	VerifySessionID string
	VerifyUsage     []byte
}

// CompleteReviewRun stores a run's outcome in a single transaction: the run is
// finished only when it is still running, findings are upserted, resolved
// findings are marked, and the clone token is discarded. Any error rolls the
// whole set back. A run superseded while executing returns ErrRunNotActive and
// persists nothing, so stale results can never be published.
func (s *Store) CompleteReviewRun(ctx context.Context, id uint, c RunCompletion) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		coverage, usage := c.Coverage, c.Usage
		if len(coverage) == 0 {
			coverage = []byte(`{}`)
		}
		if len(usage) == 0 {
			usage = []byte(`{}`)
		}
		now := time.Now().UTC()
		// An untyped nil interface makes the column NULL; a typed nil pointer
		// would depend on how the driver treats it. Without an engine result the
		// whole set stays NULL, so aggregates skip the run instead of counting a
		// fake zero (#129).
		var input, output, reasoning, cacheRead, cacheWrite, total any
		if c.TokensKnown {
			input, output, reasoning = c.Tokens.Input, c.Tokens.Output, c.Tokens.Reasoning
			cacheRead, cacheWrite, total = c.Tokens.CacheRead, c.Tokens.CacheWrite, c.Tokens.Total
		}
		var maxContext any
		if c.TokensKnown && c.Tokens.MaxContext != nil {
			maxContext = *c.Tokens.MaxContext
		}
		var verifyUsage any
		if len(c.VerifyUsage) > 0 {
			verifyUsage = datatypes.JSON(c.VerifyUsage)
		}
		updates := map[string]any{
			"status":             c.Status,
			"error_code":         c.ErrorCode,
			"error_message":      c.ErrorMessage,
			"summary":            c.Summary,
			"coverage":           datatypes.JSON(coverage),
			"usage":              datatypes.JSON(usage),
			"input_tokens":       input,
			"output_tokens":      output,
			"reasoning_tokens":   reasoning,
			"cache_read_tokens":  cacheRead,
			"cache_write_tokens": cacheWrite,
			"total_tokens":       total,
			"max_context_tokens": maxContext,
			"verify_session_id":  c.VerifySessionID,
			"verify_usage":       verifyUsage,
			"clone_token_enc":    "",
			// A finished run has no phase: keeping the last one would make the
			// panel show a step that is no longer running.
			"phase":            "",
			"phase_started_at": nil,
			"finished_at":      now,
			"updated_at":       now,
		}
		// Only a resolved base replaces the column: an early failure has no base
		// and must not erase one the run already knew.
		if c.BaseSHA != "" {
			updates["base_sha"] = c.BaseSHA
		}
		res := tx.Model(&ReviewRun{}).
			Where("id = ? AND status = ?", id, RunRunning).
			Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("complete review run %d: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("complete review run %d: %w", id, ErrRunNotActive)
		}

		// A person who triaged the finding after this run began decided later
		// than the result being stored now, so the run must not overwrite them.
		var runStarted time.Time
		if err := tx.Raw(
			"SELECT COALESCE(started_at, created_at) FROM review_runs WHERE id = ?", id).
			Scan(&runStarted).Error; err != nil {
			return fmt.Errorf("read start of review run %d: %w", id, err)
		}

		for _, f := range c.Findings {
			if _, err := upsertFinding(tx, f, runStarted); err != nil {
				return err
			}
		}
		for _, findingID := range c.Resolved {
			if err := updateFindingStatus(tx, findingID, FindingResolved, id, runStarted); err != nil {
				return err
			}
		}
		return nil
	})
}

// StaleRunPolicy decides which running rows the watchdog closes and the message
// each case leaves behind. HeartbeatBefore is the oldest heartbeat considered
// alive: older silence means the worker is gone, not that the review overran.
type StaleRunPolicy struct {
	StartedBefore     time.Time
	HeartbeatBefore   time.Time
	OverranMessage    string
	LostWorkerMessage string
}

// InterruptStaleRuns fails every running run whose start is older than the
// policy cutoff and returns the runs it moved. A fresh heartbeat classifies the
// run as overrun; silence classifies it as a lost worker. Queued and terminal
// runs are never touched, and the single UPDATE keeps concurrent sweeps exact:
// row locks serialize them and the losers see zero rows, which keeps the count
// exact and each run interrupted once.
func (s *Store) InterruptStaleRuns(ctx context.Context, p StaleRunPolicy) ([]ReviewRun, error) {
	now := time.Now().UTC()
	// A running run always has started_at; the COALESCE keeps a row claimed
	// without one from staying stranded forever.
	const query = `
UPDATE review_runs
SET status = ?, error_code = ?, error_message = CASE
        WHEN heartbeat_at IS NULL OR heartbeat_at < ? THEN ?
        ELSE ? END,
    clone_token_enc = '', phase = '', phase_started_at = NULL, finished_at = ?, updated_at = ?
WHERE status = ? AND COALESCE(started_at, created_at) < ?
RETURNING *`

	var runs []ReviewRun
	err := s.DB.WithContext(ctx).Raw(query,
		RunFailed, RunErrorInterrupted,
		p.HeartbeatBefore, p.LostWorkerMessage, p.OverranMessage,
		now, now, RunRunning, p.StartedBefore,
	).Scan(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("interrupt review runs started before %s: %w",
			p.StartedBefore.UTC().Format(time.RFC3339), err)
	}
	return runs, nil
}

// SetReviewRunPhase records the current step of a running run and when it
// began. A queued, finished or superseded run rejects the write with
// ErrRunNotActive so a late phase cannot resurrect a terminal row.
func (s *Store) SetReviewRunPhase(ctx context.Context, id uint, phase string) error {
	now := time.Now().UTC()
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning).
		Updates(map[string]any{
			"phase":            phase,
			"phase_started_at": now,
			"updated_at":       now,
		})
	if res.Error != nil {
		return fmt.Errorf("set phase of review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set phase of review run %d: %w", id, ErrRunNotActive)
	}
	return nil
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
	return upsertFinding(s.DB.WithContext(ctx), f, time.Time{})
}

// upsertFinding refreshes a finding re-detected by a run. When runStarted is
// set, a finding whose status an authorized thread event decided after the run
// began is left untouched: the run must not overwrite that decision.
func upsertFinding(db *gorm.DB, f Finding, runStarted time.Time) (Finding, error) {
	if f.IdentityVersion == 0 {
		f.IdentityVersion = IdentityVersionCurrent
	}

	// A row written before #144 keeps its ID and references, but its fingerprint
	// belongs to the old scheme. Migrating it in place is the only way the next
	// run can match it by the new identity without re-keying comments or threads.
	legacy, ok, err := legacyFindingByID(db, f)
	if err != nil {
		return Finding{}, err
	}
	if ok {
		return migrateLegacyFinding(db, legacy, f, runStarted)
	}

	// A verifier dismissal survives while the cited code and range stay the
	// same; reopening it on every run would undo the second pass. A human
	// verdict is never discarded here.
	if err := db.Model(&Finding{}).
		Where("pull_request_id = ? AND fingerprint = ? AND triage_source = ?",
			f.PullRequestID, f.Fingerprint, TriageSourceVerifier).
		Where("existing_code <> ? OR start_line <> ? OR end_line <> ?",
			f.ExistingCode, f.StartLine, f.EndLine).
		Updates(map[string]any{
			"triage_verdict": "", "triaged_by": "", "triaged_at": nil, "triage_source": "",
		}).Error; err != nil {
		return Finding{}, fmt.Errorf("clear stale verifier triage for %q: %w", f.ID, err)
	}

	// A run that started before an authorized thread decision must not refresh
	// the row it decided on, not even its status.
	conflictWhere := clause.Expr{
		SQL: "(findings.triage_verdict = '' OR findings.existing_code <> EXCLUDED.existing_code" +
			" OR findings.start_line <> EXCLUDED.start_line OR findings.end_line <> EXCLUDED.end_line)",
	}
	if !runStarted.IsZero() {
		conflictWhere.SQL += " AND (findings.github_thread_decision_at IS NULL" +
			" OR findings.github_thread_decision_at <= ?)"
		conflictWhere.Vars = []any{runStarted}
	}
	res := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "fingerprint"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"path", "start_line", "end_line", "severity", "category", "title",
			"body", "suggestion", "confidence", "existing_code", "status",
			"identity_version", "last_seen_run_id", "updated_at",
		}),
		Where: clause.Where{Exprs: []clause.Expression{conflictWhere}},
	}).Create(&f)
	if res.Error != nil {
		return Finding{}, fmt.Errorf("upsert finding %q: %w", f.ID, res.Error)
	}

	var saved Finding
	err = quietDB(db).Where("pull_request_id = ? AND fingerprint = ?", f.PullRequestID, f.Fingerprint).
		Take(&saved).Error
	if err != nil {
		return Finding{}, fmt.Errorf("reload finding %q: %w", f.ID, err)
	}
	return saved, nil
}

// legacyFindingByID returns the version-1 row that owns f.ID in the same pull
// request, when there is one. Only a current-scheme upsert can migrate it:
// keeping the legacy fingerprint would make every future run treat the row as
// a new finding.
func legacyFindingByID(db *gorm.DB, f Finding) (Finding, bool, error) {
	if f.ID == "" || f.IdentityVersion < IdentityVersionCurrent {
		return Finding{}, false, nil
	}
	var legacy Finding
	err := quietDB(db).
		Where("id = ? AND pull_request_id = ? AND identity_version < ?",
			f.ID, f.PullRequestID, IdentityVersionCurrent).
		Take(&legacy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Finding{}, false, nil
	}
	if err != nil {
		return Finding{}, false, fmt.Errorf("find legacy finding %q: %w", f.ID, err)
	}
	return legacy, true, nil
}

func migrateLegacyFinding(db *gorm.DB, legacy, f Finding, runStarted time.Time) (Finding, error) {
	now := time.Now().UTC()
	// The verifier rule is the same as the normal upsert, keyed by ID because the
	// legacy row still carries its old fingerprint.
	if err := db.Model(&Finding{}).
		Where("id = ? AND triage_source = ?", legacy.ID, TriageSourceVerifier).
		Where("existing_code <> ? OR start_line <> ? OR end_line <> ?",
			f.ExistingCode, f.StartLine, f.EndLine).
		Updates(map[string]any{
			"triage_verdict": "", "triaged_by": "", "triaged_at": nil, "triage_source": "",
		}).Error; err != nil {
		return Finding{}, fmt.Errorf("clear stale verifier triage for %q: %w", f.ID, err)
	}

	// The identity migrates unless another row of the same PR already owns the
	// target fingerprint. Taking it would violate the unique index and abort the
	// whole run, so in that case the row stays on the legacy scheme; its ID and
	// references are still kept and its contents are refreshed below.
	if err := db.Model(&Finding{}).Where("id = ?", legacy.ID).
		Where("NOT EXISTS (SELECT 1 FROM findings other "+
			"WHERE other.pull_request_id = ? AND other.fingerprint = ? AND other.id <> ?)",
			f.PullRequestID, f.Fingerprint, legacy.ID).
		Updates(map[string]any{
			"fingerprint":      f.Fingerprint,
			"identity_version": IdentityVersionCurrent,
			"updated_at":       now,
		}).Error; err != nil {
		return Finding{}, fmt.Errorf("migrate finding %q identity: %w", f.ID, err)
	}

	q := db.Model(&Finding{}).Where("id = ?", legacy.ID).
		Where("(triage_verdict = '' OR existing_code <> ? OR start_line <> ? OR end_line <> ?)",
			f.ExistingCode, f.StartLine, f.EndLine)
	if !runStarted.IsZero() {
		q = q.Where("(github_thread_decision_at IS NULL OR github_thread_decision_at <= ?)", runStarted)
	}
	res := q.Updates(map[string]any{
		"path": f.Path, "start_line": f.StartLine, "end_line": f.EndLine,
		"severity": f.Severity, "category": f.Category, "title": f.Title,
		"body": f.Body, "suggestion": f.Suggestion, "confidence": f.Confidence,
		"existing_code": f.ExistingCode, "status": f.Status,
		"last_seen_run_id": f.LastSeenRunID, "updated_at": now,
	})
	if res.Error != nil {
		return Finding{}, fmt.Errorf("migrate finding %q contents: %w", f.ID, res.Error)
	}

	var saved Finding
	if err := quietDB(db).Where("id = ?", legacy.ID).Take(&saved).Error; err != nil {
		return Finding{}, fmt.Errorf("reload finding %q: %w", f.ID, err)
	}
	return saved, nil
}

// RecordFindingVerification stores the verifier's answer for one finding
// without touching its status. Every decision keeps its evidence, whether it
// was applied or not.
func (s *Store) RecordFindingVerification(ctx context.Context, findingID string, verification []byte) error {
	res := s.DB.WithContext(ctx).Model(&Finding{}).Where("id = ?", findingID).
		Updates(map[string]any{
			"verification": datatypes.JSON(verification),
			"updated_at":   time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("record verification of finding %q: %w", findingID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record verification of finding %q: %w", findingID, ErrNotFound)
	}
	return nil
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

	var findings []Finding
	err := q.Order("id").Limit(2).Find(&findings).Error
	if err != nil {
		return Finding{}, fmt.Errorf("find finding for review thread: %w", err)
	}
	switch len(findings) {
	case 0:
		return Finding{}, fmt.Errorf("finding for review thread: %w", ErrNotFound)
	case 1:
		return findings[0], nil
	default:
		return Finding{}, fmt.Errorf("finding for review thread: %w", ErrFindingThreadAmbiguous)
	}
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
	return updateFindingStatus(s.DB.WithContext(ctx), id, status, runID, time.Time{})
}

// updateFindingStatus applies a lifecycle transition on behalf of a run. When
// runStarted is not zero, a human triage or an authorized thread decision
// recorded after the run began wins: that is the concurrent decision the
// finishing run must not overwrite.
func updateFindingStatus(db *gorm.DB, id, status string, runID uint, runStarted time.Time) error {
	q := db.Model(&Finding{}).Where("id = ?", id)
	if !runStarted.IsZero() {
		q = q.Where("NOT (triage_source = ? AND triaged_at IS NOT NULL AND triaged_at > ?)",
			TriageSourceHuman, runStarted)
		q = q.Where("(github_thread_decision_at IS NULL OR github_thread_decision_at <= ?)", runStarted)
	}
	res := q.Updates(map[string]any{
		"status":           status,
		"last_seen_run_id": runID,
		"updated_at":       time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("update finding %q status: %w", id, res.Error)
	}
	if res.RowsAffected > 0 {
		return nil
	}

	var count int64
	if err := db.Model(&Finding{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return fmt.Errorf("find finding %q: %w", id, err)
	}
	if count == 0 {
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

// SetFindingThreadResolved records the thread state the App's own GraphQL
// mutation produced, so the echo of that mutation can be ignored without
// losing the state. It never touches the triage verdict.
func (s *Store) SetFindingThreadResolved(ctx context.Context, id string, resolved bool) error {
	res := s.DB.WithContext(ctx).Model(&Finding{}).Where("id = ?", id).Updates(map[string]any{
		"github_thread_resolved": resolved,
		"updated_at":             time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("set finding %q thread state: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set finding %q thread state: %w", id, ErrNotFound)
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
	// DeliveryID is GitHub's X-GitHub-Delivery id. Recording it makes a
	// redelivered thread event a no-op instead of flipping the state back.
	DeliveryID string
	// Source is who took the decision: TriageSourceHuman when empty, or
	// TriageSourceVerifier when the second OCR pass triages.
	Source string
}

// TriageFinding records a panel decision on a finding: status is FindingOpen
// for "confirmed" and FindingWithdrawn for "false positive". Confirming an
// already open finding is a real change because it records who vouched for it.
// It reports whether the status moved, which is what tells the caller to touch
// the GitHub thread. Repeating the same decision by the same person is a no-op
// and is not audited twice. A resolved finding is no longer QA's decision and
// returns ErrFindingResolved.
func (s *Store) TriageFinding(ctx context.Context, id, status string, entry FindingAudit) (statusChanged bool, err error) {
	verdict := VerdictConfirmed
	if status == FindingWithdrawn {
		verdict = VerdictFalsePositive
	}
	source := entry.Source
	if source == "" {
		source = TriageSourceHuman
	}
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Finding
		err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("triage finding %q: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("find finding %q: %w", id, err)
		}
		if current.Status == FindingResolved {
			return fmt.Errorf("triage finding %q: %w", id, ErrFindingResolved)
		}
		if current.TriageSource == TriageSourceHuman && source == TriageSourceVerifier {
			return fmt.Errorf("triage finding %q: %w", id, ErrFindingHumanTriaged)
		}
		if current.Status == status && current.TriageVerdict == verdict &&
			current.TriagedBy == entry.Actor && current.TriageSource == source {
			return nil
		}

		now := time.Now().UTC()
		err = tx.Model(&Finding{}).Where("id = ?", id).Updates(map[string]any{
			"status":         status,
			"triage_verdict": verdict,
			"triaged_by":     entry.Actor,
			"triaged_at":     now,
			"triage_source":  source,
			"updated_at":     now,
		}).Error
		if err != nil {
			return fmt.Errorf("triage finding %q: %w", id, err)
		}
		entry.Source = source
		if err := writeFindingAudit(tx, id, entry); err != nil {
			return err
		}
		statusChanged = current.Status != status
		return nil
	})
	if err != nil {
		return false, err
	}
	return statusChanged, nil
}

// ThreadSync describes one pull_request_review_thread delivery for a finding.
type ThreadSync struct {
	Resolved bool
	// Status is the finding state the thread event means. It is applied only
	// when ApplyStatus is true.
	Status string
	// ApplyStatus is false when the sender lacks triage permission or is the
	// App echoing its own mutation: only the thread state is recorded. It is
	// also ignored when a person already triaged the finding, so a late thread
	// event never overrides them.
	ApplyStatus bool
	// EventAt is the payload's updated_at. A delivery at or before the last
	// applied payload timestamp is ignored, so a delayed event cannot flip the
	// state back.
	EventAt time.Time
	Audit   FindingAudit
}

// SyncFindingThread records GitHub's thread state for a finding and, when the
// sender is authorized and no person has triaged it, applies the matching
// status transition and audit row in one transaction. Older deliveries and
// deliveries already applied are no-ops, which keeps redeliveries, echoes and
// out-of-order events from flipping the state back.
func (s *Store) SyncFindingThread(ctx context.Context, id string, in ThreadSync) (changed, statusChanged bool, err error) {
	// PostgreSQL keeps microseconds; normalizing here makes a repeated delivery
	// compare equal to the timestamp already stored.
	eventAt := in.EventAt.Truncate(time.Microsecond)
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Finding
		err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("sync finding %q thread: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("find finding %q: %w", id, err)
		}

		// The payload timestamp orders events even when a late delivery was
		// never seen before, which delivery-id deduplication cannot detect.
		// GitHub's updated_at has second precision, so an equal timestamp is
		// ambiguous and must not flip the state either.
		if !eventAt.IsZero() && current.GithubThreadEventAt != nil &&
			!eventAt.After(*current.GithubThreadEventAt) {
			return nil
		}
		if in.Audit.DeliveryID != "" {
			var applied int64
			if err := tx.Model(&AuditLog{}).
				Where("target_type = ? AND target_id = ? AND data->>'delivery_id' = ?",
					"finding", id, in.Audit.DeliveryID).
				Count(&applied).Error; err != nil {
				return fmt.Errorf("look up thread delivery %q: %w", in.Audit.DeliveryID, err)
			}
			if applied > 0 {
				return nil
			}
		}

		now := time.Now().UTC()
		updates := map[string]any{}
		if !eventAt.IsZero() &&
			(current.GithubThreadEventAt == nil || eventAt.After(*current.GithubThreadEventAt)) {
			updates["github_thread_event_at"] = eventAt
		}
		if in.Audit.ThreadID != "" && current.GithubThreadID == "" {
			updates["github_thread_id"] = in.Audit.ThreadID
		}
		if current.GithubThreadResolved != in.Resolved {
			updates["github_thread_resolved"] = in.Resolved
		}
		applyStatus := in.ApplyStatus && in.Status != "" && current.TriageSource != TriageSourceHuman
		if applyStatus && current.Status != in.Status {
			updates["status"] = in.Status
			decisionAt := eventAt
			if decisionAt.IsZero() {
				decisionAt = now
			}
			updates["github_thread_decision_at"] = decisionAt
			statusChanged = true
		}
		if len(updates) == 0 {
			return nil
		}
		updates["updated_at"] = now
		if err := tx.Model(&Finding{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return fmt.Errorf("sync finding %q thread: %w", id, err)
		}
		changed = true
		if statusChanged {
			if err := writeFindingAudit(tx, id, in.Audit); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return changed, statusChanged, nil
}

func writeFindingAudit(tx *gorm.DB, findingID string, entry FindingAudit) error {
	data := map[string]any{"actor": entry.Actor}
	if entry.Source != "" {
		data["source"] = entry.Source
	}
	if entry.CommentID != 0 {
		data["comment_id"] = entry.CommentID
	}
	if entry.Reason != "" {
		data["reason"] = entry.Reason
	}
	if entry.ThreadID != "" {
		data["thread_id"] = entry.ThreadID
	}
	if entry.DeliveryID != "" {
		data["delivery_id"] = entry.DeliveryID
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	row := AuditLog{
		ActorUserID: entry.ActorID,
		Action:      entry.Action,
		TargetType:  "finding",
		TargetID:    findingID,
		Data:        datatypes.JSON(raw),
	}
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	return nil
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
