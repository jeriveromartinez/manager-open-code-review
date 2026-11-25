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

	// ArchitectureStale marks evidence that a later push, base change or rule
	// change invalidated. A stale verdict never counts as complete: its
	// dimensions stay unknown until a review re-evidences them (#163).
	ArchitectureStale = "stale"
)

// Versioned contract formats and their compatibility verdicts. The comparison
// is local and deterministic: it never asks the model and never guesses. A
// format without a parser is unknown, never compatible.
const (
	ContractFormatOpenAPI    = "openapi"
	ContractFormatJSONSchema = "json-schema"
	ContractFormatProto      = "proto"
	ContractFormatGoTypes    = "go-exported"
	ContractFormatUnknown    = "unknown"

	ContractCompatible   = "compatible"
	ContractIncompatible = "incompatible"
	ContractUnknown      = "unknown"

	ContractsNotDeclared = "not_declared"

	ContractChangeAdded    = "added"
	ContractChangeRemoved  = "removed"
	ContractChangeModified = "modified"
)

// Consumer impact states. The lookup is observation-only: it records what it
// could prove about authorized consumers in other repositories and never
// changes the check, the QA status or the review outcome.
const (
	ConsumersNotConfigured = "not_configured"
	ConsumersComplete      = "complete"
	ConsumersPartial       = "partial"
	ConsumersUnknown       = "unknown"

	ConsumerStatusAffected   = "affected"
	ConsumerStatusCompatible = "compatible"
	ConsumerStatusUnknown    = "unknown"
	ConsumerStatusOutOfScope = "out_of_scope"
	ConsumerStatusSkipped    = "skipped"

	ConsumerImpactVerified = "verified"
	ConsumerImpactNone     = "none"
	ConsumerImpactUnknown  = "unknown"

	ConsumerSourcePolicy   = "policy"
	ConsumerSourceBaseline = "baseline"

	ConsumerReasonUnauthorized      = "unauthorized"
	ConsumerReasonUnavailable       = "unavailable"
	ConsumerReasonTimeout           = "timeout"
	ConsumerReasonLookupFailed      = "lookup_failed"
	ConsumerReasonLookupBudget      = "lookup_budget_exhausted"
	ConsumerReasonTooLarge          = "contract_too_large"
	ConsumerReasonNoDigest          = "no_digest"
	ConsumerReasonContractUnchanged = "contract_not_changed"
	ConsumerReasonDifferentVersion  = "different_version"
	ConsumerReasonOnHead            = "already_on_head"
	ConsumerReasonOnBase            = "still_on_base"
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
	// OpsEvidence is the deterministic evaluation of the operational dimensions
	// the change touches. It is computed from the diff, not from the engine
	// report, and never changes the review outcome.
	OpsEvidence *OpsEvidence `json:"ops_evidence,omitempty"`
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
	// Contracts is the deterministic base–head comparison of versioned public
	// contracts. It is independent of the model analysis and its status never
	// changes the review outcome.
	Contracts ArchitectureContracts `json:"contracts"`
	// Consumers records the authorized producer–consumer dependencies of the
	// changed contracts, the pinned revision consulted for each one and the
	// remaining evidence gaps. It is independent of the model analysis and its
	// status never changes the review outcome.
	Consumers ArchitectureConsumers `json:"consumers"`
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
	// CarriedFrom is the head whose analysis still backs this verdict when the
	// current run did not re-evidence it. Empty means the verdict was produced
	// by this run.
	CarriedFrom string `json:"carried_from,omitempty"`
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
	// Sources binds the dimension's conclusion to the files whose content it
	// relied on. A later change to one of them invalidates the dimension until
	// the analysis runs again (#163).
	Sources []ArchitectureEvidenceSource `json:"sources,omitempty"`
	// Stale marks evidence that a later push, base change or rule change may
	// have invalidated. A stale dimension never asserts its previous outcome.
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
}

