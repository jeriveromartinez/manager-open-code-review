package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeOCRBin(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs(filepath.Join("testdata", "fakeocr.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestParsePreviewFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/ocr-preview.json")
	if err != nil {
		t.Fatal(err)
	}

	got, err := parsePreview(b)
	if err != nil {
		t.Fatalf("parsePreview: %v", err)
	}
	if got.MergeBase == "" {
		t.Error("merge_base no mapeado")
	}
	if got.Mode == "" {
		t.Error("mode no mapeado")
	}
	if len(got.ReviewableFiles) == 0 {
		t.Fatal("reviewable_files vacío")
	}
	if len(got.ExcludedFiles) == 0 {
		t.Fatal("excluded_files vacío")
	}
	if got.ReviewableFiles[0].Path != "auth.go" || got.ReviewableFiles[0].Insertions != 7 {
		t.Errorf("reviewable mal mapeado: %+v", got.ReviewableFiles[0])
	}
	if got.ExcludedFiles[0].Reason != "unsupported_ext" {
		t.Errorf("exclude_reason no mapeado: %+v", got.ExcludedFiles[0])
	}
}

func TestParsePreviewRejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"merge_base":       `{"mode":"range","reviewable_files":[]}`,
		"reviewable_files": `{"mode":"range","merge_base":"abc"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePreview([]byte(raw)); err == nil {
				t.Fatal("esperaba error por campo obligatorio ausente")
			}
		})
	}
}

func TestParseRuleFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/ocr-rule.json")
	if err != nil {
		t.Fatal(err)
	}

	got, err := parseRules(b)
	if err != nil {
		t.Fatalf("parseRules: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("groups vacío")
	}
	withFiles := 0
	for _, g := range got {
		if len(g.Files) > 0 {
			withFiles++
		}
	}
	if withFiles == 0 {
		t.Fatal("ningún grupo tiene archivos")
	}
	if !strings.Contains(got[0].Rules, "Go Review Principles") {
		t.Errorf("campo `rule` no mapeado a Rules: %q", got[0].Rules[:min(80, len(got[0].Rules))])
	}
}

func TestParseRulesRejectsMissingGroups(t *testing.T) {
	if _, err := parseRules([]byte(`{"schema_version":"1"}`)); err == nil {
		t.Fatal("esperaba error por groups ausente")
	}
}

func TestExecOCRPassesRepoAndExcludeFlags(t *testing.T) {
	ctx := context.Background()
	o := &OCR{Bin: fakeOCRBin(t), RepoDir: "/repo/de/prueba", Excludes: "**/*.pb.go,vendor/**"}

	argvPreview := filepath.Join(t.TempDir(), "preview.argv")
	t.Setenv("FAKE_OCR_ARGV_FILE", argvPreview)
	if _, err := o.Preview(ctx, "BASE123", "HEAD456"); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	assertArgv(t, argvPreview, []string{
		"delegate", "preview", "--format", "json",
		"--repo", "/repo/de/prueba", "--from", "BASE123", "--to", "HEAD456",
		"--exclude", "**/*.pb.go,vendor/**",
	})

	// The rules command must see the same exclusions as the preview, so the
	// pass plan and the resolved rules describe the same file set.
	argvRule := filepath.Join(t.TempDir(), "rule.argv")
	t.Setenv("FAKE_OCR_ARGV_FILE", argvRule)
	if _, err := o.Rules(ctx, "BASE123", "HEAD456", []string{"main.go", "auth.go"}); err != nil {
		t.Fatalf("Rules: %v", err)
	}
	assertArgv(t, argvRule, []string{
		"delegate", "rule", "--format", "json",
		"--repo", "/repo/de/prueba", "--from", "BASE123", "--to", "HEAD456",
		"--exclude", "**/*.pb.go,vendor/**",
		"main.go", "auth.go",
	})
}

func TestExecOCROmitsEmptyExcludeFlag(t *testing.T) {
	o := &OCR{Bin: fakeOCRBin(t), RepoDir: "/repo"}

	argv := filepath.Join(t.TempDir(), "preview.argv")
	t.Setenv("FAKE_OCR_ARGV_FILE", argv)
	if _, err := o.Preview(context.Background(), "BASE", "HEAD"); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	assertArgv(t, argv, []string{
		"delegate", "preview", "--format", "json",
		"--repo", "/repo", "--from", "BASE", "--to", "HEAD",
	})
}

func TestOCRIncludesStderrInError(t *testing.T) {
	t.Setenv("FAKE_OCR_FAIL", "boom")
	o := &OCR{Bin: fakeOCRBin(t), RepoDir: "/repo"}

	_, err := o.Preview(context.Background(), "BASE", "HEAD")
	if err == nil {
		t.Fatal("esperaba error del binario")
	}
	if !strings.Contains(err.Error(), "fakeocr falló: boom") {
		t.Fatalf("stderr ausente en el error: %v", err)
	}
}

func assertArgv(t *testing.T, path string, want []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n%q\nquiere\n%q", got, want)
	}
}
