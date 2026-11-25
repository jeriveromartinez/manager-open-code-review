package review

// Local evaluation harness for the versioned review corpus (#140).
//
// The corpus records base/head comparisons, expected findings and the output of
// the fake-provider OCR double for clean and defective pull requests. This file
// replays those recorded outputs through the real mapping and outcome functions
// (parseNativeReview, mergeNativeFindings, nativeOutcome, reviewedPaths) and
// computes per-language and per-change-type metrics. It never calls a provider,
// the OCR binary or GitHub, so it runs on every platform with the rest of the
// package tests.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type corpusManifest struct {
	CorpusVersion  string         `json:"corpus_version"`
	OCRVersion     string         `json:"ocr_version"`
	HarnessVersion string         `json:"harness_version"`
	Profile        corpusProfile  `json:"profile"`
	Autonomy       corpusAutonomy `json:"autonomy_criteria"`
	Entries        []corpusEntry  `json:"entries"`
}

type corpusProfile struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

// corpusAutonomy holds the promotion thresholds documented in the corpus README.
// The harness evaluates them against the metrics it computed from the fixtures;
// they are not copied from a previous report.
type corpusAutonomy struct {
	MinCoverage                    float64 `json:"min_coverage"`
	MaxFalsePositives              int     `json:"max_false_positives"`
	MaxMissedSevereDefects         int     `json:"max_missed_severe_defects"`
	MaxCleanPRsClassifiedDefective int     `json:"max_clean_prs_classified_defective"`
	MaxDeliveryFailures            int     `json:"max_delivery_failures"`
	MaxEligiblePRsWithoutResult    int     `json:"max_eligible_prs_without_result"`
	MaxUnnecessaryEscalations      int     `json:"max_unnecessary_escalations"`
	MinCitationAccuracy            float64 `json:"min_citation_accuracy"`
	MinIndependentPRs              int     `json:"min_independent_prs"`
	// FalsePositiveUpperBoundReference is reference-only documentation: it
	// records the statistical bound that justifies the sample target, not a
	// pass/fail threshold.
	FalsePositiveUpperBoundReference float64 `json:"false_positive_upper_bound_reference"`
}

type corpusEntry struct {
	ID          string              `json:"id"`
	Language    string              `json:"language"`
	ChangeType  string              `json:"change_type"`
	Comparison  corpusComparison    `json:"comparison"`
	HeadFiles   map[string][]string `json:"head_files"`
	Expected    corpusExpected      `json:"expected"`
	Observation corpusObservation   `json:"observation"`
	MustMatch   bool                `json:"must_match"`
}

type corpusComparison struct {
	Base string `json:"base"`
	Head string `json:"head"`
}

type corpusExpected struct {
	Result           string                  `json:"result"`
	Escalation       bool                    `json:"escalation"`
	EscalationReason string                  `json:"escalation_reason"`
	ReviewableFiles  []string                `json:"reviewable_files"`
	Findings         []corpusExpectedFinding `json:"findings"`
}

