package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Criterion assessment statuses (#232). They are the only values the store
// accepts; an evaluator answer outside this set is normalized before it reaches
// here.
const (
	CriterionSatisfied     = "cumplido"
	CriterionUnsatisfied   = "incumplido"
	CriterionUnverifiable  = "no verificable"
	CriterionNotApplicable = "no aplicable"
)

// ValidCriterionStatus reports whether status is one the panel and the
// publisher know.
func ValidCriterionStatus(status string) bool {
	switch status {
	case CriterionSatisfied, CriterionUnsatisfied, CriterionUnverifiable, CriterionNotApplicable:
		return true
	default:
		return false
	}
}

// CriterionEvidence is one file range that supports a criterion verdict. Digest
// is the content hash read at the evaluated head; a later evaluation compares
// it to decide whether the evidence still describes the current checkout.
type CriterionEvidence struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Test      bool   `json:"test,omitempty"`
	Digest    string `json:"digest,omitempty"`
}

// CriterionAssessment is the current verdict of one criterion of the linked
// issue for one pull request (#232). The criterion keeps the quote of its
// source, the source version and the fingerprint of its text, so a changed
// issue or a moved source is detectable instead of silently reusing a stale
// answer.
type CriterionAssessment struct {
	ID            uint   `gorm:"column:id;primaryKey"`
	PullRequestID uint   `gorm:"column:pull_request_id"`
	ReviewRunID   uint   `gorm:"column:review_run_id"`
	HeadSHA       string `gorm:"column:head_sha"`
	BaseRef       string `gorm:"column:base_ref"`

	CriterionID   string `gorm:"column:criterion_id"`
	Fingerprint   string `gorm:"column:fingerprint"`
	SourceKind    string `gorm:"column:source_kind"`
	SourceRepo    string `gorm:"column:source_repo"`
	SourceNumber  int    `gorm:"column:source_number"`
	SourceURL     string `gorm:"column:source_url"`
	SourceTitle   string `gorm:"column:source_title"`
	SourceVersion string `gorm:"column:source_version"`
	Text          string `gorm:"column:criterion_text"`

	Status      string         `gorm:"column:status"`
	Evidence    datatypes.JSON `gorm:"column:evidence"`
	Observed    string         `gorm:"column:observed"`
	Required    string         `gorm:"column:required"`
	Reason      string         `gorm:"column:reason"`
	Uncertainty string         `gorm:"column:uncertainty"`
	Evaluator   string         `gorm:"column:evaluator"`

	EvaluatedAt       time.Time  `gorm:"column:evaluated_at"`
	InvalidatedAt     *time.Time `gorm:"column:invalidated_at"`
	InvalidatedReason string     `gorm:"column:invalidated_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName pins the table so a rename of the Go type cannot move the data.
func (CriterionAssessment) TableName() string { return "pr_criteria" }

// EncodeCriterionEvidence serializes evidence for storage. Nil becomes an empty
// array, never SQL NULL: an empty evidence list is a fact, not a missing value.
func EncodeCriterionEvidence(evidence []CriterionEvidence) datatypes.JSON {
	if evidence == nil {
		evidence = []CriterionEvidence{}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		// The struct has no unencodable fields; a failure is a programming error.
		panic(fmt.Sprintf("store: encode criterion evidence: %v", err))
	}
	return datatypes.JSON(encoded)
}

// DecodeCriterionEvidence reads the stored evidence. Malformed rows yield no
// evidence instead of failing a whole panel or publication.
func DecodeCriterionEvidence(raw datatypes.JSON) []CriterionEvidence {
	if len(raw) == 0 {
		return nil
	}
	var evidence []CriterionEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return nil
	}
	return evidence
}

// SaveCriterionAssessments replaces the current criterion verdicts of a pull
// request. The previous rows are invalidated in the same transaction, so the
// panel and the publisher never see two current answers for one criterion.
func (s *Store) SaveCriterionAssessments(ctx context.Context, assessments []CriterionAssessment) error {
	if len(assessments) == 0 {
		return errors.New("save criterion assessments: empty set")
	}
	prID := assessments[0].PullRequestID
	if prID == 0 {
		return errors.New("save criterion assessments: missing pull request id")
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Model(&CriterionAssessment{}).
			Where("pull_request_id = ? AND invalidated_at IS NULL", prID).
			Updates(map[string]any{
				"invalidated_at":     now,
				"invalidated_reason": "superseded",
				"updated_at":         now,
			}).Error; err != nil {
			return fmt.Errorf("invalidate criterion assessments of pull request %d: %w", prID, err)
		}
		for i := range assessments {
			if assessments[i].PullRequestID != prID {
				return fmt.Errorf("save criterion assessments: mixed pull request ids %d and %d", prID, assessments[i].PullRequestID)
			}
			if !ValidCriterionStatus(assessments[i].Status) {
				return fmt.Errorf("save criterion assessment %q: invalid status %q", assessments[i].CriterionID, assessments[i].Status)
			}
			if assessments[i].EvaluatedAt.IsZero() {
				assessments[i].EvaluatedAt = now
			}
			if len(assessments[i].Evidence) == 0 || string(assessments[i].Evidence) == "null" {
				// An empty citation list is a fact, not a missing value; the column
				// is NOT NULL so a deterministically settled criterion still
				// persists an explicit empty array.
				assessments[i].Evidence = datatypes.JSON([]byte(`[]`))
			}
			assessments[i].InvalidatedAt = nil
			assessments[i].InvalidatedReason = ""
		}
		if err := tx.CreateInBatches(assessments, 100).Error; err != nil {
			return fmt.Errorf("save criterion assessments of pull request %d: %w", prID, err)
		}
		return nil
	})
}

// CurrentCriterionAssessments returns the current verdicts of a pull request.
// A non-empty headSHA restricts the answer to that head, which is what the
// publisher uses so a moved head cannot publish evidence from the old one.
func (s *Store) CurrentCriterionAssessments(ctx context.Context, prID uint, headSHA string) ([]CriterionAssessment, error) {
	query := s.DB.WithContext(ctx).
		Where("pull_request_id = ? AND invalidated_at IS NULL", prID)
	if headSHA != "" {
		query = query.Where("head_sha = ?", headSHA)
	}
	var assessments []CriterionAssessment
	if err := query.Order("source_number, criterion_id").Find(&assessments).Error; err != nil {
		return nil, fmt.Errorf("list criterion assessments of pull request %d: %w", prID, err)
	}
	return assessments, nil
}

// InvalidateCriterionAssessments closes the current verdicts of a pull request
// without a replacement. It is used when the comparison moves and no run has
// evaluated the new head yet, so stale evidence is never published as current.
func (s *Store) InvalidateCriterionAssessments(ctx context.Context, prID uint, reason string) error {
	now := time.Now().UTC()
	if err := s.DB.WithContext(ctx).Model(&CriterionAssessment{}).
		Where("pull_request_id = ? AND invalidated_at IS NULL", prID).
		Updates(map[string]any{
			"invalidated_at":     now,
			"invalidated_reason": reason,
			"updated_at":         now,
		}).Error; err != nil {
		return fmt.Errorf("invalidate criterion assessments of pull request %d: %w", prID, err)
	}
	return nil
}
