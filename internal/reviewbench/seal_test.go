package reviewbench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sealTestDataset(t *testing.T) (Manifest, Dataset, SealedDataset, SealedMapping) {
	t.Helper()
	manifest := validTestManifest()
	manifest.Cases[0].Expected = []ExpectedFinding{{
		Path: "main.go", Category: "correctness", Severity: "minor", StartLine: 3, EndLine: 3, CitedCode: "func main() {}",
	}}
	mustFinalize(t, &manifest)
	c := manifest.Cases[0]
	novelA := ObservedFinding{
		Path: "main.go", StartLine: 1, EndLine: 1, Severity: "minor", Category: "design",
		Title: "Package name too generic", ExistingCode: "package main",
	}
	novelB := ObservedFinding{
		Path: "main.go", StartLine: 2, EndLine: 2, Severity: "nit", Category: "style",
		Title: "Blank line after package", Body: "Remove the blank line.",
	}
	own := validTestObservation(manifest, c)
	own.Findings = []ObservedFinding{novelA, novelB}
	external := validTestObservation(manifest, c)
	external.Reviewer = Reviewer{ID: "external", Name: "External Reviewer", Kind: KindExternal, Version: "9.1", Config: map[string]string{"mode": "chill"}}
	external.Findings = []ObservedFinding{novelA}
	dataset, err := BuildDataset(manifest, []Observation{own, external})
	if err != nil {
		t.Fatal(err)
	}
	sealed, mapping := Seal(dataset)
	return manifest, dataset, sealed, mapping
}

func TestSealHidesNamesAndRevealsThroughMapping(t *testing.T) {
	_, _, sealed, mapping := sealTestDataset(t)

	encoded, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Own reviewer", "External Reviewer", "external"} {
		if strings.Contains(string(encoded), name) {
			t.Errorf("sealed dataset leaks %q", name)
		}
	}
	for _, result := range sealed.Results {
		if !strings.HasPrefix(result.Reviewer, "reviewer-") {
			t.Errorf("reviewer label = %q, want reviewer-NN", result.Reviewer)
		}
	}
	if sealed.TargetReviewer != "" {
		t.Errorf("target reviewer = %q, want empty when the manifest does not configure one", sealed.TargetReviewer)
	}

	ownLabel := labelFor(t, mapping, "own")
	reviewer, ok := mapping.Reviewer(ownLabel)
	if !ok || reviewer.Name != "Own reviewer" || reviewer.Config["model"] != "test" {
		t.Fatalf("mapping reviewer = %+v, ok=%v", reviewer, ok)
	}

	mappingJSON, err := json.Marshal(mapping)
	if err != nil {
		t.Fatal(err)
	}
	if string(mappingJSON) != "{}" {
		t.Errorf("sealed mapping JSON = %s, want {}", mappingJSON)
	}
}

func TestNovelFindingsGroupAcrossReviewersDeterministically(t *testing.T) {
	_, dataset, sealed, mapping := sealTestDataset(t)

	findings := NovelFindings(sealed)
	if len(findings) != 2 {
		t.Fatalf("novel findings = %d, want 2", len(findings))
	}
	byTitle := make(map[string]BlindFinding)
	for _, finding := range findings {
		byTitle[finding.Title] = finding
	}
	shared := byTitle["Package name too generic"]
	if len(shared.Reviewers) != 2 {
		t.Errorf("shared finding reviewers = %v, want both blind labels", shared.Reviewers)
	}
	for _, label := range shared.Reviewers {
		if _, ok := mapping.ReviewerID(label); !ok {
			t.Errorf("label %q is not in the sealed mapping", label)
		}
	}
	if len(byTitle["Blank line after package"].Reviewers) != 1 {
		t.Errorf("single-reviewer finding grouped incorrectly: %+v", byTitle["Blank line after package"])
	}
	if shared.Token == "" || strings.Contains(shared.Token, "own") || strings.Contains(shared.Token, "external") {
		t.Errorf("token %q leaks identity", shared.Token)
	}

	again, _ := Seal(dataset)
	repeated := NovelFindings(again)
	for i := range findings {
		if findings[i].Token != repeated[i].Token {
			t.Errorf("token %d changed between seals: %q vs %q", i, findings[i].Token, repeated[i].Token)
		}
	}
}

