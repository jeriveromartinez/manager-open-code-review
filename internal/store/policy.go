package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Policy field provenance. The effective policy of a repository is the team
// policy with the fields its override sets replaced; these values tell the
// panel where each field came from.
const (
	PolicySourceTeam   = "team"
	PolicySourceRepo   = "repo"
	PolicySourceGlobal = "global"
)

// ErrInvalidPolicy marks an override the store rejected. The API maps it to a
// 400 instead of reporting a server failure.
var ErrInvalidPolicy = errors.New("invalid policy")

// ResolvedPolicy is the effective review policy of one repository plus the
// provenance of every field and the revision that identifies it (#153).
type ResolvedPolicy struct {
	RepositoryID int64
	Team         QAPolicy
	Override     *RepositoryPolicy
	Effective    QAPolicy
	Enforced     QAPolicy
	Observation  bool
	Revision     string
	Provenance   map[string]string
}

// ValidBlockingThreshold reports whether threshold is one the publisher knows.
func ValidBlockingThreshold(threshold string) bool {
	switch threshold {
	case "blocker", "major", "minor", "nit":
		return true
	default:
		return false
	}
}

// RepositoryPolicyFor loads the explicit overrides of a repository. A missing
// row means the repository inherits the team policy and is ErrNotFound.
func (s *Store) RepositoryPolicyFor(ctx context.Context, repoID int64) (RepositoryPolicy, error) {
	var policy RepositoryPolicy
	err := quietDB(s.DB.WithContext(ctx)).Where("repository_id = ?", repoID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RepositoryPolicy{}, fmt.Errorf("repository policy %d: %w", repoID, ErrNotFound)
	}
	if err != nil {
		return RepositoryPolicy{}, fmt.Errorf("find repository policy %d: %w", repoID, err)
	}
	return policy, nil
}

// SaveRepositoryPolicy replaces the overrides of a repository. The caller
// passes the complete override it wants; fields left nil are stored as NULL
// and inherit the team value.
func (s *Store) SaveRepositoryPolicy(ctx context.Context, p RepositoryPolicy) (RepositoryPolicy, error) {
	normalized, err := validateRepositoryPolicy(p)
	if err != nil {
		return RepositoryPolicy{}, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	normalized.CreatedAt = time.Time{}
	normalized.UpdatedAt = time.Now().UTC()
	err = s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "repository_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"observation_mode", "blocking_threshold", "extra_instructions", "exclude_globs",
			"max_files_per_pass", "max_diff_lines_per_pass", "max_passes",
			"require_green_ci", "required_checks", "review_context_ceiling", "review_effort",
			"review_token_budget", "auto_triage_mode", "auto_withdraw",
			"verify_max_findings", "verify_max_files", "verify_token_share",
			"verify_time_share", "verify_token_max", "updated_at",
		}),
	}).Create(&normalized).Error
	if err != nil {
		return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: %w", p.RepositoryID, err)
	}
	return s.RepositoryPolicyFor(ctx, p.RepositoryID)
}

// DeleteRepositoryPolicy removes every override of a repository and returns it
// to inheritance. Deleting a repository that has no override is a no-op.
func (s *Store) DeleteRepositoryPolicy(ctx context.Context, repoID int64) error {
	if err := s.DB.WithContext(ctx).
		Where("repository_id = ?", repoID).
		Delete(&RepositoryPolicy{}).Error; err != nil {
		return fmt.Errorf("delete repository policy %d: %w", repoID, err)
	}
	return nil
}

// ResolveReviewPolicy loads the team policy and the repository overrides and
// resolves them. A repository without a team or a team without a policy is
// ErrNoReviewConfig, the same boundary ReviewConfigForPR enforces.
func (s *Store) ResolveReviewPolicy(ctx context.Context, repoID int64) (ResolvedPolicy, error) {
	var repo Repository
	if err := quietDB(s.DB.WithContext(ctx)).Take(&repo, repoID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedPolicy{}, fmt.Errorf("repository %d: %w", repoID, ErrNotFound)
		}
		return ResolvedPolicy{}, fmt.Errorf("find repository %d: %w", repoID, err)
	}
	if repo.TeamID == nil {
		return ResolvedPolicy{}, fmt.Errorf("repository %d has no team: %w", repoID, ErrNoReviewConfig)
	}
	team, err := s.QAPolicyForTeam(ctx, *repo.TeamID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ResolvedPolicy{}, fmt.Errorf("repository %d: %w", repoID, ErrNoReviewConfig)
		}
		return ResolvedPolicy{}, err
	}
	return s.ResolvePolicyForRepo(ctx, repo.ID, team)
}

