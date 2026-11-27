package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Review rule statuses. A proposed rule is not applied; an observed rule is
// measured and shows its influence without changing a finding; an active rule
// may suppress a repeated false positive. A disabled or superseded row is
// history and never applies.
const (
	RuleStatusProposed    = "proposed"
	RuleStatusObservation = "observation"
	RuleStatusActive      = "active"
	RuleStatusDisabled    = "disabled"
	RuleStatusSuperseded  = "superseded"
)

// Review rule decisions. Withdraw suppresses a repeated false positive; confirm
// records that the class is a real defect and never hides it.
const (
	RuleDecisionWithdraw = "withdraw"
	RuleDecisionConfirm  = "confirm"
)

// RuleEvidenceValidity bounds how long a measured sample supports applying a
// rule. Once it expires the rule stops applying until it is evaluated again.
const RuleEvidenceValidity = 90 * 24 * time.Hour

// ErrInvalidRule marks a rule the store rejected. The API maps it to 400.
var ErrInvalidRule = errors.New("invalid review rule")

// ReviewRuleKey is the stable identity of a rule: repository, path scope and
// category. The decision is deliberately excluded, so changing the decision
// creates a version of the same rule instead of an unrelated one.
func ReviewRuleKey(repositoryID int64, pathScope, category string) string {
	parts := []string{
		fmt.Sprintf("%d", repositoryID),
		normalizeRulePathScope(pathScope),
		strings.ToLower(strings.TrimSpace(category)),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// EncodeRuleExamples stores the observed sample as JSON.
func EncodeRuleExamples(examples []RuleExample) datatypes.JSON {
	if examples == nil {
		return datatypes.JSON([]byte(`[]`))
	}
	encoded, err := json.Marshal(examples)
	if err != nil {
		return datatypes.JSON([]byte(`[]`))
	}
	return datatypes.JSON(encoded)
}

// DecodeRuleExamples reads the observed sample. A malformed value is an empty
// sample, which can never support applying a rule.
func DecodeRuleExamples(raw datatypes.JSON) []RuleExample {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var examples []RuleExample
	if err := json.Unmarshal(raw, &examples); err != nil {
		return nil
	}
	return examples
}

// EncodeRuleEvaluation stores the measured sample.
func EncodeRuleEvaluation(evaluation RuleEvaluation) datatypes.JSON {
	encoded, err := json.Marshal(evaluation)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}

// DecodeRuleEvaluation reads the measured sample. A malformed or absent value
// has a zero EvaluatedAt, which never counts as a valid observation.
func DecodeRuleEvaluation(raw datatypes.JSON) RuleEvaluation {
	var evaluation RuleEvaluation
	if len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &evaluation)
	}
	return evaluation
}

// DecodeRuleInfluences reads the rule audit stored on a finding. A malformed or
// absent value means no rule influenced it.
func DecodeRuleInfluences(raw datatypes.JSON) []RuleInfluence {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var influences []RuleInfluence
	if err := json.Unmarshal(raw, &influences); err != nil {
		return nil
	}
	return influences
}

// EncodeRuleInfluences stores the rule audit on a finding.
func EncodeRuleInfluences(influences []RuleInfluence) datatypes.JSON {
	if influences == nil {
		return datatypes.JSON([]byte(`[]`))
	}
	encoded, err := json.Marshal(influences)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}

// RuleTarget is the finding data the matcher compares. Title and Anchor are the
// evidence a human correction showed, so a rule cannot suppress a finding that
// merely shares its path and category.
type RuleTarget struct {
	Path     string
	Category string
	Title    string
	Anchor   string
}

// ReviewRuleMatches reports whether the rule, in its approved scope, applies to
// the target. The rule needs positive evidence: with no positive example there
// is nothing a person corrected, so it never matches.
func ReviewRuleMatches(rule ReviewRule, target RuleTarget) bool {
	if !rulePathScopeMatches(rule.PathScope, target.Path) {
		return false
	}
	if scoped := strings.TrimSpace(rule.Category); scoped != "" &&
		!strings.EqualFold(scoped, strings.TrimSpace(target.Category)) {
		return false
	}
	for _, example := range DecodeRuleExamples(rule.PositiveExamples) {
		if ruleEvidenceMatches(example, target) {
			return true
		}
	}
	return false
}

