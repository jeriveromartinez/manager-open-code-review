package review

import (
	"reflect"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func architectureCitationVerdict(paths ...string) store.ArchitectureVerdict {
	included := make([]store.ArchitectureScopeFile, 0, len(paths))
	for _, path := range paths {
		included = append(included, store.ArchitectureScopeFile{Path: path})
	}
	return store.ArchitectureVerdict{
		Requested: true,
		Status:    store.ArchitectureComplete,
		Mode:      store.ArchitectureAnalysisObservation,
		Scope:     store.ArchitectureScope{Included: included},
	}
}

func TestArchitectureEvidenceCitationsStructuredFormats(t *testing.T) {
	paths := []string{
		"internal/review/run.go",
		"internal/review/a.go",
		"internal/review/b.go",
		"internal/review/c.go",
		"new-client.ts",
	}
	cases := []struct {
		name  string
		risks []string
		want  []store.FindingCitation
	}{
		{
			name:  "single file with range",
			risks: []string{"Cambia `internal/review/run.go:10-12`."},
			want: []store.FindingCitation{
				{Path: "internal/review/run.go", StartLine: 10, EndLine: 12, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "multiple paths in one finding",
			risks: []string{"Cambia `internal/review/a.go:3` y `internal/review/b.go:7-9`."},
			want: []store.FindingCitation{
				{Path: "internal/review/a.go", StartLine: 3, EndLine: 3, Side: store.FindingCitationHead},
				{Path: "internal/review/b.go", StartLine: 7, EndLine: 9, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "one location shared by several files",
			risks: []string{"El límite entre `internal/review/a.go` y `internal/review/b.go` (líneas 10-12) se rompe."},
			want: []store.FindingCitation{
				{Path: "internal/review/a.go", StartLine: 10, EndLine: 12, Side: store.FindingCitationHead},
				{Path: "internal/review/b.go", StartLine: 10, EndLine: 12, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "shared colon location for a path list",
			risks: []string{"Afecta a `internal/review/a.go`, `internal/review/b.go`:20."},
			want: []store.FindingCitation{
				{Path: "internal/review/a.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
				{Path: "internal/review/b.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
			},
		},
		{
			name: "previous side applies to the whole clause",
			risks: []string{
				"Antes `internal/review/run.go:5` y `internal/review/run.go:7`; " +
					"ahora `internal/review/run.go:20`.",
			},
			want: []store.FindingCitation{
				{Path: "internal/review/run.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationBase},
				{Path: "internal/review/run.go", StartLine: 7, EndLine: 7, Side: store.FindingCitationBase},
				{Path: "internal/review/run.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "previous side suffix",
			risks: []string{"`internal/review/run.go:5` (previous) frente a `internal/review/run.go:20` (current)."},
			want: []store.FindingCitation{
				{Path: "internal/review/run.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationBase},
				{Path: "internal/review/run.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "github line anchor",
			risks: []string{"Cambia `internal/review/run.go#L10-L12`."},
			want: []store.FindingCitation{
				{Path: "internal/review/run.go", StartLine: 10, EndLine: 12, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "hyphenated path is not swallowed by a side word",
			risks: []string{"Cambia `new-client.ts:3`."},
			want: []store.FindingCitation{
				{Path: "new-client.ts", StartLine: 3, EndLine: 3, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "semicolon does not join a path list",
			risks: []string{"`internal/review/a.go`, `internal/review/b.go`; `internal/review/c.go:5`."},
			want: []store.FindingCitation{
				{Path: "internal/review/c.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationHead},
			},
		},
		{
			name:  "semicolon without spaces resets the side state",
			risks: []string{"Antes `internal/review/run.go:5`;`internal/review/run.go:7`."},
			want: []store.FindingCitation{
				{Path: "internal/review/run.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationBase},
				{Path: "internal/review/run.go", StartLine: 7, EndLine: 7, Side: store.FindingCitationHead},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := architectureCitationVerdict(paths...)
			verdict.Risks = tc.risks
			got := architectureEvidenceCitations(verdict)
			want := store.NormalizeFindingCitations(tc.want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("citations = %+v, want %+v", got, want)
			}
		})
	}
}

func TestArchitectureEvidenceCitationsDedupeStructuredCitations(t *testing.T) {
	verdict := architectureCitationVerdict("internal/review/a.go", "internal/review/b.go")
	verdict.Risks = []string{
		"`internal/review/a.go:10`, `internal/review/a.go:10-10` y `internal/review/a.go`, `internal/review/b.go`:10.",
	}
	got := architectureEvidenceCitations(verdict)
	want := store.NormalizeFindingCitations([]store.FindingCitation{
		{Path: "internal/review/a.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationHead},
		{Path: "internal/review/b.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationHead},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("citations = %+v, want %+v", got, want)
	}
}

func TestArchitectureEvidenceCitationsDropMalformed(t *testing.T) {
	verdict := architectureCitationVerdict(
		"internal/review/good.go",
		"internal/review/a.go",
		"internal/review/b.go",
		"internal/review/c.go",
		"internal/review/d.go",
		"internal/review/e.go",
	)
	verdict.Risks = []string{
		"Válido `internal/review/good.go:4`; malformed `internal/review/a.go:abc`, " +
			"`internal/review/b.go:0`, `internal/review/c.go:20-10`, `not-a-path:12`, " +
			"`../internal/review/d.go:3`, `internal/review/x/../d.go:3` y " +
			"`https://example.com/internal/review/e.go:9`.",
	}
	got := architectureEvidenceCitations(verdict)
	want := []store.FindingCitation{
		{Path: "internal/review/good.go", StartLine: 4, EndLine: 4, Side: store.FindingCitationHead},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("citations = %+v, want only the valid citation", got)
	}
}

func TestArchitectureEvidenceCitationsCarryStandaloneSideAcrossLines(t *testing.T) {
	verdict := architectureCitationVerdict("internal/review/run.go")
	verdict.Dimensions = []store.ArchitectureDimension{{
		Name: "Riesgos",
		Content: "Antes:\n- `internal/review/run.go:5`\n" +
			"ahora:\n- `internal/review/run.go:20`",
	}}
	got := architectureEvidenceCitations(verdict)
	want := store.NormalizeFindingCitations([]store.FindingCitation{
		{Path: "internal/review/run.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationBase},
		{Path: "internal/review/run.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationHead},
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("citations = %+v, want %+v", got, want)
	}
}

func TestArchitectureEvidenceCitationsDoNotRederiveQualifiedItems(t *testing.T) {
	verdict := architectureCitationVerdict("internal/review/run.go")
	verdict.Dimensions = []store.ArchitectureDimension{{
		Name:    "Riesgos",
		Content: "Antes:\n- `internal/review/run.go:5`",
	}}
	// The extracted items lose the "Antes:" context; parsing them as well would
	// publish the same line as head side too.
	verdict.Risks = []string{"Antes:", "`internal/review/run.go:5`"}

	got := architectureEvidenceCitations(verdict)
	want := []store.FindingCitation{
		{Path: "internal/review/run.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationBase},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("citations = %+v, want only the previous-side citation", got)
	}
}

func TestArchitectureEvidenceCitationsRejectPreviousSideOnAddedFile(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true,
		Status:    store.ArchitectureComplete,
		Mode:      store.ArchitectureAnalysisObservation,
		Scope: store.ArchitectureScope{Included: []store.ArchitectureScopeFile{
			{Path: "internal/review/new.go", Kind: "added"},
		}},
		Risks: []string{"Antes `internal/review/new.go:5`."},
	}
	got := architectureEvidenceCitations(verdict)
	want := []store.FindingCitation{
		{Path: "internal/review/new.go", StartLine: 5, EndLine: 5, Side: store.FindingCitationHead},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("citations = %+v, want no base citation for an added file", got)
	}
}

func TestArchitectureFindingsWholePRWithoutPath(t *testing.T) {
	verdict := architectureCitationVerdict()
	verdict.Risks = []string{"El cambio afecta al contrato público de todo el PR, líneas 10-12 del diseño."}
	findings := ArchitectureFindings(verdict)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want one design finding", len(findings))
	}
	if len(findings[0].Citations) != 0 || findings[0].Scope != store.FindingScopePR {
		t.Errorf("finding = %+v, want a PR-scope finding without citations", findings[0])
	}
}

func TestArchitectureFindingIdentityIncludesStructuredCitations(t *testing.T) {
	base := AgentFinding{Title: "Arquitectura: límite nuevo", Category: "design", Scope: store.FindingScopeFiles}
	first := base
	first.Citations = []store.FindingCitation{
		{Path: "internal/review/a.go", StartLine: 1, EndLine: 1, Side: store.FindingCitationHead},
		{Path: "internal/review/b.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationHead},
	}
	reordered := base
	reordered.Citations = []store.FindingCitation{first.Citations[1], first.Citations[0]}
	other := base
	other.Citations = []store.FindingCitation{
		first.Citations[0],
		{Path: "internal/review/c.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationHead},
	}

	if Fingerprint(1, first, nil, 0) != Fingerprint(1, reordered, nil, 0) {
		t.Error("the same citation set in another order changed the finding identity")
	}
	if Fingerprint(1, first, nil, 0) == Fingerprint(1, other, nil, 0) {
		t.Error("a different citation set kept the finding identity")
	}
}