type corpusExpectedFinding struct {
	Path      string `json:"path"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	CitedCode string `json:"cited_code"`
}

type corpusObservation struct {
	// Review is empty when no run result was recorded for the pull request.
	Review   string `json:"review"`
	Delivery string `json:"delivery"`
}

// corpusMetrics counts what the harness observed for a group of entries. Ratios
// are computed by finalize from the counters; a zero denominator is vacuously
// perfect so a corpus without comments or findings does not report NaN.
type corpusMetrics struct {
	PRs                      int            `json:"prs"`
	PRsWithEvaluatedComments int            `json:"prs_with_evaluated_comments"`
	ReviewableFiles          int            `json:"reviewable_files"`
	ReviewedFiles            int            `json:"reviewed_files"`
	Coverage                 float64        `json:"coverage"`
	ActualComments           int            `json:"actual_comments"`
	MatchedComments          int            `json:"matched_comments"`
	FalsePositives           int            `json:"false_positives"`
	Precision                float64        `json:"precision"`
	ExpectedFindings         int            `json:"expected_findings"`
	DetectedFindings         int            `json:"detected_findings"`
	MissedFindings           int            `json:"missed_findings"`
	Detection                float64        `json:"detection"`
	SevereFindings           int            `json:"severe_findings"`
	MissedSevereDefects      int            `json:"missed_severe_defects"`
	SeverityMismatches       int            `json:"severity_mismatches"`
	CitationChecked          int            `json:"citation_checked"`
	CitationAccurate         int            `json:"citation_accurate"`
	CitationAccuracy         float64        `json:"citation_accuracy"`
	CleanPRs                 int            `json:"clean_prs"`
	CleanClassifiedDefective int            `json:"clean_classified_defective"`
	EligiblePRsWithoutResult int            `json:"eligible_prs_without_result"`
	DeliveryFailures         int            `json:"delivery_failures"`
	ConfirmedEscalations     int            `json:"confirmed_escalations"`
	UsefulEscalations        int            `json:"useful_escalations"`
	UnnecessaryEscalations   int            `json:"unnecessary_escalations"`
	MissingEscalations       int            `json:"missing_escalations"`
	EscalationSampleSize     int            `json:"escalation_sample_size"`
	EscalationReasons        map[string]int `json:"escalation_reasons"`
}

type corpusEntryResult struct {
	ID                    string   `json:"id"`
	Language              string   `json:"language"`
	ChangeType            string   `json:"change_type"`
	ExpectedOutcome       string   `json:"expected_outcome"`
	ActualOutcome         string   `json:"actual_outcome"`
	ActualReason          string   `json:"actual_reason"`
	Delivery              string   `json:"delivery"`
	ExpectedFindings      int      `json:"expected_findings"`
	ActualFindings        int      `json:"actual_findings"`
	Provenance            string   `json:"provenance"`
	GroundTruthMismatches []string `json:"ground_truth_mismatches"`
	MatchesExpectation    bool     `json:"matches_expectation"`
	MustMatch             bool     `json:"must_match"`
	Mismatches            []string `json:"mismatches"`
}

// Provenance states for an entry. A pull request with no recorded observation
// is "unrecorded", not "verified".
const (
	corpusProvenanceVerified   = "verified"
	corpusProvenanceUnrecorded = "unrecorded"
	corpusProvenanceMismatch   = "mismatch"
)

type corpusCriteriaResult struct {
	Met                              bool     `json:"met"`
	FalsePositiveUpperBoundReference float64  `json:"false_positive_upper_bound_reference"`
	Failures                         []string `json:"failures"`
}

type corpusReport struct {
	CorpusVersion  string                    `json:"corpus_version"`
	OCRVersion     string                    `json:"ocr_version"`
	HarnessVersion string                    `json:"harness_version"`
	Profile        corpusProfile             `json:"profile"`
	SampleSize     int                       `json:"sample_size"`
	ByLanguage     map[string]*corpusMetrics `json:"by_language"`
	ByChangeType   map[string]*corpusMetrics `json:"by_change_type"`
	Overall        corpusMetrics             `json:"overall"`
	Entries        []corpusEntryResult       `json:"entries"`
	Autonomy       corpusCriteriaResult      `json:"autonomy"`
}

func newCorpusMetrics() *corpusMetrics {
	return &corpusMetrics{EscalationReasons: map[string]int{}}
}

func (m *corpusMetrics) add(other corpusMetrics) {
	m.PRs += other.PRs
	m.PRsWithEvaluatedComments += other.PRsWithEvaluatedComments
	m.ReviewableFiles += other.ReviewableFiles
	m.ReviewedFiles += other.ReviewedFiles
	m.ActualComments += other.ActualComments
	m.MatchedComments += other.MatchedComments
	m.FalsePositives += other.FalsePositives
	m.ExpectedFindings += other.ExpectedFindings
	m.DetectedFindings += other.DetectedFindings
	m.MissedFindings += other.MissedFindings
	m.SevereFindings += other.SevereFindings
	m.MissedSevereDefects += other.MissedSevereDefects
	m.SeverityMismatches += other.SeverityMismatches
	m.CitationChecked += other.CitationChecked
	m.CitationAccurate += other.CitationAccurate
	m.CleanPRs += other.CleanPRs
	m.CleanClassifiedDefective += other.CleanClassifiedDefective
	m.EligiblePRsWithoutResult += other.EligiblePRsWithoutResult
	m.DeliveryFailures += other.DeliveryFailures
	m.ConfirmedEscalations += other.ConfirmedEscalations
	m.UsefulEscalations += other.UsefulEscalations
	m.UnnecessaryEscalations += other.UnnecessaryEscalations
	m.MissingEscalations += other.MissingEscalations
	m.EscalationSampleSize += other.EscalationSampleSize
	for reason, count := range other.EscalationReasons {
		m.EscalationReasons[reason] += count
	}
}

func (m *corpusMetrics) finalize() {
	m.Coverage = corpusRatio(m.ReviewedFiles, m.ReviewableFiles)
	m.Precision = corpusRatio(m.MatchedComments, m.ActualComments)
	m.Detection = corpusRatio(m.DetectedFindings, m.ExpectedFindings)
	m.CitationAccuracy = corpusRatio(m.CitationAccurate, m.CitationChecked)
	if m.EscalationReasons == nil {
		m.EscalationReasons = map[string]int{}
	}
}

func corpusRatio(good, total int) float64 {
	if total == 0 {
		return 1
	}
	return float64(good) / float64(total)
}

func loadReviewCorpus(dir string) (corpusManifest, error) {
	var manifest corpusManifest
	raw, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		return manifest, fmt.Errorf("read corpus manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("decode corpus manifest: %w", err)
	}
	seen := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if entry.ID == "" {
			return manifest, fmt.Errorf("corpus entry without id")
		}
		if seen[entry.ID] {
			return manifest, fmt.Errorf("duplicate corpus entry %q", entry.ID)
		}
		seen[entry.ID] = true
	}
	return manifest, nil
}

func evaluateReviewCorpus(dir string, manifest corpusManifest) (corpusReport, error) {
	report := corpusReport{
		CorpusVersion:  manifest.CorpusVersion,
		OCRVersion:     manifest.OCRVersion,
		HarnessVersion: manifest.HarnessVersion,
		Profile:        manifest.Profile,
		SampleSize:     len(manifest.Entries),
		ByLanguage:     map[string]*corpusMetrics{},
		ByChangeType:   map[string]*corpusMetrics{},
	}
	report.Overall = *newCorpusMetrics()

	for index, entry := range manifest.Entries {
		result, metrics, err := evaluateCorpusEntry(dir, manifest, entry, uint(index+1))
		if err != nil {
			return report, fmt.Errorf("corpus entry %s: %w", entry.ID, err)
		}
		report.Entries = append(report.Entries, result)
		report.Overall.add(metrics)
		language := report.ByLanguage[entry.Language]
		if language == nil {
			language = newCorpusMetrics()
			report.ByLanguage[entry.Language] = language
		}
		language.add(metrics)
		changeType := report.ByChangeType[entry.ChangeType]
		if changeType == nil {
			changeType = newCorpusMetrics()
			report.ByChangeType[entry.ChangeType] = changeType
		}
		changeType.add(metrics)
	}

	report.Overall.finalize()
	for _, group := range report.ByLanguage {
		group.finalize()
	}
	for _, group := range report.ByChangeType {
		group.finalize()
	}
	report.Autonomy = evaluateCorpusCriteria(manifest.Autonomy, report.Overall)
	return report, nil
}

func evaluateCorpusEntry(dir string, manifest corpusManifest, entry corpusEntry, prID uint) (corpusEntryResult, corpusMetrics, error) {
	result := corpusEntryResult{
		ID:               entry.ID,
		Language:         entry.Language,
		ChangeType:       entry.ChangeType,
		ExpectedOutcome:  entry.Expected.Result,
		Delivery:         entry.Observation.Delivery,
		ExpectedFindings: len(entry.Expected.Findings),
		MustMatch:        entry.MustMatch,
	}
	metrics := *newCorpusMetrics()
	metrics.PRs = 1
	cleanExpected := len(entry.Expected.Findings) == 0 && entry.Expected.Result == store.RunSucceeded
	if cleanExpected {
		metrics.CleanPRs = 1
	}

	if entry.Observation.Review == "" {
		result.ActualOutcome = "missing"
		result.Provenance = corpusProvenanceUnrecorded
		if entry.Observation.Delivery == "failed" {
			// No review and a failed delivery: this is a delivery failure, not an
			// eligible pull request left without a result.
			metrics.DeliveryFailures = 1
			result.Mismatches = append(result.Mismatches,
				fmt.Sprintf("delivery: failed with no review recorded (expected %s)", entry.Expected.Result))
		} else {
			// An eligible pull request that produced no result at all.
			metrics.EligiblePRsWithoutResult = 1
			result.Mismatches = append(result.Mismatches,
				fmt.Sprintf("outcome: expected %s, no result recorded", entry.Expected.Result))
		}
		result.MatchesExpectation = false
		metrics.finalize()
		return result, metrics, nil
	}

	raw, err := os.ReadFile(filepath.Join(dir, entry.Observation.Review))
	if err != nil {
		return result, metrics, fmt.Errorf("read review observation: %w", err)
	}
	review, err := parseNativeReview(raw)
	if err != nil {
		return result, metrics, fmt.Errorf("parse review observation: %w", err)
	}
	provenanceOK, provenanceMismatches := corpusProvenance(raw, manifest, entry)
	if provenanceOK {
		result.Provenance = corpusProvenanceVerified
	} else {
		result.Provenance = corpusProvenanceMismatch
	}
	result.Mismatches = append(result.Mismatches, provenanceMismatches...)

	status, code := nativeOutcome(review)
	result.ActualOutcome = status
	result.ActualReason = code

	actual := mergeNativeFindings(prID, review.Comments, corpusAnchors(entry.HeadFiles)).Findings
	result.ActualFindings = len(actual)

	reviewed := reviewedPaths(review.Manifest.Coverage)
	metrics.ReviewableFiles = len(entry.Expected.ReviewableFiles)
	for _, path := range entry.Expected.ReviewableFiles {
		normalized := normalizeCoveragePath(path)
		if reviewed[normalized] {
			metrics.ReviewedFiles++
			continue
		}
		if entry.Expected.Result == store.RunSucceeded {
			result.Mismatches = append(result.Mismatches,
				fmt.Sprintf("coverage: %s not reviewed", path))
		}
	}

	matches, actualMatched := corpusMatchFindings(entry.Expected.Findings, actual)
	if len(actual) > 0 {
		metrics.PRsWithEvaluatedComments = 1
	}
	for i, want := range entry.Expected.Findings {
		metrics.ExpectedFindings++
		severe := severityRank(want.Severity) >= 3
		if severe {
			metrics.SevereFindings++
		}
		if want.CitedCode != "" {
			expectedCitation := AgentFinding{
				Path:         want.Path,
				StartLine:    want.StartLine,
				EndLine:      want.EndLine,
				ExistingCode: want.CitedCode,
			}
			if !corpusCitation(corpusFileLines(entry.HeadFiles, want.Path), expectedCitation) {
				mismatch := fmt.Sprintf(
					"expected citation: %s:%d %q not in head", want.Path, want.StartLine, want.CitedCode)
				result.Mismatches = append(result.Mismatches, mismatch)
				result.GroundTruthMismatches = append(result.GroundTruthMismatches, mismatch)
			}
		}
		if matches[i] >= 0 {
			metrics.DetectedFindings++
			if got := actual[matches[i]]; corpusNormalizeSeverity(want.Severity) != corpusNormalizeSeverity(got.Severity) {
				metrics.SeverityMismatches++
				result.Mismatches = append(result.Mismatches, fmt.Sprintf(
					"severity: expected %s, got %s for %s:%d",
					want.Severity, got.Severity, want.Path, want.StartLine))
			}
			continue
		}
		metrics.MissedFindings++
		if severe {
			metrics.MissedSevereDefects++
		}
		result.Mismatches = append(result.Mismatches, fmt.Sprintf(
			"missed finding: %s:%d %s/%s", want.Path, want.StartLine, want.Category, want.Severity))
	}
	for i, got := range actual {
		metrics.ActualComments++
		if actualMatched[i] {
			metrics.MatchedComments++
		} else {
			metrics.FalsePositives++
			result.Mismatches = append(result.Mismatches, fmt.Sprintf(
				"unexpected finding: %s:%d %s/%s", got.Path, got.StartLine, got.Category, got.Severity))
		}
		metrics.CitationChecked++
		if corpusCitation(corpusFileLines(entry.HeadFiles, got.Path), got) {
			metrics.CitationAccurate++
		}
	}

	if status != entry.Expected.Result {
		result.Mismatches = append(result.Mismatches, fmt.Sprintf(
			"outcome: expected %s, got %s (%s)", entry.Expected.Result, status, code))
	} else if entry.Expected.Escalation && entry.Expected.EscalationReason != "" &&
		code != entry.Expected.EscalationReason {
		result.Mismatches = append(result.Mismatches, fmt.Sprintf(
			"escalation: expected reason %s, got %s", entry.Expected.EscalationReason, code))
	}

	if entry.Observation.Delivery == "failed" {
		// A delivery failure is not a confirmed escalation: the review may have
		// concluded correctly and only the publication failed.
		metrics.DeliveryFailures = 1
	} else {
		metrics.EscalationSampleSize = 1
		switch {
		case status != store.RunSucceeded:
			metrics.ConfirmedEscalations = 1
			metrics.EscalationReasons[code]++
			if entry.Expected.Escalation &&
				(entry.Expected.EscalationReason == "" || code == entry.Expected.EscalationReason) {
				metrics.UsefulEscalations = 1
			}
			if !entry.Expected.Escalation {
				metrics.UnnecessaryEscalations = 1
			}
		case entry.Expected.Escalation:
			metrics.MissingEscalations = 1
		}
	}
	if cleanExpected && len(actual) > 0 {
		metrics.CleanClassifiedDefective = 1
	}

	metrics.finalize()
	result.MatchesExpectation = len(result.Mismatches) == 0
	return result, metrics, nil
}

// corpusProvenance checks that the recorded observation was produced from the
// comparison, engine version and profile fixed in the manifest.
func corpusProvenance(raw []byte, manifest corpusManifest, entry corpusEntry) (bool, []string) {
	var wire struct {
		Manifest struct {
			Input struct {
				ResolvedBase string `json:"resolved_base"`
				ResolvedHead string `json:"resolved_head"`
			} `json:"input"`
			Execution struct {
				OCRVersion string `json:"ocr_version"`
				Model      string `json:"model"`
				Provider   string `json:"provider"`
				Effort     string `json:"effort"`
			} `json:"execution"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return false, []string{fmt.Sprintf("provenance: decode observation: %v", err)}
	}
	var problems []string
	if !strings.EqualFold(wire.Manifest.Input.ResolvedBase, entry.Comparison.Base) {
		problems = append(problems, fmt.Sprintf(
			"provenance: base %s != manifest %s", wire.Manifest.Input.ResolvedBase, entry.Comparison.Base))
	}
	if !strings.EqualFold(wire.Manifest.Input.ResolvedHead, entry.Comparison.Head) {
		problems = append(problems, fmt.Sprintf(
			"provenance: head %s != manifest %s", wire.Manifest.Input.ResolvedHead, entry.Comparison.Head))
	}
	if wire.Manifest.Execution.OCRVersion != manifest.OCRVersion {
		problems = append(problems, fmt.Sprintf(
			"provenance: OCR %q != corpus %q",
			wire.Manifest.Execution.OCRVersion, manifest.OCRVersion))
	}
	if wire.Manifest.Execution.Model != manifest.Profile.Model {
		problems = append(problems, fmt.Sprintf(
			"provenance: model %s != profile %s",
			wire.Manifest.Execution.Model, manifest.Profile.Model))
	}
	if wire.Manifest.Execution.Provider != manifest.Profile.Provider {
		problems = append(problems, fmt.Sprintf(
			"provenance: provider %s != profile %s",
			wire.Manifest.Execution.Provider, manifest.Profile.Provider))
	}
	if wire.Manifest.Execution.Effort != manifest.Profile.Effort {
		problems = append(problems, fmt.Sprintf(
			"provenance: effort %s != profile %s",
			wire.Manifest.Execution.Effort, manifest.Profile.Effort))
	}
	return len(problems) == 0, problems
}