// ruleEvidenceMatches compares the evidence a person attached to an example with
// the target. A title or anchor is required: an example that only names a path
// cannot tell a false positive from a real defect of the same class.
func ruleEvidenceMatches(example RuleExample, target RuleTarget) bool {
	if title := normalizeRuleText(example.Title); title != "" &&
		title == normalizeRuleText(target.Title) {
		return true
	}
	if anchor := normalizeRuleText(example.Anchor); anchor != "" &&
		anchor == normalizeRuleText(target.Anchor) {
		return true
	}
	return false
}

// EvaluateReviewRule measures a rule against its own sample. The positive
// examples are repeated false positives the rule should suppress; the negative
// examples are real defects of the same class it must keep detecting. A rule
// that matches a negative example would hide a real defect, so it cannot be
// promoted.
func EvaluateReviewRule(rule ReviewRule, now time.Time) (RuleEvaluation, error) {
	positive := DecodeRuleExamples(rule.PositiveExamples)
	negative := DecodeRuleExamples(rule.NegativeExamples)
	for _, example := range append(append([]RuleExample(nil), positive...), negative...) {
		if err := validateRuleExample(example); err != nil {
			return RuleEvaluation{}, err
		}
	}

	evaluation := RuleEvaluation{
		EvaluatedAt: now.UTC(),
		SampleSize:  len(positive) + len(negative),
	}
	for _, example := range positive {
		if ReviewRuleMatches(rule, ruleTarget(example)) {
			evaluation.MatchedPositive++
		}
	}
	for _, example := range negative {
		if !ReviewRuleMatches(rule, ruleTarget(example)) {
			continue
		}
		evaluation.MatchedNegative++
		if ruleSeverityRank(example.Severity) >= ruleSeverityRank("major") {
			evaluation.SevereOmissions++
		}
	}
	if len(positive) > 0 {
		evaluation.FalsePositiveRepetition = float64(evaluation.MatchedPositive) / float64(len(positive))
	}
	return evaluation, nil
}

// RulePromotionError enforces the observation gate: a rule may only be promoted
// after it suppressed every positive example and no real defect, severe or not,
// was part of that match.
func RulePromotionError(rule ReviewRule, evaluation RuleEvaluation) error {
	positive := DecodeRuleExamples(rule.PositiveExamples)
	negative := DecodeRuleExamples(rule.NegativeExamples)
	if len(positive) == 0 {
		return fmt.Errorf("%w: rule has no positive example", ErrInvalidRule)
	}
	if len(negative) == 0 {
		return fmt.Errorf("%w: rule has no negative example", ErrInvalidRule)
	}
	if evaluation.EvaluatedAt.IsZero() {
		return fmt.Errorf("%w: rule has not been evaluated", ErrInvalidRule)
	}
	if evaluation.MatchedPositive != len(positive) {
		return fmt.Errorf("%w: rule repeats %d of %d false positives",
			ErrInvalidRule, evaluation.MatchedPositive, len(positive))
	}
	if evaluation.MatchedNegative > 0 || evaluation.SevereOmissions > 0 {
		return fmt.Errorf("%w: rule would suppress %d real defect(s), %d severe",
			ErrInvalidRule, evaluation.MatchedNegative, evaluation.SevereOmissions)
	}
	return nil
}

// ReviewRuleApplicable reports whether the rule may influence a finding at now.
// An observation or active rule whose evidence expired stops applying.
func ReviewRuleApplicable(rule ReviewRule, now time.Time) bool {
	if rule.Status != RuleStatusObservation && rule.Status != RuleStatusActive {
		return false
	}
	if rule.EvidenceExpiresAt == nil || !now.Before(*rule.EvidenceExpiresAt) {
		return false
	}
	return true
}

// RuleSpecificity orders overlapping rules: an exact path is more specific than
// a glob, a glob more than a directory prefix, and the whole repository last.
func RuleSpecificity(rule ReviewRule) int {
	scope := normalizeRulePathScope(rule.PathScope)
	switch {
	case scope == "":
		return 0
	case strings.HasSuffix(scope, "/"):
		return 1
	case strings.ContainsAny(scope, "*?["):
		return 2
	default:
		return 3
	}
}

