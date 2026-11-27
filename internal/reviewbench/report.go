package reviewbench

import (
	"fmt"
	"sort"
)

// Operational counts every observation by result. Incomplete coverage,
// abstention, timeout and delivery error are sample results, not missing data.
type Operational struct {
	Observations       int `json:"observations"`
	Completed          int `json:"completed"`
	IncompleteCoverage int `json:"incomplete_coverage"`
	Abstention         int `json:"abstention"`
	Timeout            int `json:"timeout"`
	DeliveryError      int `json:"delivery_error"`
	SupersededPush     int `json:"superseded_push"`
}

// Metrics are the measured outcomes for one reviewer or one class. Every ratio
// keeps its numerator and denominator and a Wilson 95% interval.
type Metrics struct {
	UniqueFindings     int        `json:"unique_findings"`
	MatchedFindings    int        `json:"matched_findings"`
	ExpectedFindings   int        `json:"expected_findings"`
	SevereExpected     int        `json:"severe_expected"`
	SevereDetected     int        `json:"severe_detected"`
	SeverityMismatches int        `json:"severity_mismatches"`
	BlockingFindings   int        `json:"blocking_findings"`
	FalseBlocks        int        `json:"false_blocks"`
	DuplicateComments  int        `json:"duplicate_comments"`
	RawComments        int        `json:"raw_comments"`
	CitationChecked    int        `json:"citation_checked"`
	CitationAccurate   int        `json:"citation_accurate"`
	Precision          Proportion `json:"precision"`
	Detection          Proportion `json:"detection"`
	SevereDetection    Proportion `json:"severe_detection"`
	CitationAccuracy   Proportion `json:"citation_accuracy"`
	FalseBlockRate     Proportion `json:"false_block_rate"`
	DuplicateRate      Proportion `json:"duplicate_rate"`
}

// Cost reports the cost per completed review only when every completed review
// has a comparable cost; otherwise PerCompletedUSD is nil.
type Cost struct {
	Completed         int      `json:"completed"`
	CompletedWithCost int      `json:"completed_with_cost"`
	TotalUSD          float64  `json:"total_usd"`
	PerCompletedUSD   *float64 `json:"per_completed_usd,omitempty"`
	Comparable        bool     `json:"comparable"`
}

// ReviewerReport is the blinded result for one reviewer.
type ReviewerReport struct {
	Reviewer         string       `json:"reviewer"`
	Coverage         Proportion   `json:"coverage"`
	Operational      Operational  `json:"operational"`
	Metrics          Metrics      `json:"metrics"`
	LatencyCompleted LatencyStats `json:"latency_completed"`
	LatencyAll       LatencyStats `json:"latency_all"`
	Cost             Cost         `json:"cost"`
}

// SampleReport describes the sample and its operational outcomes globally.
type SampleReport struct {
	Cases              int `json:"cases"`
	BlindCases         int `json:"blind_cases"`
	Observations       int `json:"observations"`
	Reviewers          int `json:"reviewers"`
	Completed          int `json:"completed"`
	IncompleteCoverage int `json:"incomplete_coverage"`
	Abstention         int `json:"abstention"`
	Timeout            int `json:"timeout"`
	DeliveryError      int `json:"delivery_error"`
	SupersededPush     int `json:"superseded_push"`
}

// ClassReport is one reviewer's metrics for one language or change type.
type ClassReport struct {
	Reviewer    string      `json:"reviewer"`
	Dimension   string      `json:"dimension"`
	Class       string      `json:"class"`
	Coverage    Proportion  `json:"coverage"`
	Operational Operational `json:"operational"`
	Metrics     Metrics     `json:"metrics"`
}

