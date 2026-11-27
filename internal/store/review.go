package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// ErrRunLeaseLost is returned when a run is still running but owned by
	// another execution: the previous worker recovered it after its lease
	// expired, so this one must stop writing (#228).
	ErrRunLeaseLost = errors.New("review run lease lost")
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
	// ErrStaleHealthCheck is returned when a health check finishes after the
	// profile's credentials, endpoint or model changed: its verdict describes a
	// configuration that no longer exists and must not be recorded.
	ErrStaleHealthCheck = errors.New("health check predates a profile change")
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

	// Architecture analysis has a budget of its own, separate from the file
	// review and the verifier. The shares bound it when the run has a token or
	// time budget; the absolute maximum keeps it small when there is none.
	DefaultArchitectureTokenShare = 25
	DefaultArchitectureTimeShare  = 20
	DefaultArchitectureTokenMax   = 100000
	ArchitectureTokenMaxMin       = 10000

	// DefaultArchitectureBaselinePath is the trusted rule file resolved from
	// the base commit. The exception policy defaults to deny so an exception
	// only applies when an administrator opts in.
	DefaultArchitectureBaselinePath    = ".architecture/baseline.json"
	DefaultArchitectureExceptionPolicy = ArchitectureExceptionsDeny
	// Consumer lookups are bounded independently of the review budget. The
	// allow-list is small by design; the byte cap keeps a single vendored
	// contract from dominating the run.
	DefaultConsumerMaxLookups = 8
	ConsumerMaxLookupsMin     = 1
	ConsumerMaxLookupsMax     = 50
	DefaultConsumerMaxBytes   = 1 << 20
	ConsumerMaxBytesMin       = 4096
	ConsumerMaxBytesMax       = 16 << 20

	ConsumerVisibilityPublic  = "public"
	ConsumerVisibilityPrivate = "private"
)

// ConsumerRef is one authorized producer–consumer dependency in a review
// policy. Repository, Contract and Ref identify what to read; Path is the
// consumer-side file that carries the pinned contract and defaults to Contract.
// Ref may be a tag or a full commit SHA, but a branch is a moving ref: the
// lookup resolves and records the observed commit SHA in the verdict, so the
// evidence is reproducible even though a later run may see a different tip.
// Visibility defaults to private: a consumer is only published by name when the
// team explicitly declares it public and the reviewed repository is public too.
type ConsumerRef struct {
	Repository     string `json:"repository"`
	Contract       string `json:"contract"`
	Path           string `json:"path,omitempty"`
	Ref            string `json:"ref"`
	InstallationID int64  `json:"installation_id,omitempty"`
	Essential      bool   `json:"essential,omitempty"`
	Visibility     string `json:"visibility,omitempty"`
	Version        string `json:"version,omitempty"`
}

// ConsumerIsPrivate reports whether the consumer's details may not be
// published in a pull request. An undeclared visibility is private, so a
// forgotten field never leaks another repository.
func ConsumerIsPrivate(ref ConsumerRef) bool {
	return strings.ToLower(strings.TrimSpace(ref.Visibility)) != ConsumerVisibilityPublic
}

// ValidConsumerVisibility reports whether visibility is empty, public or
// private. Empty keeps the fail-closed private default.
func ValidConsumerVisibility(visibility string) bool {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "", ConsumerVisibilityPublic, ConsumerVisibilityPrivate:
		return true
	default:
		return false
	}
}

// ParseConsumerAllowList decodes the policy column. An empty or null value is
// an empty list, not an error.
func ParseConsumerAllowList(raw datatypes.JSON) ([]ConsumerRef, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var refs []ConsumerRef
	if err := json.Unmarshal(trimmed, &refs); err != nil {
		return nil, fmt.Errorf("decode consumer allow list: %w", err)
	}
	return refs, nil
}

// NormalizeConsumerAllowList validates and canonicalizes the allow-list. It
// rejects duplicate repository/contract pairs instead of guessing which entry
// the team meant.
func NormalizeConsumerAllowList(refs []ConsumerRef) (datatypes.JSON, error) {
	normalized := make([]ConsumerRef, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		ref.Repository = strings.TrimSpace(ref.Repository)
		ref.Contract = normalizePolicyPath(ref.Contract)
		ref.Path = normalizePolicyPath(ref.Path)
		ref.Ref = strings.TrimSpace(ref.Ref)
		ref.Visibility = strings.ToLower(strings.TrimSpace(ref.Visibility))
		ref.Version = strings.TrimSpace(ref.Version)
		if ref.Path == "" {
			ref.Path = ref.Contract
		}
		if !validConsumerRepository(ref.Repository) {
			return nil, fmt.Errorf("invalid consumer repository %q", ref.Repository)
		}
		if !validConsumerPath(ref.Contract) {
			return nil, fmt.Errorf("invalid consumer contract %q", ref.Contract)
		}
		if !validConsumerPath(ref.Path) {
			return nil, fmt.Errorf("invalid consumer path %q", ref.Path)
		}
		if ref.Ref == "" {
			return nil, fmt.Errorf("consumer %s has no pinned ref", ref.Repository)
		}
		if ref.InstallationID < 0 {
			return nil, fmt.Errorf("consumer %s has an invalid installation id", ref.Repository)
		}
		if !ValidConsumerVisibility(ref.Visibility) {
			return nil, fmt.Errorf("consumer %s has an invalid visibility %q", ref.Repository, ref.Visibility)
		}
		if ref.Visibility == "" {
			ref.Visibility = ConsumerVisibilityPrivate
		}
		key := ref.Repository + "\x00" + ref.Contract
		if seen[key] {
			return nil, fmt.Errorf("consumer %s for contract %s is duplicated", ref.Repository, ref.Contract)
		}
		seen[key] = true
		normalized = append(normalized, ref)
	}
	if normalized == nil {
		normalized = []ConsumerRef{}
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode consumer allow list: %w", err)
	}
	return datatypes.JSON(encoded), nil
}