// ProposeReviewRule creates the next version of a rule and supersedes the
// previous one. It is the only way a rule is born: a person converts a decision
// and a reason into explicit, versioned data.
func (s *Store) ProposeReviewRule(ctx context.Context, in ReviewRule) (ReviewRule, error) {
	normalized, err := validateReviewRule(in)
	if err != nil {
		return ReviewRule{}, err
	}

	var saved ReviewRule
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var maxVersion int
		if err := tx.Model(&ReviewRule{}).
			Where("repository_id = ? AND rule_key = ?", normalized.RepositoryID, normalized.RuleKey).
			Select("COALESCE(MAX(version), 0)").Scan(&maxVersion).Error; err != nil {
			return fmt.Errorf("read last review rule version: %w", err)
		}
		now := time.Now().UTC()
		normalized.Version = maxVersion + 1
		normalized.Status = RuleStatusProposed
		normalized.ID = 0
		normalized.ApprovedAt = nil
		normalized.ApprovedBy, normalized.ApprovedByID = "", nil
		normalized.SupersededBy = nil
		normalized.EvidenceExpiresAt = nil
		normalized.CreatedAt, normalized.UpdatedAt = now, now
		if err := tx.Create(&normalized).Error; err != nil {
			return fmt.Errorf("insert review rule: %w", err)
		}
		if err := supersedePreviousRules(tx, normalized, now); err != nil {
			return err
		}
		saved = normalized
		return nil
	})
	if err != nil {
		return ReviewRule{}, err
	}
	return saved, nil
}

// ReviewRule loads one version by id.
func (s *Store) ReviewRule(ctx context.Context, id uint) (ReviewRule, error) {
	var rule ReviewRule
	err := quietDB(s.DB.WithContext(ctx)).Take(&rule, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReviewRule{}, fmt.Errorf("review rule %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return ReviewRule{}, fmt.Errorf("load review rule %d: %w", id, err)
	}
	return rule, nil
}

// ReviewRulesForRepo lists every version of every rule of a repository, oldest
// first, so the panel can show the history and what replaced what.
func (s *Store) ReviewRulesForRepo(ctx context.Context, repoID int64) ([]ReviewRule, error) {
	var rules []ReviewRule
	err := s.DB.WithContext(ctx).
		Where("repository_id = ?", repoID).
		Order("rule_key, version").Find(&rules).Error
	if err != nil {
		return nil, fmt.Errorf("list review rules of repository %d: %w", repoID, err)
	}
	return rules, nil
}

// ApplicableReviewRules lists the observation and active rules of a repository
// whose measured evidence is still valid at now.
func (s *Store) ApplicableReviewRules(ctx context.Context, repoID int64, now time.Time) ([]ReviewRule, error) {
	var rules []ReviewRule
	err := s.DB.WithContext(ctx).
		Where("repository_id = ? AND status IN ? AND evidence_expires_at IS NOT NULL AND evidence_expires_at > ?",
			repoID, []string{RuleStatusObservation, RuleStatusActive}, now.UTC()).
		Order("rule_key, version DESC").Find(&rules).Error
	if err != nil {
		return nil, fmt.Errorf("list applicable review rules of repository %d: %w", repoID, err)
	}
	return rules, nil
}

// RecordReviewRuleEvaluation stores a measured sample. A rule that is already
// observing or active refreshes its evidence window, which is how an expired
// rule becomes applicable again after a fresh evaluation.
func (s *Store) RecordReviewRuleEvaluation(ctx context.Context, id uint, evaluation RuleEvaluation, now time.Time) (ReviewRule, error) {
	if evaluation.EvaluatedAt.IsZero() {
		evaluation.EvaluatedAt = now.UTC()
	}
	var saved ReviewRule
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rule ReviewRule
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&rule).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("review rule %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find review rule %d: %w", id, err)
		}
		if rule.Status == RuleStatusSuperseded {
			return fmt.Errorf("%w: a superseded rule cannot be evaluated", ErrInvalidRule)
		}
		updates := map[string]any{
			"evaluation": encodedJSON(evaluation),
			"updated_at": now.UTC(),
		}
		if rule.Status == RuleStatusObservation || rule.Status == RuleStatusActive {
			// A fresh sample that would hide a real defect invalidates the rule:
			// keeping the old expiry would let the stale evidence keep applying.
			if err := RulePromotionError(rule, evaluation); err != nil {
				updates["evidence_expires_at"] = nil
			} else {
				updates["evidence_expires_at"] = now.UTC().Add(RuleEvidenceValidity)
			}
		}
		if err := tx.Model(&ReviewRule{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return fmt.Errorf("record evaluation of review rule %d: %w", id, err)
		}
		saved = rule
		saved.Evaluation = encodedJSON(evaluation)
		saved.UpdatedAt = now.UTC()
		if expiry, ok := updates["evidence_expires_at"].(time.Time); ok {
			saved.EvidenceExpiresAt = &expiry
		}
		return nil
	})
	if err != nil {
		return ReviewRule{}, err
	}
	return saved, nil
}

