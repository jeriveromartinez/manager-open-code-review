package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// fakeBaselineTree is an in-memory repository: ref -> path -> content. Blob
// SHAs are derived from content so a changed or missing head file is visible
// without invoking git.
type fakeBaselineTree struct {
	files map[string]map[string][]byte
}

func newFakeBaselineTree() fakeBaselineTree {
	return fakeBaselineTree{files: map[string]map[string][]byte{
		baselineRefBase: {},
		baselineRefHead: {},
	}}
}

func (f fakeBaselineTree) put(ref, path, content string) {
	f.files[ref][path] = []byte(content)
}

func (f fakeBaselineTree) Read(ref, path string) ([]byte, bool, error) {
	content, ok := f.files[ref][path]
	return content, ok, nil
}

func (f fakeBaselineTree) Blob(ref, path string) (string, bool, error) {
	content, ok := f.files[ref][path]
	if !ok {
		return "", false, nil
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8]), true, nil
}

const baselineTestModule = "module example.com/app\n\ngo 1.22\n"

func baselineTestPolicy() store.QAPolicy {
	return store.QAPolicy{
		ArchitectureAnalysisMode:    store.ArchitectureAnalysisObservation,
		ArchitectureBaselinePath:    store.DefaultArchitectureBaselinePath,
		ArchitectureExceptionPolicy: store.ArchitectureExceptionsDeny,
	}
}

func marshalBaselineFile(t *testing.T, version string, decisions, rules, exceptions []map[string]any) string {
	t.Helper()
	payload := map[string]any{"version": version}
	if decisions != nil {
		payload["decisions"] = decisions
	}
	if rules != nil {
		payload["rules"] = rules
	}
	if exceptions != nil {
		payload["exceptions"] = exceptions
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func baselineRule() map[string]any {
	return map[string]any{
		"id": "ARCH-001", "version": "1",
		"from": "internal/store", "to": "internal/review",
		"reason": "store must not depend on review",
	}
}

func TestArchitectureBaselineLayerCrossingViolation(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})

	if baseline.Status != store.ArchitectureBaselineViolation {
		t.Fatalf("status = %q, want violation (errors: %v)", baseline.Status, baseline.Errors)
	}
	if len(baseline.Checks) != 1 {
		t.Fatalf("checks = %+v, want one", baseline.Checks)
	}
	check := baseline.Checks[0]
	if check.RuleID != "ARCH-001" || check.FromModule != "internal/store" ||
		check.ToModule != "internal/review" || check.Import != "example.com/app/internal/review" {
		t.Errorf("check = %+v, want rule, modules and concrete import", check)
	}
	if check.File != "internal/store/evil.go" || check.Status != store.ArchitectureDependencyViolation {
		t.Errorf("check = %+v", check)
	}
}

func TestArchitectureBaselineAuthorizedException(t *testing.T) {
	tree := newFakeBaselineTree()
	exception := map[string]any{
		"id": "EX-001", "rule": "ARCH-001",
		"from": "internal/store", "to": "internal/review",
		"reason": "legacy bridge", "approved_by": "architecture-board",
	}
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, []map[string]any{exception}))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, []map[string]any{exception}))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")

	policy := baselineTestPolicy()
	policy.ArchitectureExceptionPolicy = store.ArchitectureExceptionsBaseline
	baseline := checkArchitectureBaseline(tree, policy,
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})

	if baseline.Status != store.ArchitectureBaselinePass {
		t.Fatalf("status = %q, want pass (errors: %v)", baseline.Status, baseline.Errors)
	}
	if len(baseline.Checks) != 1 || baseline.Checks[0].Status != store.ArchitectureDependencyException ||
		baseline.Checks[0].ExceptionID != "EX-001" {
		t.Fatalf("checks = %+v, want one authorized exception", baseline.Checks)
	}
	if baseline.Controls.ExceptionsApplied != 1 {
		t.Errorf("applied exceptions = %d, want 1", baseline.Controls.ExceptionsApplied)
	}
}

