package store

import (
	"encoding/json"
	"strings"
)

// Architecture analysis modes and verdict states. The mode is always
// observation on this branch: the verdict is published as evidence but never
// changes the check, the QA status or the inline comments.
const (
	ArchitectureAnalysisOff         = "off"
	ArchitectureAnalysisObservation = "observation"

	ArchitectureNotRequested = "not_requested"
	ArchitectureComplete     = "complete"
	ArchitecturePartial      = "partial"
	ArchitectureInconclusive = "inconclusive"
)

// ArchitectureVerdict is the structured result of the architectural analysis
// of one base–head comparison. It is stored as JSON on the review run, so the
// panel, the summary comment and the check run all read the same object.
type ArchitectureVerdict struct {
	Status    string `json:"status"`
	Requested bool   `json:"requested"`
	Mode      string `json:"mode"`
	// Signals are the explicit change facts that activated the analysis.
	Signals    []string               `json:"signals,omitempty"`
	Comparison ArchitectureComparison `json:"comparison"`
	Scope      ArchitectureScope      `json:"scope"`
	// Dimensions are the report sections OCR accepted, with their evidence.
	Dimensions []ArchitectureDimension `json:"dimensions,omitempty"`
	// BehaviorChanges, Risks, Alternatives, AcceptanceConditions and
	// Uncertainties are extracted from the report headings so the panel can
	// show them without parsing Markdown.
	BehaviorChanges      []string             `json:"behavior_changes,omitempty"`
	Risks                []string             `json:"risks,omitempty"`
	Alternatives         []string             `json:"alternatives,omitempty"`
	AcceptanceConditions []string             `json:"acceptance_conditions,omitempty"`
	Uncertainties        []string             `json:"uncertainties,omitempty"`
	Evidence             ArchitectureEvidence `json:"evidence"`
	Controls             ArchitectureControls `json:"controls"`
	Budget               ArchitectureBudget   `json:"budget"`
	Usage                ArchitectureUsage    `json:"usage"`
	// Recommendation is the original maintainer recommendation from the
	// analysis. It stays as evidence even after a human decision changes the
	// effective outcome; EffectiveDecision records that later outcome.
	Recommendation     string   `json:"recommendation,omitempty"`
	EffectiveDecision  string   `json:"effective_decision,omitempty"`
	ExecutionStatus    string   `json:"execution_status,omitempty"`
	ReviewStatus       string   `json:"review_status,omitempty"`
	DeclaredCompletion string   `json:"declared_completion,omitempty"`
	StopReason         string   `json:"stop_reason,omitempty"`
	Model              string   `json:"model,omitempty"`
	SessionID          string   `json:"session_id,omitempty"`
	Errors             []string `json:"errors,omitempty"`
}

type ArchitectureComparison struct {
	Repository  string `json:"repository,omitempty"`
	Mode        string `json:"mode,omitempty"`
	BaseRef     string `json:"base_ref,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	BaseSHA     string `json:"base_sha,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	BaseIsEmpty bool   `json:"base_is_empty,omitempty"`
}

type ArchitectureScope struct {
	Included   []ArchitectureScopeFile `json:"included,omitempty"`
	Excluded   []ArchitectureScopeFile `json:"excluded,omitempty"`
	OutOfScope []string                `json:"out_of_scope,omitempty"`
}

type ArchitectureScopeFile struct {
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ArchitectureDimension struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ArchitectureEvidence struct {
	SchemaVersion       string                  `json:"schema_version,omitempty"`
	Markdown            string                  `json:"markdown,omitempty"`
	ArtifactDir         string                  `json:"artifact_dir,omitempty"`
	ObservationIDs      []string                `json:"observation_ids,omitempty"`
	CitedObservationIDs []string                `json:"cited_observation_ids,omitempty"`
	EngineScope         []ArchitectureScopeFile `json:"engine_scope,omitempty"`
}

// ArchitectureControls records what the analysis was allowed to do. On this
// branch it is always observation: it has no write decision.
type ArchitectureControls struct {
	Observation    bool `json:"observation"`
	WritesDecision bool `json:"writes_decision"`
}

type ArchitectureBudget struct {
	TokenBudget       int64 `json:"token_budget"`
	TokenShare        int   `json:"token_share"`
	TimeBudgetSeconds int64 `json:"time_budget_seconds"`
	TimeShare         int   `json:"time_share"`
}

type ArchitectureUsage struct {
	Known      bool  `json:"known"`
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Total      int64 `json:"total"`
}

// DecodeArchitectureVerdict reads a stored verdict. An empty or malformed value
// yields the zero verdict, which the publisher treats as "nothing to show".
func DecodeArchitectureVerdict(raw []byte) ArchitectureVerdict {
	var verdict ArchitectureVerdict
	if len(strings.TrimSpace(string(raw))) == 0 {
		return verdict
	}
	_ = json.Unmarshal(raw, &verdict)
	return verdict
}

// ArchitectureVerdictJSON is the canonical encoding persisted on the run.
func ArchitectureVerdictJSON(v ArchitectureVerdict) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