// ObserveReviewRule promotes a measured rule to observation: it now records its
// influence on findings but never changes their status. The approval is kept
// with the person and the reason that allowed it.
func (s *Store) ObserveReviewRule(ctx context.Context, id uint, actor string, actorID *uint, now time.Time) (ReviewRule, error) {
	return s.promoteReviewRule(ctx, id, RuleStatusObservation, actor, actorID, now)
}

// ActivateReviewRule promotes a measured rule to active. An active withdraw rule
// may suppress a repeated false positive; it never suppresses a severe finding.
func (s *Store) ActivateReviewRule(ctx context.Context, id uint, actor string, actorID *uint, now time.Time) (ReviewRule, error) {
	return s.promoteReviewRule(ctx, id, RuleStatusActive, actor, actorID, now)
}

func (s *Store) promoteReviewRule(ctx context.Context, id uint, status, actor string, actorID *uint, now time.Time) (ReviewRule, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return ReviewRule{}, fmt.Errorf("%w: rule approval needs an author", ErrInvalidRule)
	}
	var saved ReviewRule
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rule ReviewRule
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&rule).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("review rule %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find review rule %d: %w", id, err)
		}
		switch rule.Status {
		case RuleStatusProposed, RuleStatusObservation:
		default:
			return fmt.Errorf("%w: cannot promote a %s rule", ErrInvalidRule, rule.Status)
		}
		if status == RuleStatusObservation && rule.Status == RuleStatusActive {
			return fmt.Errorf("%w: an active rule cannot go back to observation", ErrInvalidRule)
		}
		evaluation := DecodeRuleEvaluation(rule.Evaluation)
		if err := RulePromotionError(rule, evaluation); err != nil {
			return err
		}
		if !evaluation.EvaluatedAt.Add(RuleEvidenceValidity).After(now.UTC()) {
			return fmt.Errorf("%w: the observation is older than its evidence window", ErrInvalidRule)
		}
		approvedAt := now.UTC()
		expires := approvedAt.Add(RuleEvidenceValidity)
		if err := tx.Model(&ReviewRule{}).Where("id = ?", id).Updates(map[string]any{
			"status":              status,
			"approved_by":         actor,
			"approved_by_id":      actorID,
			"approved_at":         approvedAt,
			"evidence_expires_at": expires,
			"updated_at":          approvedAt,
		}).Error; err != nil {
			return fmt.Errorf("promote review rule %d: %w", id, err)
		}
		saved = rule
		saved.Status = status
		saved.ApprovedBy, saved.ApprovedByID = actor, actorID
		saved.ApprovedAt, saved.EvidenceExpiresAt = &approvedAt, &expires
		saved.UpdatedAt = approvedAt
		return nil
	})
	if err != nil {
		return ReviewRule{}, err
	}
	return saved, nil
}

// DisableReviewRule stops applying a rule. Its version history stays intact, so
// it can be reverted or evaluated again later.
func (s *Store) DisableReviewRule(ctx context.Context, id uint, now time.Time) (ReviewRule, error) {
	var saved ReviewRule
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rule ReviewRule
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&rule).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("review rule %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find review rule %d: %w", id, err)
		}
		if rule.Status == RuleStatusSuperseded {
			return fmt.Errorf("%w: a superseded rule cannot be disabled", ErrInvalidRule)
		}
		if err := tx.Model(&ReviewRule{}).Where("id = ?", id).Updates(map[string]any{
			"status":     RuleStatusDisabled,
			"updated_at": now.UTC(),
		}).Error; err != nil {
			return fmt.Errorf("disable review rule %d: %w", id, err)
		}
		saved = rule
		saved.Status = RuleStatusDisabled
		saved.UpdatedAt = now.UTC()
		return nil
	})
	if err != nil {
		return ReviewRule{}, err
	}
	return saved, nil
}

