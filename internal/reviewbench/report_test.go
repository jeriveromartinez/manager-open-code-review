package reviewbench

import (
	"encoding/json"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) (Manifest, Dataset, SealedDataset, SealedMapping) {
	t.Helper()
	manifest, err := LoadManifest("testdata/v1/manifest.json")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	observations, err := LoadObservations("testdata/v1/observations.json")
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	dataset, err := BuildDataset(manifest, observations)
	if err != nil {
		t.Fatalf("BuildDataset: %v", err)
	}
	sealed, mapping := Seal(dataset)
	return manifest, dataset, sealed, mapping
}

func findReviewerReport(t *testing.T, reports []ReviewerReport, label string) ReviewerReport {
	t.Helper()
	for _, report := range reports {
		if report.Reviewer == label {
			return report
		}
	}
	t.Fatalf("reviewer %q not found", label)
	return ReviewerReport{}
}

func wantProportion(t *testing.T, name string, proportion Proportion, successes, total int) {
	t.Helper()
	if proportion.Successes != successes || proportion.Total != total {
		t.Errorf("%s = %d/%d, want %d/%d", name, proportion.Successes, proportion.Total, successes, total)
	}
	want := 0.0
	if total > 0 {
		want = float64(successes) / float64(total)
		if proportion.Estimate == nil || !almostEqual(*proportion.Estimate, want, 1e-12) {
			t.Errorf("%s estimate = %v, want %v", name, proportion.Estimate, want)
		}
		if proportion.CI95 == nil {
			t.Errorf("%s has no confidence interval", name)
		}
	} else if proportion.Estimate != nil {
		t.Errorf("%s estimate = %v, want undefined", name, *proportion.Estimate)
	}
}

func checkMetricSet(t *testing.T, prefix string, metrics Metrics, expected Metrics) {
	t.Helper()
	if metrics.UniqueFindings != expected.UniqueFindings ||
		metrics.MatchedFindings != expected.MatchedFindings ||
		metrics.ExpectedFindings != expected.ExpectedFindings ||
		metrics.SevereExpected != expected.SevereExpected ||
		metrics.SevereDetected != expected.SevereDetected ||
		metrics.SeverityMismatches != expected.SeverityMismatches ||
		metrics.BlockingFindings != expected.BlockingFindings ||
		metrics.FalseBlocks != expected.FalseBlocks ||
		metrics.DuplicateComments != expected.DuplicateComments ||
		metrics.RawComments != expected.RawComments ||
		metrics.CitationChecked != expected.CitationChecked ||
		metrics.CitationAccurate != expected.CitationAccurate {
		t.Errorf("%s counters = %+v, want %+v", prefix, metrics, expected)
	}
	wantProportion(t, prefix+" precision", metrics.Precision, expected.Precision.Successes, expected.Precision.Total)
	wantProportion(t, prefix+" detection", metrics.Detection, expected.Detection.Successes, expected.Detection.Total)
	wantProportion(t, prefix+" severe", metrics.SevereDetection, expected.SevereDetection.Successes, expected.SevereDetection.Total)
	wantProportion(t, prefix+" citation", metrics.CitationAccuracy, expected.CitationAccuracy.Successes, expected.CitationAccuracy.Total)
	wantProportion(t, prefix+" false block", metrics.FalseBlockRate, expected.FalseBlockRate.Successes, expected.FalseBlockRate.Total)
	wantProportion(t, prefix+" duplicate", metrics.DuplicateRate, expected.DuplicateRate.Successes, expected.DuplicateRate.Total)
}

func TestFixtureSampleAccounting(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := SampleReport{
		Cases: 9, BlindCases: 4, Observations: 18, Reviewers: 2,
		Completed: 12, IncompleteCoverage: 3, Abstention: 1, Timeout: 1,
		DeliveryError: 1, SupersededPush: 2,
	}
	if report.Sample != want {
		t.Errorf("sample = %+v, want %+v", report.Sample, want)
	}
	own := labelFor(t, mapping, "own")
	external := labelFor(t, mapping, "coderabbit")
	ownReport := findReviewerReport(t, report.Reviewers, own)
	externalReport := findReviewerReport(t, report.Reviewers, external)
	if ownReport.Operational.Observations != 9 || ownReport.Operational.Completed != 4 ||
		ownReport.Operational.IncompleteCoverage != 2 || ownReport.Operational.Abstention != 1 ||
		ownReport.Operational.Timeout != 1 || ownReport.Operational.DeliveryError != 1 ||
		ownReport.Operational.SupersededPush != 1 {
		t.Errorf("own operational = %+v", ownReport.Operational)
	}
	wantProportion(t, "own coverage", ownReport.Coverage, 4, 9)
	if externalReport.Operational.Completed != 8 || externalReport.Operational.IncompleteCoverage != 1 ||
		externalReport.Operational.SupersededPush != 1 {
		t.Errorf("external operational = %+v", externalReport.Operational)
	}
	wantProportion(t, "external coverage", externalReport.Coverage, 8, 9)
}

