package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// newChunkRepo inicializa un repo temporal con identidad fija y sin conversión
// de fin de línea para que el contenido de los blobs sea el que escribe el test.
func newChunkRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "core.autocrlf", "false")
	return dir
}

// commitChunkFiles escribe y confirma varios archivos en un solo commit.
func commitChunkFiles(t *testing.T, dir, message string, files map[string]string) string {
	t.Helper()

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "--", path)
	}
	runGit(t, dir, "commit", "-qm", message)
	return runGit(t, dir, "rev-parse", "HEAD")
}

func numberedLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%02d", i+1)
	}
	return lines
}

func textLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestParseFileDiff(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  []diffHunk
		renam bool
	}{
		{
			name: "archivo nuevo",
			raw: "diff --git a/new.txt b/new.txt\n" +
				"new file mode 100644\n" +
				"index 0000000..1111111\n" +
				"--- /dev/null\n" +
				"+++ b/new.txt\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+alpha\n" +
				"+beta\n",
			want: []diffHunk{{
				OldStart: 0, OldCount: 0, NewStart: 1, NewCount: 2,
				Lines: []string{"+alpha", "+beta"},
			}},
		},
		{
			name: "varios hunks",
			raw: "diff --git a/file.txt b/file.txt\n" +
				"--- a/file.txt\n" +
				"+++ b/file.txt\n" +
				"@@ -2 +2 @@\n" +
				"-two\n" +
				"+TWO\n" +
				"@@ -10,2 +10 @@\n" +
				"-ten\n" +
				"-eleven\n" +
				"+TEN\n",
			want: []diffHunk{
				{OldStart: 2, OldCount: 1, NewStart: 2, NewCount: 1, Lines: []string{"-two", "+TWO"}},
				{OldStart: 10, OldCount: 2, NewStart: 10, NewCount: 1, Lines: []string{"-ten", "-eleven", "+TEN"}},
			},
		},
		{
			name: "borrado puro",
			raw: "diff --git a/file.txt b/file.txt\n" +
				"--- a/file.txt\n" +
				"+++ b/file.txt\n" +
				"@@ -3,2 +2,0 @@\n" +
				"-three\n" +
				"-four\n",
			want: []diffHunk{{
				OldStart: 3, OldCount: 2, NewStart: 2, NewCount: 0,
				Lines: []string{"-three", "-four"},
			}},
		},
		{
			name: "sin salto final",
			raw: "diff --git a/file.txt b/file.txt\n" +
				"--- a/file.txt\n" +
				"+++ b/file.txt\n" +
				"@@ -1,2 +1,2 @@\n" +
				" one\n" +
				"-two\n" +
				"\\ No newline at end of file\n" +
				"+TWO\n" +
				"\\ No newline at end of file\n",
			want: []diffHunk{{
				OldStart: 1, OldCount: 2, NewStart: 1, NewCount: 2,
				Lines: []string{" one", "-two", "\\ No newline at end of file", "+TWO", "\\ No newline at end of file"},
			}},
		},
		{
			name: "CRLF conserva el retorno de carro",
			raw: "diff --git a/file.txt b/file.txt\n" +
				"--- a/file.txt\n" +
				"+++ b/file.txt\n" +
				"@@ -1 +1 @@\n" +
				"-one\r\n" +
				"+ONE\r\n",
			want: []diffHunk{{
				OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1,
				Lines: []string{"-one\r", "+ONE\r"},
			}},
		},
		{
			name: "binario sin hunks",
			raw:  "diff --git a/file.bin b/file.bin\nBinary files a/file.bin and b/file.bin differ\n",
			want: nil,
		},
		{
			name: "rename se rechaza",
			raw: "diff --git a/old.txt b/new.txt\n" +
				"similarity index 100%\n" +
				"rename from old.txt\n" +
				"rename to new.txt\n",
			renam: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFileDiff([]byte(tt.raw))
			if tt.renam {
				if !errors.Is(err, errRenameDiff) {
					t.Fatalf("parseFileDiff() error = %v, want errRenameDiff", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFileDiff() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseFileDiff() =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

func TestGroupHunks(t *testing.T) {
	// Un cuerpo de 101 bytes (prefijo incluido) más la cabecera y el salto suma 118.
	hunks := make([]diffHunk, 3)
	for i := range hunks {
		hunks[i] = diffHunk{
			OldStart: i + 1, OldCount: 1, NewStart: i + 1, NewCount: 1,
			Lines: []string{"+" + strings.Repeat("a", 99) + fmt.Sprint(i)},
		}
		if got := hunkSize(hunks[i]); got != 118 {
			t.Fatalf("hunkSize() = %d, want 118", got)
		}
	}

	tests := []struct {
		name     string
		maxBytes int
		want     [][]diffHunk
	}{
		{name: "todo cabe", maxBytes: 1 << 20, want: [][]diffHunk{hunks}},
		{name: "corta por presupuesto", maxBytes: 250, want: [][]diffHunk{hunks[:2], hunks[2:]}},
		{name: "hunk mayor que el límite va solo", maxBytes: 50, want: [][]diffHunk{{hunks[0]}, {hunks[1]}, {hunks[2]}}},
		{name: "cero desactiva el límite", maxBytes: 0, want: [][]diffHunk{hunks}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := groupHunks(hunks, tt.maxBytes); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("groupHunks() = %#v, want %#v", got, tt.want)
			}
		})
	}

	if got := groupHunks(nil, 10); got != nil {
		t.Fatalf("groupHunks(nil) = %#v, want nil", got)
	}
}

// assertChunkMatches comprueba el contrato del commit sintético: P..C contiene
// exactamente los hunks del trozo, C coincide con head y la numeración del lado
// nuevo (la que OCR reporta) es la del chunk.
//
// OldStart no se compara con el del diff original: P conserva los demás trozos
// de head, así que su lado viejo se numera desde head y no desde la base.
func assertChunkMatches(t *testing.T, dir, baseSHA, headSHA, path string, ch chunk) {
	t.Helper()

	raw, err := gitRaw(context.Background(), dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", ch.Parent, ch.Commit, "--", path)
	if err != nil {
		t.Fatalf("git diff P C: %v", err)
	}
	got, err := parseFileDiff(raw)
	if err != nil {
		t.Fatalf("parseFileDiff(P C): %v", err)
	}
	if len(got) != len(ch.Hunks) {
		t.Fatalf("P..C tiene %d hunks, want %d", len(got), len(ch.Hunks))
	}
	for i := range got {
		if got[i].NewStart != ch.Hunks[i].NewStart || got[i].NewCount != ch.Hunks[i].NewCount {
			t.Fatalf("hunk %d: lado nuevo = %d,%d, want %d,%d",
				i, got[i].NewStart, got[i].NewCount, ch.Hunks[i].NewStart, ch.Hunks[i].NewCount)
		}
		if got[i].OldCount != ch.Hunks[i].OldCount {
			t.Fatalf("hunk %d: OldCount = %d, want %d", i, got[i].OldCount, ch.Hunks[i].OldCount)
		}
		if !reflect.DeepEqual(got[i].Lines, ch.Hunks[i].Lines) {
			t.Fatalf("hunk %d: cuerpo = %#v, want %#v", i, got[i].Lines, ch.Hunks[i].Lines)
		}
	}

	headContent, err := gitRaw(context.Background(), dir, "test-show-head", "show", headSHA+":"+path)
	if err != nil {
		t.Fatalf("git show head: %v", err)
	}
	chunkContent, err := gitRaw(context.Background(), dir, "test-show-chunk", "show", ch.Commit+":"+path)
	if err != nil {
		t.Fatalf("git show C: %v", err)
	}
	if !bytes.Equal(headContent, chunkContent) {
		t.Fatalf("C:%s = %q, want head %q", path, chunkContent, headContent)
	}

	parent := strings.TrimSpace(string(mustGitRaw(t, dir, "test-rev-parse", "rev-parse", ch.Parent+"^")))
	if parent != baseSHA {
		t.Fatalf("P^ = %q, want base %q", parent, baseSHA)
	}
}

func mustGitRaw(t *testing.T, dir, op string, args ...string) []byte {
	t.Helper()
	out, err := gitRaw(context.Background(), dir, op, args...)
	if err != nil {
		t.Fatalf("git %s: %v", op, err)
	}
	return out
}

func TestChunkFileDiff(t *testing.T) {
	multiBase := numberedLines(30)
	multiHead := append([]string(nil), multiBase...)
	multiHead[1] = "changed02"
	multiHead[9] = "changed10"
	multiHead[10] = "changed11"
	multiHead[27] = "changed28"

	delBase := numberedLines(12)
	delHead := append([]string(nil), delBase[:2]...)
	delHead = append(delHead, delBase[3:6]...)
	delHead = append(delHead, delBase[8:]...)

	tests := []struct {
		name      string
		base      map[string]string
		head      map[string]string
		path      string
		wantHunks int
	}{
		{
			name:      "archivo nuevo",
			base:      map[string]string{"keep.txt": "keep\n"},
			head:      map[string]string{"keep.txt": "keep\n", "new.txt": "alpha\nbeta\ngamma\n"},
			path:      "new.txt",
			wantHunks: 1,
		},
		{
			name:      "modificación en varios hunks",
			base:      map[string]string{"file.txt": textLines(multiBase)},
			head:      map[string]string{"file.txt": textLines(multiHead)},
			path:      "file.txt",
			wantHunks: 3,
		},
		{
			name:      "borrados puros",
			base:      map[string]string{"file.txt": textLines(delBase)},
			head:      map[string]string{"file.txt": textLines(delHead)},
			path:      "file.txt",
			wantHunks: 2,
		},
		{
			name:      "sin salto final",
			base:      map[string]string{"file.txt": "one\ntwo"},
			head:      map[string]string{"file.txt": "one\nTWO"},
			path:      "file.txt",
			wantHunks: 1,
		},
		{
			name:      "CRLF",
			base:      map[string]string{"file.txt": "one\r\ntwo\r\nthree\r\n"},
			head:      map[string]string{"file.txt": "one\r\nTWO\r\nthree\r\nfour\r\n"},
			path:      "file.txt",
			wantHunks: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newChunkRepo(t)
			baseSHA := commitChunkFiles(t, dir, "base", tt.base)
			headSHA := commitChunkFiles(t, dir, "head", tt.head)

			raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", baseSHA, headSHA, "--", tt.path)
			all, err := parseFileDiff(raw)
			if err != nil {
				t.Fatalf("parseFileDiff() error = %v", err)
			}
			if len(all) != tt.wantHunks {
				t.Fatalf("diff tiene %d hunks, want %d", len(all), tt.wantHunks)
			}

			for _, maxBytes := range []int{1 << 20, 1} {
				wantChunks := 1
				if maxBytes == 1 {
					wantChunks = len(all)
				}
				chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, tt.path, maxBytes)
				if err != nil {
					t.Fatalf("chunkFileDiff(maxBytes=%d) error = %v", maxBytes, err)
				}
				if len(chunks) != wantChunks {
					t.Fatalf("chunkFileDiff(maxBytes=%d) devolvió %d chunks, want %d", maxBytes, len(chunks), wantChunks)
				}

				var flat []diffHunk
				for _, ch := range chunks {
					flat = append(flat, ch.Hunks...)
					assertChunkMatches(t, dir, baseSHA, headSHA, tt.path, ch)
				}
				if !reflect.DeepEqual(flat, all) {
					t.Fatalf("hunks agrupados = %#v, want %#v", flat, all)
				}
			}
		})
	}
}

func TestBuildReviewArgsCommit(t *testing.T) {
	base := nativeInvocation{
		RepoDir:        "/work/run-1",
		BaseRef:        "base-sha",
		HeadRef:        "head-sha",
		BackgroundFile: "/work/run-1/background.md",
		Excludes:       "vendor/**",
		Profile:        store.LLMProfile{Model: "model-x"},
		Effort:         "high",
		MaxTokens:      270000,
		TokenBudget:    500000,
		Timeout:        90 * time.Second,
		Commit:         "chunk-sha",
	}

	got := buildReviewArgs(base)
	want := []string{
		"review",
		"--repo", "/work/run-1",
		"--commit", "chunk-sha",
		"--format", "json",
		"--audience", "agent",
		"--provider", nativeProviderName,
		"--model", "model-x",
		"--effort", "high",
		"--max-tokens", "270000",
		"--background-file", "/work/run-1/background.md",
		"--max-tokens-budget", "500000",
		"--timeout", "2",
		"--exclude", "vendor/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n%q\nwant\n%q", got, want)
	}
	for _, arg := range got {
		if arg == "--from" || arg == "--to" {
			t.Fatalf("unexpected %s with Commit set: %q", arg, got)
		}
	}

	legacy := base
	legacy.Commit = ""
	got = buildReviewArgs(legacy)
	want = []string{
		"review",
		"--repo", "/work/run-1",
		"--from", "base-sha",
		"--to", "head-sha",
		"--format", "json",
		"--audience", "agent",
		"--provider", nativeProviderName,
		"--model", "model-x",
		"--effort", "high",
		"--max-tokens", "270000",
		"--background-file", "/work/run-1/background.md",
		"--max-tokens-budget", "500000",
		"--timeout", "2",
		"--exclude", "vendor/**",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args sin Commit =\n%q\nwant\n%q", got, want)
	}
	for _, arg := range got {
		if arg == "--commit" {
			t.Fatalf("unexpected --commit without Commit: %q", got)
		}
	}
}