// RevertReviewRule restores the previous version of a rule as a new version.
// A revert never rewrites history: the current version is superseded and the
// restored behaviour starts in observation, so it is measured again before it
// can apply.
func (s *Store) RevertReviewRule(ctx context.Context, id uint, actor string, actorID *uint, now time.Time) (ReviewRule, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return ReviewRule{}, fmt.Errorf("%w: rule revert needs an author", ErrInvalidRule)
	}
	var saved ReviewRule
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current ReviewRule
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("review rule %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find review rule %d: %w", id, err)
		}
		if current.Status == RuleStatusSuperseded {
			return fmt.Errorf("%w: a superseded rule cannot be reverted", ErrInvalidRule)
		}
		var previous ReviewRule
		err := quietDB(tx).
			Where("repository_id = ? AND rule_key = ? AND version < ?",
				current.RepositoryID, current.RuleKey, current.Version).
			Order("version DESC").Take(&previous).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: rule has no previous version to revert to", ErrInvalidRule)
		}
		if err != nil {
			return fmt.Errorf("find previous review rule version: %w", err)
		}
		evaluation := DecodeRuleEvaluation(previous.Evaluation)
		if err := RulePromotionError(previous, evaluation); err != nil {
			return err
		}

		restored := previous
		restored.ID = 0
		restored.Version = current.Version + 1
		restored.Status = RuleStatusObservation
		restored.ApprovedBy, restored.ApprovedByID = actor, actorID
		restored.ApprovedAt = timePtr(now.UTC())
		restored.EvidenceExpiresAt = timePtr(now.UTC().Add(RuleEvidenceValidity))
		restored.SupersededBy = nil
		restored.CreatedAt, restored.UpdatedAt = now.UTC(), now.UTC()

		if err := tx.Model(&ReviewRule{}).Where("id = ?", current.ID).Updates(map[string]any{
			"status":     RuleStatusSuperseded,
			"updated_at": now.UTC(),
		}).Error; err != nil {
			return fmt.Errorf("supersede review rule %d: %w", current.ID, err)
		}
		if err := tx.Create(&restored).Error; err != nil {
			return fmt.Errorf("insert reverted review rule: %w", err)
		}
		if err := tx.Model(&ReviewRule{}).Where("id = ?", current.ID).
			Update("superseded_by", restored.ID).Error; err != nil {
			return fmt.Errorf("link reverted review rule: %w", err)
		}
		saved = restored
		return nil
	})
	if err != nil {
		return ReviewRule{}, err
	}
	return saved, nil
}

