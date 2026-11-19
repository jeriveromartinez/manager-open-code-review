package review

import (
	"fmt"
	"testing"
)

func numberedFiles(n int) []PreviewFile {
	files := make([]PreviewFile, n)
	for i := range files {
		files[i] = PreviewFile{
			Path:       fmt.Sprintf("file%02d.go", i),
			Status:     "modified",
			Insertions: 1,
			Deletions:  1,
		}
	}
	return files
}

func pathsOf(files []PreviewFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func allPaths(files []PreviewFile) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func TestPlanRespectsBudget(t *testing.T) {
	files := numberedFiles(10)
	paths := pathsOf(files)
	passes, uncovered, err := PlanPasses(PlanInput{
		Files:        files,
		Groups:       []RuleGroup{{Rules: "R1", Files: paths}},
		MaxFiles:     4,
		MaxDiffLines: 1000,
		MaxPasses:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 3 {
		t.Fatalf("pasadas = %d, quiere 3: %+v", len(passes), passes)
	}
	wantSizes := []int{4, 4, 2}
	seen := map[string]bool{}
	next := 0
	for i, pass := range passes {
		if len(pass.Files) != wantSizes[i] {
			t.Errorf("pasada %d tiene %d archivos, quiere %d", i, len(pass.Files), wantSizes[i])
		}
		if pass.DiffLines != 2*len(pass.Files) {
			t.Errorf("pasada %d DiffLines = %d, quiere %d", i, pass.DiffLines, 2*len(pass.Files))
		}
		if len(pass.Rules) != 1 || pass.Rules[0].Rules != "R1" {
			t.Errorf("pasada %d reglas = %+v", i, pass.Rules)
		}
		for _, p := range pass.Files {
			if seen[p] {
				t.Errorf("archivo repetido: %s", p)
			}
			seen[p] = true
			if p != paths[next] {
				t.Errorf("pasada %d: archivo %s, quiere %s", i, p, paths[next])
			}
			next++
		}
	}
	if next != len(files) {
		t.Errorf("archivos cubiertos = %d, quiere %d", next, len(files))
	}
	if len(uncovered) != 0 {
		t.Errorf("sin cubrir = %v, quiere vacío", pathsOf(uncovered))
	}
}

func TestPlanReportsUncovered(t *testing.T) {
	files := numberedFiles(10)
	passes, uncovered, err := PlanPasses(PlanInput{
		Files:        files,
		Groups:       []RuleGroup{{Rules: "R1", Files: pathsOf(files)}},
		MaxFiles:     4,
		MaxDiffLines: 1000,
		MaxPasses:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 2 {
		t.Fatalf("pasadas = %d, quiere 2", len(passes))
	}
	if got, want := pathsOf(uncovered), []string{"file08.go", "file09.go"}; !equalStrings(got, want) {
		t.Fatalf("sin cubrir = %v, quiere %v", got, want)
	}
}

func TestPlanRespectsDiffLineBudget(t *testing.T) {
	files := []PreviewFile{
		{Path: "a.go", Insertions: 5},
		{Path: "b.go", Insertions: 3},
		{Path: "c.go", Insertions: 3},
		{Path: "d.go", Insertions: 1},
		{Path: "huge.go", Insertions: 100},
	}
	passes, uncovered, err := PlanPasses(PlanInput{
		Files:        files,
		Groups:       []RuleGroup{{Rules: "R1", Files: allPaths(files)}},
		MaxFiles:     10,
		MaxDiffLines: 6,
		MaxPasses:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"a.go"}, {"b.go", "c.go"}, {"d.go"}}
	if len(passes) != len(want) {
		t.Fatalf("pasadas = %d, quiere %d: %+v", len(passes), len(want), passes)
	}
	for i, pass := range passes {
		if !equalStrings(pass.Files, want[i]) {
			t.Errorf("pasada %d = %v, quiere %v", i, pass.Files, want[i])
		}
		if pass.DiffLines > 6 {
			t.Errorf("pasada %d excede MaxDiffLines: %d", i, pass.DiffLines)
		}
	}
	if got := pathsOf(uncovered); !equalStrings(got, []string{"huge.go"}) {
		t.Errorf("sin cubrir = %v, quiere [huge.go]", got)
	}
}

func TestPlanNoDuplicateFilesAcrossGroups(t *testing.T) {
	files := []PreviewFile{
		{Path: "a.go", Insertions: 1},
		{Path: "b.go", Insertions: 1},
		{Path: "c.go", Insertions: 1},
	}
	passes, uncovered, err := PlanPasses(PlanInput{
		Files: files,
		Groups: []RuleGroup{
			{Rules: "R1", Files: []string{"a.go", "b.go"}},
			{Rules: "R2", Files: []string{"b.go", "c.go"}},
		},
		MaxFiles:     10,
		MaxDiffLines: 100,
		MaxPasses:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	covered := 0
	for _, pass := range passes {
		for _, p := range pass.Files {
			if seen[p] {
				t.Errorf("archivo repetido: %s", p)
			}
			seen[p] = true
			covered++
		}
	}
	if covered != len(files) {
		t.Errorf("cubiertos = %d, quiere %d (sin cubrir %v)", covered, len(files), pathsOf(uncovered))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