func corpusAnchors(files map[string][]string) AnchorProvider {
	normalized := make(map[string][]string, len(files))
	for path, lines := range files {
		normalized[normalizeCoveragePath(path)] = lines
	}
	return func(path string) []string {
		return normalized[normalizeCoveragePath(path)]
	}
}

func corpusFileLines(files map[string][]string, path string) []string {
	for candidate, lines := range files {
		if normalizeCoveragePath(candidate) == normalizeCoveragePath(path) {
			return lines
		}
	}
	return nil
}

// corpusMatchFindings pairs each expected finding with the first unmatched
// observed finding that shares path, category and a nearby range. The returned
// index is -1 when the expectation was not detected. Severity is compared by the
// caller so a mapping regression stays visible without hiding the detection.
func corpusMatchFindings(expected []corpusExpectedFinding, actual []AgentFinding) ([]int, []bool) {
	matches := make([]int, len(expected))
	for i := range matches {
		matches[i] = -1
	}
	actualMatched := make([]bool, len(actual))
	for i, want := range expected {
		wantFinding := AgentFinding{
			Path:      want.Path,
			Category:  want.Category,
			Severity:  want.Severity,
			StartLine: want.StartLine,
			EndLine:   want.EndLine,
		}
		for j, got := range actual {
			if actualMatched[j] {
				continue
			}
			if normalizeCoveragePath(got.Path) != normalizeCoveragePath(want.Path) {
				continue
			}
			if strings.TrimSpace(got.Category) != strings.TrimSpace(want.Category) {
				continue
			}
			if !rangesNearby(wantFinding, got, contextRadius) {
				continue
			}
			matches[i] = j
			actualMatched[j] = true
			break
		}
	}
	return matches, actualMatched
}