func TestSealTargetReviewerBecomesLabel(t *testing.T) {
	manifest := validTestManifest()
	manifest.TargetReviewer = "own"
	manifest.Thresholds = Thresholds{Configured: true, MinEvaluatedCases: 1, MinCoverage: 0.5}
	mustFinalize(t, &manifest)
	c := manifest.Cases[0]
	observation := validTestObservation(manifest, c)
	dataset, err := BuildDataset(manifest, []Observation{observation})
	if err != nil {
		t.Fatal(err)
	}
	sealed, mapping := Seal(dataset)
	if sealed.TargetReviewer == "" || sealed.TargetReviewer == "own" {
		t.Fatalf("target reviewer = %q, want a blind label", sealed.TargetReviewer)
	}
	if id, _ := mapping.ReviewerID(sealed.TargetReviewer); id != "own" {
		t.Errorf("target label resolves to %q, want own", id)
	}
}

func TestAdjudicateRequiresEvidenceAndKnownTokens(t *testing.T) {
	_, _, sealed, mapping := sealTestDataset(t)
	findings := NovelFindings(sealed)
	token := findings[0].Token

	tests := []struct {
		name      string
		decisions []AdjudicationDecision
		want      error
	}{
		{"unknown token", []AdjudicationDecision{{Token: "missing", Verdict: AdjudicationConfirmed, Evidence: "x"}}, ErrAdjudication},
		{"unknown verdict", []AdjudicationDecision{{Token: token, Verdict: "maybe", Evidence: "x"}}, ErrAdjudication},
		{"missing evidence", []AdjudicationDecision{{Token: token, Verdict: AdjudicationRejected}}, ErrAdjudication},
		{"duplicate decision", []AdjudicationDecision{
			{Token: token, Verdict: AdjudicationRejected, Evidence: "x"},
			{Token: token, Verdict: AdjudicationConfirmed, Evidence: "y"},
		}, ErrAdjudication},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Adjudicate(findings, test.decisions); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	decisions := decisionsFor(findings, AdjudicationRejected)
	report, err := Adjudicate(findings, decisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Pending) != 0 || report.Rejected != len(findings) {
		t.Fatalf("report = %+v", report)
	}
	if len(report.ByReviewer) == 0 {
		t.Fatal("expected per-reviewer adjudication counts")
	}
	if len(report.Decisions) != len(findings) {
		t.Fatalf("decisions = %d, want %d", len(report.Decisions), len(findings))
	}
	_ = mapping
}

func TestRevealRefusesWhilePendingAndRevealsNamesAfter(t *testing.T) {
	_, _, sealed, mapping := sealTestDataset(t)
	findings := NovelFindings(sealed)
	report, err := BuildReport(sealed, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Own reviewer", "External Reviewer"} {
		if strings.Contains(string(encoded), name) {
			t.Errorf("report leaks %q before adjudication", name)
		}
	}
	if _, err := Reveal(report, mapping); !errors.Is(err, ErrAdjudicationPending) {
		t.Fatalf("Reveal error = %v, want ErrAdjudicationPending", err)
	}

	decisions := decisionsFor(findings, AdjudicationRejected)
	report, err = BuildReport(sealed, decisions)
	if err != nil {
		t.Fatal(err)
	}
	revealed, err := Reveal(report, mapping)
	if err != nil {
		t.Fatalf("Reveal after adjudication: %v", err)
	}
	names := make(map[string]bool)
	for _, reviewer := range revealed.Reviewers {
		names[reviewer.Name] = true
	}
	if !names["Own reviewer"] || !names["External Reviewer"] {
		t.Fatalf("revealed reviewers = %+v", revealed.Reviewers)
	}
	if len(revealed.Findings) != len(findings) {
		t.Fatalf("revealed findings = %d, want %d", len(revealed.Findings), len(findings))
	}
	for _, reviewers := range revealed.Findings {
		if len(reviewers) == 0 {
			t.Error("a revealed finding has no reviewers")
		}
	}
}

func labelFor(t *testing.T, mapping SealedMapping, id string) string {
	t.Helper()
	for label := range mapping.labels {
		if resolved, _ := mapping.ReviewerID(label); resolved == id {
			return label
		}
	}
	t.Fatalf("no label for reviewer %q", id)
	return ""
}

func decisionsFor(findings []BlindFinding, verdict string) []AdjudicationDecision {
	decisions := make([]AdjudicationDecision, 0, len(findings))
	for _, finding := range findings {
		decisions = append(decisions, AdjudicationDecision{
			Token: finding.Token, Verdict: verdict,
			Evidence: "simulated adjudication evidence",
		})
	}
	return decisions
}
