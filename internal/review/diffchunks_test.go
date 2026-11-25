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
	return commitChunkFilesModes(t, dir, message, files, nil)
}

// commitChunkFilesModes permite marcar archivos ejecutables en el índice; el
// bit se fija con update-index para que no dependa del filesystem del test.
func commitChunkFilesModes(t *testing.T, dir, message string, files map[string]string, executable map[string]bool) string {
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
		if executable[path] {
			runGit(t, dir, "update-index", "--chmod=+x", "--", path)
		}
	}
	runGit(t, dir, "commit", "-qm", message)
	return runGit(t, dir, "rev-parse", "HEAD")
}

func lsTreeMode(t *testing.T, dir, sha, path string) string {
	t.Helper()

	fields := strings.Fields(runGit(t, dir, "ls-tree", sha, "--", path))
	if len(fields) < 2 || fields[1] != "blob" {
		t.Fatalf("ls-tree %s %s = %q, want blob", sha, path, fields)
	}
	return fields[0]
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

func TestChunkFileDiffKeepsExecutableMode(t *testing.T) {
	dir := newChunkRepo(t)
	path := "bin/run.sh"
	base := numberedLines(20)
	head := append([]string(nil), base...)
	head[1] = "changed02"
	head[12] = "changed13"
	head[18] = "changed19"

	baseSHA := commitChunkFilesModes(t, dir, "base",
		map[string]string{path: textLines(base)}, map[string]bool{path: true})
	headSHA := commitChunkFilesModes(t, dir, "head",
		map[string]string{path: textLines(head)}, map[string]bool{path: true})
	if mode := lsTreeMode(t, dir, headSHA, path); mode != "100755" {
		t.Fatalf("head mode = %q, want 100755", mode)
	}

	chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, path, 1)
	if err != nil {
		t.Fatalf("chunkFileDiff() error = %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunkFileDiff() devolvió %d chunks, want 3", len(chunks))
	}

	for _, ch := range chunks {
		if mode := lsTreeMode(t, dir, ch.Parent, path); mode != "100755" {
			t.Fatalf("P mode = %q, want 100755", mode)
		}
		if mode := lsTreeMode(t, dir, ch.Commit, path); mode != "100755" {
			t.Fatalf("C mode = %q, want 100755", mode)
		}

		raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", ch.Parent, ch.Commit, "--", path)
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") {
				t.Fatalf("P..C cambió el modo: %q", line)
			}
		}
		assertChunkMatches(t, dir, baseSHA, headSHA, path, ch)
	}
}

func TestChunkFileDiffPathWithComma(t *testing.T) {
	dir := newChunkRepo(t)
	path := "dir/a,b.txt"
	base := numberedLines(20)
	head := append([]string(nil), base...)
	head[1] = "changed02"
	head[12] = "changed13"
	head[18] = "changed19"

	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{path: textLines(base)})
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{path: textLines(head)})

	chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, path, 1)
	if err != nil {
		t.Fatalf("chunkFileDiff() error = %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunkFileDiff() devolvió %d chunks, want 3", len(chunks))
	}
	for _, ch := range chunks {
		assertChunkMatches(t, dir, baseSHA, headSHA, path, ch)
	}
}