// ResolvePolicyForRepo resolves the overrides of one repository against an
// already loaded team policy.
func (s *Store) ResolvePolicyForRepo(ctx context.Context, repoID int64, team QAPolicy) (ResolvedPolicy, error) {
	override, err := s.RepositoryPolicyFor(ctx, repoID)
	if errors.Is(err, ErrNotFound) {
		return ResolvePolicy(repoID, team, nil), nil
	}
	if err != nil {
		return ResolvedPolicy{}, err
	}
	return ResolvePolicy(repoID, team, &override), nil
}

// ResolvePolicy applies the explicit overrides on top of the team policy. The
// enforced policy is the team policy while observation mode is on: overrides
// are resolved, recorded and measured, but they do not change behaviour.
func ResolvePolicy(repoID int64, team QAPolicy, override *RepositoryPolicy) ResolvedPolicy {
	effective := team
	provenance := policyProvenance(team)
	observation := false
	if override != nil {
		observation = override.ObservationMode
		provenance["observation_mode"] = PolicySourceRepo

		if override.BlockingThreshold != nil {
			effective.BlockingThreshold = *override.BlockingThreshold
			provenance["blocking_threshold"] = PolicySourceRepo
		}
		if override.ExtraInstructions != nil {
			effective.ExtraInstructions = *override.ExtraInstructions
			provenance["extra_instructions"] = PolicySourceRepo
		}
		if override.ExcludeGlobs != nil {
			effective.ExcludeGlobs = override.ExcludeGlobs
			provenance["exclude_globs"] = PolicySourceRepo
		}
		if override.MaxFilesPerPass != nil {
			effective.MaxFilesPerPass = *override.MaxFilesPerPass
			provenance["max_files"] = PolicySourceRepo
		}
		if override.MaxDiffLinesPerPass != nil {
			effective.MaxDiffLinesPerPass = *override.MaxDiffLinesPerPass
			provenance["max_diff_lines"] = PolicySourceRepo
		}
		if override.MaxPasses != nil {
			effective.MaxPasses = *override.MaxPasses
			provenance["max_passes"] = PolicySourceRepo
		}
		if override.RequireGreenCI != nil {
			effective.RequireGreenCI = *override.RequireGreenCI
			provenance["require_green_ci"] = PolicySourceRepo
		}
		if override.RequiredChecks != nil {
			effective.RequiredChecks = override.RequiredChecks
			provenance["required_checks"] = PolicySourceRepo
		}
		if override.ReviewContextCeiling != nil {
			effective.ReviewContextCeiling = override.ReviewContextCeiling
			provenance["review_context_ceiling"] = PolicySourceRepo
		}
		if override.ReviewEffort != nil {
			effective.ReviewEffort = *override.ReviewEffort
			provenance["review_effort"] = PolicySourceRepo
		}
		if override.ReviewTokenBudget != nil {
			effective.ReviewTokenBudget = *override.ReviewTokenBudget
			provenance["review_token_budget"] = PolicySourceRepo
		}
		if override.AutoTriageMode != nil {
			effective.AutoTriageMode = *override.AutoTriageMode
			provenance["auto_triage_mode"] = PolicySourceRepo
		}
		if override.AutoWithdraw != nil {
			effective.AutoWithdraw = *override.AutoWithdraw
			provenance["auto_withdraw"] = PolicySourceRepo
		}
		if override.VerifyMaxFindings != nil {
			effective.VerifyMaxFindings = *override.VerifyMaxFindings
			provenance["verify_max_findings"] = PolicySourceRepo
		}
		if override.VerifyMaxFiles != nil {
			effective.VerifyMaxFiles = *override.VerifyMaxFiles
			provenance["verify_max_files"] = PolicySourceRepo
		}
		if override.VerifyTokenShare != nil {
			effective.VerifyTokenShare = *override.VerifyTokenShare
			provenance["verify_token_share"] = PolicySourceRepo
		}
		if override.VerifyTimeShare != nil {
			effective.VerifyTimeShare = *override.VerifyTimeShare
			provenance["verify_time_share"] = PolicySourceRepo
		}
		if override.VerifyTokenMax != nil {
			effective.VerifyTokenMax = *override.VerifyTokenMax
			provenance["verify_token_max"] = PolicySourceRepo
		}
	}

	enforced := effective
	if observation {
		enforced = team
	}
	return ResolvedPolicy{
		RepositoryID: repoID,
		Team:         team,
		Override:     override,
		Effective:    effective,
		Enforced:     enforced,
		Observation:  observation,
		Revision:     PolicyRevision(effective, observation),
		Provenance:   provenance,
	}
}