func corpusNormalizeSeverity(severity string) string {
	return strings.ToLower(strings.TrimSpace(severity))
}

// corpusCitation verifies the cited code against the head file lines. A finding
// without cited code is accurate when its range exists in the file.
func corpusCitation(lines []string, finding AgentFinding) bool {
	if len(lines) == 0 {
		return false
	}
	start, end := finding.StartLine, finding.EndLine
	if start < 1 || end < start || end > len(lines) {
		return false
	}
	cited := normalizeAnchor(finding.ExistingCode)
	if cited == "" {
		return true
	}
	window := normalizeAnchor(strings.Join(lines[start-1:end], "\n"))
	return strings.Contains(window, cited)
}

func evaluateCorpusCriteria(criteria corpusAutonomy, metrics corpusMetrics) corpusCriteriaResult {
	result := corpusCriteriaResult{
		FalsePositiveUpperBoundReference: criteria.FalsePositiveUpperBoundReference,
	}
	fail := func(format string, args ...any) {
		result.Failures = append(result.Failures, fmt.Sprintf(format, args...))
	}
	if metrics.Coverage < criteria.MinCoverage {
		fail("coverage %.4f < %.4f", metrics.Coverage, criteria.MinCoverage)
	}
	if metrics.FalsePositives > criteria.MaxFalsePositives {
		fail("false positives %d > %d", metrics.FalsePositives, criteria.MaxFalsePositives)
	}
	if metrics.MissedSevereDefects > criteria.MaxMissedSevereDefects {
		fail("missed severe defects %d > %d", metrics.MissedSevereDefects, criteria.MaxMissedSevereDefects)
	}
	if metrics.CleanClassifiedDefective > criteria.MaxCleanPRsClassifiedDefective {
		fail("clean PRs classified defective %d > %d",
			metrics.CleanClassifiedDefective, criteria.MaxCleanPRsClassifiedDefective)
	}
	if metrics.DeliveryFailures > criteria.MaxDeliveryFailures {
		fail("delivery failures %d > %d", metrics.DeliveryFailures, criteria.MaxDeliveryFailures)
	}
	if metrics.EligiblePRsWithoutResult > criteria.MaxEligiblePRsWithoutResult {
		fail("eligible PRs without result %d > %d",
			metrics.EligiblePRsWithoutResult, criteria.MaxEligiblePRsWithoutResult)
	}
	if metrics.UnnecessaryEscalations > criteria.MaxUnnecessaryEscalations {
		fail("unnecessary escalations %d > %d",
			metrics.UnnecessaryEscalations, criteria.MaxUnnecessaryEscalations)
	}
	if metrics.CitationAccuracy < criteria.MinCitationAccuracy {
		fail("citation accuracy %.4f < %.4f", metrics.CitationAccuracy, criteria.MinCitationAccuracy)
	}
	if metrics.PRsWithEvaluatedComments < criteria.MinIndependentPRs {
		fail("independent PRs with evaluated comments %d < %d",
			metrics.PRsWithEvaluatedComments, criteria.MinIndependentPRs)
	}
	result.Met = len(result.Failures) == 0
	return result
}