// PairedReport is the exact McNemar comparison of two blind reviewers on the
// cases both completed. Outcome is false_positive_case or
// severe_case_detection; Dimension is all, language or change_type.
type PairedReport struct {
	ReviewerA      string     `json:"reviewer_a"`
	ReviewerB      string     `json:"reviewer_b"`
	Outcome        string     `json:"outcome"`
	Dimension      string     `json:"dimension"`
	Class          string     `json:"class,omitempty"`
	Cases          int        `json:"cases"`
	Both           int        `json:"both"`
	Neither        int        `json:"neither"`
	AOnly          int        `json:"a_only"`
	BOnly          int        `json:"b_only"`
	A              Proportion `json:"a"`
	B              Proportion `json:"b"`
	Difference     float64    `json:"difference"`
	DifferenceCI95 *Interval  `json:"difference_ci95,omitempty"`
	McNemarP       float64    `json:"mcnemar_p"`
}

// ThresholdCheck is one protocol threshold evaluated against the blind sample.
type ThresholdCheck struct {
	Name   string   `json:"name"`
	Target float64  `json:"target"`
	Actual *float64 `json:"actual,omitempty"`
	Met    bool     `json:"met"`
}

// ThresholdReport is the pilot gate for the configured target reviewer.
type ThresholdReport struct {
	Reviewer  string           `json:"reviewer,omitempty"`
	Scope     string           `json:"scope"`
	Evaluated bool             `json:"evaluated"`
	Met       bool             `json:"met"`
	Reason    string           `json:"reason,omitempty"`
	Checks    []ThresholdCheck `json:"checks"`
}

// Report is the blinded benchmark report. It contains blind labels and finding
// tokens only; names are revealed separately with Reveal after adjudication.
type Report struct {
	SchemaVersion   string             `json:"schema_version"`
	ManifestVersion string             `json:"manifest_version"`
	ManifestLock    string             `json:"manifest_lock"`
	Sample          SampleReport       `json:"sample"`
	Reviewers       []ReviewerReport   `json:"reviewers"`
	BlindReviewers  []ReviewerReport   `json:"blind_reviewers"`
	ClassMetrics    []ClassReport      `json:"class_metrics"`
	Paired          []PairedReport     `json:"paired"`
	Adjudication    AdjudicationReport `json:"adjudication"`
	Thresholds      ThresholdReport    `json:"thresholds"`
}

// RevealedReport is the post-adjudication reveal. It exists only through
// Reveal, which refuses while any novel finding is still pending.
type RevealedReport struct {
	ManifestVersion string                `json:"manifest_version"`
	Reviewers       map[string]Reviewer   `json:"reviewers"`
	Findings        map[string][]Reviewer `json:"findings"`
}

// BuildReport computes every metric from the sealed dataset. It never reads
// reviewer names.
func BuildReport(dataset SealedDataset, decisions []AdjudicationDecision) (Report, error) {
	if err := dataset.Manifest.Validate(); err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion:   SchemaVersion,
		ManifestVersion: dataset.Manifest.Version,
		ManifestLock:    dataset.Manifest.Lock,
	}
	labels := reviewerLabels(dataset)
	report.Sample = sampleReport(dataset, labels)
	for _, label := range labels {
		report.Reviewers = append(report.Reviewers, buildReviewerReport(label, dataset.Manifest, filterResults(dataset.Results, func(r BlindResult) bool {
			return r.Reviewer == label
		})))
		blind := filterResults(dataset.Results, func(r BlindResult) bool {
			return r.Reviewer == label && caseBlind(dataset.Manifest, r.CaseID)
		})
		report.BlindReviewers = append(report.BlindReviewers, buildReviewerReport(label, dataset.Manifest, blind))
		report.ClassMetrics = append(report.ClassMetrics, buildClassReports(label, dataset.Manifest, dataset.Results)...)
	}
	report.Paired = buildPairedReports(dataset, labels)

	novel := NovelFindings(dataset)
	adjudication, err := Adjudicate(novel, decisions)
	if err != nil {
		return Report{}, err
	}
	report.Adjudication = adjudication
	report.Thresholds = evaluateThresholds(dataset)
	return report, nil
}

