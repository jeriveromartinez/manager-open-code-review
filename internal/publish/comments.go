package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// addedLines maps a file path to the line numbers added by the pull request.
type addedLines map[string]map[int]bool

func (a addedLines) has(path string, line int) bool {
	return a[path][line]
}

// fetchAddedLines reads the PR files and parses unified-diff hunk headers. Only
// lines with a '+' prefix (excluding the +++ file header) are considered added.
func (p *publishContext) fetchAddedLines(ctx context.Context) (addedLines, error) {
	result := addedLines{}
	base := p.repoPath(fmt.Sprintf("pulls/%d/files", p.pr.Number))
	for page := 1; ; page++ {
		var files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		path := fmt.Sprintf("%s?per_page=100&page=%d", base, page)
		if err := p.request(ctx, http.MethodGet, path, nil, &files); err != nil {
			return nil, fmt.Errorf("list pull request files: %w", err)
		}
		for _, file := range files {
			if result[file.Filename] == nil {
				result[file.Filename] = map[int]bool{}
			}
			parsePatch(result[file.Filename], file.Patch)
		}
		if len(files) < 100 {
			break
		}
	}
	return result, nil
}

func parsePatch(lines map[int]bool, patch string) {
	current, last := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			// Format: @@ -old_start,old_count +new_start,new_count @@
			parts := strings.SplitN(line, " ", 4)
			if len(parts) < 3 {
				current, last = 0, 0
				continue
			}
			header := strings.TrimPrefix(parts[2], "+")
			rangeParts := strings.Split(header, ",")
			start, err := strconv.Atoi(rangeParts[0])
			if err != nil {
				current, last = 0, 0
				continue
			}
			count := 1
			if len(rangeParts) > 1 {
				count, err = strconv.Atoi(rangeParts[1])
				if err != nil {
					count = 1
				}
			}
			current, last = start, start+count-1
			continue
		}
		if last == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			if current <= last {
				lines[current] = true
			}
			current++
		case strings.HasPrefix(line, "-"):
			// Deleted lines do not advance the new-file counter.
		case strings.HasPrefix(line, " "), strings.HasPrefix(line, "\\"):
			// Context lines advance it; "\ No newline" does not, but it never
			// appears before another hunk line in practice.
			if !strings.HasPrefix(line, "\\") {
				current++
			}
		}
	}
}

type reviewCommentPayload struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