// RecordFindingRuleInfluences stores which rules matched a finding and whether
// they changed it. It never touches the status: applying the decision is a
// separate, audited triage.
func (s *Store) RecordFindingRuleInfluences(ctx context.Context, findingID string, influences []RuleInfluence) error {
	encoded := EncodeRuleInfluences(influences)
	if encoded == nil {
		return fmt.Errorf("encode rule influences of finding %q", findingID)
	}
	res := s.DB.WithContext(ctx).Model(&Finding{}).Where("id = ?", findingID).
		Updates(map[string]any{
			"rule_influences": encoded,
			"updated_at":      time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("record rule influences of finding %q: %w", findingID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("record rule influences of finding %q: %w", findingID, ErrNotFound)
	}
	return nil
}

func supersedePreviousRules(tx *gorm.DB, current ReviewRule, now time.Time) error {
	var previous []ReviewRule
	if err := tx.Model(&ReviewRule{}).
		Where("repository_id = ? AND rule_key = ? AND id <> ? AND status <> ?",
			current.RepositoryID, current.RuleKey, current.ID, RuleStatusSuperseded).
		Find(&previous).Error; err != nil {
		return fmt.Errorf("list previous review rule versions: %w", err)
	}
	for _, rule := range previous {
		if err := tx.Model(&ReviewRule{}).Where("id = ?", rule.ID).Updates(map[string]any{
			"status":        RuleStatusSuperseded,
			"superseded_by": current.ID,
			"updated_at":    now.UTC(),
		}).Error; err != nil {
			return fmt.Errorf("supersede review rule %d: %w", rule.ID, err)
		}
	}
	return nil
}

func validateReviewRule(in ReviewRule) (ReviewRule, error) {
	if in.RepositoryID <= 0 {
		return ReviewRule{}, fmt.Errorf("%w: missing repository", ErrInvalidRule)
	}
	switch in.Decision {
	case RuleDecisionWithdraw, RuleDecisionConfirm:
	default:
		return ReviewRule{}, fmt.Errorf("%w: unsupported decision %q", ErrInvalidRule, in.Decision)
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return ReviewRule{}, fmt.Errorf("%w: the human reason is required", ErrInvalidRule)
	}
	scope, err := normalizeRuleScope(in.PathScope)
	if err != nil {
		return ReviewRule{}, err
	}
	in.PathScope = scope
	in.Category = strings.TrimSpace(in.Category)
	if in.RuleKey == "" {
		in.RuleKey = ReviewRuleKey(in.RepositoryID, in.PathScope, in.Category)
	}
	positive := DecodeRuleExamples(in.PositiveExamples)
	if len(positive) == 0 {
		return ReviewRule{}, fmt.Errorf("%w: at least one positive example is required", ErrInvalidRule)
	}
	for _, example := range append(append([]RuleExample(nil), positive...), DecodeRuleExamples(in.NegativeExamples)...) {
		if err := validateRuleExample(example); err != nil {
			return ReviewRule{}, err
		}
	}
	in.PositiveExamples = EncodeRuleExamples(positive)
	in.NegativeExamples = EncodeRuleExamples(DecodeRuleExamples(in.NegativeExamples))
	return in, nil
}

func validateRuleExample(example RuleExample) error {
	if strings.TrimSpace(example.Path) == "" {
		return fmt.Errorf("%w: an example needs a path", ErrInvalidRule)
	}
	if normalizeRuleText(example.Title) == "" && normalizeRuleText(example.Anchor) == "" {
		return fmt.Errorf("%w: an example needs a title or an anchor", ErrInvalidRule)
	}
	switch ruleSeverityRank(example.Severity) {
	case 0:
		return fmt.Errorf("%w: unsupported example severity %q", ErrInvalidRule, example.Severity)
	}
	return nil
}

func normalizeRuleScope(scope string) (string, error) {
	scope = strings.TrimSpace(strings.ReplaceAll(scope, `\`, "/"))
	if scope == "" {
		return "", nil
	}
	if strings.HasPrefix(scope, "/") {
		return "", fmt.Errorf("%w: path scope must be repository relative", ErrInvalidRule)
	}
	for _, part := range strings.Split(scope, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: path scope escapes the repository", ErrInvalidRule)
		}
	}
	return scope, nil
}

func normalizeRulePathScope(scope string) string {
	scope = strings.TrimSpace(strings.ReplaceAll(scope, `\`, "/"))
	if scope == "" || strings.ContainsAny(scope, "*?[") {
		return scope
	}
	clean := path.Clean(scope)
	if clean == "." {
		return ""
	}
	if scope[len(scope)-1] == '/' && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean
}

func rulePathScopeMatches(scope, value string) bool {
	scope = normalizeRulePathScope(scope)
	value = normalizeRulePathScope(value)
	if scope == "" {
		return true
	}
	if strings.HasSuffix(scope, "/") {
		return strings.HasPrefix(value, scope)
	}
	if strings.ContainsAny(scope, "*?[") {
		matched, err := path.Match(scope, value)
		return err == nil && matched
	}
	return scope == value
}

func normalizeRuleText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func ruleTarget(example RuleExample) RuleTarget {
	return RuleTarget{
		Path: example.Path, Category: example.Category,
		Title: example.Title, Anchor: example.Anchor,
	}
}

func ruleSeverityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "blocker", "critical":
		return 4
	case "major", "high":
		return 3
	case "minor", "medium":
		return 2
	case "nit", "low":
		return 1
	case "":
		return 2
	default:
		return 0
	}
}

func encodedJSON(value any) datatypes.JSON {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}

func timePtr(value time.Time) *time.Time { return &value }
