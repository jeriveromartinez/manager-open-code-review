package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// textRouteDir keeps synthetic review paths out of the repository's real
	// namespaces. OCR reviews them as Go, a language it always accepts.
	textRouteDir = "qa-review-text"
	// textRouteExtension is the compatible extension used for text OCR omitted.
	// Follow-up (#147): validate this relabel with real OCR on a Markdown and a
	// YAML sample in a deployed dry run before trusting it beyond the fixtures.
	textRouteExtension = ".go"
)

// textReviewPath derives a stable synthetic path for one unsupported text file.
// The hash keeps two files with the same basename apart and makes the route
// reproducible across runs.
func textReviewPath(file string) string {
	sum := sha256.Sum256([]byte(normalizeCoveragePath(file)))
	return path.Join(textRouteDir, hex.EncodeToString(sum[:8])+textRouteExtension)
}

// textRouteCandidates selects changed text files the main and chunked passes
// left uncovered. A path OCR excluded for a reason other than an unsupported
// extension is a deliberate decision, and a path a chunk already attempted is
// not retried: the same engine would fail again.
func textRouteCandidates(changed []changedFile, patterns []string, covered, attempted map[string]bool, res nativeReview) []string {
	reasons := nativeExclusionReasons(res)
	reported := nativeReportedPaths(res)
	var paths []string
	for _, entry := range changed {
		p := normalizeCoveragePath(entry.Path)
		if p == "" || covered[p] || attempted[p] {
			continue
		}
		if entry.Kind != changedAdded && entry.Kind != changedModified {
			continue
		}
		if policyExcludesPath(patterns, p) {
			continue
		}
		if reported[p] && reasons[p] != "unsupported_ext" {
			continue
		}
		paths = append(paths, p)
	}
	return paths
}

func nativeExclusionReasons(res nativeReview) map[string]string {
	reasons := make(map[string]string)
	for _, item := range res.Manifest.Coverage.Excluded {
		p := normalizeCoveragePath(item.Path)
		if p == "" {
			continue
		}
		reason := strings.TrimSpace(item.ExcludeReason)
		if reason == "" {
			reason = strings.TrimSpace(item.Reason)
		}
		reasons[p] = strings.ToLower(reason)
	}
	return reasons
}

func nativeReportedPaths(res nativeReview) map[string]bool {
	reported := make(map[string]bool)
	coverage := res.Manifest.Coverage
	for _, group := range [][]nativeCoverageItem{
		coverage.Selected, coverage.Completed, coverage.Reused,
		coverage.Failed, coverage.Waived, coverage.Excluded,
	} {
		for _, item := range group {
			if p := normalizeCoveragePath(item.Path); p != "" {
				reported[p] = true
			}
		}
	}
	return reported
}

// markTextPending records a text-route failure. An invalid or unbuildable
// result means there is no verifiable analysis, so it also needs a human.
func (s *chunkSummary) markTextPending(file, reason string) {
	s.markNotReviewed(file, coverageMethodText, reason)
	if reason == "text_route_invalid" || reason == "text_route_error" {
		entry := s.statuses[file]
		entry.Human = true
		s.statuses[file] = entry
	}
}