func TestArchitectureBaselineExceptionRequiresPolicyAuthorization(t *testing.T) {
	tree := newFakeBaselineTree()
	exception := map[string]any{
		"id": "EX-001", "rule": "ARCH-001",
		"reason": "legacy bridge", "approved_by": "architecture-board",
	}
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, []map[string]any{exception}))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, []map[string]any{exception}))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")

	// The default policy denies exceptions, so the declared exception does not
	// authorize the crossing.
	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})
	if baseline.Status != store.ArchitectureBaselineViolation {
		t.Fatalf("status = %q, want violation", baseline.Status)
	}
	if baseline.Checks[0].ExceptionID != "" {
		t.Errorf("exception applied without policy authorization: %+v", baseline.Checks[0])
	}
}

func TestArchitectureBaselineReplacedADRIsUnknown(t *testing.T) {
	decision := map[string]any{"id": "ADR-001", "path": "docs/adr/0001-layers.md", "version": "1"}
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", []map[string]any{decision}, nil, nil))
	tree.put(baselineRefBase, "docs/adr/0001-layers.md", "store must not depend on review\n")
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", []map[string]any{decision}, nil, nil))
	tree.put(baselineRefHead, "docs/adr/0001-layers.md", "review must not depend on store\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), nil)
	if baseline.Status != store.ArchitectureBaselineUnknown {
		t.Fatalf("status = %q, want unknown, not pass", baseline.Status)
	}
	if len(baseline.Decisions) != 1 || baseline.Decisions[0].Status != store.ArchitectureDecisionReplaced {
		t.Fatalf("decisions = %+v, want replaced", baseline.Decisions)
	}
	if baseline.Decisions[0].ID != "ADR-001" || baseline.Decisions[0].Path == "" ||
		baseline.Decisions[0].Version != "1" || baseline.Decisions[0].SHA == "" {
		t.Errorf("decision reference = %+v, want id, path, version and SHA", baseline.Decisions[0])
	}
}

func TestArchitectureBaselineRuleChangeCannotApprove(t *testing.T) {
	relaxed := map[string]any{
		"id": "ARCH-001", "version": "2",
		"from": "internal/other", "to": "internal/review",
		"reason": "scoped down in this PR",
	}
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "2", nil, []map[string]any{relaxed}, nil))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})

	// The rules changed in the PR, but the base rule still catches the crossing.
	if baseline.Status != store.ArchitectureBaselineViolation {
		t.Fatalf("status = %q, want violation under the base rule", baseline.Status)
	}
	if !baseline.ChangedInPR || !baseline.Controls.RuleFileChanged {
		t.Errorf("controls = %+v, want the PR rule change recorded", baseline.Controls)
	}
	if baseline.RulesVersion != "1" {
		t.Errorf("rules version = %q, want the trusted base version", baseline.RulesVersion)
	}
	if baseline.RulesHeadSHA == "" || baseline.RulesHeadSHA == baseline.RulesSHA {
		t.Errorf("head rule SHA = %q, want the proposed rule recorded separately", baseline.RulesHeadSHA)
	}
}

func TestArchitectureBaselineRuleChangeWithoutViolationIsUnknown(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "2", nil, []map[string]any{baselineRule()}, nil))

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), nil)
	if baseline.Status != store.ArchitectureBaselineUnknown {
		t.Fatalf("status = %q, want unknown when the rule changed in the PR", baseline.Status)
	}
	if !baseline.ChangedInPR {
		t.Error("rule change in the PR was not recorded")
	}
}

func TestArchitectureBaselineMissingIsUnknownAndRecorded(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefHead, "go.mod", baselineTestModule)

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), nil)
	if baseline.Status != store.ArchitectureBaselineUnknown {
		t.Fatalf("status = %q, want unknown when the baseline is missing", baseline.Status)
	}
	if !baseline.Controls.BaselineMissing || !baseline.Controls.BaseOnly {
		t.Errorf("controls = %+v, want missing baseline and base-only", baseline.Controls)
	}
	if len(baseline.Errors) == 0 {
		t.Error("missing baseline produced no traceable error")
	}
}