// ValidConsumerBaselinePath reports whether path can be read from the base
// tree. It must be repository-relative and cannot escape the checkout.
func ValidConsumerBaselinePath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	normalized := normalizePolicyPath(path)
	if normalized == "" || normalized != path {
		return false
	}
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, `\`) {
		return false
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}

func normalizePolicyPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, `\`, "/"))
	path = strings.TrimPrefix(path, "./")
	path = strings.TrimPrefix(path, "/")
	return path
}

func validConsumerRepository(fullName string) bool {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, " \t\r\n") {
			return false
		}
	}
	return true
}

func validConsumerPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
		return false
	}
	if strings.ContainsAny(path, "*?[]") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

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

// ValidArchitectureAnalysisMode reports whether mode is one the worker
// understands. Observation is the only active mode on this branch.
func ValidArchitectureAnalysisMode(mode string) bool {
	switch mode {
	case ArchitectureAnalysisOff, ArchitectureAnalysisObservation:
		return true
	default:
		return false
	}
}

// ValidArchitectureExceptionPolicy reports whether the baseline exception
// policy is known. Unknown values are rejected instead of silently allowing
// or denying exceptions.
func ValidArchitectureExceptionPolicy(policy string) bool {
	switch policy {
	case ArchitectureExceptionsDeny, ArchitectureExceptionsBaseline:
		return true
	default:
		return false
	}
}

// ReviewConfig is the complete configuration that applies to one pull request.
type ReviewConfig struct {
	PR   PullRequest
	Repo Repository
	// Policy is the policy the run enforces. While the repository observes its
	// overrides, this is the inherited team policy.
	Policy QAPolicy
	// EffectivePolicy is the resolved policy with the repository overrides
	// applied, whether or not they are being enforced (#153).
	EffectivePolicy   QAPolicy
	PolicyRevision    string
	PolicyObservation bool
	Profile           LLMProfile
	Installation      Installation
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

// ProfileConfigRevision fingerprints the fields that decide where a request
// goes and which credential signs it. A health check captures it at start and
// records its verdict only while it still matches, so a rotation during the
// probe cannot publish a verdict for the previous configuration.
func ProfileConfigRevision(p LLMProfile) string {
	baseURL, variant := "", ""
	if p.BaseURL != nil {
		baseURL = *p.BaseURL
	}
	if p.Variant != nil {
		variant = *p.Variant
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strconv.FormatUint(uint64(p.ID), 10),
		p.Provider, p.Model, variant, p.Mode, baseURL, p.APIKeyEnc,
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

// UpdateLLMProfile replaces the editable fields of a profile. APIKeyEnc is the
// worker-sealed value; an empty value keeps the stored ciphertext instead of
// wiping it. Changing the provider, model, variant, mode, base URL or key
// invalidates the stored health verdict, because it was measured against a
// destination that no longer exists.
func (s *Store) UpdateLLMProfile(ctx context.Context, p LLMProfile) error {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current LLMProfile
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current, p.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load llm profile %d: %w", p.ID, err)
		}

		keyEnc := current.APIKeyEnc
		updates := map[string]any{
			"name":               p.Name,
			"provider":           p.Provider,
			"model":              p.Model,
			"variant":            p.Variant,
			"mode":               p.Mode,
			"base_url":           p.BaseURL,
			"concurrency":        p.Concurrency,
			"enabled":            p.Enabled,
			"allow_private_data": p.AllowPrivateData,
			"updated_at":         time.Now().UTC(),
		}
		if p.APIKeyEnc != "" {
			keyEnc = p.APIKeyEnc
			updates["api_key_enc"] = p.APIKeyEnc
		}
		changed := current.Provider != p.Provider ||
			current.Model != p.Model ||
			derefOrEmpty(current.Variant) != derefOrEmpty(p.Variant) ||
			current.Mode != p.Mode ||
			derefOrEmpty(current.BaseURL) != derefOrEmpty(p.BaseURL) ||
			current.APIKeyEnc != keyEnc
		if changed {
			updates["health_status"] = HealthUnknown
			updates["health_checked_at"] = nil
			updates["health_error"] = ""
		}

		res := tx.Model(&LLMProfile{}).Where("id = ?", p.ID).Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("update llm profile %d: %w", p.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("update llm profile %d: %w", p.ID, err)
		}
		return err
	}
	return nil
}

func derefOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// DeleteLLMProfile removes a profile unless a QA policy still references it.
// The check and the delete run in one transaction so a policy cannot be added
// between them.
func (s *Store) DeleteLLMProfile(ctx context.Context, id uint) error {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var policies int64
		if err := tx.Model(&QAPolicy{}).
			Where("llm_profile_id = ? OR fallback_profile_ids @> ?::jsonb", id, fmt.Sprintf("[%d]", id)).
			Count(&policies).Error; err != nil {
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
	return s.RecordLLMHealthAt(ctx, id, "", status, message, usage)
}

// RecordLLMHealthAt is RecordLLMHealth with a configuration guard: when
// expectedRevision is not empty, the verdict is discarded with
// ErrStaleHealthCheck if the profile changed while the check ran. The row is
// locked, so a concurrent UpdateLLMProfile cannot slip between the comparison
// and the write.
func (s *Store) RecordLLMHealthAt(ctx context.Context, id uint, expectedRevision, status, message string, usage *HealthUsage) error {
	checkedAt := time.Now().UTC()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current LLMProfile
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("set llm profile %d health: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("load llm profile %d: %w", id, err)
		}
		if expectedRevision != "" && ProfileConfigRevision(current) != expectedRevision {
			return ErrStaleHealthCheck
		}

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

// FallbackProfileIDs decodes the ordered fallback destinations of a policy. An
// absent or invalid list yields no fallback.
func FallbackProfileIDs(p QAPolicy) []uint {
	if len(bytes.TrimSpace(p.FallbackProfileIDs)) == 0 {
		return nil
	}
	var ids []uint
	if err := json.Unmarshal(p.FallbackProfileIDs, &ids); err != nil {
		return nil
	}
	return ids
}

// NormalizeFallbackProfileIDs encodes an ordered, explicitly authorized list:
// zero IDs are dropped and a repeated destination keeps its first position, so
// the stored order is the one the worker will try.
func NormalizeFallbackProfileIDs(ids []uint) (datatypes.JSON, error) {
	kept := make([]uint, 0, len(ids))
	seen := make(map[uint]bool, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		kept = append(kept, id)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, fmt.Errorf("encode fallback profiles: %w", err)
	}
	return datatypes.JSON(encoded), nil
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
	if p.VerifyTokenShare < 0 || p.VerifyTokenShare >= 100 ||
		p.VerifyTimeShare < 0 || p.VerifyTimeShare >= 100 {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid verify shares %d/%d",
			p.TeamID, p.VerifyTokenShare, p.VerifyTimeShare)
	}
	if p.VerifyTokenMax < VerifyTokenMaxMin || p.VerifyTokenMax > ReviewTokenBudgetMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid verify token max %d",
			p.TeamID, p.VerifyTokenMax)
	}
	fallbacks, err := NormalizeFallbackProfileIDs(FallbackProfileIDs(p))
	if err != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, err)
	}
	p.FallbackProfileIDs = fallbacks
	if p.ArchitectureAnalysisMode == "" {
		p.ArchitectureAnalysisMode = ArchitectureAnalysisObservation
	}
	if !ValidArchitectureAnalysisMode(p.ArchitectureAnalysisMode) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid architecture analysis mode %q",
			p.TeamID, p.ArchitectureAnalysisMode)
	}
	if p.ArchitectureTokenShare == 0 {
		p.ArchitectureTokenShare = DefaultArchitectureTokenShare
	}
	if p.ArchitectureTimeShare == 0 {
		p.ArchitectureTimeShare = DefaultArchitectureTimeShare
	}
	if p.ArchitectureTokenMax == 0 {
		p.ArchitectureTokenMax = DefaultArchitectureTokenMax
	}
	if p.ArchitectureTokenShare < 0 || p.ArchitectureTokenShare > 90 ||
		p.ArchitectureTimeShare < 0 || p.ArchitectureTimeShare > 90 {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid architecture shares %d/%d",
			p.TeamID, p.ArchitectureTokenShare, p.ArchitectureTimeShare)
	}
	if p.ArchitectureTokenMax < ArchitectureTokenMaxMin || p.ArchitectureTokenMax > ReviewTokenBudgetMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid architecture token max %d",
			p.TeamID, p.ArchitectureTokenMax)
	}
	if p.ArchitectureBaselinePath == "" {
		p.ArchitectureBaselinePath = DefaultArchitectureBaselinePath
	}
	if p.ArchitectureExceptionPolicy == "" {
		p.ArchitectureExceptionPolicy = DefaultArchitectureExceptionPolicy
	}
	if !ValidArchitectureExceptionPolicy(p.ArchitectureExceptionPolicy) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid architecture exception policy %q",
			p.TeamID, p.ArchitectureExceptionPolicy)
	}
	if p.ConsumerMaxLookups == 0 {
		p.ConsumerMaxLookups = DefaultConsumerMaxLookups
	}
	if p.ConsumerMaxBytes == 0 {
		p.ConsumerMaxBytes = DefaultConsumerMaxBytes
	}
	if p.ConsumerMaxLookups < ConsumerMaxLookupsMin || p.ConsumerMaxLookups > ConsumerMaxLookupsMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid consumer max lookups %d",
			p.TeamID, p.ConsumerMaxLookups)
	}
	if p.ConsumerMaxBytes < ConsumerMaxBytesMin || p.ConsumerMaxBytes > ConsumerMaxBytesMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid consumer max bytes %d",
			p.TeamID, p.ConsumerMaxBytes)
	}
	p.ConsumerBaselinePath = strings.TrimSpace(p.ConsumerBaselinePath)
	if !ValidConsumerBaselinePath(p.ConsumerBaselinePath) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid consumer baseline path %q",
			p.TeamID, p.ConsumerBaselinePath)
	}
	consumers, parseErr := ParseConsumerAllowList(p.ConsumerAllowList)
	if parseErr != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, parseErr)
	}
	normalizedConsumers, normalizeErr := NormalizeConsumerAllowList(consumers)
	if normalizeErr != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, normalizeErr)
	}
	p.ConsumerAllowList = normalizedConsumers
	globs := bytes.TrimSpace(p.ExcludeGlobs)
	if len(globs) == 0 || bytes.Equal(globs, []byte("null")) {
		p.ExcludeGlobs = datatypes.JSON([]byte(`[]`))
	}
	critGlobs, critGlobsErr := normalizePolicyGlobs(p.CriticalGlobs)
	if critGlobsErr != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid critical globs: %w", p.TeamID, critGlobsErr)
	}
	p.CriticalGlobs = critGlobs
	p.ReviewerLogin = strings.TrimSpace(p.ReviewerLogin)
	p.ReviewerTeam = strings.TrimSpace(p.ReviewerTeam)
	if !ValidReviewRecipient(p.ReviewerLogin) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid reviewer login %q", p.TeamID, p.ReviewerLogin)
	}
	if p.ReviewerTeam != "" && !ValidReviewRecipient(p.ReviewerTeam) {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid reviewer team %q", p.TeamID, p.ReviewerTeam)
	}
	if p.MaxExternalDiffLines == 0 {
		p.MaxExternalDiffLines = DefaultExternalDiffLines
	}
	if p.MaxExternalRunsPerDay == 0 {
		p.MaxExternalRunsPerDay = DefaultExternalRunsPerDay
	}
	if p.MaxExternalDiffLines < 0 || p.MaxExternalRunsPerDay < 0 ||
		p.ExternalDailyTokenBudget < 0 || p.ExternalDailyTokenBudget > ReviewTokenBudgetMax {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid external limits", p.TeamID)
	}
	associations, associationsErr := normalizeExternalAssociations(p.ExternalTrustedAssociations)
	if associationsErr != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, associationsErr)
	}
	encodedAssociations, err := json.Marshal(associations)
	if err != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: encode trusted associations: %w", p.TeamID, err)
	}
	p.ExternalTrustedAssociations = datatypes.JSON(encodedAssociations)
	var configured []RequiredCheck
	if raw := bytes.TrimSpace(p.RequiredChecks); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if err := json.Unmarshal(raw, &configured); err != nil {
			return QAPolicy{}, fmt.Errorf("save qa policy for team %d: invalid required checks: %w", p.TeamID, err)
		}
	}
	required, normErr := NormalizeRequiredChecks(configured)
	if normErr != nil {
		return QAPolicy{}, fmt.Errorf("save qa policy for team %d: %w", p.TeamID, normErr)
	}
	p.RequiredChecks = required

	// The primary key and timestamps belong to the database: a caller may pass
	// a policy loaded earlier, and an upsert must not try to insert its ID.
	p.ID = 0
	p.CreatedAt = time.Time{}
	p.UpdatedAt = time.Time{}

	err = s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "team_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"llm_profile_id", "fallback_profile_ids", "blocking_threshold", "extra_instructions", "exclude_globs",
			"critical_globs",
			"max_files_per_pass", "max_diff_lines_per_pass", "max_passes",
			"require_green_ci", "required_checks", "review_context_ceiling", "review_effort",
			"review_token_budget", "auto_triage_mode", "auto_withdraw",
			"verify_max_findings", "verify_max_files", "verify_token_share",
			"verify_time_share", "verify_token_max", "reviewer_login", "reviewer_team",
			"allow_external_reviews", "external_trusted_associations",
			"max_external_diff_lines", "max_external_runs_per_day",
			"external_daily_token_budget",
			"architecture_analysis_mode", "architecture_token_share",
			"architecture_time_share", "architecture_token_max",
			"architecture_baseline_path", "architecture_exception_policy",
			"consumer_allow_list", "consumer_baseline_path",
			"consumer_max_lookups", "consumer_max_bytes", "updated_at",
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
	if pr.Draft {
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
	resolved, err := s.ResolvePolicyForRepo(ctx, repo.ID, policy)
	if err != nil {
		return ReviewConfig{}, fmt.Errorf("resolve review policy of pull request %d: %w", prID, err)
	}
	// The profile belongs to the team policy and is not overridable per
	// repository, so both the effective and the enforced policy carry it.
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
		PR: pr, Repo: repo, Policy: resolved.Enforced, EffectivePolicy: resolved.Effective,
		PolicyRevision: resolved.Revision, PolicyObservation: resolved.Observation,
		Profile: profile, Installation: installation,
	}, nil
}

// PendingReviewPRs lists open, non-draft PRs of enabled repositories with a
// team, a policy and an enabled profile, whose current comparison has no run in
// an active or terminal state. External pull requests are included so the
// trigger can apply the external policy: an authorized contribution is
// reviewed, and one outside the policy is escalated to a person. A run covers
// the pull request only when its head, base branch and resolved merge base
// match the current ones; legacy runs without a key still cover it once.
// Superseded runs never suppress a PR. Teams that gate reviews on CI only
// return a PR once its configured gate is green. That verdict is applied to the
// ordered candidates before the limit, so a crowded batch of PRs still waiting
// for checks (or missing a required one) cannot push an eligible candidate past
// it.
// PendingReviewPRs returns at most limit candidates. It has no production
// caller: EnqueuePending reads PendingReviewCandidates so a row the external
// policy rejects cannot consume the batch. It is kept for the store tests that
// pin the query's ordering and CI gate.
func (s *Store) PendingReviewPRs(ctx context.Context, limit int) ([]PullRequest, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.pendingReviewPRs(ctx, limit)
}

// PendingReviewCandidates lists every pending pull request in the same
// priority order as PendingReviewPRs, without a limit. The trigger uses it to
// apply the external policy to each candidate before counting it against the
// batch, so contributions that will never be reviewed cannot crowd out an
// eligible pull request.
func (s *Store) PendingReviewCandidates(ctx context.Context) ([]PullRequest, error) {
	return s.pendingReviewPRs(ctx, 0)
}

func (s *Store) pendingReviewPRs(ctx context.Context, limit int) ([]PullRequest, error) {
	const query = `
