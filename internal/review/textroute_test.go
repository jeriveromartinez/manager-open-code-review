package review

import (
	"context"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestTextReviewPathIsDeterministicAndIsolated(t *testing.T) {
	got := textReviewPath("docs/notes.md")
	if want := textReviewPath("./docs/notes.md"); got != want {
		t.Errorf("textReviewPath() = %q, want normalized %q", got, want)
	}
	if path.Dir(got) != textRouteDir || path.Ext(got) != textRouteExtension {
		t.Errorf("textReviewPath() = %q, want %s/*%s", got, textRouteDir, textRouteExtension)
	}
	if other := textReviewPath("docs/other.md"); other == got {
		t.Errorf("two different files share the synthetic path %q", got)
	}
}

func TestTextRouteCandidatesSelectsOnlyUncoveredText(t *testing.T) {
	res := nativeReview{Manifest: nativeManifest{Coverage: nativeCoverage{
		Selected:  []nativeCoverageItem{{Path: "auth.go"}, {Path: "notes.md"}, {Path: "failed.go"}},
		Completed: []nativeCoverageItem{{Path: "auth.go"}},
		Failed:    []nativeCoverageItem{{Path: "failed.go", Classification: "provider"}},
		Excluded:  []nativeCoverageItem{{Path: "notes.md", ExcludeReason: "unsupported_ext"}},
	}}}
	changed := []changedFile{
		{Path: "auth.go", Kind: changedModified},
		{Path: "notes.md", Kind: changedModified},
		{Path: "config.yaml", Kind: changedAdded},
		{Path: "vendor/blob.bin", Kind: changedBinary},
		{Path: "docs/doc.md", Kind: changedAdded},
		{Path: "failed.go", Kind: changedModified},
		{Path: "already.md", Kind: changedModified},
		{Path: "attempted.md", Kind: changedModified},
	}
	covered := map[string]bool{"auth.go": true, "already.md": true}
	attempted := map[string]bool{"attempted.md": true}

	got := textRouteCandidates(changed, []string{"docs/**"}, covered, attempted, res)
	want := []string{"notes.md", "config.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("textRouteCandidates() = %v, want %v", got, want)
	}
}

func TestTextReviewPathIsNotTheOriginalPath(t *testing.T) {
	file := "notes.md"
	reviewPath := textReviewPath(file)
	if reviewPath == file || strings.HasSuffix(reviewPath, file) {
		t.Errorf("textReviewPath(%q) = %q, want an isolated path", file, reviewPath)
	}
}

func TestTextRouteValid(t *testing.T) {
	reviewPath := textReviewPath("notes.md")
	valid := nativeReview{
		Status: "complete",
		Comments: []nativeComment{{
			Path: reviewPath, Content: "Hallazgo\n\ncuerpo", StartLine: 2, EndLine: 3,
		}},
		Manifest: nativeManifest{Coverage: nativeCoverage{
			Completed: []nativeCoverageItem{{Path: reviewPath}},
		}},
	}
	if reason, ok := textRouteValid(valid, reviewPath, 3); !ok {
		t.Errorf("textRouteValid() = %q, want a valid result", reason)
	}

	tests := []struct {
		name   string
		mutate func(*nativeReview)
		want   string
	}{
		{
			name: "sin la ruta sintética completada",
			mutate: func(res *nativeReview) {
				res.Manifest.Coverage.Completed = nil
			},
			want: "text_route_invalid",
		},
		{
			name: "comentario de otra ruta",
			mutate: func(res *nativeReview) {
				res.Comments[0].Path = "notes.md"
			},
			want: "text_route_invalid",
		},
		{
			name: "rango fuera del archivo",
			mutate: func(res *nativeReview) {
				res.Comments[0].EndLine = 4
			},
			want: "text_route_invalid",
		},
		{
			name: "contenido vacío",
			mutate: func(res *nativeReview) {
				res.Comments[0].Content = " "
			},
			want: "text_route_invalid",
		},
		{
			name: "excluida pese a completarse",
			mutate: func(res *nativeReview) {
				res.Manifest.Coverage.Excluded = []nativeCoverageItem{{Path: reviewPath, ExcludeReason: "unsupported_ext"}}
			},
			want: "text_route_invalid",
		},
		{
			name: "resultado parcial",
			mutate: func(res *nativeReview) {
				res.Status = "partial"
			},
			want: "coverage_incomplete",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := valid
			res.Comments = append([]nativeComment(nil), valid.Comments...)
			res.Manifest.Coverage.Completed = append([]nativeCoverageItem(nil), valid.Manifest.Coverage.Completed...)
			tt.mutate(&res)
			if reason, ok := textRouteValid(res, reviewPath, 3); ok || reason != tt.want {
				t.Errorf("textRouteValid() = %q/%v, want %q/false", reason, ok, tt.want)
			}
		})
	}
}