// corpusExpectedCriteriaFailures is the test oracle for the autonomy gate. It
// restates the documented thresholds so the test asserts the exact failure
// lines computed from the current fixtures, and a grown corpus that satisfies
// the criteria still passes.
func corpusExpectedCriteriaFailures(criteria corpusAutonomy, metrics corpusMetrics) []string {
	var failures []string
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	if metrics.Coverage < criteria.MinCoverage {
		fail("coverage %.4f < %.4f", metrics.Coverage, criteria.MinCoverage)
	}
	if metrics.FalsePositives > criteria.MaxFalsePositives {
		fail("false positives %d > %d", metrics.FalsePositives, criteria.MaxFalsePositives)
	}
	if metrics.MissedSevereDefects > criteria.MaxMissedSevereDefects {
		fail("missed severe defects %d > %d", metrics.MissedSevereDefects, criteria.MaxMissedSevereDefects)
	}
	if metrics.CleanClassifiedDefective > criteria.MaxCleanPRsClassifiedDefective {
		fail("clean PRs classified defective %d > %d",
			metrics.CleanClassifiedDefective, criteria.MaxCleanPRsClassifiedDefective)
	}
	if metrics.DeliveryFailures > criteria.MaxDeliveryFailures {
		fail("delivery failures %d > %d", metrics.DeliveryFailures, criteria.MaxDeliveryFailures)
	}
	if metrics.EligiblePRsWithoutResult > criteria.MaxEligiblePRsWithoutResult {
		fail("eligible PRs without result %d > %d",
			metrics.EligiblePRsWithoutResult, criteria.MaxEligiblePRsWithoutResult)
	}
	if metrics.UnnecessaryEscalations > criteria.MaxUnnecessaryEscalations {
		fail("unnecessary escalations %d > %d",
			metrics.UnnecessaryEscalations, criteria.MaxUnnecessaryEscalations)
	}
	if metrics.CitationAccuracy < criteria.MinCitationAccuracy {
		fail("citation accuracy %.4f < %.4f", metrics.CitationAccuracy, criteria.MinCitationAccuracy)
	}
	if metrics.PRsWithEvaluatedComments < criteria.MinIndependentPRs {
		fail("independent PRs with evaluated comments %d < %d",
			metrics.PRsWithEvaluatedComments, criteria.MinIndependentPRs)
	}
	sort.Strings(failures)
	return failures
}