SELECT pr.*,
       q.require_green_ci AS team_require_green_ci,
       q.required_checks AS team_required_checks,
       rp.observation_mode AS policy_observation,
       rp.require_green_ci AS repo_require_green_ci,
       rp.required_checks AS repo_required_checks
FROM pull_requests pr
JOIN repositories r ON r.id = pr.repository_id
JOIN qa_policies q ON q.team_id = r.team_id
JOIN llm_profiles p ON p.id = q.llm_profile_id AND p.enabled = true
LEFT JOIN repository_policies rp ON rp.repository_id = r.id
WHERE pr.state = 'open' AND pr.draft = false
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
ORDER BY pr.updated_at DESC`

	// The named gate needs the policy, so it is evaluated here rather than in
	// SQL: expression indexes cannot see the required_checks JSON and a
	// counters-only prefilter would hide required-green/optional-red heads.
	var candidates []pendingReviewCandidate
	if err := s.DB.WithContext(ctx).Raw(query).Scan(&candidates).Error; err != nil {
		return nil, fmt.Errorf("list pending review pull requests: %w", err)
	}

	prs := make([]PullRequest, 0, limit)
	for i := range candidates {
		candidate := &candidates[i]
		if requireGreen, requiredChecks := candidate.effectiveCIGate(); requireGreen {
			legacy := PRValidations{
				Passed: candidate.ValidationsPassed,
				Failed: candidate.ValidationsFailed,
				Total:  candidate.ValidationsTotal,
			}
			verdict := EvaluateConfiguredCI(
				decodeObservation(candidate.ValidationsDetail), legacy, requiredChecks)
			if !verdict.Green() {
				continue
			}
		}
		prs = append(prs, candidate.PullRequest)
		if limit > 0 && len(prs) == limit {
			break
		}
	}
	return prs, nil
}

// pendingReviewCandidate is one ordered candidate plus the policy facts the CI
// gate needs. PullRequest is embedded so the query can keep selecting pr.*.
// The team and repository columns are kept apart because an absent repository
// override must inherit the team value, and observation mode enforces the team
// value even when an override exists.
type pendingReviewCandidate struct {
	PullRequest
	TeamRequireGreenCI bool           `gorm:"column:team_require_green_ci"`
	TeamRequiredChecks datatypes.JSON `gorm:"column:team_required_checks"`
	PolicyObservation  *bool          `gorm:"column:policy_observation"`
	RepoRequireGreenCI *bool          `gorm:"column:repo_require_green_ci"`
	RepoRequiredChecks datatypes.JSON `gorm:"column:repo_required_checks"`
}

// effectiveCIGate resolves the CI gate the run would enforce. Observation mode
// deliberately returns the team values: the overrides are measured, not
// enforced.
func (c pendingReviewCandidate) effectiveCIGate() (bool, datatypes.JSON) {
	requireGreen := c.TeamRequireGreenCI
	requiredChecks := c.TeamRequiredChecks
	if c.PolicyObservation == nil || *c.PolicyObservation {
		return requireGreen, requiredChecks
	}
	if c.RepoRequireGreenCI != nil {
		requireGreen = *c.RepoRequireGreenCI
	}
	if len(bytes.TrimSpace(c.RepoRequiredChecks)) > 0 {
		requiredChecks = c.RepoRequiredChecks
	}
	return requireGreen, requiredChecks
}

// QueuedRunWork flags the two pieces of work a queued review run depends on: a
// live River job that will execute it and the GitHub check run it reports to.
type QueuedRunWork struct {
	RunID         uint `gorm:"column:run_id"`
	PullRequestID uint `gorm:"column:pull_request_id"`
	HasJob        bool `gorm:"column:has_job"`
	HasCheck      bool `gorm:"column:has_check"`
}

// riverLiveReviewJob matches the River jobs that keep a queued run moving.
// Finalized jobs (completed, discarded, cancelled) are the ones a repair may
// replace, so they are deliberately excluded.
const riverLiveReviewJob = `
	SELECT 1 FROM river_job j
	WHERE j.kind = 'review_run'
	  AND j.state IN ('available','running','scheduled','pending','retryable')
	  AND j.args->>'run_id' = rr.id::text`

// StrandedQueuedRuns lists queued runs that lost their live River job, their
// GitHub check, or both. PendingReviewPRs excludes a PR with any queued run, so
// these rows would otherwise never be looked at again.
func (s *Store) StrandedQueuedRuns(ctx context.Context, limit int) ([]QueuedRunWork, error) {
	query := `
