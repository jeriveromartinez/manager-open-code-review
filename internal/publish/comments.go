package publish

import (
	"context"
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

// publishInlineComments posts one COMMENT review with only findings that do
// not have a GitHub comment yet and whose start line is in a diff hunk.
func (p *publishContext) publishInlineComments(ctx context.Context, added addedLines, findings []store.Finding) error {
	candidates := make([]store.Finding, 0)
	for _, f := range findings {
		if f.Status == store.FindingWithdrawn || f.GithubCommentID != 0 || f.StartLine <= 0 {
			continue
		}
		if added.has(f.Path, f.StartLine) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	comments := make([]reviewCommentPayload, 0, len(candidates))
	for _, f := range candidates {
		body := p.sanitize(f.Title)
		if f.Body != "" {
			body += "\n\n" + p.sanitize(f.Body)
		}
		end := f.EndLine
		if end < f.StartLine {
			end = f.StartLine
		}
		if f.Suggestion != "" {
			if wholeRangeAdded(added, f.Path, f.StartLine, end) {
				body += "\n\n```suggestion\n" + p.sanitize(f.Suggestion) + "\n```"
			} else {
				body += "\n\nSugerencia: " + p.sanitize(f.Suggestion)
			}
		}
		comment := reviewCommentPayload{
			Path: f.Path, Line: end, Side: "RIGHT", Body: body,
		}
		if end != f.StartLine {
			comment.StartLine = f.StartLine
			comment.StartSide = "RIGHT"
		}
		comments = append(comments, comment)
	}

	var created struct {
		ID int64 `json:"id"`
	}
	payload := map[string]any{
		"event":    "COMMENT",
		"body":     p.sanitize("Comentarios automáticos de QA Control Center"),
		"comments": comments,
	}
	path := p.repoPath(fmt.Sprintf("pulls/%d/reviews", p.pr.Number))
	if err := p.request(ctx, http.MethodPost, path, payload, &created); err != nil {
		return fmt.Errorf("create review: %w", err)
	}
	if created.ID == 0 {
		return fmt.Errorf("create review: GitHub returned no review id")
	}

	// The review object does not include its comments; fetch them so each
	// finding can be linked to its comment and, later, to its thread.
	var posted []postedReviewComment
	listPath := p.repoPath(fmt.Sprintf("pulls/%d/reviews/%d/comments", p.pr.Number, created.ID))
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
	for _, f := range candidates {
		end := f.EndLine
		if end < f.StartLine {
			end = f.StartLine
		}
		match := findComment(posted, used, f.Path, end)
		if match == nil {
			match = findComment(posted, used, f.Path, f.StartLine)
		}
		// GitHub omits `line` on some review comments it returns for a review
		// that was just created (the payload only carries `position`). The
		// comments come back in creation order, so pair the remaining candidate
		// with the first unconsumed comment on the same path instead of losing
		// its coordinates and, with them, the thread link resolutions need.
		if match == nil {
			match = findCommentByPath(posted, used, f.Path)
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

	_, err := p.w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: p.pr.ID,
		Kind:          store.ArtifactReview,
		HeadSHA:       p.run.HeadSHA,
		ExternalID:    strconv.FormatInt(created.ID, 10),
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