func TestWindowHunksSplitsOversizedHunkWithoutDuplication(t *testing.T) {
	lines := make([]string, 24)
	for i := range lines {
		lines[i] = "+" + strings.Repeat("x", 300) + fmt.Sprint(i)
	}
	hunk := diffHunk{OldStart: 0, OldCount: 0, NewStart: 1, NewCount: len(lines), Lines: lines}
	if hunkSize(hunk) <= 2<<10 {
		t.Fatalf("hunk size = %d, want it above the window budget", hunkSize(hunk))
	}

	windows := windowHunks([]diffHunk{hunk}, 2<<10)
	if len(windows) < 2 {
		t.Fatalf("windows = %d, want the oversized hunk split", len(windows))
	}
	seen := make(map[string]int, len(lines))
	var flat []string
	nextNew := 1
	for _, window := range windows {
		if got := hunkSize(window); got > 2<<10 {
			t.Errorf("window size = %d, want <= 2048", got)
		}
		if window.NewStart != nextNew {
			t.Errorf("window NewStart = %d, want %d", window.NewStart, nextNew)
		}
		if window.NewCount != len(window.Lines) {
			t.Errorf("window NewCount = %d, want %d", window.NewCount, len(window.Lines))
		}
		if window.OldStart != 0 || window.OldCount != 0 {
			t.Errorf("window old side = %d,%d, want 0,0", window.OldStart, window.OldCount)
		}
		nextNew += window.NewCount
		for _, line := range window.Lines {
			seen[line]++
			flat = append(flat, line)
		}
	}
	if !reflect.DeepEqual(flat, lines) {
		t.Fatalf("flattened windows = %#v, want the original lines in order", flat)
	}
	for line, count := range seen {
		if count != 1 {
			t.Errorf("line %q appears %d times, want exactly once", line, count)
		}
	}
}

func TestChunkFileDiffWindowsOversizedHunk(t *testing.T) {
	dir := newChunkRepo(t)
	path := "big.txt"
	base := numberedLines(160)
	head := append([]string(nil), base...)
	for i := 40; i < 120; i++ {
		head[i] = fmt.Sprintf("%s%03d", strings.Repeat("x", 40), i)
	}

	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{path: textLines(base)})
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{path: textLines(head)})

	raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", baseSHA, headSHA, "--", path)
	all, err := parseFileDiff(raw)
	if err != nil {
		t.Fatalf("parseFileDiff() error = %v", err)
	}
	if len(all) != 1 || hunkSize(all[0]) <= 2<<10 {
		t.Fatalf("diff has %d hunk(s) of size %d, want one oversized hunk", len(all), hunkSize(all[0]))
	}

	chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, path, 2<<10)
	if err != nil {
		t.Fatalf("chunkFileDiff() error = %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunkFileDiff() returned %d chunks, want the hunk windowed", len(chunks))
	}
	var flat []string
	for _, ch := range chunks {
		assertChunkMatches(t, dir, baseSHA, headSHA, path, ch)
		for _, window := range ch.Hunks {
			flat = append(flat, window.Lines...)
		}
	}
	if !reflect.DeepEqual(flat, all[0].Lines) {
		t.Fatalf("windows lose or duplicate lines: %d vs %d", len(flat), len(all[0].Lines))
	}
}