func TestFixtureReviewerMetricsAreExact(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	own := findReviewerReport(t, report.Reviewers, labelFor(t, mapping, "own"))
	checkMetricSet(t, "own", own.Metrics, Metrics{
		UniqueFindings: 4, MatchedFindings: 3, ExpectedFindings: 3,
		SevereExpected: 2, SevereDetected: 2, SeverityMismatches: 1,
		BlockingFindings: 2, FalseBlocks: 1, DuplicateComments: 1, RawComments: 5,
		CitationChecked: 4, CitationAccurate: 3,
		Precision:        Proportion{Successes: 3, Total: 4},
		Detection:        Proportion{Successes: 3, Total: 3},
		SevereDetection:  Proportion{Successes: 2, Total: 2},
		CitationAccuracy: Proportion{Successes: 3, Total: 4},
		FalseBlockRate:   Proportion{Successes: 1, Total: 2},
		DuplicateRate:    Proportion{Successes: 1, Total: 5},
	})
	if own.LatencyCompleted.Samples != 4 || own.LatencyCompleted.P50MS != 1000 || own.LatencyCompleted.P95MS != 1200 {
		t.Errorf("own completed latency = %+v", own.LatencyCompleted)
	}
	if own.LatencyAll.Samples != 9 || own.LatencyAll.P50MS != 1200 || own.LatencyAll.P95MS != 30000 {
		t.Errorf("own all latency = %+v", own.LatencyAll)
	}
	if !own.Cost.Comparable || own.Cost.CompletedWithCost != 4 ||
		!almostEqual(own.Cost.TotalUSD, 0.1, 1e-12) ||
		own.Cost.PerCompletedUSD == nil || !almostEqual(*own.Cost.PerCompletedUSD, 0.025, 1e-12) {
		t.Errorf("own cost = %+v", own.Cost)
	}

	external := findReviewerReport(t, report.Reviewers, labelFor(t, mapping, "coderabbit"))
	checkMetricSet(t, "external", external.Metrics, Metrics{
		UniqueFindings: 6, MatchedFindings: 5, ExpectedFindings: 7,
		SevereExpected: 5, SevereDetected: 4, SeverityMismatches: 0,
		BlockingFindings: 2, FalseBlocks: 0, DuplicateComments: 0, RawComments: 6,
		CitationChecked: 6, CitationAccurate: 6,
		Precision:        Proportion{Successes: 5, Total: 6},
		Detection:        Proportion{Successes: 5, Total: 7},
		SevereDetection:  Proportion{Successes: 4, Total: 5},
		CitationAccuracy: Proportion{Successes: 6, Total: 6},
		FalseBlockRate:   Proportion{Successes: 0, Total: 2},
		DuplicateRate:    Proportion{Successes: 0, Total: 6},
	})
	if external.LatencyCompleted.Samples != 8 || external.LatencyCompleted.P50MS != 1800 || external.LatencyCompleted.P95MS != 2200 {
		t.Errorf("external completed latency = %+v", external.LatencyCompleted)
	}
	if external.LatencyAll.Samples != 9 || external.LatencyAll.P50MS != 1800 || external.LatencyAll.P95MS != 2200 {
		t.Errorf("external all latency = %+v", external.LatencyAll)
	}
	if external.Cost.Comparable || external.Cost.CompletedWithCost != 0 || external.Cost.PerCompletedUSD != nil {
		t.Errorf("external cost = %+v, want not comparable", external.Cost)
	}
}

func TestFixtureBlindSubsetMetrics(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	own := findReviewerReport(t, report.BlindReviewers, labelFor(t, mapping, "own"))
	wantProportion(t, "blind own coverage", own.Coverage, 3, 4)
	checkMetricSet(t, "blind own", own.Metrics, Metrics{
		UniqueFindings: 3, MatchedFindings: 3, ExpectedFindings: 3,
		SevereExpected: 2, SevereDetected: 2, SeverityMismatches: 1,
		BlockingFindings: 1, FalseBlocks: 0, DuplicateComments: 1, RawComments: 4,
		CitationChecked: 3, CitationAccurate: 2,
		Precision:        Proportion{Successes: 3, Total: 3},
		Detection:        Proportion{Successes: 3, Total: 3},
		SevereDetection:  Proportion{Successes: 2, Total: 2},
		CitationAccuracy: Proportion{Successes: 2, Total: 3},
		FalseBlockRate:   Proportion{Successes: 0, Total: 1},
		DuplicateRate:    Proportion{Successes: 1, Total: 4},
	})

	external := findReviewerReport(t, report.BlindReviewers, labelFor(t, mapping, "coderabbit"))
	wantProportion(t, "blind external coverage", external.Coverage, 4, 4)
	checkMetricSet(t, "blind external", external.Metrics, Metrics{
		UniqueFindings: 4, MatchedFindings: 3, ExpectedFindings: 4,
		SevereExpected: 2, SevereDetected: 2, SeverityMismatches: 0,
		BlockingFindings: 0, FalseBlocks: 0, DuplicateComments: 0, RawComments: 4,
		CitationChecked: 4, CitationAccurate: 4,
		Precision:        Proportion{Successes: 3, Total: 4},
		Detection:        Proportion{Successes: 3, Total: 4},
		SevereDetection:  Proportion{Successes: 2, Total: 2},
		CitationAccuracy: Proportion{Successes: 4, Total: 4},
		FalseBlockRate:   Proportion{Successes: 0, Total: 0},
		DuplicateRate:    Proportion{Successes: 0, Total: 4},
	})
}