// Reveal exposes names after adjudication. It fails while any candidate is
// pending, so names cannot become visible early.
func Reveal(report Report, mapping SealedMapping) (RevealedReport, error) {
	if len(report.Adjudication.Pending) > 0 {
		return RevealedReport{}, fmt.Errorf("%w: %d findings pending", ErrAdjudicationPending, len(report.Adjudication.Pending))
	}
	revealed := RevealedReport{
		ManifestVersion: report.ManifestVersion,
		Reviewers:       make(map[string]Reviewer),
		Findings:        make(map[string][]Reviewer),
	}
	for label, reviewer := range mapping.labels {
		revealed.Reviewers[label] = reviewer
	}
	for _, decision := range report.Adjudication.Decisions {
		reviewers, ok := mapping.FindingReviewers(decision.Token)
		if !ok {
			return RevealedReport{}, fmt.Errorf("%w: token %q is absent from the sealed mapping", ErrAdjudication, decision.Token)
		}
		revealed.Findings[decision.Token] = reviewers
	}
	return revealed, nil
}

type tally struct {
	operational      Operational
	unique           int
	matched          int
	expected         int
	detected         int
	severeExpected   int
	severeDetected   int
	severityMismatch int
	blocking         int
	falseBlocks      int
	duplicates       int
	raw              int
	citationChecked  int
	citationAccurate int
	completedLatency []int64
	allLatency       []int64
	costTotal        float64
	costCount        int
	costMissing      bool
}

func (t *tally) add(manifest Manifest, result BlindResult) {
	t.operational.Observations++
	if result.LatencyMS > 0 {
		t.allLatency = append(t.allLatency, result.LatencyMS)
	}
	switch result.Result {
	case ResultCompleted:
		t.operational.Completed++
		if result.LatencyMS > 0 {
			t.completedLatency = append(t.completedLatency, result.LatencyMS)
		}
		if result.CostUSD != nil {
			t.costTotal += *result.CostUSD
			t.costCount++
		} else {
			t.costMissing = true
		}
	case ResultIncompleteCoverage:
		t.operational.IncompleteCoverage++
	case ResultAbstention:
		t.operational.Abstention++
	case ResultTimeout:
		t.operational.Timeout++
	case ResultDeliveryError:
		t.operational.DeliveryError++
	}
	if result.Superseded {
		t.operational.SupersededPush++
	}
	if result.Result != ResultCompleted {
		return
	}
	c, ok := caseByID(manifest, result.CaseID)
	if !ok {
		return
	}
	t.duplicates += result.Duplicates
	t.raw += len(result.Findings) + result.Duplicates
	matches, actualMatched := matchFindings(c.Expected, result.Findings, manifest.ContextRadius)
	expectedByFinding := make([]int, len(result.Findings))
	for i := range expectedByFinding {
		expectedByFinding[i] = -1
	}
	for i, matchedIndex := range matches {
		if matchedIndex >= 0 {
			expectedByFinding[matchedIndex] = i
		}
	}
	for i, want := range c.Expected {
		t.expected++
		severe := IsSevere(want.Severity)
		if severe {
			t.severeExpected++
		}
		if matches[i] < 0 {
			continue
		}
		t.detected++
		if severe {
			t.severeDetected++
		}
		if observed := result.Findings[matches[i]].Severity; observed != want.Severity {
			t.severityMismatch++
		}
	}
	for j, finding := range result.Findings {
		t.unique++
		if actualMatched[j] {
			t.matched++
		}
		t.citationChecked++
		if citationAccurate(c, finding) {
			t.citationAccurate++
		}
		if !isBlocking(finding) {
			continue
		}
		t.blocking++
		trueBlock := expectedByFinding[j] >= 0 && IsSevere(c.Expected[expectedByFinding[j]].Severity)
		if !trueBlock {
			t.falseBlocks++
		}
	}
}

