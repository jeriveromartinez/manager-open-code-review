package api

import (
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// criterionDTO is one per-criterion verdict (#232) as the panel reads it. The
// source quote, its version and the head-bound evidence travel together so the
// panel can show an uncertainty instead of flattening it into a pass.
type criterionDTO struct {
	ID          string                 `json:"id"`
	Text        string                 `json:"text"`
	Status      string                 `json:"status"`
	Source      criterionSourceDTO     `json:"source"`
	Link        string                 `json:"link,omitempty"`
	Evidence    []criterionEvidenceDTO `json:"evidence"`
	Observed    string                 `json:"observed,omitempty"`
	Required    string                 `json:"required,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
	Uncertainty string                 `json:"uncertainty,omitempty"`
	Evaluator   string                 `json:"evaluator,omitempty"`
	HeadSHA     string                 `json:"head_sha"`
	EvaluatedAt string                 `json:"evaluated_at"`
}

type criterionSourceDTO struct {
	Kind    string `json:"kind"`
	Repo    string `json:"repo,omitempty"`
	Number  int    `json:"number,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type criterionEvidenceDTO struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Test      bool   `json:"test,omitempty"`
}

func criterionDTOOf(assessment store.CriterionAssessment) criterionDTO {
	evidence := store.DecodeCriterionEvidence(assessment.Evidence)
	items := make([]criterionEvidenceDTO, 0, len(evidence))
	for _, item := range evidence {
		items = append(items, criterionEvidenceDTO{
			Path:      item.Path,
			StartLine: item.StartLine,
			EndLine:   item.EndLine,
			Test:      item.Test,
		})
	}
	return criterionDTO{
		ID:     assessment.CriterionID,
		Text:   assessment.Text,
		Status: assessment.Status,
		Source: criterionSourceDTO{
			Kind:    assessment.SourceKind,
			Repo:    assessment.SourceRepo,
			Number:  assessment.SourceNumber,
			Title:   assessment.SourceTitle,
			Version: assessment.SourceVersion,
		},
		Link:        assessment.SourceURL,
		Evidence:    items,
		Observed:    assessment.Observed,
		Required:    assessment.Required,
		Reason:      assessment.Reason,
		Uncertainty: assessment.Uncertainty,
		Evaluator:   assessment.Evaluator,
		HeadSHA:     assessment.HeadSHA,
		EvaluatedAt: assessment.EvaluatedAt.UTC().Format(time.RFC3339),
	}
}
