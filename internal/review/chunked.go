package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// coverageFile es una entrada de la cobertura por archivo que pinta el panel.
type coverageFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Chunks int    `json:"chunks,omitempty"`
	Reason string `json:"reason,omitempty"`
}

const (
	coverageReviewed        = "reviewed"
	coverageReviewedByParts = "reviewed_by_parts"
	coverageNotReviewed     = "not_reviewed"
	coverageExcluded        = "excluded"

	// chunkBudgetPercent is the fraction of --max-tokens a chunk may use. OCR
	// excludes a file as too_large above 80% of the ceiling; 30% leaves a wide
	// safety margin and keeps every chunk a small, independent conversation.
	chunkBudgetPercent = 30
	// bytesPerToken estimates the serialized diff size of one token.
	bytesPerToken = 3
)

// chunkSummary accumulates the outcome of reviewing too-large files by parts.
type chunkSummary struct {
	statuses map[string]coverageFile
	comments []nativeComment
	tokens   tokenUsage
}

func newChunkSummary() chunkSummary {
	return chunkSummary{statuses: make(map[string]coverageFile)}
}

func (s *chunkSummary) markNotReviewed(path, reason string) {
	s.statuses[path] = coverageFile{Path: path, Status: coverageNotReviewed, Reason: reason}
}

// allReviewed reports whether every chunked file finished fully reviewed.
func (s chunkSummary) allReviewed() bool {
	if len(s.statuses) == 0 {
		return false
	}
	for _, file := range s.statuses {
		if file.Status != coverageReviewedByParts {
			return false
		}
	}
	return true
}

// reviewedAny reports whether at least one chunked file finished fully reviewed.
func (s chunkSummary) reviewedAny() bool {
	for _, file := range s.statuses {
		if file.Status == coverageReviewedByParts {
			return true
		}
	}
	return false
}

// reviewedPaths returns the paths whose chunks all succeeded, so a run may
// resolve their open findings.
func (s chunkSummary) reviewedPaths() map[string]bool {
	paths := make(map[string]bool, len(s.statuses))
	for path, file := range s.statuses {
		if file.Status == coverageReviewedByParts {
			paths[path] = true
		}
	}
	return paths
}