WITH work AS (
	SELECT rr.id AS run_id, rr.pull_request_id,
		EXISTS (` + riverLiveReviewJob + `) AS has_job,
		EXISTS (
			SELECT 1 FROM github_artifacts a
			WHERE a.pull_request_id = rr.pull_request_id
			  AND a.kind = 'check_run'
			  AND a.head_sha = rr.head_sha
		) AS has_check
	FROM review_runs rr
	WHERE rr.status = ?
)
SELECT run_id, pull_request_id, has_job, has_check
FROM work
WHERE NOT has_job OR NOT has_check
ORDER BY run_id
LIMIT ?`

	var runs []QueuedRunWork
	if err := s.DB.WithContext(ctx).Raw(query, RunQueued, limit).Scan(&runs).Error; err != nil {
		return nil, fmt.Errorf("list stranded queued review runs: %w", err)
	}
	return runs, nil
}

// QueuedRunWork re-reads the job and check state of one queued run. Callers use
// it under the pull request lock, right before repairing, so a concurrent
// delivery that already healed the run does not cause duplicated work.
func (s *Store) QueuedRunWork(ctx context.Context, runID uint) (hasJob, hasCheck bool, err error) {
	query := `
SELECT
	EXISTS (` + riverLiveReviewJob + `) AS has_job,
	EXISTS (
		SELECT 1 FROM github_artifacts a
		WHERE a.pull_request_id = rr.pull_request_id
		  AND a.kind = 'check_run'
		  AND a.head_sha = rr.head_sha
	) AS has_check
FROM review_runs rr
WHERE rr.id = ?`

	var row struct {
		HasJob   bool `gorm:"column:has_job"`
		HasCheck bool `gorm:"column:has_check"`
	}
	if err := s.DB.WithContext(ctx).Raw(query, runID).Scan(&row).Error; err != nil {
		return false, false, fmt.Errorf("read work of review run %d: %w", runID, err)
	}
	return row.HasJob, row.HasCheck, nil
}

// ActiveRunsForReconcile lists queued or running runs, oldest first. Eligibility
// is not decided here: the trigger re-reads the pull request under its lock, so
// a policy, profile, installation or state that disappeared after the query is
// still caught instead of leaving the run to fail later at configuration load.
func (s *Store) ActiveRunsForReconcile(ctx context.Context, limit int) ([]ReviewRun, error) {
	const query = `