func TestChunkFileDiffDeletedFileKeepsBaseSide(t *testing.T) {
	dir := newChunkRepo(t)
	path := "gone.txt"
	base := numberedLines(400)
	baseContent := textLines(base)
	lineNumber := make(map[string]int, len(base))
	for i, line := range base {
		lineNumber[line] = i + 1
	}
	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{path: baseContent})
	runGit(t, dir, "rm", "-q", path)
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{"keep.txt": "keep\n"})

	chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, path, 2<<10)
	if err != nil {
		t.Fatalf("chunkFileDiff() error = %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunkFileDiff() returned %d chunks, want the deletion windowed", len(chunks))
	}

	seen := make(map[string]bool, len(base))
	for i, ch := range chunks {
		// Every window keeps the full base as the parent side, so the old line
		// numbers are the real ones and no window depends on another.
		parentContent, mode, exists, err := blobAt(context.Background(), dir, ch.Parent, path)
		if err != nil || !exists {
			t.Fatalf("window %d P is missing %s: exists=%v err=%v", i+1, path, exists, err)
		}
		if mode != "100644" || string(parentContent) != baseContent {
			t.Fatalf("window %d P = mode %q content %q, want the full base", i+1, mode, parentContent)
		}
		if parent := strings.TrimSpace(string(mustGitRaw(t, dir, "test-rev-parse", "rev-parse", ch.Parent+"^"))); parent != baseSHA {
			t.Errorf("window %d P^ = %q, want base %q", i+1, parent, baseSHA)
		}

		raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", ch.Parent, ch.Commit, "--", path)
		got, err := parseFileDiff(raw)
		if err != nil {
			t.Fatalf("window %d parseFileDiff(P C): %v", i+1, err)
		}
		if len(got) != len(ch.Hunks) {
			t.Fatalf("window %d P..C has %d hunks, want %d", i+1, len(got), len(ch.Hunks))
		}
		for j, hunk := range got {
			if hunk.NewCount != 0 || hunk.OldCount != len(hunk.Lines) {
				t.Fatalf("window %d hunk %d = %+v, want a pure deletion", i+1, j+1, hunk)
			}
			if !reflect.DeepEqual(hunk.Lines, ch.Hunks[j].Lines) {
				t.Fatalf("window %d hunk %d = %#v, want %#v", i+1, j+1, hunk.Lines, ch.Hunks[j].Lines)
			}
			first := strings.TrimPrefix(hunk.Lines[0], "-")
			if hunk.OldStart != lineNumber[first] {
				t.Errorf("window %d hunk %d OldStart = %d, want the base line %d",
					i+1, j+1, hunk.OldStart, lineNumber[first])
			}
			for _, line := range hunk.Lines {
				if !strings.HasPrefix(line, "-") {
					t.Fatalf("line %q is not a deletion", line)
				}
				text := strings.TrimPrefix(line, "-")
				if seen[text] {
					t.Fatalf("base line %q appears in more than one window", text)
				}
				seen[text] = true
			}
		}
	}
	if len(seen) != len(base) {
		t.Errorf("windows deleted %d base lines, want %d", len(seen), len(base))
	}
}

func TestChunkFileDiffRenameKeepsHeadSide(t *testing.T) {
	dir := newChunkRepo(t)
	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{"old.txt": "one\ntwo\nthree\n"})
	runGit(t, dir, "mv", "old.txt", "new.txt")
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{"new.txt": "one\nTWO\nthree\n"})

	// The test must exercise a real rename, not a copy plus an addition.
	nameStatus := strings.TrimSpace(string(mustGitRaw(t, dir, "test-name-status",
		"diff", "--name-status", "-M", baseSHA, headSHA)))
	if !strings.HasPrefix(nameStatus, "R") ||
		!strings.Contains(nameStatus, "old.txt") || !strings.Contains(nameStatus, "new.txt") {
		t.Fatalf("git diff --name-status = %q, want a rename old.txt -> new.txt", nameStatus)
	}
	if _, _, exists, err := blobAt(context.Background(), dir, baseSHA, "new.txt"); err != nil || exists {
		t.Fatalf("base records new.txt (exists=%v, err=%v), want the old path only", exists, err)
	}
	if _, exists, err := fileModeAt(context.Background(), dir, headSHA, "old.txt"); err != nil || exists {
		t.Fatalf("head records old.txt (exists=%v, err=%v), want the new path only", exists, err)
	}

	chunks, err := chunkFileDiff(context.Background(), dir, baseSHA, headSHA, "new.txt", 1<<20)
	if err != nil {
		t.Fatalf("chunkFileDiff() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunkFileDiff() returned %d chunks, want 1", len(chunks))
	}
	ch := chunks[0]
	// The new path is reviewed as an addition on the head side, so every
	// reported line belongs to the file that exists at head.
	if len(ch.Hunks) != 1 || ch.Hunks[0].NewStart != 1 || ch.Hunks[0].NewCount != 3 {
		t.Fatalf("rename hunk = %+v, want the head side starting at line 1", ch.Hunks)
	}
	assertChunkMatches(t, dir, baseSHA, headSHA, "new.txt", ch)
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
		TaskTimeout:    2 * time.Minute,
		Commit:         "chunk-sha",
	}

	got := buildReviewArgs(base)
	want := []string{
		"review",
		"--repo", "/work/run-1",
		"--commit", "chunk-sha",
		"--format", "json",
		"--audience", "human",
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
		"--audience", "human",
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
