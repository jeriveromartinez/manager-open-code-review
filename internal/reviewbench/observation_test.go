package reviewbench

import (
	"errors"
	"testing"
)

func validTestObservation(manifest Manifest, c Case) Observation {
	return Observation{
		SchemaVersion: ObservationSchemaVersion,
		CaseID:        c.ID,
		ManifestLock:  manifest.Lock,
		Reviewer: Reviewer{
			ID: "own", Name: "Own reviewer", Kind: KindOwn, Version: "v1",
			Config: map[string]string{"model": "test"},
		},
		Date:         "2026-11-26T10:00:00Z",
		Comparison:   c.Comparison,
		ReviewedHead: c.Comparison.Head,
		Result:       ResultCompleted,
		LatencyMS:    100,
		Coverage:     Coverage{Reviewed: append([]string(nil), c.AllowedContext...)},
		Findings:     []ObservedFinding{},
	}
}

func buildTestDataset(t *testing.T, observations []Observation) Dataset {
	t.Helper()
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	dataset, err := BuildDataset(manifest, observations)
	if err != nil {
		t.Fatalf("BuildDataset: %v", err)
	}
	return dataset
}

func TestBuildDatasetRejectsComparisonMismatch(t *testing.T) {
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	c := manifest.Cases[0]

	base := validTestObservation(manifest, c)
	base.Comparison.Base = testSHA("f")
	if _, err := BuildDataset(manifest, []Observation{base}); !errors.Is(err, ErrComparisonMismatch) {
		t.Fatalf("base mismatch error = %v, want ErrComparisonMismatch", err)
	}

	head := validTestObservation(manifest, c)
	head.ReviewedHead = testSHA("f")
	head.Comparison.Head = testSHA("f")
	if _, err := BuildDataset(manifest, []Observation{head}); !errors.Is(err, ErrComparisonMismatch) {
		t.Fatalf("undeclared head error = %v, want ErrComparisonMismatch", err)
	}

	inconsistent := validTestObservation(manifest, c)
	inconsistent.Comparison.Head = testSHA("f")
	if _, err := BuildDataset(manifest, []Observation{inconsistent}); !errors.Is(err, ErrComparisonMismatch) {
		t.Fatalf("inconsistent head error = %v, want ErrComparisonMismatch", err)
	}
}

func TestBuildDatasetRejectsStaleManifestLock(t *testing.T) {
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.ManifestLock = testSHA("0")
	if _, err := BuildDataset(manifest, []Observation{observation}); !errors.Is(err, ErrLockMismatch) {
		t.Fatalf("error = %v, want ErrLockMismatch", err)
	}
}

func TestBuildDatasetRejectsUnknownSeverity(t *testing.T) {
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.Findings = []ObservedFinding{{
		Path: "main.go", StartLine: 3, EndLine: 3, Severity: "urgent",
		Category: "bug", Title: "Something",
	}}
	if _, err := BuildDataset(manifest, []Observation{observation}); !errors.Is(err, ErrUnknownSeverity) {
		t.Fatalf("error = %v, want ErrUnknownSeverity", err)
	}
}

func TestBuildDatasetNormalizesSeverityAliases(t *testing.T) {
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.Findings = []ObservedFinding{{
		Path: "main.go", StartLine: 3, EndLine: 3, Severity: "CRITICAL",
		Category: "bug", Title: "Something",
	}}
	dataset, err := BuildDataset(manifest, []Observation{observation})
	if err != nil {
		t.Fatal(err)
	}
	if got := dataset.Results[0].Observation.Findings[0].Severity; got != "blocker" {
		t.Errorf("severity = %q, want blocker", got)
	}
}

