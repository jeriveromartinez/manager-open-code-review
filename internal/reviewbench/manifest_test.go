package reviewbench

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testSHA(char string) string {
	return strings.Repeat(char, 40)
}

func validTestManifest() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		Version:       "test-1",
		ContextRadius: 3,
		Cases: []Case{{
			ID:         "case-clean",
			Language:   "go",
			ChangeType: "code",
			Comparison: Comparison{Base: testSHA("a"), Head: testSHA("b")},
			AllowedContext: []string{
				"main.go",
			},
			HeadFiles: map[string][]string{
				"main.go": {"package main", "", "func main() {}"},
			},
			Expected: []ExpectedFinding{},
			Blind:    true,
		}},
	}
}

func mustFinalize(t *testing.T, manifest *Manifest) {
	t.Helper()
	if err := manifest.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
}

func TestManifestLockRoundTrip(t *testing.T) {
	manifest := validTestManifest()
	mustFinalize(t, &manifest)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if parsed.Lock != manifest.Lock {
		t.Errorf("lock = %q, want %q", parsed.Lock, manifest.Lock)
	}
}

func TestManifestLockInvalidatesChangedGroundTruth(t *testing.T) {
	manifest := validTestManifest()
	manifest.Cases[0].Expected = []ExpectedFinding{{
		Path: "main.go", Category: "correctness", Severity: "minor",
		StartLine: 3, EndLine: 3, CitedCode: "func main() {}",
	}}
	mustFinalize(t, &manifest)
	original := manifest.Lock

	changed := manifest
	changed.Cases = append([]Case(nil), manifest.Cases...)
	changed.Cases[0].Expected = append([]ExpectedFinding(nil), manifest.Cases[0].Expected...)
	changed.Cases[0].Expected[0].Severity = "major"
	changedLock, err := changed.ComputeLock()
	if err != nil {
		t.Fatal(err)
	}
	if changedLock == original {
		t.Fatal("changing ground truth did not change the lock")
	}

	// The stored lock still describes the old ground truth, so parsing the
	// changed document must fail even though the JSON is otherwise valid.
	data, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(data); !errors.Is(err, ErrLockMismatch) {
		t.Fatalf("ParseManifest error = %v, want ErrLockMismatch", err)
	}
}

func TestManifestRejectsInvalidGroundTruth(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"unknown language", func(m *Manifest) { m.Cases[0].Language = "cobol" }},
		{"unknown change type", func(m *Manifest) { m.Cases[0].ChangeType = "docs" }},
		{"invalid base sha", func(m *Manifest) { m.Cases[0].Comparison.Base = "not-a-sha" }},
		{"no blind case", func(m *Manifest) { m.Cases[0].Blind = false }},
		{"context without head content", func(m *Manifest) { m.Cases[0].AllowedContext = []string{"missing.go"} }},
		{"expected outside context", func(m *Manifest) {
			m.Cases[0].Expected = []ExpectedFinding{{Path: "other.go", Category: "bug", Severity: "minor", StartLine: 1, EndLine: 1}}
		}},
		{"expected unknown severity", func(m *Manifest) {
			m.Cases[0].Expected = []ExpectedFinding{{Path: "main.go", Category: "bug", Severity: "urgent", StartLine: 3, EndLine: 3}}
		}},
		{"expected invalid citation", func(m *Manifest) {
			m.Cases[0].Expected = []ExpectedFinding{{Path: "main.go", Category: "bug", Severity: "minor", StartLine: 3, EndLine: 3, CitedCode: "absent code"}}
		}},
		{"expected range out of bounds", func(m *Manifest) {
			m.Cases[0].Expected = []ExpectedFinding{{Path: "main.go", Category: "bug", Severity: "minor", StartLine: 9, EndLine: 9}}
		}},
		{"push sequence does not end at head", func(m *Manifest) {
			m.Cases[0].Pushes = []string{testSHA("c"), testSHA("d")}
		}},
		{"thresholds without target", func(m *Manifest) {
			m.Thresholds = Thresholds{Configured: true}
		}},
		{"threshold out of range", func(m *Manifest) {
			m.TargetReviewer = "own"
			m.Thresholds = Thresholds{Configured: true, MinCoverage: 1.5}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validTestManifest()
			test.mutate(&manifest)
			if err := manifest.Validate(); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Validate error = %v, want ErrInvalidManifest", err)
			}
			if err := manifest.Finalize(); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Finalize error = %v, want ErrInvalidManifest", err)
			}
		})
	}
}

func TestManifestRejectsUnknownFields(t *testing.T) {
	data := []byte(`{"schema_version":"reviewbench/v1","version":"x","unknown":true}`)
	if _, err := ParseManifest(data); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ParseManifest error = %v, want ErrInvalidManifest", err)
	}
}

func TestManifestMultiPushAcceptsDeclaredHead(t *testing.T) {
	manifest := validTestManifest()
	manifest.Cases[0].Pushes = []string{testSHA("c"), testSHA("b")}
	mustFinalize(t, &manifest)
	observation := validTestObservation(manifest, manifest.Cases[0])
	observation.Comparison.Head = testSHA("c")
	observation.ReviewedHead = testSHA("c")
	dataset, err := BuildDataset(manifest, []Observation{observation})
	if err != nil {
		t.Fatalf("BuildDataset: %v", err)
	}
	if !dataset.Results[0].Superseded {
		t.Fatal("an earlier push must be marked superseded")
	}
	if dataset.Results[0].Observation.Result != ResultIncompleteCoverage {
		t.Errorf("result = %q, want incomplete_coverage", dataset.Results[0].Observation.Result)
	}
}