func TestArchitectureBaselineRecordsAppliedControls(t *testing.T) {
	decision := map[string]any{"id": "ADR-001", "path": "docs/adr/0001.md", "version": "3"}
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, "docs/adr/0001.md", "decision\n")
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "7", []map[string]any{decision}, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, "docs/adr/0001.md", "decision\n")
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "7", []map[string]any{decision}, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "internal/store/new.go", "package store\n\nimport _ \"example.com/app/internal/api\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/new.go", Kind: changedAdded}})

	controls := baseline.Controls
	if !controls.BaseOnly || controls.RuleFileChanged || controls.BaselineMissing {
		t.Errorf("controls = %+v, want base-only without changes or missing baseline", controls)
	}
	if controls.DecisionsChecked != 1 || controls.RulesChecked != 1 || controls.FilesChecked != 1 {
		t.Errorf("controls = %+v, want one decision, rule and file checked", controls)
	}
	if controls.ExceptionsPolicy != store.ArchitectureExceptionsDeny {
		t.Errorf("exceptions policy = %q", controls.ExceptionsPolicy)
	}
	if baseline.Status != store.ArchitectureBaselinePass {
		t.Fatalf("status = %q, want pass", baseline.Status)
	}
}

func TestArchitectureBaselineOnlyChecksAddedImports(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefBase, "internal/store/file.go",
		"package store\n\nimport _ \"example.com/app/internal/review\"\n")
	tree.put(baselineRefHead, "internal/store/file.go",
		"package store\n\nimport (\n\t_ \"example.com/app/internal/review\"\n\t\"fmt\"\n)\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/file.go", Kind: changedModified}})
	if len(baseline.Checks) != 0 {
		t.Fatalf("checks = %+v, want none for a pre-existing import", baseline.Checks)
	}
}

func TestArchitectureBaselineAcceptsQualifiedRulePaths(t *testing.T) {
	rule := map[string]any{
		"id": "ARCH-002", "version": "1",
		"from": "example.com/app/internal/store", "to": "example.com/app/internal/review",
	}
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{rule}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{rule}, nil))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})
	if baseline.Status != store.ArchitectureBaselineViolation {
		t.Fatalf("status = %q, want violation for a qualified rule", baseline.Status)
	}
}

func TestArchitectureBaselineIsReproducible(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "internal/store/evil.go", "package store\n\nimport _ \"example.com/app/internal/review\"\n")
	changes := []changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}}

	first, err := json.Marshal(checkArchitectureBaseline(tree, baselineTestPolicy(), changes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(checkArchitectureBaseline(tree, baselineTestPolicy(), changes))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("same inputs produced different baselines:\n%s\n%s", first, second)
	}
}

type unreadableBaselineTree struct {
	fakeBaselineTree
}

func (t unreadableBaselineTree) Read(ref, file string) ([]byte, bool, error) {
	if file == store.DefaultArchitectureBaselinePath {
		return nil, true, errors.New("disk error")
	}
	return t.fakeBaselineTree.Read(ref, file)
}

type blobErrorBaselineTree struct {
	fakeBaselineTree
}

func (t blobErrorBaselineTree) Blob(ref, file string) (string, bool, error) {
	if ref == baselineRefBase && file == store.DefaultArchitectureBaselinePath {
		return "", false, errors.New("git object store failed")
	}
	return t.fakeBaselineTree.Blob(ref, file)
}

