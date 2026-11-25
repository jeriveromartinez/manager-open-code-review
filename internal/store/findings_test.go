package store_test

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestNormalizeFindingCitationsDropsInvalidAndOrders(t *testing.T) {
	citations := store.NormalizeFindingCitations([]store.FindingCitation{
		{Path: "./internal\\b.go", StartLine: 8, EndLine: 4},
		{Path: "", StartLine: 3},
		{Path: "internal/a.go", StartLine: 0},
		{Path: "internal/a.go", StartLine: 2, EndLine: 2, ExistingCode: "  guard()  "},
		{Path: "internal/a.go", StartLine: 2, EndLine: 2},
	})
	if len(citations) != 2 {
		t.Fatalf("citations = %+v, want two valid entries", citations)
	}
	if citations[0].Path != "internal/a.go" || citations[0].StartLine != 2 ||
		citations[0].EndLine != 2 || citations[0].Side != store.FindingCitationHead ||
		citations[0].ExistingCode != "guard()" {
		t.Errorf("first citation = %+v, want the normalized head citation", citations[0])
	}
	if citations[1].Path != "internal/b.go" || citations[1].StartLine != 8 ||
		citations[1].EndLine != 8 {
		t.Errorf("second citation = %+v, want the clamped b.go citation", citations[1])
	}
}

func TestNormalizeFindingScopeFollowsCitations(t *testing.T) {
	tests := []struct {
		name      string
		scope     string
		citations []store.FindingCitation
		want      string
	}{
		{"empty and no citations is a file", "", nil, store.FindingScopeFile},
		{"two paths are files", "", []store.FindingCitation{
			{Path: "a.go", StartLine: 1}, {Path: "b.go", StartLine: 1},
		}, store.FindingScopeFiles},
		{"one path stays a file", store.FindingScopeFiles, []store.FindingCitation{
			{Path: "a.go", StartLine: 1},
		}, store.FindingScopeFile},
		{"explicit pr wins", store.FindingScopePR, []store.FindingCitation{
			{Path: "a.go", StartLine: 1},
		}, store.FindingScopePR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := store.NormalizeFindingScope(tt.scope, tt.citations); got != tt.want {
				t.Errorf("scope = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindingCitationIdentityIgnoresLineNumbers(t *testing.T) {
	a := []store.FindingCitation{{Path: "internal/a.go", StartLine: 3, EndLine: 3}}
	b := []store.FindingCitation{{Path: "internal/a.go", StartLine: 30, EndLine: 30}}
	if store.FindingCitationIdentity(a) != store.FindingCitationIdentity(b) {
		t.Fatal("citation identity changed with the line number")
	}
	c := []store.FindingCitation{{Path: "internal/b.go", StartLine: 3, EndLine: 3}}
	if store.FindingCitationIdentity(a) == store.FindingCitationIdentity(c) {
		t.Fatal("citations in different files share an identity")
	}
}

func TestUpsertFindingPersistsScopeAndCitations(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	design := store.Finding{
		ID: "f_design", PullRequestID: pr.ID, Fingerprint: "fp-design",
		Path: "internal/a.go", StartLine: 10, EndLine: 10,
		Severity: "major", Category: "design", Title: "Divide el límite",
		Status: store.FindingOpen, Scope: store.FindingScopeFiles,
		Advisory: true,
		Citations: store.EncodeFindingCitations([]store.FindingCitation{
			{Path: "internal/a.go", StartLine: 10, EndLine: 10},
			{Path: "internal/b.go", StartLine: 20, EndLine: 20, Side: store.FindingCitationBase},
		}),
	}
	if _, err := f.s.UpsertFinding(f.ctx, design); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}

	saved, err := f.s.Finding(f.ctx, design.ID)
	if err != nil {
		t.Fatalf("Finding() error = %v", err)
	}
	if saved.Scope != store.FindingScopeFiles {
		t.Errorf("scope = %q, want files", saved.Scope)
	}
	if !saved.Advisory {
		t.Error("advisory flag was not persisted")
	}
	citations := store.DecodeFindingCitations(saved.Citations)
	if len(citations) != 2 || citations[1].Side != store.FindingCitationBase {
		t.Errorf("citations = %+v, want the base-side citation", citations)
	}

	// A later run moves the evidence; the row keeps its ID and refreshes the
	// citations instead of keeping stale lines.
	moved := design
	moved.StartLine, moved.EndLine = 14, 14
	moved.Citations = store.EncodeFindingCitations([]store.FindingCitation{
		{Path: "internal/a.go", StartLine: 14, EndLine: 14},
	})
	if _, err := f.s.UpsertFinding(f.ctx, moved); err != nil {
		t.Fatalf("second UpsertFinding() error = %v", err)
	}
	saved, err = f.s.Finding(f.ctx, design.ID)
	if err != nil {
		t.Fatalf("Finding() after update error = %v", err)
	}
	citations = store.DecodeFindingCitations(saved.Citations)
	if len(citations) != 1 || citations[0].StartLine != 14 {
		t.Errorf("citations = %+v, want the refreshed evidence", citations)
	}
	if saved.Scope != store.FindingScopeFile {
		t.Errorf("scope = %q, want file after one citation", saved.Scope)
	}
}