func (t *tally) metrics() Metrics {
	return Metrics{
		UniqueFindings:     t.unique,
		MatchedFindings:    t.matched,
		ExpectedFindings:   t.expected,
		SevereExpected:     t.severeExpected,
		SevereDetected:     t.severeDetected,
		SeverityMismatches: t.severityMismatch,
		BlockingFindings:   t.blocking,
		FalseBlocks:        t.falseBlocks,
		DuplicateComments:  t.duplicates,
		RawComments:        t.raw,
		CitationChecked:    t.citationChecked,
		CitationAccurate:   t.citationAccurate,
		Precision:          NewProportion(t.matched, t.unique),
		Detection:          NewProportion(t.detected, t.expected),
		SevereDetection:    NewProportion(t.severeDetected, t.severeExpected),
		CitationAccuracy:   NewProportion(t.citationAccurate, t.citationChecked),
		FalseBlockRate:     NewProportion(t.falseBlocks, t.blocking),
		DuplicateRate:      NewProportion(t.duplicates, t.raw),
	}
}

// cost reports the total and the per-completed-review cost when every
// completed review carried a comparable cost.
func (t *tally) cost() Cost {
	cost := Cost{Completed: t.operational.Completed, CompletedWithCost: t.costCount, TotalUSD: t.costTotal}
	if t.operational.Completed > 0 && !t.costMissing && t.costCount == t.operational.Completed {
		per := t.costTotal / float64(t.costCount)
		cost.PerCompletedUSD = &per
		cost.Comparable = true
	}
	return cost
}

func buildReviewerReport(label string, manifest Manifest, results []BlindResult) ReviewerReport {
	t := &tally{}
	for _, result := range results {
		t.add(manifest, result)
	}
	return ReviewerReport{
		Reviewer:         label,
		Coverage:         NewProportion(t.operational.Completed, t.operational.Observations),
		Operational:      t.operational,
		Metrics:          t.metrics(),
		LatencyCompleted: Latency(t.completedLatency),
		LatencyAll:       Latency(t.allLatency),
		Cost:             t.cost(),
	}
}

func sampleReport(dataset SealedDataset, labels []string) SampleReport {
	sample := SampleReport{Cases: len(dataset.Manifest.Cases), Reviewers: len(labels)}
	for _, c := range dataset.Manifest.Cases {
		if c.Blind {
			sample.BlindCases++
		}
	}
	for _, result := range dataset.Results {
		sample.Observations++
		switch result.Result {
		case ResultCompleted:
			sample.Completed++
		case ResultIncompleteCoverage:
			sample.IncompleteCoverage++
		case ResultAbstention:
			sample.Abstention++
		case ResultTimeout:
			sample.Timeout++
		case ResultDeliveryError:
			sample.DeliveryError++
		}
		if result.Superseded {
			sample.SupersededPush++
		}
	}
	return sample
}

func buildClassReports(label string, manifest Manifest, results []BlindResult) []ClassReport {
	var reports []ClassReport
	for _, dimension := range []string{"language", "change_type"} {
		byClass := make(map[string][]BlindResult)
		for _, result := range results {
			if result.Reviewer != label {
				continue
			}
			c, ok := caseByID(manifest, result.CaseID)
			if !ok {
				continue
			}
			class := c.Language
			if dimension == "change_type" {
				class = c.ChangeType
			}
			byClass[class] = append(byClass[class], result)
		}
		classes := make([]string, 0, len(byClass))
		for class := range byClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			t := &tally{}
			for _, result := range byClass[class] {
				t.add(manifest, result)
			}
			reports = append(reports, ClassReport{
				Reviewer: label, Dimension: dimension, Class: class,
				Coverage:    NewProportion(t.operational.Completed, t.operational.Observations),
				Operational: t.operational, Metrics: t.metrics(),
			})
		}
	}
	return reports
}