func TestArchitectureBaselineUnreadableIsUnknown(t *testing.T) {
	base := newFakeBaselineTree()
	base.put(baselineRefBase, "go.mod", baselineTestModule)
	base.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))

	cases := []struct {
		name string
		tree baselineTree
		want string
	}{
		{"read failure", unreadableBaselineTree{base}, "no se pudo leer"},
		{"blob failure", blobErrorBaselineTree{base}, "no se pudo comprobar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := checkArchitectureBaseline(tc.tree, baselineTestPolicy(), nil)
			if baseline.Status != store.ArchitectureBaselineUnknown {
				t.Fatalf("status = %q, want unknown", baseline.Status)
			}
			if !strings.Contains(strings.Join(baseline.Errors, "; "), tc.want) {
				t.Errorf("errors = %v, want %q", baseline.Errors, tc.want)
			}
		})
	}
}

func TestArchitectureBaselineEmptyDocumentIsUnknown(t *testing.T) {
	cases := map[string]string{
		"null document":   "null",
		"empty object":    "{}",
		"missing version": `{"rules":[{"id":"ARCH-001","from":"a","to":"b"}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			tree := newFakeBaselineTree()
			tree.put(baselineRefBase, "go.mod", baselineTestModule)
			tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath, content)
			tree.put(baselineRefHead, "go.mod", baselineTestModule)
			tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath, content)

			baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), nil)
			if baseline.Status != store.ArchitectureBaselineUnknown {
				t.Fatalf("status = %q, want unknown for %s", baseline.Status, content)
			}
		})
	}
}

func TestArchitectureBaselineModuleChangeIsUnknown(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", "module example.com/renamed\n\ngo 1.22\n")
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "internal/store/evil.go",
		"package store\n\nimport _ \"example.com/renamed/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(),
		[]changedFile{{Path: "internal/store/evil.go", Kind: changedAdded}})
	if baseline.Status != store.ArchitectureBaselineUnknown {
		t.Fatalf("status = %q, want unknown when the module renamed", baseline.Status)
	}
	if !strings.Contains(strings.Join(baseline.Errors, "; "), "módulo Go cambió") {
		t.Errorf("errors = %v, want module change", baseline.Errors)
	}
}

func TestArchitectureBaselineRenameUsesOldPath(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	// The forbidden edge already existed at the base path: renaming the file
	// inside the same module does not attribute it to this change.
	tree.put(baselineRefBase, "internal/store/legacy.go",
		"package store\n\nimport _ \"example.com/app/internal/review\"\n")
	tree.put(baselineRefHead, "internal/store/renamed.go",
		"package store\n\nimport _ \"example.com/app/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), []changedFile{{
		Path: "internal/store/renamed.go", OldPath: "internal/store/legacy.go",
		Kind: changedModified, Renamed: true,
	}})
	if baseline.Status != store.ArchitectureBaselinePass || len(baseline.Checks) != 0 {
		t.Fatalf("status = %q checks = %+v, want no new edge", baseline.Status, baseline.Checks)
	}
}

func TestArchitectureBaselineRenameIntoForbiddenModuleIsViolation(t *testing.T) {
	tree := newFakeBaselineTree()
	tree.put(baselineRefBase, "go.mod", baselineTestModule)
	tree.put(baselineRefBase, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefHead, "go.mod", baselineTestModule)
	tree.put(baselineRefHead, store.DefaultArchitectureBaselinePath,
		marshalBaselineFile(t, "1", nil, []map[string]any{baselineRule()}, nil))
	tree.put(baselineRefBase, "internal/legacy/file.go",
		"package legacy\n\nimport _ \"example.com/app/internal/review\"\n")
	tree.put(baselineRefHead, "internal/store/file.go",
		"package store\n\nimport _ \"example.com/app/internal/review\"\n")

	baseline := checkArchitectureBaseline(tree, baselineTestPolicy(), []changedFile{{
		Path: "internal/store/file.go", OldPath: "internal/legacy/file.go",
		Kind: changedModified, Renamed: true,
	}})
	if baseline.Status != store.ArchitectureBaselineViolation {
		t.Fatalf("status = %q, want violation for a rename into the forbidden module", baseline.Status)
	}
}

func TestGitBaselineTreeResolvesBaseAndHead(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	path := store.DefaultArchitectureBaselinePath
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAndCommit := func(content, message string) string {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-qm", message)
		return runGit(t, dir, "rev-parse", "HEAD")
	}
	base := writeAndCommit(`{"version":"1"}`, "base")
	head := writeAndCommit(`{"version":"2"}`, "head")

	tree := gitBaselineTree{ctx: context.Background(), dir: dir, base: base, head: head}
	baseContent, ok, err := tree.Read(baselineRefBase, path)
	if err != nil || !ok || !strings.Contains(string(baseContent), `"1"`) {
		t.Fatalf("base read = %q ok=%v err=%v", baseContent, ok, err)
	}
	headContent, ok, err := tree.Read(baselineRefHead, path)
	if err != nil || !ok || !strings.Contains(string(headContent), `"2"`) {
		t.Fatalf("head read = %q ok=%v err=%v", headContent, ok, err)
	}
	baseBlob, _, err := tree.Blob(baselineRefBase, path)
	if err != nil {
		t.Fatal(err)
	}
	headBlob, _, err := tree.Blob(baselineRefHead, path)
	if err != nil {
		t.Fatal(err)
	}
	if baseBlob == headBlob {
		t.Errorf("blobs = %q, want different base and head", baseBlob)
	}
	if _, exists, err := tree.Read(baselineRefBase, "missing.json"); err != nil || exists {
		t.Errorf("missing path: exists=%v err=%v, want missing without error", exists, err)
	}
}

func TestApplyArchitectureBaselineIntegratesDimensionAndStatus(t *testing.T) {
	baseline := store.ArchitectureBaseline{Status: store.ArchitectureBaselineViolation}
	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true, []string{"persistence"},
		store.ArchitectureScope{}, store.ArchitectureComparison{}, store.ArchitectureBudget{})
	verdict.Dimensions = []store.ArchitectureDimension{{Name: "Problem", Content: "text"}}

	got := applyArchitectureBaseline(verdict, baseline)
	if got.Status != store.ArchitectureViolation {
		t.Errorf("status = %q, want violation", got.Status)
	}
	if got.Baseline == nil || got.Baseline.Status != store.ArchitectureBaselineViolation {
		t.Errorf("baseline = %+v", got.Baseline)
	}
	if len(got.Dimensions) != 2 || got.Dimensions[0].Name != architectureBaselineDimensionName {
		t.Errorf("dimensions = %+v, want the baseline dimension first", got.Dimensions)
	}

	unknown := applyArchitectureBaseline(verdict, store.ArchitectureBaseline{Status: store.ArchitectureBaselineUnknown})
	if unknown.Status != store.ArchitectureInconclusive {
		t.Errorf("status = %q, want inconclusive for an unknown baseline", unknown.Status)
	}
}

func TestMarkArchitectureInconclusiveKeepsTrustedViolation(t *testing.T) {
	violation := store.ArchitectureVerdict{Status: store.ArchitectureViolation}
	var out runOutcome
	markArchitectureInconclusive(&out, &violation, errors.New("OCR no disponible"))
	if violation.Status != store.ArchitectureViolation {
		t.Fatalf("status = %q, want the trusted violation preserved", violation.Status)
	}
	if len(violation.Errors) != 1 || !strings.Contains(violation.Errors[0], "OCR") {
		t.Errorf("errors = %v, want the failure recorded", violation.Errors)
	}
	if saved := store.DecodeArchitectureVerdict(out.architecture); saved.Status != store.ArchitectureViolation {
		t.Errorf("stored status = %q, want violation", saved.Status)
	}

	unfinished := store.ArchitectureVerdict{Status: store.ArchitectureComplete}
	markArchitectureInconclusive(&out, &unfinished, nil)
	if unfinished.Status != store.ArchitectureInconclusive {
		t.Errorf("status = %q, want inconclusive when there is no trusted violation", unfinished.Status)
	}
}