func TestFixtureClassMetrics(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	own := labelFor(t, mapping, "own")
	external := labelFor(t, mapping, "coderabbit")
	find := func(reviewer, dimension, class string) ClassReport {
		t.Helper()
		for _, entry := range report.ClassMetrics {
			if entry.Reviewer == reviewer && entry.Dimension == dimension && entry.Class == class {
				return entry
			}
		}
		t.Fatalf("class %s/%s for %s not found", dimension, class, reviewer)
		return ClassReport{}
	}

	goOwn := find(own, "language", "go")
	wantProportion(t, "own go coverage", goOwn.Coverage, 2, 4)
	checkMetricSet(t, "own go", goOwn.Metrics, Metrics{
		UniqueFindings: 2, MatchedFindings: 2, ExpectedFindings: 2,
		SevereExpected: 1, SevereDetected: 1, SeverityMismatches: 0,
		BlockingFindings: 1, FalseBlocks: 0, DuplicateComments: 1, RawComments: 3,
		CitationChecked: 2, CitationAccurate: 2,
		Precision:        Proportion{Successes: 2, Total: 2},
		Detection:        Proportion{Successes: 2, Total: 2},
		SevereDetection:  Proportion{Successes: 1, Total: 1},
		CitationAccuracy: Proportion{Successes: 2, Total: 2},
		FalseBlockRate:   Proportion{Successes: 0, Total: 1},
		DuplicateRate:    Proportion{Successes: 1, Total: 3},
	})

	goExternal := find(external, "language", "go")
	wantProportion(t, "external go coverage", goExternal.Coverage, 4, 4)
	checkMetricSet(t, "external go", goExternal.Metrics, Metrics{
		UniqueFindings: 3, MatchedFindings: 2, ExpectedFindings: 4,
		SevereExpected: 3, SevereDetected: 2, SeverityMismatches: 0,
		BlockingFindings: 1, FalseBlocks: 0, DuplicateComments: 0, RawComments: 3,
		CitationChecked: 3, CitationAccurate: 3,
		Precision:        Proportion{Successes: 2, Total: 3},
		Detection:        Proportion{Successes: 2, Total: 4},
		SevereDetection:  Proportion{Successes: 2, Total: 3},
		CitationAccuracy: Proportion{Successes: 3, Total: 3},
		FalseBlockRate:   Proportion{Successes: 0, Total: 1},
		DuplicateRate:    Proportion{Successes: 0, Total: 3},
	})

	codeOwn := find(own, "change_type", "code")
	wantProportion(t, "own code coverage", codeOwn.Coverage, 3, 4)
	checkMetricSet(t, "own code", codeOwn.Metrics, Metrics{
		UniqueFindings: 3, MatchedFindings: 3, ExpectedFindings: 3,
		SevereExpected: 2, SevereDetected: 2, SeverityMismatches: 1,
		BlockingFindings: 1, FalseBlocks: 0, DuplicateComments: 1, RawComments: 4,
		CitationChecked: 3, CitationAccurate: 2,
		Precision:        Proportion{Successes: 3, Total: 3},
		Detection:        Proportion{Successes: 3, Total: 3},
		SevereDetection:  Proportion{Successes: 2, Total: 2},
		CitationAccuracy: Proportion{Successes: 2, Total: 3},
		FalseBlockRate:   Proportion{Successes: 0, Total: 1},
		DuplicateRate:    Proportion{Successes: 1, Total: 4},
	})
}