func TestBuildDatasetRejectsInvalidObservations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(Manifest, *Observation)
	}{
		{"unknown result", func(_ Manifest, o *Observation) { o.Result = "finished" }},
		{"bad date", func(_ Manifest, o *Observation) { o.Date = "yesterday" }},
		{"negative latency", func(_ Manifest, o *Observation) { o.LatencyMS = -1 }},
		{"coverage outside context", func(_ Manifest, o *Observation) { o.Coverage.Reviewed = []string{"other.go"} }},
		{"empty reviewer", func(_ Manifest, o *Observation) { o.Reviewer.ID = "" }},
		{"unknown reviewer kind", func(_ Manifest, o *Observation) { o.Reviewer.Kind = "robot" }},
		{"invalid finding range", func(_ Manifest, o *Observation) {
			o.Findings = []ObservedFinding{{Path: "main.go", StartLine: 3, EndLine: 2, Severity: "minor", Category: "bug", Title: "x"}}
		}},
		{"missing title", func(_ Manifest, o *Observation) {
			o.Findings = []ObservedFinding{{Path: "main.go", StartLine: 3, EndLine: 3, Severity: "minor", Category: "bug"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validTestManifest()
			mustFinalize(t, &manifest)
			observation := validTestObservation(manifest, manifest.Cases[0])
			test.mutate(manifest, &observation)
			if _, err := BuildDataset(manifest, []Observation{observation}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestBuildDatasetDowngradesIncompleteCoverage(t *testing.T) {
	manifest := validTestManifest()
	manifest.Cases[0].AllowedContext = []string{"main.go", "extra.go"}
	manifest.Cases[0].HeadFiles["extra.go"] = []string{"package main"}
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.Coverage.Reviewed = []string{"main.go"}
	dataset, err := BuildDataset(manifest, []Observation{observation})
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Results[0].Observation.Result != ResultIncompleteCoverage {
		t.Errorf("result = %q, want incomplete_coverage", dataset.Results[0].Observation.Result)
	}
	if dataset.Results[0].Superseded {
		t.Error("a full-coverage-but-partial-file observation is not superseded")
	}
}

func TestBuildDatasetCountsDuplicates(t *testing.T) {
	manifest := validTestManifest()
	manifest.ContextRadius = 3
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.Findings = []ObservedFinding{
		{Path: "main.go", StartLine: 3, EndLine: 3, Severity: "minor", Category: "bug", Title: "Same issue", ExistingCode: "func main() {}"},
		{Path: "./main.go", StartLine: 4, EndLine: 4, Severity: "minor", Category: "bug", Title: "same   issue", ExistingCode: "func main() {}"},
		{Path: "main.go", StartLine: 3, EndLine: 3, Severity: "minor", Category: "style", Title: "Other issue"},
	}
	dataset, err := BuildDataset(manifest, []Observation{observation})
	if err != nil {
		t.Fatal(err)
	}
	result := dataset.Results[0]
	if len(result.Observation.Findings) != 2 {
		t.Errorf("unique findings = %d, want 2", len(result.Observation.Findings))
	}
	if result.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", result.Duplicates)
	}
}

func TestBuildDatasetRejectsDuplicateCaseReviewerAndMetadataChanges(t *testing.T) {
	manifest := validTestManifest()
	manifest.Cases = append(manifest.Cases, manifest.Cases[0])
	manifest.Cases[1].ID = "case-other"
	manifest.Cases[1].Comparison = Comparison{Base: testSHA("c"), Head: testSHA("d")}
	manifest.Cases[1].Pushes = nil
	mustFinalize(t, &manifest)
	first := validTestObservation(manifest, manifest.Cases[0])
	duplicate := first
	if _, err := BuildDataset(manifest, []Observation{first, duplicate}); err == nil {
		t.Fatal("expected duplicate reviewer/case rejection")
	}

	changed := validTestObservation(manifest, manifest.Cases[1])
	changed.Reviewer.Name = "Other name"
	if _, err := BuildDataset(manifest, []Observation{first, changed}); err == nil {
		t.Fatal("expected reviewer metadata change rejection")
	}
}

func TestBuildDatasetRequiresConfiguredTargetReviewer(t *testing.T) {
	manifest := validTestManifest()
	manifest.TargetReviewer = "missing"
	manifest.Thresholds = Thresholds{Configured: true}
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	if _, err := BuildDataset(manifest, []Observation{observation}); err == nil {
		t.Fatal("expected an error for a target reviewer with no observations")
	}
}

func TestParseObservationsRejectsUnknownFields(t *testing.T) {
	data := []byte(`[{"schema_version":"reviewbench.observation/v1","mystery":1}]`)
	if _, err := ParseObservations(data); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("error = %v, want ErrInvalidObservation", err)
	}
}
