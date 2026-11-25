package review

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	changedAdded     = "added"
	changedModified  = "modified"
	changedDeleted   = "deleted"
	changedBinary    = "binary"
	changedSubmodule = "submodule"
)

// changedFile is one entry of the diff inventory for the reviewed base..head.
// Kind distinguishes the entries that are not reviewable by text (deletions,
// binaries and submodules) from the ordinary changed paths.
type changedFile struct {
	Path string
	// OldPath is the source of a rename or copy. It lets the dependency check
	// compare the head imports with the file's actual base content instead of
	// treating every import as new.
	OldPath string
	Kind    string
	// Renamed distinguishes a move from an in-place edit. It is a structural
	// change signal for the architectural analysis.
	Renamed bool
}

// gitChangedFiles inventories every path the reviewed range changed. It reads
// the raw diff to classify deletions and submodules and the numstat diff to
// identify binaries, which OCR cannot review either.
func gitChangedFiles(ctx context.Context, dir, baseRef, headRef string) ([]changedFile, error) {
	raw, err := git(ctx, dir, "diff --raw", "--no-pager", "diff", "--raw", "-z", baseRef, headRef)
	if err != nil {
		return nil, err
	}
	binaries, err := gitBinaryPaths(ctx, dir, baseRef, headRef)
	if err != nil {
		return nil, err
	}

	files := parseRawDiff(raw)
	for i := range files {
		files[i].Path = normalizeCoveragePath(files[i].Path)
		files[i].OldPath = normalizeCoveragePath(files[i].OldPath)
		if files[i].Path == "" {
			continue
		}
		if files[i].Kind != changedDeleted && binaries[files[i].Path] {
			files[i].Kind = changedBinary
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// parseRawDiff reads `git diff --raw -z`. Each entry is a NUL-terminated
// metadata field followed by one path, or two for a rename or copy.
func parseRawDiff(raw string) []changedFile {
	tokens := strings.Split(raw, "\x00")
	files := make([]changedFile, 0, len(tokens)/2)
	for i := 0; i < len(tokens); {
		meta := tokens[i]
		i++
		if meta == "" {
			continue
		}
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		fields := strings.Fields(meta[1:])
		if len(fields) < 5 {
			continue
		}
		status := fields[4]
		newMode := fields[1]
		switch status[0] {
		case 'D':
			if i >= len(tokens) {
				return files
			}
			files = append(files, changedFile{Path: tokens[i], Kind: changedDeleted})
			i++
		case 'R', 'C':
			if i+1 >= len(tokens) {
				return files
			}
			kind := changedModified
			if newMode == "160000" {
				kind = changedSubmodule
			}
			files = append(files, changedFile{
				Path: tokens[i+1], OldPath: tokens[i], Kind: kind, Renamed: status[0] == 'R',
			})
			i += 2
		default:
			if i >= len(tokens) {
				return files
			}
			kind := changedModified
			if status[0] == 'A' {
				kind = changedAdded
			}
			if newMode == "160000" {
				kind = changedSubmodule
			}
			files = append(files, changedFile{Path: tokens[i], Kind: kind})
			i++
		}
	}
	return files
}

// gitBinaryPaths reads `git diff --numstat -z` and returns the paths whose
// diff is binary ("-" added and removed lines).
func gitBinaryPaths(ctx context.Context, dir, baseRef, headRef string) (map[string]bool, error) {
	raw, err := git(ctx, dir, "diff --numstat", "--no-pager", "diff", "--numstat", "-z", baseRef, headRef)
	if err != nil {
		return nil, err
	}
	return parseNumstat(raw), nil
}

// parseNumstat classifies binary paths from `git diff --numstat -z`. A rename
// stores an empty path followed by the old and new ones, and its binary marker
// applies to the new path.
func parseNumstat(raw string) map[string]bool {
	binaries := make(map[string]bool)
	tokens := strings.Split(raw, "\x00")
	for i := 0; i < len(tokens); {
		field := tokens[i]
		i++
		if field == "" {
			continue
		}
		parts := strings.SplitN(field, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		binary := parts[0] == "-" && parts[1] == "-"
		if parts[2] == "" {
			if i+1 >= len(tokens) {
				break
			}
			newPath := normalizeCoveragePath(tokens[i+1])
			i += 2
			if binary && newPath != "" {
				binaries[newPath] = true
			}
			continue
		}
		if binary {
			if p := normalizeCoveragePath(parts[2]); p != "" {
				binaries[p] = true
			}
		}
	}
	return binaries
}

// inventoryCoverage confronts the diff inventory with the per-file coverage of
// the main and chunked passes. A required changed file without completed
// coverage becomes pending; OCR-internal exclusions, failures and waivers are
// not coverage. Deletions, binaries, submodules and files matched by the team
// exclude_globs are recorded but not required.
func inventoryCoverage(files []coverageFile, changed []changedFile, patterns []string, signal coverageSignal) ([]coverageFile, []coverageFile) {
	index := make(map[string]int, len(files))
	for i, file := range files {
		index[file.Path] = i
	}
	var pending []coverageFile
	for _, entry := range changed {
		p := normalizeCoveragePath(entry.Path)
		if p == "" {
			continue
		}
		switch entry.Kind {
		case changedDeleted, changedBinary, changedSubmodule:
			// Binaries and submodules have no text an engine can verify. They
			// are recorded with a human flag instead of passing silently, but
			// stay outside the required set because no pass can cover them.
			human := entry.Kind == changedBinary || entry.Kind == changedSubmodule
			files = setCoverageFile(files, index, coverageFile{
				Path:   p,
				Status: entry.Kind,
				Reason: entry.Kind,
				Human:  human,
			})
			continue
		}
		if policyExcludesPath(patterns, p) {
			if i, ok := index[p]; ok {
				if files[i].Status == coverageExcluded && strings.TrimSpace(files[i].Reason) == "" {
					files[i].Reason = "policy"
				}
			} else {
				files = append(files, coverageFile{Path: p, Status: coverageExcluded, Reason: "policy"})
				index[p] = len(files) - 1
			}
			continue
		}
		if i, ok := index[p]; ok {
			switch files[i].Status {
			case coverageReviewed, coverageReviewedByParts:
				continue
			}
			reason := strings.TrimSpace(files[i].Reason)
			if reason == "" {
				reason = missingCoverageReason(files[i].Status, p, signal)
			}
			file := coverageFile{
				Path:   p,
				Status: coveragePending,
				Method: files[i].Method,
				Reason: reason,
				Human:  files[i].Human,
			}
			files[i] = file
			pending = append(pending, file)
			continue
		}
		file := coverageFile{Path: p, Status: coveragePending, Reason: missingCoverageReason("", p, signal)}
		files = append(files, file)
		index[p] = len(files) - 1
		pending = append(pending, file)
	}
	return files, pending
}

func setCoverageFile(files []coverageFile, index map[string]int, file coverageFile) []coverageFile {
	if i, ok := index[file.Path]; ok {
		files[i] = file
		return files
	}
	index[file.Path] = len(files)
	return append(files, file)
}

func missingCoverageReason(status, path string, signal coverageSignal) string {
	switch {
	case status == coverageExcluded:
		return "excluded"
	case !signal.present:
		return "manifest_missing"
	case signal.selected[path]:
		return "not_completed"
	default:
		return "not_reported"
	}
}

// coverageSignal describes what the engine manifest can prove. Selected counts
// as manifest presence, and a path that only appears there was dispatched but
// never reached a terminal group.
type coverageSignal struct {
	present  bool
	selected map[string]bool
}

func coverageSignalFrom(res nativeReview) coverageSignal {
	coverage := res.Manifest.Coverage
	signal := coverageSignal{selected: make(map[string]bool, len(coverage.Selected))}
	for _, item := range coverage.Selected {
		if path := normalizeCoveragePath(item.Path); path != "" {
			signal.selected[path] = true
		}
	}
	signal.present = len(coverage.Selected)+len(coverage.Completed)+len(coverage.Reused)+
		len(coverage.Failed)+len(coverage.Waived)+len(coverage.Excluded) > 0
	return signal
}

// policyExcludesPath matches a path against the team exclude_globs. A pattern
// without a slash also matches the basename, so "*.md" excludes a nested
// Markdown file as teams expect.
func policyExcludesPath(patterns []string, file string) bool {
	file = normalizeCoveragePath(file)
	if file == "" {
		return false
	}
	base := path.Base(file)
	for _, pattern := range patterns {
		p := strings.TrimSpace(strings.ReplaceAll(pattern, `\`, "/"))
		if p == "" {
			continue
		}
		if globMatch(p, file) {
			return true
		}
		if !strings.Contains(p, "/") && globMatch(p, base) {
			return true
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	re, err := regexp.Compile(globRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// globRegexp translates the glob subset used by exclude_globs: ** crosses
// directories, * and ? stay inside one segment.
func globRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := i + 1
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end < len(pattern) {
				b.WriteString(pattern[i : end+1])
				i = end
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return b.String()
}

// coverageNotRequiredFromFiles counts deletions, binaries and submodules. They
// are outside the reviewable set but still part of the diff totals, so a
// deletion-only pull request cannot report an empty coverage.
func coverageNotRequiredFromFiles(files []coverageFile) int {
	notRequired := 0
	for _, file := range files {
		switch file.Status {
		case changedDeleted, changedBinary, changedSubmodule:
			notRequired++
		}
	}
	return notRequired
}

// pendingRunStatus escalates a clean run when any required changed file lacks
// coverage. A run that already failed or was inconclusive keeps its more
// specific code.
func pendingRunStatus(status, code string, pending []coverageFile) (string, string) {
	if len(pending) > 0 && status == store.RunSucceeded {
		return store.RunInconclusive, "coverage_incomplete"
	}
	return status, code
}

// settledRunStatus reconciles the main pass result with the routes that ran
// afterwards. The inventory is the authority: when a second route reviewed
// every file the main pass left uncovered and no required file is pending, a
// coverage-only inconclusive result becomes succeeded. Without an inventory
// nothing can prove the gap is closed, so the main result stands.
func settledRunStatus(status, code string, pending []coverageFile, routed chunkSummary, inventoryOK bool) (string, string) {
	status, code = pendingRunStatus(status, code, pending)
	if inventoryOK && len(pending) == 0 && len(routed.statuses) > 0 && routed.allReviewed() &&
		status == store.RunInconclusive && (code == "coverage_incomplete" || code == "no_coverage") {
		return store.RunSucceeded, ""
	}
	return status, code
}
