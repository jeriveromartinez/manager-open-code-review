package review

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// architectureRuleSchema identifies the rule set an architectural verdict was
// checked against. A change to the activation or extraction rules must bump it:
// evidence bound to another schema is stale by construction (#163).
const architectureRuleSchema = "architecture-rules/v1"

// ArchitectureSourceDigest returns the content digest of a path at the reviewed
// head. A missing path (deleted or unreadable) reports false so the evidence
// that cited it is treated as invalidated.
type ArchitectureSourceDigest func(path string) (string, bool)

// ArchitectureRevalidation is the result of confronting a stored verdict with a
// later head. Verdict keeps every dimension, marking the invalidated ones stale
// instead of dropping earlier evidence. NeedsFullReview asks the runner to
// analyse the accumulated range from the base again.
type ArchitectureRevalidation struct {
	Verdict         store.ArchitectureVerdict
	NeedsFullReview bool
	Reasons         []string
}

// architectureRuleVersion derives a deterministic version from the rules that
// decide whether an analysis runs, what it may exclude and which path tables
// classify the sources. Two policies that review the same rules produce the
// same version; a rule change produces a new one and invalidates evidence bound
// to the previous value.
func architectureRuleVersion(policy store.QAPolicy, excludePatterns []string) string {
	patterns := append([]string(nil), excludePatterns...)
	sort.Strings(patterns)
	parts := []string{
		architectureRuleSchema,
		architectureRuleTablesFingerprint(),
		architectureMode(policy),
		strings.Join(patterns, "\n"),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return architectureRuleSchema + ":" + hex.EncodeToString(sum[:])[:16]
}

// architectureRuleTablesFingerprint digests the tables that decide signals and
// source kinds. Editing a table without bumping the schema must still change
// the rule version, otherwise evidence checked under the old classification
// would look current.
func architectureRuleTablesFingerprint() string {
	h := sha256.New()
	write := func(values ...string) {
		for _, value := range values {
			h.Write([]byte(value))
			h.Write([]byte{0})
		}
	}
	writeGroup := func() { h.Write([]byte{0xff}) }

	writeSlices := func(groups ...[]string) {
		for _, group := range groups {
			values := append([]string(nil), group...)
			sort.Strings(values)
			write(values...)
			writeGroup()
		}
	}
	writeMaps := func(maps ...map[string]bool) {
		for _, table := range maps {
			keys := make([]string, 0, len(table))
			for key := range table {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			write(keys...)
			writeGroup()
		}
	}

	writeSlices(
		architectureInterfaceDirs, architecturePersistenceDirs,
		architecturePermissionDirs, architectureTopologyDirs, architectureConfigDirs,
		architectureBehaviorHeadings, architectureRiskHeadings,
		architectureAlternativeHeadings, architectureAcceptanceHeadings,
		architectureUncertaintyHeadings, architectureRecommendationHeadings,
	)
	writeMaps(
		architectureDependencyFiles, architectureContainerRoots,
		architectureNonCodeRoots, architectureCodeExtensions,
	)
	write(
		strconv.Itoa(architectureFilesThreshold),
		strconv.Itoa(architectureLinesThreshold),
		architectureSignalChangeSize, architectureSignalType, architectureSignalModule,
		architectureSignalInterface, architectureSignalPersistence, architectureSignalPermissions,
		architectureSignalTopology, architectureSignalConfig, architectureSignalDeps,
	)
	return hex.EncodeToString(h.Sum(nil))
}

// checkoutSourceDigests adapts the checked-out tree to an
// ArchitectureSourceDigest. Line endings are normalised so a Windows and a
// Linux checkout of the same commit bind the same digest.
func checkoutSourceDigests(workDir string) ArchitectureSourceDigest {
	return func(path string) (string, bool) {
		p := normalizeCoveragePath(path)
		if p == "" {
			return "", false
		}
		content, err := readRepoFile(workDir, p)
		if err != nil {
			return "", false
		}
		normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
		sum := sha256.Sum256([]byte(normalized))
		return hex.EncodeToString(sum[:]), true
	}
}

var (
	architectureInlineCodePattern = regexp.MustCompile("`([^`\n]+)`")
	architecturePathTokenPattern  = regexp.MustCompile(`(?:^|[\s(])((?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_.-]+\.[A-Za-z0-9]+)`)
	// architectureLineSuffixPattern strips the citation location OCR appends
	// to a path (`file.go:42` or `file.go:42:7`) before it becomes a source.
	architectureLineSuffixPattern = regexp.MustCompile(`:\d+(?::\d+)?$`)
)

// extractArchitecturePaths reads the repository paths a dimension cited, from
// inline code spans and from plain path tokens. The extraction is deliberately
// conservative: URLs, absolute paths, SHAs and prose never become sources.
func extractArchitecturePaths(text string) []string {
	seen := make(map[string]bool)
	paths := make([]string, 0)
	add := func(raw string) {
		p := architectureSourcePath(raw)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	for _, match := range architectureInlineCodePattern.FindAllStringSubmatch(text, -1) {
		add(match[1])
	}
	for _, match := range architecturePathTokenPattern.FindAllStringSubmatch(text, -1) {
		add(match[1])
	}
	sort.Strings(paths)
	return paths
}

func architectureSourcePath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = architectureLineSuffixPattern.ReplaceAllString(raw, "")
	raw = strings.Trim(raw, "`'\"()[]{}<>,.;:")
	if raw == "" || strings.Contains(raw, "://") ||
		strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~") ||
		strings.ContainsAny(raw, " \t") {
		return ""
	}
	p := normalizeCoveragePath(raw)
	if p == "" {
		return ""
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return ""
		}
	}
	base := strings.ToLower(baseName(p))
	if architectureDependencyFiles[base] {
		return p
	}
	if !strings.Contains(p, "/") || fileExtension(p) == "" {
		return ""
	}
	if isCommitSHA(p) {
		return ""
	}
	return p
}

func isCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// architectureSourceKind labels a source so the panel and #158's baseline rules
// can be told apart without changing the binding model.
func architectureSourceKind(path string) string {
	p := normalizeCoveragePath(path)
	base := strings.ToLower(baseName(p))
	switch {
	case architectureDependencyFiles[base]:
		return "dependency"
	case strings.HasSuffix(base, ".sql") || matchesArchitecturePath(p, architecturePersistenceDirs):
		return "schema"
	case matchesArchitecturePath(p, architectureInterfaceDirs) ||
		strings.HasSuffix(base, ".proto") || strings.HasSuffix(base, ".graphql") ||
		strings.HasSuffix(base, ".gql") || strings.Contains(base, "openapi") ||
		strings.Contains(base, "swagger"):
		return "contract"
	case matchesArchitecturePath(p, architecturePermissionDirs):
		return "permission"
	case matchesArchitecturePath(p, architectureTopologyDirs) ||
		strings.HasPrefix(strings.ToLower(p), ".github/workflows/") ||
		strings.HasPrefix(base, "dockerfile") || strings.HasPrefix(base, "docker-compose") ||
		strings.HasPrefix(base, "compose."):
		return "topology"
	case matchesArchitecturePath(p, architectureConfigDirs) ||
		strings.HasPrefix(base, "config.") || strings.HasPrefix(base, "settings.") ||
		strings.HasPrefix(base, ".env") || strings.HasPrefix(base, "application.") ||
		strings.HasPrefix(base, "values."):
		return "configuration"
	default:
		return "cited"
	}
}

func architectureSources(paths []string, digests ArchitectureSourceDigest) []store.ArchitectureEvidenceSource {
	seen := make(map[string]bool, len(paths))
	sources := make([]store.ArchitectureEvidenceSource, 0, len(paths))
	for _, raw := range paths {
		p := normalizeCoveragePath(raw)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		source := store.ArchitectureEvidenceSource{Path: p, Kind: architectureSourceKind(p)}
		if digests != nil {
			source.Digest, _ = digests(p)
		}
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources
}

func architectureScopePaths(verdict store.ArchitectureVerdict) []string {
	paths := make([]string, 0, len(verdict.Scope.Included))
	for _, file := range verdict.Scope.Included {
		if p := normalizeCoveragePath(file.Path); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		for _, file := range verdict.Evidence.EngineScope {
			if p := normalizeCoveragePath(file.Path); p != "" {
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func unionArchitectureSources(groups ...[]store.ArchitectureEvidenceSource) []store.ArchitectureEvidenceSource {
	byPath := make(map[string]store.ArchitectureEvidenceSource)
	for _, group := range groups {
		for _, source := range group {
			existing, ok := byPath[source.Path]
			if !ok || (existing.Digest == "" && source.Digest != "") {
				byPath[source.Path] = source
			}
		}
	}
	sources := make([]store.ArchitectureEvidenceSource, 0, len(byPath))
	for _, source := range byPath {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources
}

// bindArchitectureEvidence ties a fresh verdict to the rule version and to the
// files each dimension relied on. A dimension that cites paths is bound to
// those paths only. A dimension that cites none is bound to the analyzed scope:
// its conclusion came from that range, so an edit inside the range invalidates
// it, while a push outside the range keeps the verdict and the incremental
// path. That breadth is the intended cost of prose-only evidence.
func bindArchitectureEvidence(verdict store.ArchitectureVerdict, ruleVersion string, digests ArchitectureSourceDigest) store.ArchitectureVerdict {
	verdict.Evidence.RuleVersion = ruleVersion
	scopePaths := architectureScopePaths(verdict)
	dimensions := make([]store.ArchitectureDimension, len(verdict.Dimensions))
	for i, dimension := range verdict.Dimensions {
		dimensions[i] = dimension
		paths := extractArchitecturePaths(dimension.Content)
		if len(paths) == 0 {
			paths = scopePaths
		}
		dimensions[i].Sources = architectureSources(paths, digests)
		dimensions[i].Stale = false
		dimensions[i].StaleReason = ""
	}
	verdict.Dimensions = dimensions
	verdict.Evidence.Sources = unionArchitectureSources(dimensionSources(dimensions)...)
	return verdict
}

func dimensionSources(dimensions []store.ArchitectureDimension) [][]store.ArchitectureEvidenceSource {
	groups := make([][]store.ArchitectureEvidenceSource, 0, len(dimensions))
	for _, dimension := range dimensions {
		groups = append(groups, dimension.Sources)
	}
	return groups
}

// revalidateArchitectureEvidence confronts a stored verdict with a later head.
// A comparison change, a rule-version change, a changed cited source or a
// digest mismatch marks the affected dimensions stale. Earlier evidence is kept
// and can be carried forward; nothing here ever promotes stale evidence to
// complete.
func revalidateArchitectureEvidence(prev store.ArchitectureVerdict, ruleVersion string, changedPaths []string,
	comparisonChanged bool, digests ArchitectureSourceDigest) ArchitectureRevalidation {
	reval := ArchitectureRevalidation{Verdict: cloneArchitectureVerdict(prev)}
	ruleChanged := strings.TrimSpace(prev.Evidence.RuleVersion) != ruleVersion
	if comparisonChanged {
		reval.Reasons = appendUnique(reval.Reasons, "comparison_changed")
	}
	// An unbound verdict (no rule version and no dimensions) holds no evidence
	// to invalidate: treating it as a rule change would force every later push
	// to discard the cheap incremental path.
	if ruleChanged && (strings.TrimSpace(prev.Evidence.RuleVersion) != "" || len(prev.Dimensions) > 0) {
		reval.Reasons = appendUnique(reval.Reasons, "rule_version_changed")
	}
	if len(prev.Dimensions) == 0 {
		reval.NeedsFullReview = len(reval.Reasons) > 0
		return reval
	}

	changed := make(map[string]bool, len(changedPaths))
	for _, path := range changedPaths {
		if p := normalizeCoveragePath(path); p != "" {
			changed[p] = true
		}
	}

	dimensions := reval.Verdict.Dimensions
	anyStale := false
	for i := range dimensions {
		reasons := make([]string, 0, 2)
		if comparisonChanged {
			reasons = appendUnique(reasons, "comparison_changed")
		}
		if ruleChanged {
			reasons = appendUnique(reasons, "rule_version_changed")
		}
		if !comparisonChanged && !ruleChanged {
			sources := dimensions[i].Sources
			if len(sources) == 0 {
				// Evidence recorded before the binding existed: derive its
				// sources from the report and the analyzed scope.
				paths := extractArchitecturePaths(dimensions[i].Content)
				if len(paths) == 0 {
					paths = architectureScopePaths(reval.Verdict)
				}
				sources = architectureSources(paths, nil)
				dimensions[i].Sources = sources
			}
			for j := range sources {
				p := normalizeCoveragePath(sources[j].Path)
				if p != "" && changed[p] {
					reasons = appendUnique(reasons, "source_changed:"+p)
					continue
				}
				if digests == nil || p == "" {
					continue
				}
				current, ok := digests(p)
				switch {
				case !ok && sources[j].Digest != "":
					// A source that was read when the evidence was recorded and
					// is gone now invalidates it.
					reasons = appendUnique(reasons, "source_missing:"+p)
				case ok && sources[j].Digest != "" && current != sources[j].Digest:
					reasons = appendUnique(reasons, "source_changed:"+p)
				case ok && sources[j].Digest == "":
					dimensions[i].Sources[j].Digest = current
				}
			}
		}
		if len(reasons) > 0 {
			dimensions[i].Stale = true
			dimensions[i].StaleReason = strings.Join(reasons, ", ")
			anyStale = true
		} else {
			dimensions[i].Stale = false
			dimensions[i].StaleReason = ""
		}
	}
	reval.Verdict.Dimensions = dimensions
	reval.Verdict.Evidence.Sources = unionArchitectureSources(prev.Evidence.Sources, unionArchitectureSources(dimensionSources(dimensions)...))
	if anyStale {
		reval.Verdict.Status = store.ArchitectureStale
		reval.NeedsFullReview = true
	}
	sort.Strings(reval.Reasons)
	return reval
}

func architectureChangedPaths(files []changedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if p := normalizeCoveragePath(file.Path); p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

func architectureEvidenceStale(verdict store.ArchitectureVerdict) bool {
	if verdict.Status == store.ArchitectureStale {
		return true
	}
	for _, dimension := range verdict.Dimensions {
		if dimension.Stale {
			return true
		}
	}
	return false
}

// finalizeArchitectureVerdict is the single post-analysis step: every fresh
// verdict with dimensions is bound to the current rule version and sources,
// whatever its status. A status that is not complete is then merged with the
// carried evidence, so an inconclusive re-check keeps what was still valid. It
// also runs on the failure path, where next has no dimensions.
func finalizeArchitectureVerdict(next, carried store.ArchitectureVerdict, carryEvidence bool,
	ruleVersion string, digests ArchitectureSourceDigest) store.ArchitectureVerdict {
	if len(next.Dimensions) > 0 {
		next = bindArchitectureEvidence(next, ruleVersion, digests)
	}
	if carryEvidence && next.Status != store.ArchitectureComplete {
		next = mergeArchitectureEvidence(next, carried)
	}
	return next
}

// mergeArchitectureEvidence keeps earlier dimensions when the current analysis
// is partial or inconclusive. A new section wins on a shared name unless it is
// empty; the carried dimensions keep their own stale markers, so a partial push
// never silently drops what was already established.
func mergeArchitectureEvidence(next, carried store.ArchitectureVerdict) store.ArchitectureVerdict {
	if len(carried.Dimensions) == 0 && strings.TrimSpace(carried.Evidence.Markdown) == "" {
		return next
	}
	merged := next
	index := make(map[string]int, len(next.Dimensions))
	dimensions := make([]store.ArchitectureDimension, 0, len(next.Dimensions)+len(carried.Dimensions))
	for _, dimension := range next.Dimensions {
		index[dimension.Name] = len(dimensions)
		dimensions = append(dimensions, dimension)
	}
	for _, dimension := range carried.Dimensions {
		if i, ok := index[dimension.Name]; ok {
			// An empty section is not evidence: keep the earlier content and
			// binding instead of erasing a valid conclusion.
			if strings.TrimSpace(dimensions[i].Content) == "" {
				dimensions[i] = dimension
			}
			continue
		}
		index[dimension.Name] = len(dimensions)
		dimensions = append(dimensions, dimension)
	}
	merged.Dimensions = dimensions
	merged.Requested = next.Requested || carried.Requested
	merged.Mode = next.Mode
	if merged.Mode == "" {
		merged.Mode = carried.Mode
	}
	merged.Evidence = mergeArchitectureEvidenceFields(next.Evidence, carried.Evidence)
	merged.CarriedFrom = carried.CarriedFrom
	if merged.CarriedFrom == "" {
		merged.CarriedFrom = carried.Comparison.HeadSHA
	}
	merged.Status = mergedArchitectureStatus(next.Status, dimensions)
	return merged
}

// mergeArchitectureEvidenceFields keeps the earlier report beside the partial
// one. Identical or contained reports are not duplicated.
func mergeArchitectureEvidenceFields(next, carried store.ArchitectureEvidence) store.ArchitectureEvidence {
	merged := next
	if merged.SchemaVersion == "" {
		merged.SchemaVersion = carried.SchemaVersion
	}
	if merged.ArtifactDir == "" {
		merged.ArtifactDir = carried.ArtifactDir
	}
	if len(merged.ObservationIDs) == 0 {
		merged.ObservationIDs = carried.ObservationIDs
	}
	if len(merged.CitedObservationIDs) == 0 {
		merged.CitedObservationIDs = carried.CitedObservationIDs
	}
	if len(merged.EngineScope) == 0 {
		merged.EngineScope = carried.EngineScope
	}
	if merged.RuleVersion == "" {
		merged.RuleVersion = carried.RuleVersion
	}
	merged.Markdown = mergeArchitectureMarkdown(next.Markdown, carried.Markdown)
	merged.Sources = unionArchitectureSources(next.Sources, carried.Sources)
	return merged
}

func mergeArchitectureMarkdown(next, carried string) string {
	next = strings.TrimSpace(next)
	carried = strings.TrimSpace(carried)
	switch {
	case next == "":
		return carried
	case carried == "" || next == carried:
		return next
	case strings.Contains(next, carried):
		return next
	case strings.Contains(carried, next):
		return carried
	default:
		return next + "\n\n---\n\n" + carried
	}
}

func mergedArchitectureStatus(next string, dimensions []store.ArchitectureDimension) string {
	for _, dimension := range dimensions {
		if dimension.Stale {
			return store.ArchitectureStale
		}
	}
	switch next {
	case store.ArchitectureInconclusive:
		// The current run could not conclude. Keeping earlier dimensions does
		// not upgrade that answer to partial or complete.
		return store.ArchitectureInconclusive
	case store.ArchitectureViolation:
		// A trusted-baseline violation is deterministic evidence: merging an
		// earlier report never turns it into a weaker partial verdict.
		return next
	case store.ArchitectureComplete, store.ArchitecturePartial:
		return next
	}
	if len(dimensions) > 0 {
		return store.ArchitecturePartial
	}
	return next
}

// carryArchitectureVerdict reattaches valid evidence to a new head that did not
// re-run the analysis. CarriedFrom keeps the head the report was produced at.
func carryArchitectureVerdict(carried store.ArchitectureVerdict, baseSHA, headSHA, mode string) store.ArchitectureVerdict {
	out := cloneArchitectureVerdict(carried)
	if out.CarriedFrom == "" {
		out.CarriedFrom = carried.Comparison.HeadSHA
	}
	out.Comparison.BaseSHA = baseSHA
	out.Comparison.HeadSHA = headSHA
	out.Mode = mode
	return out
}

func cloneArchitectureVerdict(verdict store.ArchitectureVerdict) store.ArchitectureVerdict {
	clone := verdict
	clone.Signals = append([]string(nil), verdict.Signals...)
	clone.BehaviorChanges = append([]string(nil), verdict.BehaviorChanges...)
	clone.Risks = append([]string(nil), verdict.Risks...)
	clone.Alternatives = append([]string(nil), verdict.Alternatives...)
	clone.AcceptanceConditions = append([]string(nil), verdict.AcceptanceConditions...)
	clone.Uncertainties = append([]string(nil), verdict.Uncertainties...)
	clone.Errors = append([]string(nil), verdict.Errors...)
	clone.Evidence.ObservationIDs = append([]string(nil), verdict.Evidence.ObservationIDs...)
	clone.Evidence.CitedObservationIDs = append([]string(nil), verdict.Evidence.CitedObservationIDs...)
	clone.Evidence.EngineScope = append([]store.ArchitectureScopeFile(nil), verdict.Evidence.EngineScope...)
	clone.Evidence.Sources = append([]store.ArchitectureEvidenceSource(nil), verdict.Evidence.Sources...)
	clone.Dimensions = make([]store.ArchitectureDimension, len(verdict.Dimensions))
	for i, dimension := range verdict.Dimensions {
		clone.Dimensions[i] = dimension
		clone.Dimensions[i].Sources = append([]store.ArchitectureEvidenceSource(nil), dimension.Sources...)
	}
	clone.Scope.Included = append([]store.ArchitectureScopeFile(nil), verdict.Scope.Included...)
	clone.Scope.Excluded = append([]store.ArchitectureScopeFile(nil), verdict.Scope.Excluded...)
	clone.Scope.OutOfScope = append([]string(nil), verdict.Scope.OutOfScope...)
	return clone
}