SELECT rr.* FROM review_runs rr
WHERE rr.status IN (?, ?)
ORDER BY rr.id
LIMIT ?`

	var runs []ReviewRun
	if err := s.DB.WithContext(ctx).Raw(query, RunQueued, RunRunning, limit).Scan(&runs).Error; err != nil {
		return nil, fmt.Errorf("list active runs for review reconciliation: %w", err)
	}
	return runs, nil
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
	if in.EnqueuedProfileID == 0 {
		in.EnqueuedProfileID = in.ProfileID
	}
	in.ID = 0
	in.CreatedAt = time.Time{}
	in.UpdatedAt = time.Time{}
	if err := s.DB.WithContext(ctx).Create(&in).Error; err != nil {
		return ReviewRun{}, fmt.Errorf("create review run for pull request %d: %w", in.PullRequestID, err)
	}
	return in, nil
}

// RunLeaseTTL is how long a running attempt may stay without a heartbeat before
// another execution may recover it. It matches the watchdog's heartbeat
// tolerance, so a live worker that is merely slow is never mistaken for a lost
// one.
const RunLeaseTTL = 3 * time.Minute

// MaxRunRecoveries bounds how many times an orphaned attempt is resumed. Once
// spent, the run ends in a terminal state and its check is closed through the
// publication path.
const MaxRunRecoveries = 3

// ClaimReviewRun leases a queued run or recovers a running one whose lease
// expired, and returns the run as it stands in the same transaction. A false
// result means another worker owns it, it is queued for something else, or it
// is terminal; the returned run is still the consistent state the caller must
// inspect (a terminal run with an owed publication is resumed by its caller).
// The returned LeaseOwner is the only token that can write or complete the run.
func (s *Store) ClaimReviewRun(ctx context.Context, id uint) (ReviewRun, bool, error) {
	owner, err := newRunLeaseOwner()
	if err != nil {
		return ReviewRun{}, false, err
	}

	var run ReviewRun
	var claimed, wrote bool
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The row lock serializes the claim: two workers that see the same
		// orphan cannot both recover it, and the state read is the state the
		// claim acted on.
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&run, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load review run %d for claim: %w", id, err)
		}

		now := time.Now().UTC()
		switch run.Status {
		case RunQueued:
			res := tx.Model(&ReviewRun{}).
				Where("id = ? AND status = ?", id, RunQueued).
				Updates(map[string]any{
					"status":     RunRunning,
					"started_at": now,
					// The aggregate time budget starts at the first claim and
					// survives requeues, so retrying a run cannot reset it (#149).
					"budget_started_at": gorm.Expr("COALESCE(budget_started_at, ?)", now),
					"heartbeat_at":      now,
					"lease_owner":       owner,
					"updated_at":        now,
				})
			if res.Error != nil {
				return fmt.Errorf("claim review run %d: %w", id, res.Error)
			}
			claimed = res.RowsAffected > 0
			wrote = claimed
		case RunRunning:
			if !runLeaseExpired(run, now) {
				return nil
			}
			if run.RecoveryCount >= MaxRunRecoveries {
				if err := failExhaustedRun(tx, run, now); err != nil {
					return err
				}
				wrote = true
				break
			}
			// The orphan keeps its persisted units and OCR session: they are
			// the progress the recovering attempt must reuse (#149, #228).
			res := tx.Model(&ReviewRun{}).
				Where("id = ? AND status = ?", id, RunRunning).
				Updates(map[string]any{
					"lease_owner":      owner,
					"heartbeat_at":     now,
					"recovery_count":   gorm.Expr("recovery_count + 1"),
					"phase":            RunPhaseRecovering,
					"phase_started_at": now,
					"error_code":       RunErrorRecovering,
					"error_message":    recoveryReason(run, now),
					"updated_at":       now,
				})
			if res.Error != nil {
				return fmt.Errorf("recover review run %d: %w", id, res.Error)
			}
			claimed = res.RowsAffected > 0
			wrote = claimed
		}
		if !wrote {
			return nil
		}
		if err := tx.Take(&run, id).Error; err != nil {
			return fmt.Errorf("reload review run %d after claim: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return ReviewRun{}, false, err
	}
	return run, claimed, nil
}

// runLeaseExpired decides recovery eligibility. A running row without a beat
// falls back to its start, so a claim that never beat is still protected for
// one lease period; a row with neither has no worker to protect.
func runLeaseExpired(run ReviewRun, now time.Time) bool {
	last := run.HeartbeatAt
	if last == nil {
		last = run.StartedAt
	}
	if last == nil {
		return true
	}
	return last.Before(now.Add(-RunLeaseTTL))
}

// recoveryReason records why the attempt is being resumed and what progress it
// had reached, so the panel can explain the recovery instead of showing a run
// that silently restarted.
func recoveryReason(run ReviewRun, now time.Time) string {
	last := run.HeartbeatAt
	if last == nil {
		last = run.StartedAt
	}
	silence := time.Duration(0)
	if last != nil {
		silence = now.Sub(*last).Round(time.Second)
	}
	progress := "sin latido previo"
	if run.Phase != "" {
		progress = fmt.Sprintf("último progreso en la fase %q", run.Phase)
	}
	return fmt.Sprintf(
		"el worker anterior dejó de responder hace %s (%s); la revisión se está recuperando",
		silence, progress)
}

// failExhaustedRun turns a run that spent its recovery budget into a terminal
// failure with its publication still owed, so the existing publication path
// closes its check. It runs under the claim's row lock.
func failExhaustedRun(tx *gorm.DB, run ReviewRun, now time.Time) error {
	res := tx.Model(&ReviewRun{}).
		Where("id = ? AND status = ?", run.ID, RunRunning).
		Updates(map[string]any{
			"status":           RunFailed,
			"error_code":       RunErrorInterrupted,
			"error_message":    fmt.Sprintf("se agotaron los %d reintentos de recuperación: ningún worker terminó la revisión", MaxRunRecoveries),
			"clone_token_enc":  "",
			"lease_owner":      "",
			"phase":            "",
			"phase_started_at": nil,
			"finished_at":      now,
			"publish_pending":  true,
			"updated_at":       now,
		})
	if res.Error != nil {
		return fmt.Errorf("fail exhausted review run %d: %w", run.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("fail exhausted review run %d: %w", run.ID, ErrRunNotActive)
	}
	// A terminal run is not resumable, so its per-unit retention ends here.
	if err := tx.Where("run_id = ?", run.ID).Delete(&ReviewUnit{}).Error; err != nil {
		return fmt.Errorf("delete review units of run %d: %w", run.ID, err)
	}
	// The worker that kept dying left its attempt open; the run is terminal, so
	// the attempt is closed as interrupted instead of staying "running".
	if err := tx.Model(&ReviewAttempt{}).
		Where("run_id = ? AND status = ?", run.ID, AttemptRunning).
		Updates(map[string]any{
			"status":        AttemptFailed,
			"error_code":    "interrupted",
			"error_message": "el intento no llegó a terminar",
			"finished_at":   now,
			"updated_at":    now,
		}).Error; err != nil {
		return fmt.Errorf("close open attempts of review run %d: %w", run.ID, err)
	}
	return nil
}

// newRunLeaseOwner mints the ownership token of one claim. It must be
// unpredictable only in the sense that two workers never share it.
func newRunLeaseOwner() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate review run lease owner: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// LeaseExpiredRuns lists running runs whose lease expired and that are not yet
// globally stale, oldest first. The watchdog re-queues them so the recovery
// claim happens inside the lease window instead of waiting for the global
// threshold, which fails them.
func (s *Store) LeaseExpiredRuns(ctx context.Context, beatBefore, startedAfter time.Time, limit int) ([]ReviewRun, error) {
	const query = `
SELECT * FROM review_runs
WHERE status = ?
  AND COALESCE(heartbeat_at, started_at, created_at) < ?
  AND COALESCE(started_at, created_at) >= ?