// reviewTextFiles routes each path through a bounded synthetic review. The text
// is presented to OCR under a supported extension, every window is validated,
// and only then are its findings mapped back to the original path. A file that
// cannot be validated stays pending with its reason.
func (r *Runner) reviewTextFiles(ctx context.Context, runID, profileID uint, reviewer *NativeReviewer, baseSHA, headSHA string, paths []string, budget int64, used tokenUsage, usageKnown bool, log *runLogSink) chunkSummary {
	summary := newChunkSummary()
	maxBytes := chunkByteBudget(reviewer.MaxTokens)
	stopBudget := false

	for _, file := range paths {
		if stopBudget {
			summary.markNotReviewed(file, coverageMethodText, "token_budget")
			log.Log(ctx, "worker", "", "warn", logPhaseChunks,
				fmt.Sprintf("%s no se revisó por la ruta de texto: se agotó el presupuesto de tokens", file))
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 1)
			continue
		}
		if ctx.Err() != nil {
			summary.markNotReviewed(file, coverageMethodText, "timeout")
			log.Log(ctx, "worker", "", "warn", logPhaseChunks,
				fmt.Sprintf("%s no se revisó por la ruta de texto: la revisión se agotó", file))
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 1)
			continue
		}

		baseContent := []byte(nil)
		if out, err := gitRaw(ctx, reviewer.RepoDir, "show base blob", "show", baseSHA+":"+file); err == nil {
			baseContent = out
		}
		headContent, mode, exists, err := blobAt(ctx, reviewer.RepoDir, headSHA, file)
		if err != nil || !exists {
			summary.markNotReviewed(file, coverageMethodText, "missing_blob")
			log.Log(ctx, "worker", "", "warn", logPhaseChunks,
				fmt.Sprintf("%s no se revisó por la ruta de texto: no hay blob en head", file))
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 1)
			continue
		}
		if bytes.IndexByte(headContent, 0) >= 0 {
			summary.statuses[file] = coverageFile{
				Path:   file,
				Status: coveragePending,
				Method: coverageMethodText,
				Reason: "binary",
				Human:  true,
			}
			log.Log(ctx, "worker", "", "warn", logPhaseChunks,
				fmt.Sprintf("%s parece binario; queda para revisión humana", file))
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 1)
			continue
		}

		reviewPath := textReviewPath(file)
		chunks, err := textRouteChunks(ctx, reviewer.RepoDir, baseSHA, headSHA, reviewPath, baseContent, headContent, mode, maxBytes)
		if err != nil {
			slog.Warn("text route diff failed", "run_id", runID, "path", file, "error", err)
			log.Log(ctx, "worker", "", "error", logPhaseChunks,
				fmt.Sprintf("no se pudo preparar la ruta de texto de %s: %v", file, err))
			summary.markTextPending(file, "text_route_error")
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 1)
			continue
		}
		if len(chunks) == 0 {
			// A rename without text changes has nothing to review.
			summary.statuses[file] = coverageFile{Path: file, Status: coverageReviewed, Method: coverageMethodText}
			log.Log(ctx, "worker", "", "info", logPhaseChunks,
				fmt.Sprintf("%s no cambia texto; se marca cubierto por la ruta de texto", file))
			slog.Info("text route file", "run_id", runID, "path", file, "chunks", 0, "failures", 0)
			continue
		}

		log.Log(ctx, "worker", "", "info", logPhaseChunks,
			fmt.Sprintf("revisando %s por la ruta de texto: %d ventana(s)", file, len(chunks)))
		lineCount := len(splitContent(headContent))
		failures := 0
		reason := ""
		var fileComments []nativeComment
		for i, ch := range chunks {
			invocation := fmt.Sprintf("text %d/%d", i+1, len(chunks))
			if ctx.Err() != nil {
				failures++
				if reason == "" {
					reason = "timeout"
				}
				log.Log(ctx, "worker", invocation, "warn", logPhaseChunks,
					fmt.Sprintf("ventana %d/%d de %s no ejecutada: la revisión se agotó", i+1, len(chunks), file))
				break
			}

			textReviewer := *reviewer
			textReviewer.Commit = ch.Commit
			// The analysis describes the pull request comparison, not a synthetic
			// text window: never request it again per window.
			textReviewer.Analysis = false
			textReviewer.AnalysisTokenBudget = 0
			textReviewer.AnalysisTimeout = 0
			// The original path already passed the team's exclude patterns; the
			// synthetic path is an implementation detail and must not be caught
			// by a language glob such as **/*.go.
			textReviewer.Excludes = ""
			textReviewer.OnLog = log.OCRLogger(ctx, invocation)
			// A text window is a synthetic commit, not the main pass: it must
			// not resume the main session or overwrite its stored id.
			textReviewer.OnSession = nil
			textReviewer.ResumeSession = ""
			if budget > 0 {
				remaining := chunkAllowance(budget, int64(used.Total)+int64(summary.tokens.Total), usageKnown && summary.usageKnown)
				if remaining <= 0 {
					failures++
					if reason == "" {
						reason = "token_budget"
					}
					stopBudget = true
					log.Log(ctx, "worker", invocation, "warn", logPhaseChunks,
						fmt.Sprintf("ventana %d/%d de %s no ejecutada: presupuesto de tokens agotado", i+1, len(chunks), file))
					break
				}
				textReviewer.TokenBudget = remaining
			}

			log.Log(ctx, "worker", invocation, "info", logPhaseChunks,
				fmt.Sprintf("iniciando ventana %d/%d de %s por la ruta de texto", i+1, len(chunks), file))
			started := time.Now()
			res, _, reviewErr := r.runNativeReview(ctx, profileID, &textReviewer, log, invocation)
			if reviewErr != nil {
				// The window may have spent tokens before failing.
				summary.usageKnown = false
				failures++
				if reason == "" {
					reason = chunkErrorReason(ctx, reviewErr)
				}
				log.Log(ctx, "worker", invocation, "error", logPhaseChunks,
					fmt.Sprintf("ventana %d/%d de %s falló en %s: %v", i+1, len(chunks), file,
						time.Since(started).Round(time.Second), reviewErr))
				continue
			}
			summary.tokens.add(nativeTokenUsage(res))
			if !res.hasSummary {
				summary.usageKnown = false
			}
			invalidReason, valid := textRouteValid(res, reviewPath, lineCount)
			if !valid {
				failures++
				if reason == "" {
					reason = invalidReason
				}
				log.Log(ctx, "worker", invocation, "warn", logPhaseChunks,
					fmt.Sprintf("ventana %d/%d de %s no es válida (%s); se descarta", i+1, len(chunks), file, invalidReason))
				continue
			}
			for _, comment := range res.Comments {
				comment.Path = file
				fileComments = append(fileComments, comment)
			}
			log.Log(ctx, "worker", invocation, "info", logPhaseChunks,
				fmt.Sprintf("ventana %d/%d de %s terminó en %s con %d hallazgo(s)", i+1, len(chunks), file,
					time.Since(started).Round(time.Second), len(res.Comments)))
		}

		if failures == 0 {
			summary.comments = append(summary.comments, fileComments...)
			status := coverageReviewed
			if len(chunks) > 1 {
				status = coverageReviewedByParts
			}
			summary.statuses[file] = coverageFile{
				Path:   file,
				Status: status,
				Method: coverageMethodText,
				Chunks: len(chunks),
			}
			log.Log(ctx, "worker", "", "info", logPhaseChunks,
				fmt.Sprintf("%s revisado por la ruta de texto en %d ventana(s)", file, len(chunks)))
		} else {
			if reason == "" {
				reason = "provider"
			}
			summary.markTextPending(file, reason)
			log.Log(ctx, "worker", "", "warn", logPhaseChunks,
				fmt.Sprintf("%s quedó pendiente por la ruta de texto (%s): %d ventana(s) fallaron", file, reason, failures))
		}
		slog.Info("text route file",
			"run_id", runID, "path", file, "chunks", len(chunks), "failures", failures)
	}
	return summary
}