// ArchitectureEvidenceSource is one input the analysis relied on: a cited file,
// a contract, a baseline rule or a scope file. Digest is the content digest the
// evidence was bound to, so a later push can detect a change without trusting
// the model's wording (#163).
type ArchitectureEvidenceSource struct {
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type ArchitectureEvidence struct {
	SchemaVersion       string                  `json:"schema_version,omitempty"`
	Markdown            string                  `json:"markdown,omitempty"`
	ArtifactDir         string                  `json:"artifact_dir,omitempty"`
	ObservationIDs      []string                `json:"observation_ids,omitempty"`
	CitedObservationIDs []string                `json:"cited_observation_ids,omitempty"`
	EngineScope         []ArchitectureScopeFile `json:"engine_scope,omitempty"`
	// RuleVersion is the version of the rules the analysis checked. Evidence
	// built under another version is stale and has to be re-evidenced (#163).
	RuleVersion string `json:"rule_version,omitempty"`
	// Sources is the union of the dimension sources, with their digests.
	Sources []ArchitectureEvidenceSource `json:"sources,omitempty"`
}

// ArchitectureControls records what the analysis was allowed to do. On this
// branch it is always observation: it has no write decision.
type ArchitectureControls struct {
	Observation    bool `json:"observation"`
	WritesDecision bool `json:"writes_decision"`
	// ContractsReadOnly records that the contract adapter only reads the base
	// and head trees; it never writes to the repository.
	ContractsReadOnly bool `json:"contracts_read_only"`
}

// ArchitectureContracts is the aggregate of every versioned contract the
// change touches. Contracts holds only the entries that differ between base and
// head, so an unchanged contract does not add noise to the verdict.
type ArchitectureContracts struct {
	Status    string                 `json:"status"`
	Declared  int                    `json:"declared"`
	Compared  int                    `json:"compared"`
	Contracts []ArchitectureContract `json:"contracts,omitempty"`
	Errors    []string               `json:"errors,omitempty"`
}

// ArchitectureContract is one contract file compared between base and head.
// Adapter names the parser that produced the result; digests identify the exact
// inputs, so the same content always yields the same verdict.
type ArchitectureContract struct {
	Path       string                         `json:"path"`
	Format     string                         `json:"format"`
	Adapter    string                         `json:"adapter"`
	Status     string                         `json:"status"`
	Change     string                         `json:"change,omitempty"`
	BaseDigest string                         `json:"base_digest,omitempty"`
	HeadDigest string                         `json:"head_digest,omitempty"`
	Evidence   []ArchitectureContractEvidence `json:"evidence,omitempty"`
	Errors     []string                       `json:"errors,omitempty"`
}

// ArchitectureContractEvidence points at the exact operation or field whose
// compatibility changed and names the objective break, independent of whether
// the team accepts it.
type ArchitectureContractEvidence struct {
	Kind    string `json:"kind"`
	Pointer string `json:"pointer,omitempty"`
	Detail  string `json:"detail"`
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

// Operations evidence states. A dimension that applies but lacks evidence is
// unknown, never satisfied: a green check cannot substitute an ordered,
// reversible migration or a rollback path.
const (
	OpsStatusNotApplicable = "not_applicable"
	OpsStatusSatisfied     = "satisfied"
	OpsStatusUnknown       = "unknown"
	OpsStatusRisk          = "risk"

	OpsDimensionSchema      = "schema"
	OpsDimensionTopology    = "topology"
	OpsDimensionPermissions = "permissions"
	OpsDimensionLoad        = "load"
)

// OpsEvidence is the per-dimension result of the operational review: what the
// change touches, what evidence it carries and what is still missing. It is
// evidence only; the controls record that it cannot change a decision.
type OpsEvidence struct {
	Status     string         `json:"status"`
	Dimensions []OpsDimension `json:"dimensions,omitempty"`
	Controls   OpsControls    `json:"controls"`
	// Error records why the evaluation could not inspect the change at all
	// (for example, an unreadable diff inventory). An error is an unknown, not
	// a pass.
	Error string `json:"error,omitempty"`
}

// OpsDimension records the verdict and evidence of one operational dimension.
// Applies distinguishes "not touched by this change" from "touched but
// unproven"; only the latter escalates an uncertainty.
type OpsDimension struct {
	Name     string   `json:"name"`
	Applies  bool     `json:"applies"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence,omitempty"`
	Missing  []string `json:"missing,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

type OpsControls struct {
	Observation    bool `json:"observation"`
	WritesDecision bool `json:"writes_decision"`
}

// HasApplicable reports whether any dimension was touched by the change.
func (o OpsEvidence) HasApplicable() bool {
	for _, dimension := range o.Dimensions {
		if dimension.Applies {
			return true
		}
	}
	return false
}

// ArchitectureConsumers is the aggregate of the consumer lookup. Checked counts
// the entries that were actually queried; an entry outside the changed
// contracts or past the lookup budget stays visible without being contacted.
type ArchitectureConsumers struct {
	Status        string                 `json:"status"`
	Configured    int                    `json:"configured"`
	Checked       int                    `json:"checked"`
	Essential     int                    `json:"essential,omitempty"`
	RequiresHuman bool                   `json:"requires_human,omitempty"`
	Consumers     []ArchitectureConsumer `json:"consumers,omitempty"`
	Sources       []string               `json:"sources,omitempty"`
	Errors        []string               `json:"errors,omitempty"`
}

// ArchitectureConsumer is one configured dependency. SHA is the pinned commit
// the consumer ref resolved to; ContractSHA and Digest identify the exact
// consumer-side contract read, so the same input always yields the same result.
type ArchitectureConsumer struct {
	Repository     string `json:"repository"`
	Contract       string `json:"contract"`
	Path           string `json:"path,omitempty"`
	Ref            string `json:"ref,omitempty"`
	SHA            string `json:"sha,omitempty"`
	ContractSHA    string `json:"contract_sha,omitempty"`
	Version        string `json:"version,omitempty"`
	Status         string `json:"status"`
	Impact         string `json:"impact"`
	Essential      bool   `json:"essential,omitempty"`
	Source         string `json:"source,omitempty"`
	Private        bool   `json:"private,omitempty"`
	Redacted       bool   `json:"redacted,omitempty"`
	Digest         string `json:"digest,omitempty"`
	ProducerDigest string `json:"producer_digest,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// MaskRedactedConsumers clears every identifying field of a consumer whose
// details may not be published. The verdict is persisted and served to the
// panel, so redaction has to happen before serialization and not only in the
// summary renderer.
func MaskRedactedConsumers(verdict ArchitectureVerdict) ArchitectureVerdict {
	verdict.Consumers = MaskRedactedConsumerImpact(verdict.Consumers)
	return verdict
}

// MaskRedactedConsumerImpact masks the consumer aggregate itself, so the
// control process can persist a snapshot before the worker ever sees it.
func MaskRedactedConsumerImpact(consumers ArchitectureConsumers) ArchitectureConsumers {
	for i := range consumers.Consumers {
		consumer := &consumers.Consumers[i]
		if !consumer.Redacted {
			continue
		}
		consumer.Repository = ""
		consumer.Contract = ""
		consumer.Path = ""
		consumer.Ref = ""
		consumer.SHA = ""
		consumer.ContractSHA = ""
		consumer.Version = ""
		consumer.Digest = ""
		consumer.ProducerDigest = ""
	}
	return consumers
}

// ConsumerImpactJSON encodes the masked consumer snapshot persisted on a run.
func ConsumerImpactJSON(consumers ArchitectureConsumers) []byte {
	raw, err := json.Marshal(consumers)
	if err != nil {
		return nil
	}
	return raw
}

// DecodeConsumerImpact reads the snapshot the control process stored on the
// run. An empty value yields the zero aggregate, which the worker treats as a
// missing lookup and fills with an explicit unavailable result.
func DecodeConsumerImpact(raw []byte) ArchitectureConsumers {
	var consumers ArchitectureConsumers
	if len(strings.TrimSpace(string(raw))) == 0 {
		return consumers
	}
	_ = json.Unmarshal(raw, &consumers)
	return consumers
}

// HasRedactedConsumers reports whether any stored consumer must be masked
// before serving the verdict.
func HasRedactedConsumers(verdict ArchitectureVerdict) bool {
	for _, consumer := range verdict.Consumers.Consumers {
		if consumer.Redacted {
			return true
		}
	}
	return false
}

// HasContent reports whether there is anything to publish: an applicable
// dimension or a failure that made the evaluation unknown.
func (o OpsEvidence) HasContent() bool {
	return o.HasApplicable() || strings.TrimSpace(o.Error) != ""
}

// RiskItems are the material operational risks, in dimension order, so the
// verdict can surface them next to the engine's own risks.
func (o OpsEvidence) RiskItems() []string {
	var items []string
	for _, dimension := range o.Dimensions {
		if !dimension.Applies || dimension.Status != OpsStatusRisk || dimension.Reason == "" {
			continue
		}
		items = append(items, "ops/"+dimension.Name+": "+dimension.Reason)
	}
	return items
}

// UncertaintyItems are the applicable dimensions that lack evidence. A missing
// dimension is an unproven area, not a pass. An evaluation error is reported as
// an inventory uncertainty so a failed diff cannot look silent.
func (o OpsEvidence) UncertaintyItems() []string {
	var items []string
	if errorText := strings.TrimSpace(o.Error); errorText != "" {
		items = append(items, "ops/inventory: "+errorText)
	}
	for _, dimension := range o.Dimensions {
		if !dimension.Applies || dimension.Status != OpsStatusUnknown {
			continue
		}
		item := "ops/" + dimension.Name + ": " + dimension.Reason
		if len(dimension.Missing) > 0 {
			item += " (missing: " + strings.Join(dimension.Missing, ", ") + ")"
		}
		items = append(items, item)
	}
	return items
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