ORDER BY id
LIMIT ?`

	var runs []ReviewRun
	if err := s.DB.WithContext(ctx).Raw(query,
		RunRunning, beatBefore.UTC(), startedAfter.UTC(), limit).Scan(&runs).Error; err != nil {
		return nil, fmt.Errorf("list review runs with an expired lease: %w", err)
	}
	return runs, nil
}

// classifyRunWrite explains a write that matched no row: a run that stopped
// being active is ErrRunNotActive, while a still-running run under another
// owner is ErrRunLeaseLost.
func classifyRunWrite(db *gorm.DB, id uint, owner string) error {
	var row struct {
		Status     string `gorm:"column:status"`
		LeaseOwner string `gorm:"column:lease_owner"`
	}
	err := db.Model(&ReviewRun{}).Select("status", "lease_owner").Take(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrRunNotActive
	}
	if err != nil {
		return fmt.Errorf("classify write of review run %d: %w", id, err)
	}
	if row.Status != RunRunning {
		return ErrRunNotActive
	}
	if owner != "" && row.LeaseOwner != owner {
		return ErrRunLeaseLost
	}
	return ErrRunNotActive
}

// ownedRun narrows a run mutation to its lease owner. An empty owner is a
// system write that does not belong to a worker lease and keeps the status-only
// contract used by direct store callers.
func ownedRun(q *gorm.DB, owner string) *gorm.DB {
	if owner == "" {
		return q
	}
	return q.Where("lease_owner = ?", owner)
}

// HeartbeatReviewRun records that the worker owning the run is alive. Only the
// current owner of a running run accepts the write: ErrRunNotActive tells the
// worker that a new head or the watchdog closed it, and ErrRunLeaseLost that
// another execution recovered it (#228). Both mean the pipeline must stop.
func (s *Store) HeartbeatReviewRun(ctx context.Context, id uint, owner string) error {
	res := ownedRun(s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning), owner).
		Update("heartbeat_at", time.Now().UTC())
	if res.Error != nil {
		return fmt.Errorf("heartbeat review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("heartbeat review run %d: %w", id, classifyRunWrite(s.DB.WithContext(ctx), id, owner))
	}
	return nil
}

// StartReviewAttempt records the profile that is about to execute one attempt
// of a run and repoints the run at it. The enqueued profile stays in
// enqueued_profile_id, so a policy change while the job waited remains visible.
// The sequence is assigned under the run row lock; a retried run appends a new
// attempt instead of reusing the previous one. Only the lease owner may open a
// new attempt, so a worker that lost the run cannot keep attributing work (#228).
func (s *Store) StartReviewAttempt(ctx context.Context, runID uint, owner string, profile LLMProfile, revision string) (ReviewAttempt, error) {
	var attempt ReviewAttempt
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run ReviewRun
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status", "lease_owner").Take(&run, runID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrRunNotActive
		}
		if err != nil {
			return fmt.Errorf("load review run %d: %w", runID, err)
		}
		if run.Status != RunRunning {
			return ErrRunNotActive
		}
		if owner != "" && run.LeaseOwner != owner {
			return ErrRunLeaseLost
		}
		var last int
		if err := tx.Raw("SELECT COALESCE(MAX(seq), 0) FROM review_attempts WHERE run_id = ?", runID).
			Scan(&last).Error; err != nil {
			return fmt.Errorf("read last attempt of run %d: %w", runID, err)
		}
		now := time.Now().UTC()
		attempt = ReviewAttempt{
			RunID: runID, Seq: last + 1, ProfileID: profile.ID,
			Provider: profile.Provider, Model: profile.Model,
			BaseURL: derefOrEmpty(profile.BaseURL), ConfigRevision: revision,
			Status: AttemptRunning, StartedAt: now,
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return fmt.Errorf("create attempt %d of run %d: %w", attempt.Seq, runID, err)
		}
		if err := tx.Model(&ReviewRun{}).Where("id = ?", runID).
			Update("profile_id", profile.ID).Error; err != nil {
			return fmt.Errorf("attribute run %d to profile %d: %w", runID, profile.ID, err)
		}
		return nil
	})
	if err != nil {
		return ReviewAttempt{}, err
	}
	return attempt, nil
}

// FinishReviewAttempt closes one attempt with its own consumption. Usage stays
// NULL when the engine reported none, so an unknown attempt is not counted as
// a zero nor duplicated into the run total.
func (s *Store) FinishReviewAttempt(ctx context.Context, id uint, status, code, message string, usage *RunUsage, known bool) error {
	updates := map[string]any{
		"status":        status,
		"error_code":    code,
		"error_message": message,
		"finished_at":   time.Now().UTC(),
		"updated_at":    time.Now().UTC(),
	}
	var input, output, reasoning, cacheRead, cacheWrite, total any
	if known {
		input, output, reasoning = usage.Input, usage.Output, usage.Reasoning
		cacheRead, cacheWrite, total = usage.CacheRead, usage.CacheWrite, usage.Total
	}
	updates["input_tokens"] = input
	updates["output_tokens"] = output
	updates["reasoning_tokens"] = reasoning
	updates["cache_read_tokens"] = cacheRead
	updates["cache_write_tokens"] = cacheWrite
	updates["total_tokens"] = total
	if known && usage.MaxContext != nil {
		updates["max_context_tokens"] = *usage.MaxContext
	} else {
		updates["max_context_tokens"] = nil
	}
	res := s.DB.WithContext(ctx).Model(&ReviewAttempt{}).
		Where("id = ? AND status = ?", id, AttemptRunning).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("finish review attempt %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("finish review attempt %d: %w", id, ErrNotFound)
	}
	return nil
}

// SetReviewRunPolicy records the effective policy revision and observation
// flag a run is executing under. The policy can change between enqueue and
// execution, so the worker rewrites this snapshot when it starts. Only the
// lease owner may write it; ErrRunNotActive or ErrRunLeaseLost mean another
// execution owns the run.
func (s *Store) SetReviewRunPolicy(ctx context.Context, id uint, owner, revision string, observation bool) error {
	res := ownedRun(s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning), owner).
		Updates(map[string]any{
			"policy_revision":    revision,
			"policy_observation": observation,
			"updated_at":         time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("record review policy of run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record review policy of run %d: %w", id, classifyRunWrite(s.DB.WithContext(ctx), id, owner))
	}
	return nil
}

// RequeueReviewRun returns a claimed run to the queue without finishing it, so
// a worker that found no profile capacity can retry it later. The attempt
// counter doubles as the number of times the run had to wait: it bounds the
// backoff and it is what the worker compares against its budget. Only the lease
// owner may requeue; the lease is released with the claim.
func (s *Store) RequeueReviewRun(ctx context.Context, id uint, owner, errCode, errMsg string) error {
	res := ownedRun(s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning), owner).
		Updates(map[string]any{
			"status":           RunQueued,
			"attempt":          gorm.Expr("attempt + 1"),
			"error_code":       errCode,
			"error_message":    errMsg,
			"started_at":       nil,
			"heartbeat_at":     nil,
			"lease_owner":      "",
			"phase":            "",
			"phase_started_at": nil,
			"updated_at":       time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("requeue review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("requeue review run %d: %w", id, classifyRunWrite(s.DB.WithContext(ctx), id, owner))
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

// ReviewUnitsForRun returns the resumable units persisted by earlier attempts
// of a run, oldest first.
func (s *Store) ReviewUnitsForRun(ctx context.Context, runID uint) ([]ReviewUnit, error) {
	var units []ReviewUnit
	if err := s.DB.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("id").
		Find(&units).Error; err != nil {
		return nil, fmt.Errorf("list review units of run %d: %w", runID, err)
	}
	return units, nil
}

// UpsertReviewUnit records the outcome of one unit. The key is the run's
// compatibility fingerprint, so a retried unit replaces its previous row instead
// of duplicating it.
func (s *Store) UpsertReviewUnit(ctx context.Context, unit ReviewUnit) error {
	if unit.Status == "" {
		unit.Status = ReviewUnitCompleted
	}
	now := time.Now().UTC()
	unit.CreatedAt = now
	unit.UpdatedAt = now
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "run_id"}, {Name: "unit_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"kind", "path", "status", "result", "usage", "updated_at",
		}),
	}).Create(&unit).Error
	if err != nil {
		return fmt.Errorf("save review unit %q of run %d: %w", unit.UnitKey, unit.RunID, err)
	}
	return nil
}

// DeleteReviewUnits drops the resumable state of a run. It runs when the run
// reaches a terminal state: retention is bounded to interrupted reviews.
func (s *Store) DeleteReviewUnits(ctx context.Context, runID uint) error {
	if err := s.DB.WithContext(ctx).Where("run_id = ?", runID).Delete(&ReviewUnit{}).Error; err != nil {
		return fmt.Errorf("delete review units of run %d: %w", runID, err)
	}
	return nil
}

// SetReviewRunSession stores the main OCR session so the next attempt of the
// same run can resume it. Only the lease owner of an active run may write it.
func (s *Store) SetReviewRunSession(ctx context.Context, id uint, owner, sessionID, scope string) error {
	res := ownedRun(s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning), owner).
		Updates(map[string]any{
			"ocr_session_id":    sessionID,
			"ocr_session_scope": scope,
			"updated_at":        time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("set OCR session of review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set OCR session of review run %d: %w", id, classifyRunWrite(s.DB.WithContext(ctx), id, owner))
	}
	return nil
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
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&ReviewRun{}).
			Where("pull_request_id = ? AND status IN ?", prID, []string{RunQueued, RunRunning}).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Model(&ReviewRun{}).Where("id IN ?", ids).
			Updates(map[string]any{
				"status":           RunSuperseded,
				"clone_token_enc":  "",
				"phase":            "",
				"phase_started_at": nil,
				"updated_at":       time.Now().UTC(),
			}).Error; err != nil {
			return err
		}
		// A superseded run can never be resumed; its raw review JSON must not
		// outlive it (#149).
		return tx.Where("run_id IN ?", ids).Delete(&ReviewUnit{}).Error
	})
	if err != nil {
		return fmt.Errorf("supersede runs of pull request %d: %w", prID, err)
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
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		where := tx.Model(&ReviewRun{}).
			Where("pull_request_id = ? AND status IN ?", prID, []string{RunQueued, RunRunning}).
			Where(`NOT (
				head_sha = ? AND (
					(base_ref = '' AND base_sha = '')
					OR ((base_ref = '' OR base_ref = ?) AND base_sha <> '' AND ? <> '' AND base_sha = ?)
					OR (base_ref <> '' AND base_ref = ? AND base_sha = '' AND ? = '')
				)
			)`, comparison.HeadSHA, comparison.BaseRef, comparison.MergeBaseSHA,
				comparison.MergeBaseSHA, comparison.BaseRef, comparison.MergeBaseSHA)
		var ids []uint
		if err := where.Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Model(&ReviewRun{}).Where("id IN ?", ids).
			Updates(map[string]any{
				"status":           RunSuperseded,
				"clone_token_enc":  "",
				"phase":            "",
				"phase_started_at": nil,
				"updated_at":       time.Now().UTC(),
			}).Error; err != nil {
			return err
		}
		// The superseded comparison can never be resumed, so its units go with
		// it (#149).
		return tx.Where("run_id IN ?", ids).Delete(&ReviewUnit{}).Error
	})
	if err != nil {
		return fmt.Errorf("supersede other runs of pull request %d: %w", prID, err)
	}
	return nil
}

// SetPRMergeBase records the resolved merge base of the pull request's current
// comparison. The update is conditional on the head, base branch and base tip
// still being the ones the resolution was made for, so a retarget or a
// force-push landing between the Compare call and this write cannot overwrite
// the newer comparison. It returns the number of rows updated: zero means the
// comparison moved and nothing was written.
func (s *Store) SetPRMergeBase(ctx context.Context, prID uint, headSHA, baseRef, baseTipSHA, mergeBase string) (int64, error) {
	return setPRMergeBase(s.DB.WithContext(ctx), prID, headSHA, baseRef, baseTipSHA, mergeBase)
}

func setPRMergeBase(db *gorm.DB, prID uint, headSHA, baseRef, baseTipSHA, mergeBase string) (int64, error) {
	if mergeBase == "" {
		return 0, nil
	}
	res := db.Model(&PullRequest{}).
		Where("id = ? AND head_sha = ? AND base_ref = ? AND base_sha = ?",
			prID, headSHA, baseRef, baseTipSHA).
		Where("merge_base_sha = '' OR merge_base_sha = ?", mergeBase).
		Updates(map[string]any{
			"merge_base_sha": mergeBase,
			"updated_at":     time.Now().UTC(),
		})
	if res.Error != nil {
		return 0, fmt.Errorf("set merge base of pull request %d: %w", prID, res.Error)
	}
	return res.RowsAffected, nil
}

// AdoptReviewComparison records the merge base resolved for the pull request's
// current comparison and re-pins the base tip of the run adopted for it, in one
// transaction. Writing only the merge base would leave a run whose snapshot
// still names the old tip, and CurrentVerdictRun and republish would ignore it
// forever (#141). A run that no longer matches aborts the transaction so the
// pull request is never committed without the run that covers it.
// It returns the number of pull request rows updated: zero means the comparison
// moved and nothing was written.
func (s *Store) AdoptReviewComparison(ctx context.Context, prID uint, headSHA, baseRef, baseTipSHA, mergeBase string, runID uint) (int64, error) {
	var updated int64
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		affected, err := setPRMergeBase(tx, prID, headSHA, baseRef, baseTipSHA, mergeBase)
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
		// An empty tip carries no snapshot to re-pin; the run keeps whatever it
		// already has, exactly as before the comparison was adopted.
		if baseTipSHA != "" {
			res := tx.Model(&ReviewRun{}).
				Where("id = ? AND head_sha = ? AND base_ref = ?", runID, headSHA, baseRef).
				Update("base_tip_sha", baseTipSHA)
			if res.Error != nil {
				return fmt.Errorf("set base tip of review run %d: %w", runID, res.Error)
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("set base tip of review run %d: no row matched the adopted comparison", runID)
			}
		}
		updated = affected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return updated, nil
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
	// ArchitectureAnalysis is the observational architectural verdict of the
	// comparison. Empty keeps the column NULL.
	ArchitectureAnalysis []byte
}

// CompleteReviewRun stores a run's outcome in a single transaction: the run is
// finished only when it is still running under the given lease, findings are
// upserted, resolved findings are marked, and the clone token is discarded. Any
// error rolls the whole set back. A run superseded while executing returns
// ErrRunNotActive; one recovered by another execution returns ErrRunLeaseLost.
// In both cases nothing is persisted, so a stale result can never be published.
func (s *Store) CompleteReviewRun(ctx context.Context, id uint, owner string, c RunCompletion) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock and classify before writing. This is what makes finalization
		// exactly once: the first worker to hold the lock writes, and a second
		// one finds a terminal row or a foreign lease.
		var current ReviewRun
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status", "lease_owner").Take(&current, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("complete review run %d: %w", id, ErrRunNotActive)
		}
		if err != nil {
			return fmt.Errorf("load review run %d for completion: %w", id, err)
		}
		if current.Status != RunRunning {
			return fmt.Errorf("complete review run %d: %w", id, ErrRunNotActive)
		}
		if owner != "" && current.LeaseOwner != owner {
			return fmt.Errorf("complete review run %d: %w", id, ErrRunLeaseLost)
		}

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
		var architectureAnalysis any
		if len(c.ArchitectureAnalysis) > 0 {
			architectureAnalysis = datatypes.JSON(c.ArchitectureAnalysis)
		}
		updates := map[string]any{
			"status":                c.Status,
			"error_code":            c.ErrorCode,
			"error_message":         c.ErrorMessage,
			"summary":               c.Summary,
			"coverage":              datatypes.JSON(coverage),
			"usage":                 datatypes.JSON(usage),
			"input_tokens":          input,
			"output_tokens":         output,
			"reasoning_tokens":      reasoning,
			"cache_read_tokens":     cacheRead,
			"cache_write_tokens":    cacheWrite,
			"total_tokens":          total,
			"max_context_tokens":    maxContext,
			"verify_session_id":     c.VerifySessionID,
			"verify_usage":          verifyUsage,
			"architecture_analysis": architectureAnalysis,
			"clone_token_enc":       "",
			"lease_owner":           "",
			// The delivery is owed from the moment the result is durable, so a
			// crash before the worker enqueues the publish job cannot lose it.
			"publish_pending": true,
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
		// A crash or an early return can leave an attempt open. The run is
		// terminal now, so the attempt is closed as failed with no usage rather
		// than staying "running" forever.
		if err := tx.Model(&ReviewAttempt{}).
			Where("run_id = ? AND status = ?", id, AttemptRunning).
			Updates(map[string]any{
				"status":        AttemptFailed,
				"error_code":    "interrupted",
				"error_message": "el intento no llegó a terminar",
				"finished_at":   now,
				"updated_at":    now,
			}).Error; err != nil {
			return fmt.Errorf("close open attempts of review run %d: %w", id, err)
		}
		// The run is terminal: its per-unit state is no longer resumable and the
		// retention window ends here (#149).
		if err := tx.Where("run_id = ?", id).Delete(&ReviewUnit{}).Error; err != nil {
			return fmt.Errorf("delete review units of run %d: %w", id, err)
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
    clone_token_enc = '', phase = '', phase_started_at = NULL, finished_at = ?, updated_at = ?,
    publish_pending = true
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
	if len(runs) > 0 {
		ids := make([]uint, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.ID)
		}
		if err := s.DB.WithContext(ctx).Where("run_id IN ?", ids).Delete(&ReviewUnit{}).Error; err != nil {
			return runs, fmt.Errorf("delete review units of interrupted runs: %w", err)
		}
	}
	return runs, nil
}

// SetReviewRunPhase records the current step of a running run and when it
// began. A queued, finished or superseded run rejects the write with
// ErrRunNotActive, and a run recovered by another worker with ErrRunLeaseLost,
// so a late phase cannot resurrect a terminal row nor overwrite the owner's.
func (s *Store) SetReviewRunPhase(ctx context.Context, id uint, owner, phase string) error {
	now := time.Now().UTC()
	res := ownedRun(s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning), owner).
		Updates(map[string]any{
			"phase":            phase,
			"phase_started_at": now,
			"updated_at":       now,
		})
	if res.Error != nil {
		return fmt.Errorf("set phase of review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set phase of review run %d: %w", id, classifyRunWrite(s.DB.WithContext(ctx), id, owner))
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

// CurrentVerdictRun returns the latest terminal run of the pull request's
// current head, and false when that comparison has no verdict yet (a newer run
// is queued or running). A run of another head or base branch never owns the
// shared check or QA status (#141). An empty current merge base is unknown, not
// a mismatch: a run that finished before the base was resolved still owns the
// status, exactly like comparisonMovedAfterReview's rule.
func (s *Store) CurrentVerdictRun(ctx context.Context, pr PullRequest) (ReviewRun, bool, error) {
	if pr.HeadSHA == "" {
		return ReviewRun{}, false, nil
	}
	runs, err := s.ListReviewRuns(ctx, pr.ID)
	if err != nil {
		return ReviewRun{}, false, err
	}
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if !strings.EqualFold(run.HeadSHA, pr.HeadSHA) {
			continue
		}
		switch run.Status {
		case RunSucceeded, RunInconclusive, RunFailed:
			if run.BaseRef != "" && run.BaseRef != pr.BaseRef {
				return ReviewRun{}, false, nil
			}
			if run.BaseTipSHA != "" && pr.BaseSHA != "" && !strings.EqualFold(run.BaseTipSHA, pr.BaseSHA) {
				return ReviewRun{}, false, nil
			}
			// Only two known merge bases can disagree.
			if run.BaseSHA != "" && pr.MergeBaseSHA != "" && !strings.EqualFold(run.BaseSHA, pr.MergeBaseSHA) {
				return ReviewRun{}, false, nil
			}
			return run, true, nil
		}
		return ReviewRun{}, false, nil
	}
	return ReviewRun{}, false, nil
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
	citations := DecodeFindingCitations(f.Citations)
	f.Scope = NormalizeFindingScope(f.Scope, citations)
	f.Citations = EncodeFindingCitations(citations)

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
			"body", "suggestion", "confidence", "existing_code", "scope", "citations", "advisory", "status",
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
		"existing_code": f.ExistingCode, "scope": f.Scope, "citations": f.Citations,
		"advisory": f.Advisory, "status": f.Status, "last_seen_run_id": f.LastSeenRunID,
		"updated_at": now,
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

// FindingForReviewComment resolves the finding a reply belongs to. A reply
// carries the id of the root review comment it answers, which is the coordinate
// a published finding stores.
func (s *Store) FindingForReviewComment(ctx context.Context, prID uint, commentID int64) (Finding, error) {
	if commentID == 0 {
		return Finding{}, fmt.Errorf("finding for review comment: %w", ErrNotFound)
	}
	var findings []Finding
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ? AND github_comment_id = ?", prID, commentID).
		Order("id").Limit(2).Find(&findings).Error
	if err != nil {
		return Finding{}, fmt.Errorf("find finding for review comment %d: %w", commentID, err)
	}
	switch len(findings) {
	case 0:
		return Finding{}, fmt.Errorf("finding for review comment %d: %w", commentID, ErrNotFound)
	case 1:
		return findings[0], nil
	default:
		return Finding{}, fmt.Errorf("finding for review comment %d: %w", commentID, ErrFindingThreadAmbiguous)
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
	// Evidence and HeadSHA are the verifier's quote and the revision it proved,
	// so a reevaluation of an objection keeps what the decision rested on.
	Evidence string
	HeadSHA  string
	// Objection is the reply that motivated the decision. Audit rows of an
	// objection store it so the history survives even when the finding changes.
	Objection string
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
	if entry.Evidence != "" {
		data["evidence"] = entry.Evidence
	}
	if entry.HeadSHA != "" {
		data["head_sha"] = entry.HeadSHA
	}
	if entry.Objection != "" {
		data["objection"] = entry.Objection
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