func TestFixturePairedComparison(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	own := labelFor(t, mapping, "own")
	external := labelFor(t, mapping, "coderabbit")
	var allFP, allSevere, goFP *PairedReport
	for i := range report.Paired {
		entry := &report.Paired[i]
		if entry.ReviewerA != own || entry.ReviewerB != external {
			continue
		}
		switch {
		case entry.Outcome == "false_positive_case" && entry.Dimension == "all":
			allFP = entry
		case entry.Outcome == "severe_case_detection" && entry.Dimension == "all":
			allSevere = entry
		case entry.Outcome == "false_positive_case" && entry.Dimension == "language" && entry.Class == "go":
			goFP = entry
		}
	}
	if allFP == nil || allSevere == nil || goFP == nil {
		t.Fatalf("missing paired rows: %+v", report.Paired)
	}
	if allFP.Cases != 4 || allFP.Both != 0 || allFP.Neither != 2 || allFP.AOnly != 1 || allFP.BOnly != 1 {
		t.Errorf("overall false-positive pairing = %+v", allFP)
	}
	if allFP.Difference != 0 || allFP.McNemarP != 1 || allFP.DifferenceCI95 == nil {
		t.Errorf("overall false-positive test = %+v", allFP)
	}
	if allSevere.Cases != 2 || allSevere.Both != 2 || allSevere.AOnly != 0 || allSevere.BOnly != 0 ||
		allSevere.Difference != 0 || allSevere.McNemarP != 1 {
		t.Errorf("overall severe pairing = %+v", allSevere)
	}
	if goFP.Cases != 2 || goFP.AOnly+goFP.BOnly != 1 {
		t.Errorf("go false-positive pairing = %+v", goFP)
	}
}

func TestFixtureThresholds(t *testing.T) {
	_, _, sealed, _ := loadFixture(t)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	thresholds := report.Thresholds
	if !thresholds.Evaluated || thresholds.Met || thresholds.Scope != "blind" {
		t.Fatalf("thresholds = %+v", thresholds)
	}
	if thresholds.Reviewer != sealed.TargetReviewer {
		t.Errorf("threshold reviewer = %q, want %q", thresholds.Reviewer, sealed.TargetReviewer)
	}
	checks := make(map[string]ThresholdCheck)
	for _, check := range thresholds.Checks {
		checks[check.Name] = check
	}
	if len(checks) != 7 {
		t.Fatalf("checks = %d, want 7", len(checks))
	}
	if check := checks["evaluated_cases"]; !check.Met || *check.Actual != 3 {
		t.Errorf("evaluated_cases = %+v", check)
	}
	if check := checks["citation_accuracy"]; check.Met || check.Actual == nil || !almostEqual(*check.Actual, 2.0/3, 1e-12) {
		t.Errorf("citation_accuracy = %+v", check)
	}
	if check := checks["duplicate_rate"]; check.Met || check.Actual == nil || !almostEqual(*check.Actual, 0.25, 1e-12) {
		t.Errorf("duplicate_rate = %+v", check)
	}
	if check := checks["false_block_rate"]; !check.Met || *check.Actual != 0 {
		t.Errorf("false_block_rate = %+v", check)
	}
}

func TestFixtureAdjudicationAndReveal(t *testing.T) {
	_, _, sealed, mapping := loadFixture(t)
	novel := NovelFindings(sealed)
	if len(novel) != 2 {
		t.Fatalf("novel findings = %d, want 2", len(novel))
	}
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Adjudication.Candidates != 2 || len(report.Adjudication.Pending) != 2 {
		t.Errorf("pending adjudication = %+v", report.Adjudication)
	}
	encoded, _ := json.Marshal(report)
	for _, name := range []string{"Own reviewer", "CodeRabbit", "QA Manager Reviewer"} {
		if strings.Contains(string(encoded), name) {
			t.Errorf("report leaks %q", name)
		}
	}
	if _, err := Reveal(report, mapping); err == nil {
		t.Fatal("Reveal must refuse while adjudication is pending")
	}

	decisions := decisionsFor(novel, AdjudicationRejected)
	report, err = BuildReport(sealed, decisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Adjudication.Pending) != 0 || report.Adjudication.Rejected != 2 {
		t.Errorf("adjudication = %+v", report.Adjudication)
	}
	revealed, err := Reveal(report, mapping)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	names := map[string]bool{}
	for _, reviewer := range revealed.Reviewers {
		names[reviewer.Name] = true
	}
	if !names["QA Manager Reviewer"] || !names["CodeRabbit"] {
		t.Errorf("revealed reviewers = %+v", revealed.Reviewers)
	}
	if len(revealed.Findings) != 2 {
		t.Errorf("revealed findings = %d, want 2", len(revealed.Findings))
	}
}

func TestFixtureReportIsDeterministic(t *testing.T) {
	_, _, sealed, _ := loadFixture(t)
	first, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("two reports from the same dataset differ")
	}
	if first.Reviewers[0].Metrics.Precision.Estimate == nil {
		t.Fatal("precision must not be undefined for a non-empty reviewer")
	}
}
