package reviewbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Operational results a sample counts. A run that did not complete is still a
// result of the sample; it is never silently dropped from the denominators.
const (
	ResultCompleted          = "completed"
	ResultIncompleteCoverage = "incomplete_coverage"
	ResultAbstention         = "abstention"
	ResultTimeout            = "timeout"
	ResultDeliveryError      = "delivery_error"
)

// Reviewer kinds distinguish the project's own reviewer from an external one.
const (
	KindOwn      = "own"
	KindExternal = "external"
)

// Reviewer records who produced an observation, the version and the observable
// configuration. The name is sealed before the report is written.
type Reviewer struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Kind    string            `json:"kind"`
	Version string            `json:"version"`
	Config  map[string]string `json:"config,omitempty"`
}

// Coverage records the files the reviewer reports as reviewed.
type Coverage struct {
	Reviewed []string `json:"reviewed"`
}

// ObservedFinding is one comment imported from a reviewer export. Side follows
// the panel's citation sides: RIGHT is the head, LEFT the base.
type ObservedFinding struct {
	CommentID    string `json:"comment_id,omitempty"`
	Path         string `json:"path"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	Side         string `json:"side,omitempty"`
	Severity     string `json:"severity"`
	Category     string `json:"category"`
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	ExistingCode string `json:"existing_code,omitempty"`
	Blocking     bool   `json:"blocking,omitempty"`
}

// Observation is the documented import schema. It can represent an export from
// an external reviewer (for example CodeRabbit) and the project's own
// observations without translating the comparison: both must name the same
// base and the declared push head.
type Observation struct {
	SchemaVersion string `json:"schema_version"`
	CaseID        string `json:"case_id"`
	ManifestLock  string `json:"manifest_lock"`
	// Simulated marks fixtures that were produced by hand or by a script for
	// local validation. Real imports leave it false.
	Simulated    bool              `json:"simulated,omitempty"`
	Reviewer     Reviewer          `json:"reviewer"`
	Date         string            `json:"date"`
	Comparison   Comparison        `json:"comparison"`
	ReviewedHead string            `json:"reviewed_head"`
	Result       string            `json:"result"`
	LatencyMS    int64             `json:"latency_ms"`
	CostUSD      *float64          `json:"cost_usd,omitempty"`
	Coverage     Coverage          `json:"coverage"`
	Findings     []ObservedFinding `json:"findings"`
}

// Result is one validated observation bound to its fixed case. Findings are
// deduplicated and sorted; Duplicates is the number of duplicate comments
// removed. Superseded marks an observation of an earlier push, which is kept
// as an incomplete sample result.
type Result struct {
	Case        Case
	Observation Observation
	Duplicates  int
	Superseded  bool
}

// Dataset is a validated manifest plus its observations.
type Dataset struct {
	Manifest Manifest
	Results  []Result
}

// LoadObservations reads a JSON array of observations.
func LoadObservations(path string) ([]Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseObservations(data)
}

// ParseObservations decodes a strict JSON array. Unknown fields are rejected so
// a typo cannot silently change an observation.
func ParseObservations(data []byte) ([]Observation, error) {
	var observations []Observation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observations); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidObservation, err)
	}
	return observations, nil
}

// BuildDataset validates every observation against the manifest comparison,
// lock and reviewer metadata. It normalizes severities, sides and paths,
// deduplicates comments and downgrades a claimed completed run that did not
// cover the allowed context (or that reviewed an earlier push) to an incomplete
// coverage result.
func BuildDataset(manifest Manifest, observations []Observation) (Dataset, error) {
	if err := manifest.Validate(); err != nil {
		return Dataset{}, err
	}
	if manifest.Lock == "" {
		return Dataset{}, fmt.Errorf("%w: manifest lock is empty; call Finalize", ErrInvalidManifest)
	}
	cases := make(map[string]Case, len(manifest.Cases))
	for _, c := range manifest.Cases {
		cases[c.ID] = c
	}
	if manifest.TargetReviewer != "" {
		found := false
		for _, obs := range observations {
			if obs.Reviewer.ID == manifest.TargetReviewer {
				found = true
				break
			}
		}
		if !found {
			return Dataset{}, fmt.Errorf("%w: target_reviewer %q has no observations", ErrInvalidManifest, manifest.TargetReviewer)
		}
	}

	dataset := Dataset{Manifest: manifest}
	seenCase := make(map[string]bool, len(observations))
	reviewers := make(map[string]Reviewer)
	for _, raw := range observations {
		c, ok := cases[raw.CaseID]
		if !ok {
			return Dataset{}, fmt.Errorf("%w: %q", ErrUnknownCase, raw.CaseID)
		}
		if seenCase[raw.CaseID+"\x00"+raw.Reviewer.ID] {
			return Dataset{}, fmt.Errorf("%w: reviewer %q has more than one observation for case %q",
				ErrInvalidObservation, raw.Reviewer.ID, raw.CaseID)
		}
		if previous, ok := reviewers[raw.Reviewer.ID]; ok {
			if previous.Name != raw.Reviewer.Name || previous.Kind != raw.Reviewer.Kind ||
				previous.Version != raw.Reviewer.Version || !reflect.DeepEqual(previous.Config, raw.Reviewer.Config) {
				return Dataset{}, fmt.Errorf("%w: reviewer %q changed name, kind, version or config between observations",
					ErrInvalidObservation, raw.Reviewer.ID)
			}
		} else {
			reviewers[raw.Reviewer.ID] = raw.Reviewer
		}
		normalized, duplicates, superseded, err := normalizeObservation(manifest, c, raw)
		if err != nil {
			return Dataset{}, err
		}
		seenCase[raw.CaseID+"\x00"+raw.Reviewer.ID] = true
		dataset.Results = append(dataset.Results, Result{
			Case: c, Observation: normalized, Duplicates: duplicates, Superseded: superseded,
		})
	}
	sort.Slice(dataset.Results, func(i, j int) bool {
		if dataset.Results[i].Case.ID != dataset.Results[j].Case.ID {
			return dataset.Results[i].Case.ID < dataset.Results[j].Case.ID
		}
		return dataset.Results[i].Observation.Reviewer.ID < dataset.Results[j].Observation.Reviewer.ID
	})
	return dataset, nil
}

// Reviewers returns the unique reviewers sorted by stable ID.
func (d Dataset) Reviewers() []Reviewer {
	byID := make(map[string]Reviewer)
	for _, result := range d.Results {
		byID[result.Observation.Reviewer.ID] = result.Observation.Reviewer
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	reviewers := make([]Reviewer, 0, len(ids))
	for _, id := range ids {
		reviewers = append(reviewers, byID[id])
	}
	return reviewers
}

func normalizeObservation(manifest Manifest, c Case, raw Observation) (Observation, int, bool, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: case %s reviewer %s: %s",
			ErrInvalidObservation, raw.CaseID, raw.Reviewer.ID, fmt.Sprintf(format, args...))
	}
	if raw.SchemaVersion != ObservationSchemaVersion {
		return Observation{}, 0, false, invalid("schema_version %q, want %q", raw.SchemaVersion, ObservationSchemaVersion)
	}
	if !strings.EqualFold(raw.ManifestLock, manifest.Lock) {
		return Observation{}, 0, false, fmt.Errorf("%w: observation locked to %q, manifest is %q",
			ErrLockMismatch, raw.ManifestLock, manifest.Lock)
	}
	if strings.TrimSpace(raw.Reviewer.ID) == "" || strings.TrimSpace(raw.Reviewer.Name) == "" ||
		strings.TrimSpace(raw.Reviewer.Version) == "" {
		return Observation{}, 0, false, invalid("reviewer id, name and version are required")
	}
	if raw.Reviewer.Kind != KindOwn && raw.Reviewer.Kind != KindExternal {
		return Observation{}, 0, false, invalid("reviewer kind %q, want %q or %q", raw.Reviewer.Kind, KindOwn, KindExternal)
	}
	if _, err := time.Parse(time.RFC3339, raw.Date); err != nil {
		return Observation{}, 0, false, invalid("date must be RFC3339: %v", err)
	}
	if err := validateResult(raw.Result); err != nil {
		return Observation{}, 0, false, invalid("%v", err)
	}
	if raw.LatencyMS < 0 {
		return Observation{}, 0, false, invalid("latency_ms must not be negative")
	}
	if raw.CostUSD != nil && *raw.CostUSD < 0 {
		return Observation{}, 0, false, invalid("cost_usd must not be negative")
	}
	if raw.Comparison.Base != c.Comparison.Base {
		return Observation{}, 0, false, fmt.Errorf("%w: base %q, fixed comparison is %q",
			ErrComparisonMismatch, raw.Comparison.Base, c.Comparison.Base)
	}
	if raw.ReviewedHead == "" || raw.Comparison.Head != raw.ReviewedHead {
		return Observation{}, 0, false, fmt.Errorf("%w: comparison head %q and reviewed_head %q must agree",
			ErrComparisonMismatch, raw.Comparison.Head, raw.ReviewedHead)
	}
	if !pushDeclared(c, raw.ReviewedHead) {
		return Observation{}, 0, false, fmt.Errorf("%w: head %q is not a declared push of the case",
			ErrComparisonMismatch, raw.ReviewedHead)
	}

	allowed := make(map[string]bool, len(c.AllowedContext))
	for _, path := range c.AllowedContext {
		allowed[normalizePath(path)] = true
	}
	reviewed := make([]string, 0, len(raw.Coverage.Reviewed))
	reviewedSeen := make(map[string]bool, len(raw.Coverage.Reviewed))
	for _, path := range raw.Coverage.Reviewed {
		normalized := normalizePath(path)
		if normalized == "" || !allowed[normalized] {
			return Observation{}, 0, false, invalid("coverage path %q is outside allowed_context", path)
		}
		if !reviewedSeen[normalized] {
			reviewedSeen[normalized] = true
			reviewed = append(reviewed, normalized)
		}
	}
	sort.Strings(reviewed)

	findings := make([]ObservedFinding, 0, len(raw.Findings))
	for i, finding := range raw.Findings {
		normalized, err := normalizeObservedFinding(finding)
		if err != nil {
			return Observation{}, 0, false, fmt.Errorf(
				"%w: case %s reviewer %s: findings[%d]: %w",
				ErrInvalidObservation, raw.CaseID, raw.Reviewer.ID, i, err)
		}
		findings = append(findings, normalized)
	}
	findings, duplicates := dedupeFindings(findings, manifest.ContextRadius)

	normalized := raw
	normalized.Coverage.Reviewed = reviewed
	normalized.Findings = findings
	superseded := raw.ReviewedHead != c.Comparison.Head
	if superseded {
		// A review of an earlier push is not the fixed comparison; it is kept as
		// an incomplete sample result instead of being evaluated as final.
		normalized.Result = ResultIncompleteCoverage
	} else if raw.Result == ResultCompleted && !coversAllowedContext(c, reviewedSeen) {
		normalized.Result = ResultIncompleteCoverage
	}
	return normalized, duplicates, superseded, nil
}

func normalizeObservedFinding(raw ObservedFinding) (ObservedFinding, error) {
	normalized := raw
	normalized.Path = normalizePath(raw.Path)
	if normalized.Path == "" {
		return ObservedFinding{}, fmt.Errorf("path is required")
	}
	if raw.StartLine < 1 || raw.EndLine < raw.StartLine {
		return ObservedFinding{}, fmt.Errorf("range %d-%d is invalid", raw.StartLine, raw.EndLine)
	}
	side, err := normalizeSide(raw.Side)
	if err != nil {
		return ObservedFinding{}, err
	}
	normalized.Side = side
	severity, err := NormalizeSeverity(raw.Severity)
	if err != nil {
		return ObservedFinding{}, err
	}
	normalized.Severity = severity
	if strings.TrimSpace(raw.Category) == "" {
		return ObservedFinding{}, fmt.Errorf("category is required")
	}
	if strings.TrimSpace(raw.Title) == "" {
		return ObservedFinding{}, fmt.Errorf("title is required")
	}
	return normalized, nil
}

func validateResult(result string) error {
	switch result {
	case ResultCompleted, ResultIncompleteCoverage, ResultAbstention, ResultTimeout, ResultDeliveryError:
		return nil
	default:
		return fmt.Errorf("unknown operational result %q", result)
	}
}

func pushDeclared(c Case, head string) bool {
	if len(c.Pushes) == 0 {
		return head == c.Comparison.Head
	}
	for _, push := range c.Pushes {
		if push == head {
			return true
		}
	}
	return false
}

func coversAllowedContext(c Case, reviewed map[string]bool) bool {
	for _, allowed := range c.AllowedContext {
		if !reviewed[normalizePath(allowed)] {
			return false
		}
	}
	return true
}
