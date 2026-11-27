package reviewbench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SchemaVersion identifies the manifest and report format. The observation
// schema is versioned separately because external exports outlive a manifest.
const (
	SchemaVersion            = "reviewbench/v1"
	ObservationSchemaVersion = "reviewbench.observation/v1"
)

var (
	ErrInvalidManifest     = errors.New("invalid manifest")
	ErrLockMismatch        = errors.New("manifest lock mismatch")
	ErrInvalidObservation  = errors.New("invalid observation")
	ErrComparisonMismatch  = errors.New("comparison does not match the fixed case comparison")
	ErrUnknownCase         = errors.New("unknown case")
	ErrAdjudication        = errors.New("invalid adjudication")
	ErrAdjudicationPending = errors.New("adjudication is incomplete")
)

// Comparison is the base-head pair a case fixes before any output is imported.
type Comparison struct {
	Base string `json:"base"`
	Head string `json:"head"`
}

// ExpectedFinding is ground truth fixed on the final head. Severity must be one
// of the panel's canonical severities; aliases are rejected here even though
// imported observations may use them.
type ExpectedFinding struct {
	Path      string `json:"path"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	CitedCode string `json:"cited_code,omitempty"`
}

// Case is one pull request of the sample. AllowedContext also bounds which
// files a reviewer is allowed to comment on; a finding outside it counts as an
// out-of-context citation and cannot match ground truth. Pushes records the
// sequence of heads the pull request received so an observation of an earlier
// head is recognised instead of being mistaken for the final comparison.
type Case struct {
	ID             string              `json:"id"`
	Language       string              `json:"language"`
	ChangeType     string              `json:"change_type"`
	Comparison     Comparison          `json:"comparison"`
	AllowedContext []string            `json:"allowed_context"`
	Pushes         []string            `json:"pushes,omitempty"`
	HeadFiles      map[string][]string `json:"head_files"`
	Expected       []ExpectedFinding   `json:"expected"`
	Blind          bool                `json:"blind"`
}

// Thresholds are the pilot agreement from PROTOCOL.md. They are evaluated on
// the blind subset for TargetReviewer.
type Thresholds struct {
	Configured          bool    `json:"configured"`
	MinEvaluatedCases   int     `json:"min_evaluated_cases"`
	MinCoverage         float64 `json:"min_coverage"`
	MinPrecision        float64 `json:"min_precision"`
	MinSevereDetection  float64 `json:"min_severe_detection"`
	MinCitationAccuracy float64 `json:"min_citation_accuracy"`
	MaxFalseBlockRate   float64 `json:"max_false_block_rate"`
	MaxDuplicateRate    float64 `json:"max_duplicate_rate"`
}

// Manifest is the versioned sample. Lock is a SHA-256 of the canonical
// manifest with this field zeroed, so any change to a case, comparison,
// allowed context or ground truth invalidates observations that recorded the
// previous lock.
type Manifest struct {
	SchemaVersion  string     `json:"schema_version"`
	Version        string     `json:"version"`
	Lock           string     `json:"lock"`
	ContextRadius  int        `json:"context_radius"`
	TargetReviewer string     `json:"target_reviewer,omitempty"`
	Thresholds     Thresholds `json:"thresholds"`
	Cases          []Case     `json:"cases"`
}

// LoadManifest reads and verifies a manifest file.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

// ParseManifest decodes a manifest, validates its ground truth and verifies
// that Lock matches the content. A manifest whose lock does not match is
// rejected instead of silently redefining the sample.
func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	computed, err := manifest.ComputeLock()
	if err != nil {
		return Manifest{}, err
	}
	if manifest.Lock == "" || !strings.EqualFold(manifest.Lock, computed) {
		return Manifest{}, fmt.Errorf("%w: stored %q, computed %q", ErrLockMismatch, manifest.Lock, computed)
	}
	return manifest, nil
}

// ComputeLock returns the canonical SHA-256 of the manifest with Lock emptied.
// encoding/json sorts map keys, so the hash is stable across runs.
func (m Manifest) ComputeLock() (string, error) {
	clone := m
	clone.Lock = ""
	data, err := json.Marshal(clone)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Finalize validates the manifest and stores the lock. Programs that build a
// manifest in memory call this before sealing observations against it.
func (m *Manifest) Finalize() error {
	if err := m.Validate(); err != nil {
		return err
	}
	lock, err := m.ComputeLock()
	if err != nil {
		return err
	}
	m.Lock = lock
	return nil
}

// Validate checks the fixed ground truth: identifiers, comparison, context,
// head content and expected citations. It does not check the lock value.
func (m Manifest) Validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
	}
	if m.SchemaVersion != SchemaVersion {
		return invalid("schema_version %q, want %q", m.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(m.Version) == "" {
		return invalid("version is required")
	}
	if m.ContextRadius < 1 {
		return invalid("context_radius must be positive")
	}
	if len(m.Cases) == 0 {
		return invalid("at least one case is required")
	}
	blind := 0
	seen := make(map[string]bool, len(m.Cases))
	for _, c := range m.Cases {
		if c.ID == "" || seen[c.ID] {
			return invalid("case id %q is empty or duplicated", c.ID)
		}
		seen[c.ID] = true
		if c.Blind {
			blind++
		}
		if err := validateCase(m, c); err != nil {
			return err
		}
	}
	if blind == 0 {
		return invalid("at least one blind case must be reserved")
	}
	if m.Thresholds.Configured {
		if strings.TrimSpace(m.TargetReviewer) == "" {
			return invalid("thresholds are configured but target_reviewer is empty")
		}
		if err := validateThresholds(m.Thresholds); err != nil {
			return err
		}
	}
	return nil
}

func validateCase(m Manifest, c Case) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: case %s: %s", ErrInvalidManifest, c.ID, fmt.Sprintf(format, args...))
	}
	if !knownLanguages[c.Language] {
		return invalid("unknown language %q", c.Language)
	}
	if !knownChangeTypes[c.ChangeType] {
		return invalid("unknown change_type %q", c.ChangeType)
	}
	if !isSHA(c.Comparison.Base) || !isSHA(c.Comparison.Head) {
		return invalid("comparison must use two 40-character lowercase hex SHAs")
	}
	pushes := c.Pushes
	if len(pushes) == 0 {
		pushes = []string{c.Comparison.Head}
	}
	seenPush := make(map[string]bool, len(pushes))
	for _, push := range pushes {
		if !isSHA(push) {
			return invalid("push head %q is not a 40-character lowercase hex SHA", push)
		}
		if seenPush[push] {
			return invalid("push head %q is duplicated", push)
		}
		seenPush[push] = true
	}
	if pushes[len(pushes)-1] != c.Comparison.Head {
		return invalid("the last push %q is not the comparison head %q", pushes[len(pushes)-1], c.Comparison.Head)
	}

	context := make(map[string]bool, len(c.AllowedContext))
	for _, allowed := range c.AllowedContext {
		normalized := normalizePath(allowed)
		if normalized == "" {
			return invalid("allowed_context contains an empty path")
		}
		if context[normalized] {
			return invalid("allowed_context repeats %q", normalized)
		}
		context[normalized] = true
		if _, ok := caseFileLines(c, normalized); !ok {
			return invalid("allowed_context path %q has no head_files content", normalized)
		}
	}
	if len(context) == 0 {
		return invalid("allowed_context is required")
	}

	files := make(map[string]bool, len(c.HeadFiles))
	for path := range c.HeadFiles {
		normalized := normalizePath(path)
		if normalized == "" || files[normalized] {
			return invalid("head_files path %q is empty or duplicated after normalization", path)
		}
		files[normalized] = true
	}
	if len(files) == 0 {
		return invalid("head_files is required")
	}

	for i, expected := range c.Expected {
		path := normalizePath(expected.Path)
		if path == "" || !context[path] {
			return invalid("expected[%d] path %q is outside allowed_context", i, expected.Path)
		}
		if strings.TrimSpace(expected.Category) == "" {
			return invalid("expected[%d] category is required", i)
		}
		if !IsCanonicalSeverity(expected.Severity) {
			return invalid("expected[%d] severity %q is not canonical", i, expected.Severity)
		}
		lines, ok := caseFileLines(c, path)
		if !ok {
			return invalid("expected[%d] path %q has no head content", i, path)
		}
		if err := validateRange(expected.StartLine, expected.EndLine, len(lines)); err != nil {
			return invalid("expected[%d]: %v", i, err)
		}
		if cited := normalizeAnchor(expected.CitedCode); cited != "" {
			window := normalizeAnchor(strings.Join(lines[expected.StartLine-1:expected.EndLine], "\n"))
			if !strings.Contains(window, cited) {
				return invalid("expected[%d] cited code is absent from the head range", i)
			}
		}
	}
	return nil
}

func validateThresholds(t Thresholds) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: thresholds: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
	}
	if t.MinEvaluatedCases < 0 {
		return invalid("min_evaluated_cases must not be negative")
	}
	for name, value := range map[string]float64{
		"min_coverage":          t.MinCoverage,
		"min_precision":         t.MinPrecision,
		"min_severe_detection":  t.MinSevereDetection,
		"min_citation_accuracy": t.MinCitationAccuracy,
		"max_false_block_rate":  t.MaxFalseBlockRate,
		"max_duplicate_rate":    t.MaxDuplicateRate,
	} {
		if value < 0 || value > 1 {
			return invalid("%s must be within [0,1]", name)
		}
	}
	return nil
}

func validateRange(start, end, lines int) error {
	if start < 1 || end < start || end > lines {
		return fmt.Errorf("range %d-%d is outside the %d head lines", start, end, lines)
	}
	return nil
}

func isSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// caseFileLines looks a case head file up after path normalization.
func caseFileLines(c Case, path string) ([]string, bool) {
	normalized := normalizePath(path)
	for candidate, lines := range c.HeadFiles {
		if normalizePath(candidate) == normalized {
			return lines, true
		}
	}
	return nil, false
}

var knownLanguages = map[string]bool{
	"go": true, "typescript": true, "python": true, "dockerfile": true,
	"yaml": true, "sql": true, "graphql": true,
}

var knownChangeTypes = map[string]bool{
	"code": true, "test": true, "config": true, "infra": true,
	"cross_module": true, "data": true,
}
