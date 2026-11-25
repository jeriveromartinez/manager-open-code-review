package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestGitChangedFilesInventoriesTheDiff(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeRunTestFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	writeRunTestFile(t, filepath.Join(dir, "modified.txt"), "one\n")
	writeRunTestFile(t, filepath.Join(dir, "deleted.txt"), "gone\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	base := runGit(t, dir, "rev-parse", "HEAD")

	writeRunTestFile(t, filepath.Join(dir, "modified.txt"), "two\n")
	writeRunTestFile(t, filepath.Join(dir, "added.txt"), "new\n")
	runGit(t, dir, "mv", "keep.txt", "renamed.txt")
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("PNG\x00binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+base+",vendor/lib")
	runGit(t, dir, "commit", "-qm", "head")
	head := runGit(t, dir, "rev-parse", "HEAD")

	files, err := gitChangedFiles(context.Background(), dir, base, head)
	if err != nil {
		t.Fatalf("gitChangedFiles: %v", err)
	}
	got := make(map[string]string, len(files))
	for _, file := range files {
		got[file.Path] = file.Kind
	}
	want := map[string]string{
		"added.txt":    changedAdded,
		"modified.txt": changedModified,
		"deleted.txt":  changedDeleted,
		"renamed.txt":  changedModified,
		"logo.png":     changedBinary,
		"vendor/lib":   changedSubmodule,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("inventory = %v, want %v", got, want)
	}
	if _, ok := got["keep.txt"]; ok {
		t.Error("the rename source must not be reported as a changed path")
	}
}

func TestParseRawDiffHandlesRenamesAndCopies(t *testing.T) {
	raw := ":100644 100644 aaa bbb R100\x00old.go\x00new.go\x00" +
		":100644 100644 aaa bbb C075\x00source.go\x00copy.go\x00"
	files := parseRawDiff(raw)
	want := []changedFile{
		{Path: "new.go", Kind: changedModified, Renamed: true},
		{Path: "copy.go", Kind: changedModified},
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("parseRawDiff = %+v, want %+v", files, want)
	}
}

func TestParseNumstatHandlesBinaryAndRenames(t *testing.T) {
	raw := "1\t0\ta.go\x00" +
		"-\t-\tlogo.png\x00" +
		"-\t-\t\x00old.bin\x00new.bin\x00"
	binaries := parseNumstat(raw)
	if !binaries["logo.png"] {
		t.Errorf("binaries = %v, want logo.png", binaries)
	}
	if !binaries["new.bin"] {
		t.Errorf("binaries = %v, want the rename target new.bin", binaries)
	}
	if binaries["a.go"] || binaries["old.bin"] {
		t.Errorf("binaries = %v, only the new binary paths are expected", binaries)
	}
}

func TestPolicyExcludesPathMatchesTheTeamGlobs(t *testing.T) {
	cases := []struct {
		pattern string
		file    string
		want    bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/notes.md", true},
		{"auth.go", "auth.go", true},
		{"generated/**", "generated/api.go", true},
		{"generated/**", "generated", false},
		{"generated/**", "src/generated/api.go", false},
		{"**/*_test.go", "pkg/auth_test.go", true},
		{"**/*_test.go", "auth_test.go", true},
		{"vendor/*", "vendor/lib/code.go", false},
		{"src/*.go", "src/a.go", true},
	}
	for _, tc := range cases {
		if got := policyExcludesPath([]string{tc.pattern}, tc.file); got != tc.want {
			t.Errorf("policyExcludesPath(%q, %q) = %v, want %v", tc.pattern, tc.file, got, tc.want)
		}
	}
}

func TestInventoryCoverageClassifiesChangedFiles(t *testing.T) {
	changed := []changedFile{
		{Path: "auth.go", Kind: changedModified},
		{Path: "notes.md", Kind: changedModified},
		{Path: "generated/api.go", Kind: changedAdded},
		{Path: "deleted.go", Kind: changedDeleted},
		{Path: "logo.png", Kind: changedBinary},
		{Path: "vendor/lib", Kind: changedSubmodule},
	}
	files := []coverageFile{
		{Path: "auth.go", Status: coverageReviewed},
		{Path: "notes.md", Status: coverageExcluded, Reason: "unsupported_ext"},
		{Path: "generated/api.go", Status: coverageExcluded, Reason: "user_exclude"},
	}

	scoped, pending := inventoryCoverage(files, changed, []string{"generated/**"}, coverageSignal{present: true})
	if len(pending) != 1 || pending[0].Path != "notes.md" || pending[0].Reason != "unsupported_ext" {
		t.Fatalf("pending = %+v, want notes.md/unsupported_ext", pending)
	}
	byPath := make(map[string]coverageFile, len(scoped))
	for _, file := range scoped {
		byPath[file.Path] = file
	}
	for path, status := range map[string]string{
		"auth.go":          coverageReviewed,
		"notes.md":         coveragePending,
		"generated/api.go": coverageExcluded,
		"deleted.go":       changedDeleted,
		"logo.png":         changedBinary,
		"vendor/lib":       changedSubmodule,
	} {
		if got := byPath[path].Status; got != status {
			t.Errorf("%s status = %q, want %q", path, got, status)
		}
	}
}

func TestInventoryCoverageManifestMissingMakesRequiredFilesPending(t *testing.T) {
	changed := []changedFile{{Path: "auth.go", Kind: changedAdded}}
	files, pending := inventoryCoverage(nil, changed, nil, coverageSignal{})
	if len(pending) != 1 || pending[0].Reason != "manifest_missing" {
		t.Fatalf("pending = %+v, want manifest_missing", pending)
	}
	if len(files) != 1 || files[0].Status != coveragePending {
		t.Errorf("files = %+v, want one pending entry", files)
	}
}

func TestInventoryCoverageDerivesReasonsFromTheManifestGroups(t *testing.T) {
	res := nativeReview{Manifest: nativeManifest{Coverage: nativeCoverage{
		Selected: []nativeCoverageItem{{Path: "failed.go"}, {Path: "selected.go"}},
		Failed:   []nativeCoverageItem{{Path: "failed.go"}},
	}}}
	files := nativeCoverageFiles(res)
	if len(files) != 1 || files[0].Reason != "failed" {
		t.Fatalf("files = %+v, want failed/failed from the empty failed item", files)
	}

	_, pending := inventoryCoverage(files, []changedFile{
		{Path: "failed.go", Kind: changedModified},
		{Path: "selected.go", Kind: changedModified},
	}, nil, coverageSignalFrom(res))
	reasons := make(map[string]string, len(pending))
	for _, file := range pending {
		reasons[file.Path] = file.Reason
	}
	if reasons["failed.go"] != "failed" {
		t.Errorf("failed.go reason = %q, want failed", reasons["failed.go"])
	}
	// Selected is manifest presence, so a dispatched file without a terminal
	// group is not reported as a missing manifest.
	if reasons["selected.go"] != "not_completed" {
		t.Errorf("selected.go reason = %q, want not_completed", reasons["selected.go"])
	}
}

// TestCoverageRegressionMixedPR pins #135: OCR returning complete while a
// policy-required changed file was excluded internally is not full coverage.
// The run must become inconclusive, the pending file must reach the summary and
// a previous finding in that file must stay open.
func TestCoverageNotRequiredCountsNonReviewableFiles(t *testing.T) {
	files := []coverageFile{
		{Path: "deleted.go", Status: changedDeleted},
		{Path: "logo.png", Status: changedBinary},
		{Path: "vendor/lib", Status: changedSubmodule},
		{Path: "auth.go", Status: coverageReviewed},
	}
	if got := coverageNotRequiredFromFiles(files); got != 3 {
		t.Errorf("not required = %d, want 3", got)
	}
	if reviewed, skipped, excluded := coverageCountsFromFiles(files); reviewed != 1 || skipped != 0 || excluded != 0 {
		t.Errorf("coverage counts = %d/%d/%d, want 1/0/0", reviewed, skipped, excluded)
	}
}

func TestCoverageRegressionMixedPR(t *testing.T) {
	res := nativeReview{
		Status:  "complete",
		Summary: nativeSummary{FilesReviewed: 1},
		Manifest: nativeManifest{
			TerminalState: "complete",
			Coverage: nativeCoverage{
				Selected:  []nativeCoverageItem{{Path: "auth.go"}, {Path: "notes.md"}},
				Completed: []nativeCoverageItem{{Path: "auth.go"}},
				Excluded:  []nativeCoverageItem{{Path: "notes.md", ExcludeReason: "unsupported_ext"}},
			},
		},
	}
	changed := []changedFile{
		{Path: "auth.go", Kind: changedAdded},
		{Path: "notes.md", Kind: changedModified},
	}

	files := nativeCoverageFiles(res)
	files, pending := inventoryCoverage(files, changed, nil, coverageSignalFrom(res))
	baseStatus, baseCode := nativeOutcome(res)
	status, code := pendingRunStatus(baseStatus, baseCode, pending)
	if status != store.RunInconclusive || code != "coverage_incomplete" {
		t.Fatalf("outcome = %q/%q, want inconclusive/coverage_incomplete", status, code)
	}
	if len(pending) != 1 || pending[0].Path != "notes.md" || pending[0].Reason != "unsupported_ext" {
		t.Fatalf("pending = %+v, want notes.md/unsupported_ext", pending)
	}

	af := AgentFinding{
		Path: "notes.md", Category: "bug", Title: "Sigue roto",
		StartLine: 3, EndLine: 3, Severity: "major",
	}
	open := []store.Finding{{
		ID: "previous", PullRequestID: 1, Fingerprint: Fingerprint(1, af, nil, 0),
		Path: "notes.md", Category: "bug", Title: "Sigue roto",
		StartLine: 3, EndLine: 3, IdentityVersion: store.IdentityVersionCurrent,
		Status: store.FindingOpen, TriageVerdict: store.VerdictConfirmed,
	}}
	// The full known list still matches the re-reported occurrence, so its ID
	// and triage survive even though the path was not covered.
	upserts, resolved := ReconcileScoped(open, AgentOutput{Findings: []AgentFinding{af}}, 2, true, 1, nil, reviewedPaths(res.Manifest.Coverage))
	if len(upserts) != 1 || upserts[0].ID != "previous" || upserts[0].TriageVerdict != store.VerdictConfirmed {
		t.Fatalf("upserts = %+v, want the previous ID and verdict", upserts)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved = %v, want the finding in the omitted file to stay open", resolved)
	}
}

func TestInventoryCoverageMarksBinariesAndSubmodulesForHumans(t *testing.T) {
	changed := []changedFile{
		{Path: "logo.png", Kind: changedBinary},
		{Path: "vendor/lib", Kind: changedSubmodule},
		{Path: "gone.md", Kind: changedDeleted},
	}
	files, pending := inventoryCoverage(nil, changed, nil, coverageSignal{present: true})
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want non-reviewable files out of the required set", pending)
	}
	byPath := make(map[string]coverageFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	for _, path := range []string{"logo.png", "vendor/lib"} {
		if !byPath[path].Human {
			t.Errorf("%s = %+v, want human review marked", path, byPath[path])
		}
		if byPath[path].Reason != byPath[path].Status {
			t.Errorf("%s reason = %q, want the kind %q", path, byPath[path].Reason, byPath[path].Status)
		}
	}
	if byPath["gone.md"].Human {
		t.Errorf("deleted = %+v, want a reviewable deletion, not a human-only file", byPath["gone.md"])
	}
}

// TestInventoryCoverageKeepsSecondRouteMethod pins the #147 rule: a file a
// second pass covered with a valid result is reviewed, with its provenance,
// and no longer forces an inconclusive run.
func TestInventoryCoverageKeepsSecondRouteMethod(t *testing.T) {
	changed := []changedFile{{Path: "notes.md", Kind: changedModified}}
	files := []coverageFile{{Path: "notes.md", Status: coverageReviewed, Method: coverageMethodText}}
	files, pending := inventoryCoverage(files, changed, nil, coverageSignal{present: true})
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the text route to count as coverage", pending)
	}
	if len(files) != 1 || files[0].Status != coverageReviewed || files[0].Method != coverageMethodText {
		t.Errorf("files = %+v, want reviewed/text preserved", files)
	}
	status, code := pendingRunStatus(store.RunSucceeded, "", pending)
	if status != store.RunSucceeded || code != "" {
		t.Errorf("outcome = %q/%q, want succeeded", status, code)
	}

	pendingInput := []coverageFile{{
		Path: "notes.md", Status: coveragePending, Method: coverageMethodText,
		Reason: "text_route_invalid", Human: true,
	}}
	files, pending = inventoryCoverage(pendingInput, changed, nil, coverageSignal{present: true})
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the invalid text route to stay pending", pending)
	}
	got := pending[0]
	if got.Reason != "text_route_invalid" || got.Method != coverageMethodText || !got.Human {
		t.Errorf("pending = %+v, want reason, method and human flag preserved", got)
	}
}