type postedReviewComment struct {
	ID           int64  `json:"id"`
	NodeID       string `json:"node_id"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	StartLine    int    `json:"start_line"`
}

// inlineReviewMarker identifies the review a run posted. Listing the pull
// request's reviews before posting lets a retry recover the review when the
// first attempt was created on GitHub but failed before its coordinates were
// stored locally.
func inlineReviewMarker(runID uint) string {
	return fmt.Sprintf("<!-- qa-control-center:inline %d -->", runID)
}

type postedReview struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// findInlineReview returns the review created by this run, if any.
func (p *publishContext) findInlineReview(ctx context.Context, marker string) (int64, error) {
	base := p.repoPath(fmt.Sprintf("pulls/%d/reviews", p.pr.Number))
	for page := 1; ; page++ {
		var reviews []postedReview
		path := fmt.Sprintf("%s?per_page=100&page=%d", base, page)
		if err := p.request(ctx, http.MethodGet, path, nil, &reviews); err != nil {
			return 0, fmt.Errorf("list pull request reviews: %w", err)
		}
		for _, review := range reviews {
			if strings.Contains(review.Body, marker) {
				return review.ID, nil
			}
		}
		if len(reviews) < 100 {
			return 0, nil
		}
	}
}

// errInlineRejected reports that GitHub refused the whole review. The findings
// are not lost: the caller publishes them in the summary instead.
var errInlineRejected = errors.New("inline review rejected")

type inlineCandidate struct {
	finding store.Finding
	path    string
	start   int
	end     int
}

// singleHeadAnchor resolves the one head-side range a finding can be pinned
// to. A finding that spans files, that only cites removed code or whose
// citations do not match its primary path has no single anchor (#162).
func singleHeadAnchor(f store.Finding) (string, int, int, bool) {
	citations := store.DecodeFindingCitations(f.Citations)
	if store.NormalizeFindingScope(f.Scope, citations) != store.FindingScopeFile {
		return "", 0, 0, false
	}
	paths := make(map[string]bool)
	hasHead := len(citations) == 0
	for _, citation := range citations {
		paths[citation.Path] = true
		if citation.Side != store.FindingCitationBase {
			hasHead = true
		}
	}
	if len(paths) > 1 || !hasHead {
		return "", 0, 0, false
	}
	path, start, end := f.Path, f.StartLine, f.EndLine
	if path == "" || start <= 0 {
		for _, citation := range citations {
			if citation.Side == store.FindingCitationBase {
				continue
			}
			path, start, end = citation.Path, citation.StartLine, citation.EndLine
			break
		}
	}
	if path == "" || start <= 0 {
		return "", 0, 0, false
	}
	// The citation set is the finding's evidence; it must not be published on
	// a different file the producer happened to put in Path.
	if len(paths) == 1 && !paths[path] {
		return "", 0, 0, false
	}
	if end < start {
		end = start
	}
	return path, start, end, true
}

// inlineAnchor is a singleHeadAnchor that also has to exist in the added lines
// of the current comparison.
func (p *publishContext) inlineAnchor(f store.Finding, added addedLines) (string, int, int, bool) {
	path, start, end, ok := singleHeadAnchor(f)
	if !ok || !added.has(path, start) {
		return "", 0, 0, false
	}
	return path, start, end, true
}

// publishInlineComments posts one COMMENT review with only findings that do
// not have a GitHub comment yet and whose anchor is one added line of the
// current comparison. A run posts at most one review: a retry finds it by
// marker instead of creating a duplicate. A rejected review returns
// errInlineRejected so the caller can deliver the findings in the summary.
func (p *publishContext) publishInlineComments(ctx context.Context, added addedLines, findings []store.Finding) error {
	candidates := make([]inlineCandidate, 0)
	for _, f := range findings {
		if f.Status == store.FindingWithdrawn || f.GithubCommentID != 0 || f.Advisory {
			continue
		}
		if !p.inlineAllowed(f) {
			continue
		}
		path, start, end, ok := p.inlineAnchor(f, added)
		if !ok {
			continue
		}
		candidates = append(candidates, inlineCandidate{finding: f, path: path, start: start, end: end})
	}
	marker := inlineReviewMarker(p.run.ID)
	reviewID, err := p.findInlineReview(ctx, marker)
	if err != nil {
		return err
	}
	if len(candidates) == 0 && reviewID == 0 {
		return nil
	}

	comments := make([]reviewCommentPayload, 0, len(candidates))
	for _, candidate := range candidates {
		f := candidate.finding
		body := p.sanitize(f.Title)
		if f.Body != "" {
			body += "\n\n" + p.sanitize(f.Body)
		}
		if citations := store.DecodeFindingCitations(f.Citations); len(citations) > 0 {
			body += "\n\nCitas: " + p.sanitize(p.citationLocations(citations))
		}
		if f.Suggestion != "" {
			if wholeRangeAdded(added, candidate.path, candidate.start, candidate.end) {
				body += "\n\n```suggestion\n" + p.sanitize(f.Suggestion) + "\n```"
			} else {
				body += "\n\nSugerencia: " + p.sanitize(f.Suggestion)
			}
		}
		comment := reviewCommentPayload{
			Path: candidate.path, Line: candidate.end, Side: "RIGHT", Body: body,
		}
		if candidate.end != candidate.start {
			comment.StartLine = candidate.start
			comment.StartSide = "RIGHT"
		}
		comments = append(comments, comment)
	}

	if reviewID == 0 {
		var created struct {
			ID int64 `json:"id"`
		}
		payload := map[string]any{
			// Pin the review to the commit the run reviewed. Without it GitHub
			// resolves the inline comments against whatever head is current when
			// the request lands, which may be a commit the run never saw (#138).
			"commit_id": p.run.HeadSHA,
			"event":     "COMMENT",
			"body":      p.sanitize("Comentarios automáticos de QA Control Center") + "\n\n" + marker,
			"comments":  comments,
		}
		path := p.repoPath(fmt.Sprintf("pulls/%d/reviews", p.pr.Number))
		if err := p.request(ctx, http.MethodPost, path, payload, &created); err != nil {
			if isRejectedResponse(err) {
				return fmt.Errorf("%w: %v", errInlineRejected, err)
			}
			return fmt.Errorf("create review: %w", err)
		}
		if created.ID == 0 {
			return fmt.Errorf("%w: GitHub returned no review id", errInlineRejected)
		}
		reviewID = created.ID
	}

	// The review object does not include its comments; fetch them so each
	// finding can be linked to its comment and, later, to its thread.
	var posted []postedReviewComment
	listPath := p.repoPath(fmt.Sprintf("pulls/%d/reviews/%d/comments", p.pr.Number, reviewID))
	for page := 1; ; page++ {
		var batch []postedReviewComment
		path := fmt.Sprintf("%s?per_page=100&page=%d", listPath, page)
		if err := p.request(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return fmt.Errorf("list review comments: %w", err)
		}
		posted = append(posted, batch...)
		if len(batch) < 100 {
			break
		}
	}

	used := make([]bool, len(posted))
	for _, candidate := range candidates {
		f := candidate.finding
		match := findComment(posted, used, candidate.path, candidate.end)
		if match == nil {
			match = findComment(posted, used, candidate.path, candidate.start)
		}
		// GitHub omits `line` on some review comments it returns for a review
		// that was just created (the payload only carries `position`). The
		// comments come back in creation order, so pair the remaining candidate
		// with the first unconsumed comment on the same path instead of losing
		// its coordinates and, with them, the thread link resolutions need.
		if match == nil {
			match = findCommentByPath(posted, used, candidate.path)
		}
		// Never invent a GitHub id: a fake value would silently suppress
		// future publications. Coordinates stay unset when the API omits them.
		commentID, nodeID := int64(0), ""
		if match != nil {
			commentID, nodeID = match.ID, match.NodeID
		}
		if err := p.w.Store.SetFindingPublished(ctx, f.ID, commentID, nodeID, ""); err != nil {
			return err
		}
	}

	_, err = p.w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: p.pr.ID,
		Kind:          store.ArtifactReview,
		HeadSHA:       p.run.HeadSHA,
		ExternalID:    strconv.FormatInt(reviewID, 10),
		URL:           p.pr.HTMLURL,
	})
	if err != nil {
		return fmt.Errorf("save review artifact: %w", err)
	}
	return nil
}

func wholeRangeAdded(added addedLines, path string, start, end int) bool {
	for line := start; line <= end; line++ {
		if !added.has(path, line) {
			return false
		}
	}
	return true
}

// findComment looks for an unconsumed comment that reports one of the finding's
// lines. GitHub may expose the line under `line`, `original_line` or
// `start_line` depending on whether the comment is a range and whether it is
// still anchored to the current diff.
func findComment(comments []postedReviewComment, used []bool, path string, line int) *postedReviewComment {
	if line <= 0 {
		return nil
	}
	for i := range comments {
		if used[i] || comments[i].Path != path {
			continue
		}
		if comments[i].Line == line || comments[i].OriginalLine == line || comments[i].StartLine == line {
			used[i] = true
			return &comments[i]
		}
	}
	return nil
}

// findCommentByPath returns the first unconsumed comment on path. It is the
// positional fallback for responses that omit every line field.
func findCommentByPath(comments []postedReviewComment, used []bool, path string) *postedReviewComment {
	for i := range comments {
		if used[i] || comments[i].Path != path {
			continue
		}
		used[i] = true
		return &comments[i]
	}
	return nil
}