func policyProvenance(team QAPolicy) map[string]string {
	provenance := map[string]string{
		"llm_profile_id":      PolicySourceTeam,
		"blocking_threshold":  PolicySourceTeam,
		"extra_instructions":  PolicySourceTeam,
		"exclude_globs":       PolicySourceTeam,
		"max_files":           PolicySourceTeam,
		"max_diff_lines":      PolicySourceTeam,
		"max_passes":          PolicySourceTeam,
		"require_green_ci":    PolicySourceTeam,
		"required_checks":     PolicySourceTeam,
		"review_effort":       PolicySourceTeam,
		"review_token_budget": PolicySourceTeam,
		"auto_triage_mode":    PolicySourceTeam,
		"auto_withdraw":       PolicySourceTeam,
		"verify_max_findings": PolicySourceTeam,
		"verify_max_files":    PolicySourceTeam,
		"verify_token_share":  PolicySourceTeam,
		"verify_time_share":   PolicySourceTeam,
		"verify_token_max":    PolicySourceTeam,
	}
	if team.ReviewContextCeiling == nil {
		provenance["review_context_ceiling"] = PolicySourceGlobal
	} else {
		provenance["review_context_ceiling"] = PolicySourceTeam
	}
	return provenance
}

// policyRevisionFields is the canonical, behaviour-relevant content of an
// effective policy. Timestamps and database IDs are excluded so saving the
// same policy twice keeps the same revision.
type policyRevisionFields struct {
	LLMProfileID         uint            `json:"llm_profile_id"`
	BlockingThreshold    string          `json:"blocking_threshold"`
	ExtraInstructions    string          `json:"extra_instructions"`
	ExcludeGlobs         []string        `json:"exclude_globs"`
	MaxFiles             int             `json:"max_files"`
	MaxDiffLines         int             `json:"max_diff_lines"`
	MaxPasses            int             `json:"max_passes"`
	RequireGreenCI       bool            `json:"require_green_ci"`
	RequiredChecks       []RequiredCheck `json:"required_checks"`
	ReviewContextCeiling *int            `json:"review_context_ceiling"`
	ReviewEffort         string          `json:"review_effort"`
	ReviewTokenBudget    int64           `json:"review_token_budget"`
	AutoTriageMode       string          `json:"auto_triage_mode"`
	AutoWithdraw         bool            `json:"auto_withdraw"`
	VerifyMaxFindings    int             `json:"verify_max_findings"`
	VerifyMaxFiles       int             `json:"verify_max_files"`
	VerifyTokenShare     int             `json:"verify_token_share"`
	VerifyTimeShare      int             `json:"verify_time_share"`
	VerifyTokenMax       int64           `json:"verify_token_max"`
	Observation          bool            `json:"observation"`
}

// PolicyRevision returns a stable identifier of the policy content. It covers
// the policy fields only: the global review_context_ceiling is resolved from
// settings outside the policy, so runs under the same policy and different
// global ceilings share this revision. A change of any enforced or observed
// field, including switching observation on, yields a different revision;
// re-saving identical content does not.
func PolicyRevision(p QAPolicy, observation bool) string {
	fields := policyRevisionFields{
		LLMProfileID:         p.LLMProfileID,
		BlockingThreshold:    p.BlockingThreshold,
		ExtraInstructions:    p.ExtraInstructions,
		ExcludeGlobs:         decodeStringList(p.ExcludeGlobs),
		MaxFiles:             p.MaxFilesPerPass,
		MaxDiffLines:         p.MaxDiffLinesPerPass,
		MaxPasses:            p.MaxPasses,
		RequireGreenCI:       p.RequireGreenCI,
		RequiredChecks:       ParseRequiredChecks(p.RequiredChecks),
		ReviewContextCeiling: p.ReviewContextCeiling,
		ReviewEffort:         p.ReviewEffort,
		ReviewTokenBudget:    p.ReviewTokenBudget,
		AutoTriageMode:       p.AutoTriageMode,
		AutoWithdraw:         p.AutoWithdraw,
		VerifyMaxFindings:    p.VerifyMaxFindings,
		VerifyMaxFiles:       p.VerifyMaxFiles,
		VerifyTokenShare:     p.VerifyTokenShare,
		VerifyTimeShare:      p.VerifyTimeShare,
		VerifyTokenMax:       p.VerifyTokenMax,
		Observation:          observation,
	}
	// Both collections are normalized to empty slices so nil and [] share one
	// revision: they mean the same thing to the engine.
	if fields.ExcludeGlobs == nil {
		fields.ExcludeGlobs = []string{}
	}
	if fields.RequiredChecks == nil {
		fields.RequiredChecks = []RequiredCheck{}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8])
}

func decodeStringList(raw datatypes.JSON) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