type caseOutcome struct {
	falsePositive     bool
	hasSevere         bool
	allSevereDetected bool
}

func evaluateCase(manifest Manifest, result BlindResult) (caseOutcome, bool) {
	if result.Result != ResultCompleted {
		return caseOutcome{}, false
	}
	c, ok := caseByID(manifest, result.CaseID)
	if !ok {
		return caseOutcome{}, false
	}
	matches, actualMatched := matchFindings(c.Expected, result.Findings, manifest.ContextRadius)
	outcome := caseOutcome{allSevereDetected: true}
	for _, matched := range actualMatched {
		if !matched {
			outcome.falsePositive = true
			break
		}
	}
	severe := 0
	detected := 0
	for i, want := range c.Expected {
		if !IsSevere(want.Severity) {
			continue
		}
		severe++
		if matches[i] >= 0 {
			detected++
		}
	}
	outcome.hasSevere = severe > 0
	outcome.allSevereDetected = severe > 0 && detected == severe
	return outcome, true
}

func buildPairedReports(dataset SealedDataset, labels []string) []PairedReport {
	type outcomes struct {
		byCase map[string]caseOutcome
	}
	perReviewer := make(map[string]outcomes, len(labels))
	for _, label := range labels {
		entry := outcomes{byCase: make(map[string]caseOutcome)}
		for _, result := range dataset.Results {
			if result.Reviewer != label {
				continue
			}
			if outcome, ok := evaluateCase(dataset.Manifest, result); ok {
				entry.byCase[result.CaseID] = outcome
			}
		}
		perReviewer[label] = entry
	}

	scopes := pairedScopes(dataset.Manifest)
	var reports []PairedReport
	for i := 0; i < len(labels); i++ {
		for j := i + 1; j < len(labels); j++ {
			a, b := labels[i], labels[j]
			for _, outcomeName := range []string{"false_positive_case", "severe_case_detection"} {
				for _, scope := range scopes {
					cases := sharedCases(dataset.Manifest, perReviewer[a].byCase, perReviewer[b].byCase, scope)
					var aValues, bValues []bool
					for _, c := range cases {
						oa := perReviewer[a].byCase[c.ID]
						ob := perReviewer[b].byCase[c.ID]
						switch outcomeName {
						case "false_positive_case":
							aValues = append(aValues, oa.falsePositive)
							bValues = append(bValues, ob.falsePositive)
						case "severe_case_detection":
							if !oa.hasSevere {
								continue
							}
							aValues = append(aValues, oa.allSevereDetected)
							bValues = append(bValues, ob.allSevereDetected)
						}
					}
					if len(aValues) == 0 {
						continue
					}
					paired, err := ComparePaired(aValues, bValues)
					if err != nil {
						continue
					}
					reports = append(reports, PairedReport{
						ReviewerA: a, ReviewerB: b, Outcome: outcomeName,
						Dimension: scope.dimension, Class: scope.class, Cases: paired.N,
						Both: paired.Both, Neither: paired.Neither, AOnly: paired.ATrueBFalse, BOnly: paired.AFalseBTrue,
						A: paired.A, B: paired.B, Difference: paired.Difference,
						DifferenceCI95: paired.DifferenceCI95, McNemarP: paired.McNemarP,
					})
				}
			}
		}
	}
	return reports
}

type scope struct {
	dimension string
	class     string
}

func pairedScopes(manifest Manifest) []scope {
	languages := make(map[string]bool)
	changeTypes := make(map[string]bool)
	for _, c := range manifest.Cases {
		languages[c.Language] = true
		changeTypes[c.ChangeType] = true
	}
	scopes := []scope{{dimension: "all"}}
	for _, class := range sortedKeys(languages) {
		scopes = append(scopes, scope{dimension: "language", class: class})
	}
	for _, class := range sortedKeys(changeTypes) {
		scopes = append(scopes, scope{dimension: "change_type", class: class})
	}
	return scopes
}