// textRouteChunks builds the synthetic commit pair for one unsupported text
// file and splits its change into the same bounded windows as a chunked file.
func textRouteChunks(ctx context.Context, dir, baseSHA, headSHA, reviewPath string, baseContent, headContent []byte, mode string, maxBytes int) ([]chunk, error) {
	headTree := headSHA + "^{tree}"
	parentTree, err := treeWithBlob(ctx, dir, headTree, reviewPath, mode, baseContent)
	if err != nil {
		return nil, err
	}
	chunkTree, err := treeWithBlob(ctx, dir, headTree, reviewPath, mode, headContent)
	if err != nil {
		return nil, err
	}
	fullParent, fullChunk, err := commitTreePair(ctx, dir, baseSHA, parentTree, chunkTree)
	if err != nil {
		return nil, err
	}
	raw, err := gitRaw(ctx, dir, "diff text route", "diff", "-U0", "--no-color", "--no-renames",
		fullParent, fullChunk, "--", reviewPath)
	if err != nil {
		return nil, err
	}
	hunks, err := parseFileDiff(raw)
	if err != nil {
		return nil, err
	}
	groups := groupHunks(windowHunks(hunks, maxBytes), maxBytes)
	chunks := make([]chunk, 0, len(groups))
	for _, group := range groups {
		parentContent, err := revertHunks(headContent, group)
		if err != nil {
			return nil, fmt.Errorf("revertir ventana de %s: %w", reviewPath, err)
		}
		tree, err := treeWithBlob(ctx, dir, headTree, reviewPath, mode, parentContent)
		if err != nil {
			return nil, err
		}
		parent, commit, err := commitTreePair(ctx, dir, baseSHA, tree, chunkTree)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk{Parent: parent, Commit: commit, Hunks: group})
	}
	return chunks, nil
}

// textRouteValid enforces the quality contract of the second route: the engine
// must report the synthetic path completed, every comment must belong to it and
// fit the original file, and no failure or exclusion may contradict that.
func textRouteValid(res nativeReview, reviewPath string, lineCount int) (string, bool) {
	status, code := nativeOutcome(res)
	if status != store.RunSucceeded {
		if code == "" {
			code = "provider"
		}
		return code, false
	}
	coverage := res.Manifest.Coverage
	completed := false
	for _, group := range [][]nativeCoverageItem{coverage.Completed, coverage.Reused} {
		for _, item := range group {
			if normalizeCoveragePath(item.Path) == reviewPath {
				completed = true
			}
		}
	}
	if !completed {
		return "text_route_invalid", false
	}
	for _, group := range [][]nativeCoverageItem{coverage.Failed, coverage.Waived, coverage.Excluded} {
		for _, item := range group {
			if normalizeCoveragePath(item.Path) == reviewPath {
				return "text_route_invalid", false
			}
		}
	}
	for _, comment := range res.Comments {
		if normalizeCoveragePath(comment.Path) != reviewPath {
			return "text_route_invalid", false
		}
		if strings.TrimSpace(comment.Content) == "" {
			return "text_route_invalid", false
		}
		if comment.StartLine < 1 || comment.EndLine < comment.StartLine || comment.EndLine > lineCount {
			return "text_route_invalid", false
		}
	}
	return "", true
}