func TestTextRouteChunksKeepsSyntheticContent(t *testing.T) {
	dir := newChunkRepo(t)
	file := "notes.md"
	baseContent := "alpha\nbeta\ngamma\n"
	headContent := "alpha\nBETA\ndelta\ngamma\n"
	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{file: baseContent})
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{file: headContent})

	headBlob, mode, exists, err := blobAt(context.Background(), dir, headSHA, file)
	if err != nil || !exists {
		t.Fatalf("blobAt head: exists=%v err=%v", exists, err)
	}
	reviewPath := textReviewPath(file)
	chunks, err := textRouteChunks(context.Background(), dir, baseSHA, headSHA, reviewPath, []byte(baseContent), headBlob, mode, 1<<20)
	if err != nil {
		t.Fatalf("textRouteChunks() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("textRouteChunks() returned %d chunks, want 1", len(chunks))
	}
	ch := chunks[0]

	chunkContent, err := gitRaw(context.Background(), dir, "test-show-chunk", "show", ch.Commit+":"+reviewPath)
	if err != nil {
		t.Fatalf("show C:%s: %v", reviewPath, err)
	}
	if string(chunkContent) != headContent {
		t.Errorf("C:%s = %q, want head content %q", reviewPath, chunkContent, headContent)
	}
	parentContent, err := gitRaw(context.Background(), dir, "test-show-parent", "show", ch.Parent+":"+reviewPath)
	if err != nil {
		t.Fatalf("show P:%s: %v", reviewPath, err)
	}
	if string(parentContent) != baseContent {
		t.Errorf("P:%s = %q, want base content %q", reviewPath, parentContent, baseContent)
	}
	raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames", ch.Parent, ch.Commit, "--", reviewPath)
	got, err := parseFileDiff(raw)
	if err != nil {
		t.Fatalf("parseFileDiff(P C): %v", err)
	}
	if len(got) != 1 || got[0].NewStart != 2 || got[0].NewCount != 2 {
		t.Fatalf("synthetic diff = %+v, want the beta replacement on the head side", got)
	}
}

func TestTextRouteChunksAddedFile(t *testing.T) {
	dir := newChunkRepo(t)
	file := "config.yaml"
	headContent := "key: value\nother: true\n"
	baseSHA := commitChunkFiles(t, dir, "base", map[string]string{"keep.txt": "keep\n"})
	headSHA := commitChunkFiles(t, dir, "head", map[string]string{"keep.txt": "keep\n", file: headContent})

	headBlob, mode, exists, err := blobAt(context.Background(), dir, headSHA, file)
	if err != nil || !exists {
		t.Fatalf("blobAt head: exists=%v err=%v", exists, err)
	}
	reviewPath := textReviewPath(file)
	chunks, err := textRouteChunks(context.Background(), dir, baseSHA, headSHA, reviewPath, nil, headBlob, mode, 1<<20)
	if err != nil {
		t.Fatalf("textRouteChunks() error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("textRouteChunks() returned %d chunks, want 1", len(chunks))
	}
	raw := mustGitRaw(t, dir, "test-diff", "diff", "-U0", "--no-color", "--no-renames",
		chunks[0].Parent, chunks[0].Commit, "--", reviewPath)
	got, err := parseFileDiff(raw)
	if err != nil {
		t.Fatalf("parseFileDiff(P C): %v", err)
	}
	if len(got) != 1 || got[0].NewCount != 2 {
		t.Fatalf("synthetic diff = %+v, want the two added lines", got)
	}
	for _, line := range got[0].Lines {
		if !strings.HasPrefix(line, "+") {
			t.Errorf("line %q is not an addition", line)
		}
	}
}

func TestTextRouteStatusCountsAsReviewed(t *testing.T) {
	summary := chunkSummary{statuses: map[string]coverageFile{
		"notes.md": {Path: "notes.md", Status: coverageReviewed, Method: coverageMethodText},
	}}
	if !summary.allReviewed() {
		t.Error("allReviewed() = false, want a text-route file to count as coverage")
	}
	status, code := chunkedRunStatus(store.RunSucceeded, "", summary)
	if status != store.RunSucceeded || code != "" {
		t.Errorf("chunkedRunStatus() = %q/%q, want succeeded", status, code)
	}
}