func validateRepositoryPolicy(p RepositoryPolicy) (RepositoryPolicy, error) {
	if p.RepositoryID == 0 {
		return RepositoryPolicy{}, errors.New("save repository policy: missing repository id")
	}

	if p.BlockingThreshold != nil {
		value := *p.BlockingThreshold
		if value == "" {
			value = policyDefaultThreshold
		}
		if !ValidBlockingThreshold(value) {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid blocking threshold %q",
				p.RepositoryID, value)
		}
		p.BlockingThreshold = &value
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"max_files", p.MaxFilesPerPass},
		{"max_diff_lines", p.MaxDiffLinesPerPass},
		{"max_passes", p.MaxPasses},
	} {
		if field.value != nil && *field.value <= 0 {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid %s %d",
				p.RepositoryID, field.name, *field.value)
		}
	}
	if p.ReviewContextCeiling != nil &&
		(*p.ReviewContextCeiling < ReviewContextCeilingMin || *p.ReviewContextCeiling > ReviewContextCeilingMax) {
		return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid review context ceiling %d",
			p.RepositoryID, *p.ReviewContextCeiling)
	}
	if p.ReviewEffort != nil {
		value := *p.ReviewEffort
		if value == "" {
			value = policyDefaultReviewEffort
		}
		if !ValidReviewEffort(value) {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid review effort %q",
				p.RepositoryID, value)
		}
		p.ReviewEffort = &value
	}
	if p.ReviewTokenBudget != nil &&
		(*p.ReviewTokenBudget < 0 || *p.ReviewTokenBudget > ReviewTokenBudgetMax) {
		return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid review token budget %d",
			p.RepositoryID, *p.ReviewTokenBudget)
	}
	if p.AutoTriageMode != nil {
		value := *p.AutoTriageMode
		if value == "" {
			value = AutoTriageOff
		}
		if !ValidAutoTriageMode(value) {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid auto-triage mode %q",
				p.RepositoryID, value)
		}
		p.AutoTriageMode = &value
	}
	if p.VerifyMaxFindings != nil {
		if *p.VerifyMaxFindings < 0 {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid verify findings %d",
				p.RepositoryID, *p.VerifyMaxFindings)
		}
		if *p.VerifyMaxFindings == 0 {
			value := DefaultVerifyMaxFindings
			p.VerifyMaxFindings = &value
		}
	}
	if p.VerifyMaxFiles != nil {
		if *p.VerifyMaxFiles < 0 {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid verify files %d",
				p.RepositoryID, *p.VerifyMaxFiles)
		}
		if *p.VerifyMaxFiles == 0 {
			value := DefaultVerifyMaxFiles
			p.VerifyMaxFiles = &value
		}
	}
	if p.VerifyTokenShare != nil {
		if *p.VerifyTokenShare < 0 || *p.VerifyTokenShare > 100 {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid verify token share %d",
				p.RepositoryID, *p.VerifyTokenShare)
		}
		if *p.VerifyTokenShare == 0 {
			value := DefaultVerifyTokenShare
			p.VerifyTokenShare = &value
		}
	}
	if p.VerifyTimeShare != nil {
		if *p.VerifyTimeShare < 0 || *p.VerifyTimeShare > 100 {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid verify time share %d",
				p.RepositoryID, *p.VerifyTimeShare)
		}
		if *p.VerifyTimeShare == 0 {
			value := DefaultVerifyTimeShare
			p.VerifyTimeShare = &value
		}
	}
	if p.VerifyTokenMax != nil {
		if *p.VerifyTokenMax == 0 {
			value := int64(DefaultVerifyTokenMax)
			p.VerifyTokenMax = &value
		} else if *p.VerifyTokenMax < VerifyTokenMaxMin || *p.VerifyTokenMax > ReviewTokenBudgetMax {
			return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid verify token max %d",
				p.RepositoryID, *p.VerifyTokenMax)
		}
	}

	if p.ExcludeGlobs != nil {
		raw := bytes.TrimSpace(p.ExcludeGlobs)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			p.ExcludeGlobs = datatypes.JSON([]byte(`[]`))
		} else {
			var globs []string
			if err := json.Unmarshal(raw, &globs); err != nil {
				return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid exclude globs: %w",
					p.RepositoryID, err)
			}
			if globs == nil {
				globs = []string{}
			}
			encoded, err := json.Marshal(globs)
			if err != nil {
				return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: encode exclude globs: %w",
					p.RepositoryID, err)
			}
			p.ExcludeGlobs = datatypes.JSON(encoded)
		}
	}

	if p.RequiredChecks != nil {
		raw := bytes.TrimSpace(p.RequiredChecks)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			p.RequiredChecks = datatypes.JSON([]byte(`[]`))
		} else {
			var checks []RequiredCheck
			if err := json.Unmarshal(raw, &checks); err != nil {
				return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: invalid required checks: %w",
					p.RepositoryID, err)
			}
			encoded, err := NormalizeRequiredChecks(checks)
			if err != nil {
				return RepositoryPolicy{}, fmt.Errorf("save repository policy %d: %w", p.RepositoryID, err)
			}
			p.RequiredChecks = encoded
		}
	}
	return p, nil
}