// coverageJSONWithFiles serializes the headline counters and the per-file array
// the panel already expects. files stays omitted when the engine reported no
// manifest, so old runs and empty-coverage responses keep their previous shape.
func coverageJSONWithFiles(reviewed, skipped, excluded int, files []coverageFile) []byte {
	raw, err := json.Marshal(struct {
		ReviewedFiles int            `json:"reviewed_files"`
		SkippedFiles  int            `json:"skipped_files"`
		ExcludedFiles int            `json:"excluded_files"`
		Files         []coverageFile `json:"files,omitempty"`
	}{reviewed, skipped, excluded, files})
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

// nativeCoverageFiles flattens manifest coverage into the per-file array. A path
// repeated across groups keeps its first status: completed/reused win over the
// rest, which matches how OCR emits the manifest.
func nativeCoverageFiles(res nativeReview) []coverageFile {
	coverage := res.Manifest.Coverage
	var files []coverageFile
	seen := make(map[string]bool)
	add := func(items []nativeCoverageItem, status string, reason func(nativeCoverageItem) string) {
		for _, item := range items {
			path := normalizeCoveragePath(item.Path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			files = append(files, coverageFile{Path: path, Status: status, Reason: reason(item)})
		}
	}
	add(coverage.Completed, coverageReviewed, func(nativeCoverageItem) string { return "" })
	add(coverage.Reused, coverageReviewed, func(nativeCoverageItem) string { return "" })
	add(coverage.Failed, coverageNotReviewed, func(item nativeCoverageItem) string {
		if class := strings.TrimSpace(item.Classification); class != "" {
			return class
		}
		return strings.TrimSpace(item.Reason)
	})
	add(coverage.Waived, coverageNotReviewed, func(item nativeCoverageItem) string {
		return strings.TrimSpace(item.Reason)
	})
	add(coverage.Excluded, coverageExcluded, func(item nativeCoverageItem) string {
		if reason := strings.TrimSpace(item.ExcludeReason); reason != "" {
			return reason
		}
		return strings.TrimSpace(item.Reason)
	})
	return files
}

// coverageCountsFromFiles keeps the headline counters consistent with the array.
func coverageCountsFromFiles(files []coverageFile) (reviewed, skipped, excluded int) {
	for _, file := range files {
		switch file.Status {
		case coverageReviewed, coverageReviewedByParts:
			reviewed++
		case coverageNotReviewed:
			skipped++
		case coverageExcluded:
			excluded++
		}
	}
	return reviewed, skipped, excluded
}

// overlayCoverage replaces the status of chunked paths and appends any path the
// main manifest did not list.
func overlayCoverage(files []coverageFile, statuses map[string]coverageFile) []coverageFile {
	index := make(map[string]int, len(files))
	for i, file := range files {
		index[file.Path] = i
	}
	paths := make([]string, 0, len(statuses))
	for path := range statuses {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := statuses[path]
		if i, ok := index[path]; ok {
			files[i] = file
			continue
		}
		index[path] = len(files)
		files = append(files, file)
	}
	return files
}

// tooLargePaths returns the normalized paths OCR excluded for being too large.
func tooLargePaths(res nativeReview) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, item := range res.Manifest.Coverage.Excluded {
		if !strings.EqualFold(strings.TrimSpace(item.ExcludeReason), "too_large") {
			continue
		}
		path := normalizeCoveragePath(item.Path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// canChunkNativeRun limits chunking to runs whose engine still produced usable
// findings. A provider, timeout or configuration failure is classified at the
// run level and chunking would only multiply it.
func canChunkNativeRun(status, code string) bool {
	if status != store.RunSucceeded && status != store.RunInconclusive {
		return false
	}
	switch code {
	case "", "no_coverage", "coverage_incomplete":
		return true
	default:
		return false
	}
}

// chunkByteBudget converts the per-group token ceiling into a diff budget. The
// 30% cap stays well below OCR's 80% too_large gate.
func chunkByteBudget(maxTokens int) int {
	if maxTokens <= 0 {
		return 0
	}
	return maxTokens * bytesPerToken * chunkBudgetPercent / 100
}

// reviewTooLargeFiles reviews each path by synthetic chunks sequentially. A
// failing chunk is recorded and the loop continues: the rest of the PR and the
// chunks that did succeed keep their findings.
func (r *Runner) reviewTooLargeFiles(ctx context.Context, runID, profileID uint, reviewer *NativeReviewer, headSHA string, paths []string, budget int64, mainUsage tokenUsage) chunkSummary {
	summary := newChunkSummary()
	maxBytes := chunkByteBudget(reviewer.MaxTokens)
	stopBudget := false

	for _, path := range paths {
		if stopBudget {
			summary.markNotReviewed(path, "token_budget")
			slog.Info("chunked too-large file", "run_id", runID, "path", path, "chunks", 0, "failures", 1)
			continue
		}
		if ctx.Err() != nil {
			summary.markNotReviewed(path, "timeout")
			slog.Info("chunked too-large file", "run_id", runID, "path", path, "chunks", 0, "failures", 1)
			continue
		}

		chunks, err := chunkFileDiff(ctx, reviewer.RepoDir, reviewer.BaseRef, headSHA, path, maxBytes)
		if err != nil {
			slog.Warn("chunk diff failed", "run_id", runID, "path", path, "error", err)
			summary.markNotReviewed(path, "chunk_error")
			slog.Info("chunked too-large file", "run_id", runID, "path", path, "chunks", 0, "failures", 1)
			continue
		}
		if len(chunks) == 0 {
			summary.markNotReviewed(path, "too_large")
			slog.Info("chunked too-large file", "run_id", runID, "path", path, "chunks", 0, "failures", 1)
			continue
		}

		failures := 0
		reason := ""
		for _, ch := range chunks {
			if ctx.Err() != nil {
				failures++
				if reason == "" {
					reason = "timeout"
				}
				break
			}

			chunkReviewer := *reviewer
			chunkReviewer.Commit = ch.Commit
			if budget > 0 {
				remaining := budget - int64(mainUsage.Total) - int64(summary.tokens.Total)
				if remaining <= 0 {
					failures++
					if reason == "" {
						reason = "token_budget"
					}
					stopBudget = true
					break
				}
				chunkReviewer.TokenBudget = remaining
			}

			chunkRes, _, reviewErr := r.runNativeReview(ctx, profileID, &chunkReviewer)
			finishChunk(ctx, &summary, path, chunkRes, reviewErr, &failures, &reason)
		}

		if failures == 0 {
			summary.statuses[path] = coverageFile{
				Path:   path,
				Status: coverageReviewedByParts,
				Chunks: len(chunks),
			}
		} else {
			if reason == "" {
				reason = "provider"
			}
			summary.markNotReviewed(path, reason)
		}
		slog.Info("chunked too-large file",
			"run_id", runID, "path", path, "chunks", len(chunks), "failures", failures)
	}
	return summary
}

// finishChunk records one chunk result: usage always counts, a successful chunk
// contributes its comments, and a failed one only marks the file as incomplete.
func finishChunk(ctx context.Context, summary *chunkSummary, path string, res nativeReview, err error, failures *int, reason *string) {
	if err != nil {
		*failures = *failures + 1
		if *reason == "" {
			*reason = chunkErrorReason(ctx, err)
		}
		return
	}
	summary.tokens.add(nativeTokenUsage(res))
	tooLarge := chunkExcludedTooLarge(res, path)
	status, code := nativeOutcome(res)
	if status == store.RunSucceeded && !tooLarge {
		summary.comments = append(summary.comments, res.Comments...)
		return
	}
	*failures = *failures + 1
	if *reason == "" {
		*reason = chunkFailureReason(code, tooLarge)
	}
}

func chunkErrorReason(ctx context.Context, err error) string {
	if truncatedContext(ctx, err) {
		return "timeout"
	}
	return "provider"
}

func chunkFailureReason(code string, tooLarge bool) string {
	switch code {
	case "timeout":
		return "timeout"
	case "token_budget":
		return "token_budget"
	}
	if tooLarge {
		return "too_large"
	}
	return "provider"
}

// chunkExcludedTooLarge reports whether the chunk's own manifest still excluded
// the file for size, in which case the chunk did not really review it.
func chunkExcludedTooLarge(res nativeReview, path string) bool {
	for _, item := range res.Manifest.Coverage.Excluded {
		if normalizeCoveragePath(item.Path) == path &&
			strings.EqualFold(strings.TrimSpace(item.ExcludeReason), "too_large") {
			return true
		}
	}
	return false
}

// chunkedRunStatus derives the final status after chunking. A clean main run
// whose too-large files were fully reviewed becomes succeeded; any failed file
// keeps the run inconclusive. no_coverage survives only when nothing at all was
// reviewed.
func chunkedRunStatus(mainStatus, mainCode string, summary chunkSummary) (string, string) {
	if len(summary.statuses) == 0 {
		return mainStatus, mainCode
	}
	if summary.allReviewed() {
		if mainStatus == store.RunSucceeded || mainCode == "no_coverage" {
			return store.RunSucceeded, ""
		}
		return mainStatus, mainCode
	}
	if mainStatus == store.RunSucceeded {
		return store.RunInconclusive, "coverage_incomplete"
	}
	if mainCode == "no_coverage" {
		if !summary.reviewedAny() {
			return store.RunInconclusive, "no_coverage"
		}
		return store.RunInconclusive, "coverage_incomplete"
	}
	return mainStatus, mainCode
}

// coverageSummaryText renders the run summary from the final coverage counters
// instead of the engine's headline, which cannot see chunk results.
func coverageSummaryText(status string, reviewed, comments int) string {
	prefix := "Revisión completada"
	switch status {
	case store.RunInconclusive:
		prefix = "Revisión parcial"
	case store.RunFailed:
		prefix = "Revisión fallida"
	}
	return fmt.Sprintf("%s: %d %s revisados, %d %s",
		prefix, reviewed, plural(reviewed, "archivo", "archivos"),
		comments, plural(comments, "hallazgo", "hallazgos"))
}

// chunkedInconclusiveMessage keeps the specific no_coverage wording but reports
// the real counts once chunks were attempted.
func chunkedInconclusiveMessage(res nativeReview, code string, reviewed, skipped int) string {
	if code == "coverage_incomplete" {
		return fmt.Sprintf("cobertura incompleta: %d archivo(s) sin revisar de %d", skipped, reviewed+skipped)
	}
	return nativeStatusMessage(res, code)
}