// validateReviewCorpus rejects a corpus whose ground truth, mandatory
// expectations or provenance do not hold. Invalid expected evidence fails every
// entry; measurement entries may record an observed miss or false positive, but
// they cannot carry a wrong ground-truth citation.
func validateReviewCorpus(report corpusReport) error {
	var problems []string
	for _, entry := range report.Entries {
		if entry.MustMatch && !entry.MatchesExpectation {
			problems = append(problems, fmt.Sprintf("%s: %s", entry.ID, strings.Join(entry.Mismatches, "; ")))
		}
		if len(entry.GroundTruthMismatches) > 0 {
			problems = append(problems, fmt.Sprintf("%s: %s",
				entry.ID, strings.Join(entry.GroundTruthMismatches, "; ")))
		}
		if entry.Provenance == corpusProvenanceMismatch {
			problems = append(problems, fmt.Sprintf("%s: provenance mismatch", entry.ID))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("review corpus expectations not met:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func TestReviewCorpusEvaluation(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	if err := validateReviewCorpus(report); err != nil {
		t.Fatalf("%v", err)
	}

	again, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus again: %v", err)
	}
	if !reflect.DeepEqual(report, again) {
		t.Fatalf("corpus report is not deterministic")
	}
	if report.SampleSize != len(manifest.Entries) {
		t.Errorf("sample size = %d, want %d", report.SampleSize, len(manifest.Entries))
	}
	if report.CorpusVersion == "" || report.HarnessVersion == "" {
		t.Errorf("report does not record corpus and harness versions: %+v", report)
	}
	if report.OCRVersion != manifest.OCRVersion {
		t.Errorf("OCR version = %q, want %q", report.OCRVersion, manifest.OCRVersion)
	}
	if report.Profile != manifest.Profile {
		t.Errorf("profile = %+v, want %+v", report.Profile, manifest.Profile)
	}

	languageSample := 0
	for language, group := range report.ByLanguage {
		if group.PRs == 0 {
			t.Errorf("language %q has an empty sample", language)
		}
		languageSample += group.PRs
	}
	if languageSample != report.SampleSize {
		t.Errorf("per-language sample = %d, want %d", languageSample, report.SampleSize)
	}
	changeSample := 0
	for changeType, group := range report.ByChangeType {
		if group.PRs == 0 {
			t.Errorf("change type %q has an empty sample", changeType)
		}
		changeSample += group.PRs
	}
	if changeSample != report.SampleSize {
		t.Errorf("per-change-type sample = %d, want %d", changeSample, report.SampleSize)
	}

	wantFailures := corpusExpectedCriteriaFailures(manifest.Autonomy, report.Overall)
	gotFailures := append([]string(nil), report.Autonomy.Failures...)
	sort.Strings(gotFailures)
	if !reflect.DeepEqual(gotFailures, wantFailures) {
		t.Errorf("autonomy failures = %v, want %v", gotFailures, wantFailures)
	}
	if report.Autonomy.Met != (len(wantFailures) == 0) {
		t.Errorf("autonomy met = %t with failures %v", report.Autonomy.Met, wantFailures)
	}
	if report.Autonomy.FalsePositiveUpperBoundReference != manifest.Autonomy.FalsePositiveUpperBoundReference {
		t.Errorf("false-positive reference bound = %v, want %v (reference-only)",
			report.Autonomy.FalsePositiveUpperBoundReference, manifest.Autonomy.FalsePositiveUpperBoundReference)
	}

	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	t.Logf("review corpus report:\n%s", raw)
}

// TestReviewCorpusWrongExpectationFails proves the harness compares against the
// recorded observation instead of trusting the manifest: a deliberately wrong
// expectation makes its entry fail and the corpus gate reject the run.
func TestReviewCorpusWrongExpectationFails(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	index := -1
	for i, entry := range manifest.Entries {
		if entry.ID == "go-clean-cross-module" {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("go-clean-cross-module entry is missing")
	}
	manifest.Entries[index].Expected.Findings = []corpusExpectedFinding{{
		Path:      "pkg/orders/order.go",
		Category:  "bug",
		Severity:  "major",
		StartLine: 6,
		EndLine:   6,
	}}

	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	var result *corpusEntryResult
	for i := range report.Entries {
		if report.Entries[i].ID == manifest.Entries[index].ID {
			result = &report.Entries[i]
			break
		}
	}
	if result == nil {
		t.Fatal("mutated entry was not evaluated")
	}
	if result.MatchesExpectation {
		t.Fatal("a wrong expectation matched its recorded observation")
	}
	if len(result.Mismatches) == 0 || !strings.Contains(strings.Join(result.Mismatches, "; "), "missed finding") {
		t.Errorf("mismatches = %v, want a missed finding", result.Mismatches)
	}
	if err := validateReviewCorpus(report); err == nil {
		t.Fatal("corpus gate accepted a wrong expectation")
	}
}

// TestReviewCorpusSeverityMismatchFails proves the mapped observed severity is
// compared with the expected one, so a severity mapping regression is visible.
func TestReviewCorpusSeverityMismatchFails(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	index := corpusEntryIndex(t, manifest, "go-defect-auth-citation")
	manifest.Entries[index].Expected.Findings[0].Severity = "nit"

	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	result := corpusEntryResultByID(t, report, manifest.Entries[index].ID)
	if result.MatchesExpectation {
		t.Fatal("a wrong expected severity matched the observation")
	}
	if !strings.Contains(strings.Join(result.Mismatches, "; "), "severity:") {
		t.Errorf("mismatches = %v, want a severity mismatch", result.Mismatches)
	}
	if err := validateReviewCorpus(report); err == nil {
		t.Fatal("corpus gate accepted a severity mismatch")
	}
}

// TestReviewCorpusExpectedCitationValidated proves the ground-truth cited code
// is checked against the head range instead of being an unused field.
func TestReviewCorpusExpectedCitationValidated(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	index := corpusEntryIndex(t, manifest, "go-defect-auth-citation")
	manifest.Entries[index].Expected.Findings[0].CitedCode = "if stored == provided {"

	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	result := corpusEntryResultByID(t, report, manifest.Entries[index].ID)
	if !strings.Contains(strings.Join(result.Mismatches, "; "), "expected citation:") {
		t.Errorf("mismatches = %v, want an invalid expected citation", result.Mismatches)
	}
	if err := validateReviewCorpus(report); err == nil {
		t.Fatal("corpus gate accepted an expected citation absent from the head")
	}
}

// TestReviewCorpusGroundTruthGatedForMeasurementEntries proves a measurement
// entry (must_match: false) cannot carry an invalid expected citation: the gate
// checks ground truth for every entry, not only for the regression subset.
func TestReviewCorpusGroundTruthGatedForMeasurementEntries(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	index := corpusEntryIndex(t, manifest, "go-missed-severe")
	if manifest.Entries[index].MustMatch {
		t.Fatal("go-missed-severe must stay a measurement entry for this case")
	}
	manifest.Entries[index].Expected.Findings[0].CitedCode = "not present in the head"

	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	result := corpusEntryResultByID(t, report, manifest.Entries[index].ID)
	if len(result.GroundTruthMismatches) == 0 {
		t.Fatalf("ground-truth mismatches = %v, want the invalid expected citation", result.Mismatches)
	}
	if err := validateReviewCorpus(report); err == nil {
		t.Fatal("corpus gate accepted invalid ground truth on a measurement entry")
	}
}

// TestReviewCorpusDeliveryFailureWithoutReview proves a failed delivery with no
// recorded review is counted as a delivery failure, not as an eligible PR left
// without a result.
func TestReviewCorpusDeliveryFailureWithoutReview(t *testing.T) {
	manifest := corpusManifest{
		OCRVersion: "v1.14.0",
		Profile:    corpusProfile{Provider: "fake", Model: "fake-model", Effort: "low"},
	}
	entry := corpusEntry{
		ID:         "delivery-without-review",
		Language:   "go",
		ChangeType: "code",
		Expected: corpusExpected{
			Result:          store.RunSucceeded,
			ReviewableFiles: []string{"a.go"},
		},
		Observation: corpusObservation{Delivery: "failed"},
	}
	result, metrics, err := evaluateCorpusEntry(t.TempDir(), manifest, entry, 1)
	if err != nil {
		t.Fatalf("evaluate entry: %v", err)
	}
	if metrics.DeliveryFailures != 1 {
		t.Errorf("delivery failures = %d, want 1", metrics.DeliveryFailures)
	}
	if metrics.EligiblePRsWithoutResult != 0 {
		t.Errorf("eligible PRs without result = %d, want 0", metrics.EligiblePRsWithoutResult)
	}
	if result.Provenance != corpusProvenanceUnrecorded {
		t.Errorf("provenance = %q, want %q", result.Provenance, corpusProvenanceUnrecorded)
	}
}

// TestReviewCorpusProvenanceRequiresExactEngineAndProfile proves that a
// truncated engine version or a different provider/effort is rejected.
func TestReviewCorpusProvenanceRequiresExactEngineAndProfile(t *testing.T) {
	manifest := corpusManifest{
		OCRVersion: "v1.14.0",
		Profile:    corpusProfile{Provider: "fake", Model: "fake-model", Effort: "low"},
	}
	entry := corpusEntry{Comparison: corpusComparison{Base: "base", Head: "head"}}
	raw := `{"manifest":{"input":{"resolved_base":"base","resolved_head":"head"},` +
		`"execution":{"ocr_version":"v1.14.0","model":"fake-model","provider":"fake","effort":"low"}}}`
	ok, problems := corpusProvenance([]byte(raw), manifest, entry)
	if !ok {
		t.Fatalf("valid provenance rejected: %v", problems)
	}

	truncated := strings.Replace(raw, `"ocr_version":"v1.14.0"`, `"ocr_version":"v1.14"`, 1)
	if ok, problems := corpusProvenance([]byte(truncated), manifest, entry); ok {
		t.Fatalf("truncated OCR version accepted: %v", problems)
	}
	wrongEffort := strings.Replace(raw, `"effort":"low"`, `"effort":"high"`, 1)
	if ok, problems := corpusProvenance([]byte(wrongEffort), manifest, entry); ok {
		t.Fatalf("wrong effort accepted: %v", problems)
	}
	wrongProvider := strings.Replace(raw, `"provider":"fake"`, `"provider":"other"`, 1)
	if ok, problems := corpusProvenance([]byte(wrongProvider), manifest, entry); ok {
		t.Fatalf("wrong provider accepted: %v", problems)
	}
}

func corpusEntryIndex(t *testing.T, manifest corpusManifest, id string) int {
	t.Helper()
	for i, entry := range manifest.Entries {
		if entry.ID == id {
			return i
		}
	}
	t.Fatalf("corpus entry %q is missing", id)
	return -1
}

func corpusEntryResultByID(t *testing.T, report corpusReport, id string) corpusEntryResult {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("corpus result %q is missing", id)
	return corpusEntryResult{}
}

// TestReviewCorpusMetricsComeFromFixtures independently counts the raw review
// comments and the manifest expectations, so the metrics are derived from the
// fixtures instead of being hardcoded in the harness.
func TestReviewCorpusMetricsComeFromFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}

	wantComments, wantExpected := 0, 0
	wantDelivery, wantNoResult := 0, 0
	wantEvaluatedPRs := 0
	wantEscalations, wantEscalationSample := 0, 0
	wantLanguage := map[string]int{}
	for _, entry := range manifest.Entries {
		wantExpected += len(entry.Expected.Findings)
		wantLanguage[entry.Language]++
		if entry.Observation.Delivery == "failed" {
			wantDelivery++
		}
		if entry.Observation.Review == "" {
			if entry.Observation.Delivery != "failed" {
				wantNoResult++
			}
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Observation.Review))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Observation.Review, err)
		}
		var wire struct {
			Comments []json.RawMessage `json:"comments"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("decode %s: %v", entry.Observation.Review, err)
		}
		wantComments += len(wire.Comments)
		if len(wire.Comments) > 0 {
			wantEvaluatedPRs++
		}
		review, err := parseNativeReview(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Observation.Review, err)
		}
		status, _ := nativeOutcome(review)
		if entry.Observation.Delivery != "failed" {
			wantEscalationSample++
			if status != store.RunSucceeded {
				wantEscalations++
			}
		}
	}

	if report.Overall.ActualComments != wantComments {
		t.Errorf("actual comments = %d, want %d from the fixtures",
			report.Overall.ActualComments, wantComments)
	}
	if report.Overall.ExpectedFindings != wantExpected {
		t.Errorf("expected findings = %d, want %d from the manifest",
			report.Overall.ExpectedFindings, wantExpected)
	}
	if report.Overall.DeliveryFailures != wantDelivery {
		t.Errorf("delivery failures = %d, want %d from the observations",
			report.Overall.DeliveryFailures, wantDelivery)
	}
	if report.Overall.EligiblePRsWithoutResult != wantNoResult {
		t.Errorf("eligible PRs without result = %d, want %d",
			report.Overall.EligiblePRsWithoutResult, wantNoResult)
	}
	if report.Overall.PRsWithEvaluatedComments != wantEvaluatedPRs {
		t.Errorf("PRs with evaluated comments = %d, want %d from the fixtures",
			report.Overall.PRsWithEvaluatedComments, wantEvaluatedPRs)
	}
	if report.Overall.ConfirmedEscalations != wantEscalations {
		t.Errorf("confirmed escalations = %d, want %d excluding delivery failures",
			report.Overall.ConfirmedEscalations, wantEscalations)
	}
	if report.Overall.EscalationSampleSize != wantEscalationSample {
		t.Errorf("escalation sample = %d, want %d",
			report.Overall.EscalationSampleSize, wantEscalationSample)
	}
	for language, want := range wantLanguage {
		if got := report.ByLanguage[language].PRs; got != want {
			t.Errorf("language %q sample = %d, want %d", language, got, want)
		}
	}
	if report.ByLanguage["go"].CitationAccuracy >= 1 {
		t.Errorf("go citation accuracy = %.4f, want < 1 because one fixture cites code absent from the head file",
			report.ByLanguage["go"].CitationAccuracy)
	}
}

// TestReviewCorpusProvenanceRecorded checks that every observation was captured
// with the comparison, OCR version and profile fixed in advance.
func TestReviewCorpusProvenanceRecorded(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	hasReview := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		hasReview[entry.ID] = entry.Observation.Review != ""
	}
	verified, unrecorded := 0, 0
	for _, entry := range report.Entries {
		want := corpusProvenanceUnrecorded
		if hasReview[entry.ID] {
			want = corpusProvenanceVerified
		}
		if entry.Provenance != want {
			t.Errorf("%s: provenance = %q, want %q (mismatches: %v)",
				entry.ID, entry.Provenance, want, entry.Mismatches)
		}
		if entry.Provenance == corpusProvenanceVerified {
			verified++
		}
		if entry.Provenance == corpusProvenanceUnrecorded {
			unrecorded++
		}
	}
	if verified == 0 || unrecorded == 0 {
		t.Errorf("corpus needs verified and unrecorded provenance, got %d/%d", verified, unrecorded)
	}
}

// TestReviewCorpusCoversRequiredClasses encodes the acceptance criteria: the
// corpus must include clean and defective pull requests across languages,
// tests/config/infra and cross-module changes, plus every failure class.
func TestReviewCorpusCoversRequiredClasses(t *testing.T) {
	dir := filepath.Join("testdata", "corpus")
	manifest, err := loadReviewCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	report, err := evaluateReviewCorpus(dir, manifest)
	if err != nil {
		t.Fatalf("evaluate corpus: %v", err)
	}
	languages := map[string]bool{}
	changeTypes := map[string]bool{}
	clean, defective, escalated, deliveryFailure, noResult := 0, 0, 0, 0, 0
	for _, entry := range manifest.Entries {
		languages[entry.Language] = true
		changeTypes[entry.ChangeType] = true
		if len(entry.Expected.Findings) == 0 && entry.Expected.Result == store.RunSucceeded {
			clean++
		} else {
			defective++
		}
		if entry.Expected.Escalation {
			escalated++
		}
		if entry.Observation.Delivery == "failed" {
			deliveryFailure++
		}
		if entry.Observation.Review == "" && entry.Observation.Delivery != "failed" {
			noResult++
		}
	}
	for _, language := range []string{"go", "typescript", "python", "dockerfile", "yaml", "sql"} {
		if !languages[language] {
			t.Errorf("corpus has no %s pull request", language)
		}
	}
	for _, changeType := range []string{"code", "test", "config", "infra", "cross_module", "data"} {
		if !changeTypes[changeType] {
			t.Errorf("corpus has no %s change", changeType)
		}
	}
	if clean == 0 || defective == 0 {
		t.Errorf("corpus needs clean and defective PRs, got clean=%d defective=%d", clean, defective)
	}
	if escalated == 0 || deliveryFailure == 0 || noResult == 0 {
		t.Errorf("corpus needs escalation, delivery-failure and no-result cases, got %d/%d/%d",
			escalated, deliveryFailure, noResult)
	}
	// These measurement classes must stay represented; deleting their fixtures
	// would silently hide the quality gap the corpus exists to document.
	required := map[string]int{
		"missed severe defects":       report.Overall.MissedSevereDefects,
		"false positives":             report.Overall.FalsePositives,
		"useful escalations":          report.Overall.UsefulEscalations,
		"unnecessary escalations":     report.Overall.UnnecessaryEscalations,
		"delivery failures":           report.Overall.DeliveryFailures,
		"eligible PRs without result": report.Overall.EligiblePRsWithoutResult,
	}
	for name, count := range required {
		if count < 1 {
			t.Errorf("corpus lost its %s fixture (count=%d)", name, count)
		}
	}
}