func TestCoverageRegressionPolicyExcludedFileStaysSucceeded(t *testing.T) {
	res := nativeReview{
		Status: "complete",
		Manifest: nativeManifest{
			TerminalState: "complete",
			Coverage: nativeCoverage{
				Completed: []nativeCoverageItem{{Path: "auth.go"}},
				Excluded:  []nativeCoverageItem{{Path: "docs/notes.md", Reason: "user_exclude"}},
			},
		},
	}
	changed := []changedFile{
		{Path: "auth.go", Kind: changedAdded},
		{Path: "docs/notes.md", Kind: changedAdded},
	}

	files := nativeCoverageFiles(res)
	files, pending := inventoryCoverage(files, changed, []string{"**/*.md"}, coverageSignalFrom(res))
	baseStatus, baseCode := nativeOutcome(res)
	status, code := pendingRunStatus(baseStatus, baseCode, pending)
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the policy exclusion out of scope", pending)
	}
	if status != store.RunSucceeded || code != "" {
		t.Errorf("outcome = %q/%q, want succeeded", status, code)
	}
	for _, file := range files {
		if file.Path == "docs/notes.md" && file.Status != coverageExcluded {
			t.Errorf("notes.md coverage = %+v, want excluded", file)
		}
	}
}

// TestCoverageIncrementalRange pins the incremental scope: only files in the
// incremental range are required, and an open finding outside it is not
// resolved even when the engine returns complete.
func TestCoverageIncrementalRange(t *testing.T) {
	res := nativeReview{
		Status: "complete",
		Manifest: nativeManifest{
			TerminalState: "complete",
			Coverage: nativeCoverage{
				Completed: []nativeCoverageItem{{Path: "second.go"}},
			},
		},
	}
	changed := []changedFile{{Path: "second.go", Kind: changedAdded}}
	files, pending := inventoryCoverage(nativeCoverageFiles(res), changed, nil, coverageSignalFrom(res))
	if len(pending) != 0 {
		t.Fatalf("pending = %+v, want the incremental file covered", pending)
	}
	status, code := nativeOutcome(res)
	status, code = pendingRunStatus(status, code, pending)
	if status != store.RunSucceeded || code != "" {
		t.Errorf("outcome = %q/%q, want succeeded", status, code)
	}

	open := []store.Finding{{
		ID: "outside", PullRequestID: 1, Fingerprint: "outside-fp",
		Path: "main.go", Status: store.FindingOpen,
	}}
	scoped := findingsInPaths(open, reviewedPaths(res.Manifest.Coverage))
	if len(scoped) != 0 {
		t.Errorf("scoped = %+v, want the finding outside the incremental range to stay open", scoped)
	}

	// Without coverage of the changed incremental file, the run owes it.
	files, pending = inventoryCoverage(nativeCoverageFiles(res), []changedFile{{Path: "third.go", Kind: changedAdded}}, nil, coverageSignalFrom(res))
	if len(pending) != 1 || pending[0].Path != "third.go" || pending[0].Reason != "not_reported" {
		t.Errorf("pending = %+v, want third.go/not_reported", pending)
	}
	if got := len(files); got < 2 {
		t.Errorf("files = %+v, want the reviewed and the pending entry", files)
	}
}
