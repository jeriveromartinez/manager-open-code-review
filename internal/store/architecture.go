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
	// ArchitectureViolation marks a verdict whose trusted baseline rules were
	// violated by the change. It stays observational: the control flag on the
	// verdict records that it does not write the check.
	ArchitectureViolation = "violation"

	// Baseline statuses. Unknown is the safe default when the baseline is
	// missing, unreadable, replaced or changed by the pull request itself.
	ArchitectureBaselinePass      = "pass"
	ArchitectureBaselineViolation = "violation"
	ArchitectureBaselineUnknown   = "unknown"

	// Decision statuses. Only resolved is a decision the analysis could use.
	ArchitectureDecisionResolved   = "resolved"
	ArchitectureDecisionReplaced   = "replaced"
	ArchitectureDecisionMissing    = "missing"
	ArchitectureDecisionUnreadable = "unreadable"

	// Dependency check statuses.
	ArchitectureDependencyViolation = "violation"
	ArchitectureDependencyException = "allowed_exception"

	// Exception policies. Deny never honors an exception; baseline honors the
	// ones declared in the trusted baseline, with an approver and a reason.
	ArchitectureExceptionsDeny     = "deny"
	ArchitectureExceptionsBaseline = "baseline"
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
	// Baseline is the deterministic check against the trusted architecture
	// baseline. It is nil when the mode is off or the diff could not be
	// inventoried.
	Baseline *ArchitectureBaseline `json:"baseline,omitempty"`
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

// ArchitectureBaseline is the result of checking the change against the
// architecture rules resolved from the trusted base commit. Every reference
// carries its identifier, path, version and blob SHA so the verdict can be
// audited and reproduced.
type ArchitectureBaseline struct {
	Status    string `json:"status"`
	RulesPath string `json:"rules_path,omitempty"`
	RulesSHA  string `json:"rules_sha,omitempty"`
	// RulesHeadSHA is the rule file the pull request proposes. It is evidence
	// only: it is recorded for the diff, never used to validate the change.
	RulesHeadSHA string `json:"rules_head_sha,omitempty"`
	RulesVersion string `json:"rules_version,omitempty"`
	BaseSHA      string `json:"base_sha,omitempty"`
	// ChangedInPR reports that the base and head rule files differ. The check
	// keeps evaluating the base rules, so a rule relaxed by the pull request
	// cannot approve the violation it introduces.
	ChangedInPR bool                           `json:"changed_in_pr,omitempty"`
	Decisions   []ArchitectureBaselineDecision `json:"decisions,omitempty"`
	Rules       []ArchitectureBaselineRule     `json:"rules,omitempty"`
	Checks      []ArchitectureDependencyCheck  `json:"checks,omitempty"`
	Controls    ArchitectureBaselineControls   `json:"controls"`
	Errors      []string                       `json:"errors,omitempty"`
}

type ArchitectureBaselineDecision struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Status  string `json:"status"`
}

type ArchitectureBaselineRule struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	From    string `json:"from"`
	To      string `json:"to"`
	Reason  string `json:"reason,omitempty"`
}

// ArchitectureDependencyCheck cites the rule, the modules and the concrete
// change that crossed a forbidden dependency direction.
type ArchitectureDependencyCheck struct {
	RuleID      string `json:"rule_id"`
	FromModule  string `json:"from_module"`
	ToModule    string `json:"to_module"`
	File        string `json:"file"`
	Import      string `json:"import"`
	Status      string `json:"status"`
	ExceptionID string `json:"exception_id,omitempty"`
}

// ArchitectureBaselineControls records which controls actually ran and when
// the baseline was missing.
type ArchitectureBaselineControls struct {
	BaseOnly          bool   `json:"base_only"`
	RuleFileChanged   bool   `json:"rule_file_changed"`
	BaselineMissing   bool   `json:"baseline_missing"`
	ExceptionsPolicy  string `json:"exceptions_policy"`
	DecisionsChecked  int    `json:"decisions_checked"`
	RulesChecked      int    `json:"rules_checked"`
	FilesChecked      int    `json:"files_checked"`
	ExceptionsApplied int    `json:"exceptions_applied"`
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