func sharedCases(manifest Manifest, a, b map[string]caseOutcome, s scope) []Case {
	var cases []Case
	for _, c := range manifest.Cases {
		if _, ok := a[c.ID]; !ok {
			continue
		}
		if _, ok := b[c.ID]; !ok {
			continue
		}
		if s.dimension == "language" && c.Language != s.class {
			continue
		}
		if s.dimension == "change_type" && c.ChangeType != s.class {
			continue
		}
		cases = append(cases, c)
	}
	sort.SliceStable(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases
}

func evaluateThresholds(dataset SealedDataset) ThresholdReport {
	thresholds := dataset.Manifest.Thresholds
	report := ThresholdReport{Reviewer: dataset.TargetReviewer, Scope: "blind"}
	if !thresholds.Configured {
		report.Reason = "thresholds are not configured"
		return report
	}
	if dataset.TargetReviewer == "" {
		report.Reason = "no target reviewer"
		return report
	}
	var results []BlindResult
	for _, result := range dataset.Results {
		if result.Reviewer == dataset.TargetReviewer && caseBlind(dataset.Manifest, result.CaseID) {
			results = append(results, result)
		}
	}
	t := &tally{}
	for _, result := range results {
		t.add(dataset.Manifest, result)
	}
	metrics := t.metrics()
	coverage := NewProportion(t.operational.Completed, t.operational.Observations)
	if t.operational.Completed == 0 {
		report.Reason = "no completed blind observations for the target reviewer"
		return report
	}
	report.Evaluated = true
	report.Checks = []ThresholdCheck{
		atLeast("evaluated_cases", float64(thresholds.MinEvaluatedCases), float64(t.operational.Completed)),
		atLeastProportion("coverage", thresholds.MinCoverage, coverage),
		atLeastProportion("precision", thresholds.MinPrecision, metrics.Precision),
		atLeastProportion("severe_detection", thresholds.MinSevereDetection, metrics.SevereDetection),
		atLeastProportion("citation_accuracy", thresholds.MinCitationAccuracy, metrics.CitationAccuracy),
		atMostProportion("false_block_rate", thresholds.MaxFalseBlockRate, metrics.FalseBlockRate),
		atMostProportion("duplicate_rate", thresholds.MaxDuplicateRate, metrics.DuplicateRate),
	}
	report.Met = true
	for _, check := range report.Checks {
		if !check.Met {
			report.Met = false
			break
		}
	}
	return report
}

func atLeast(name string, target, actual float64) ThresholdCheck {
	value := actual
	return ThresholdCheck{Name: name, Target: target, Actual: &value, Met: actual >= target}
}

func atLeastProportion(name string, target float64, proportion Proportion) ThresholdCheck {
	if proportion.Estimate == nil {
		return ThresholdCheck{Name: name, Target: target}
	}
	return ThresholdCheck{Name: name, Target: target, Actual: proportion.Estimate, Met: *proportion.Estimate >= target}
}

func atMostProportion(name string, target float64, proportion Proportion) ThresholdCheck {
	if proportion.Estimate == nil {
		return ThresholdCheck{Name: name, Target: target}
	}
	return ThresholdCheck{Name: name, Target: target, Actual: proportion.Estimate, Met: *proportion.Estimate <= target}
}

func isBlocking(finding ObservedFinding) bool {
	return finding.Blocking || finding.Severity == "blocker"
}

func reviewerLabels(dataset SealedDataset) []string {
	seen := make(map[string]bool)
	for _, result := range dataset.Results {
		seen[result.Reviewer] = true
	}
	return sortedKeys(seen)
}

func filterResults(results []BlindResult, keep func(BlindResult) bool) []BlindResult {
	var filtered []BlindResult
	for _, result := range results {
		if keep(result) {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func caseBlind(manifest Manifest, id string) bool {
	c, ok := caseByID(manifest, id)
	return ok && c.Blind
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
