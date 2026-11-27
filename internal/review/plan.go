package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	planPriorityCritical = "critical"
	planPriorityHigh     = "high"
	planPriorityNormal   = "normal"

	planStatusPending  = "pending"
	planStatusReviewed = "reviewed"
	planStatusReused   = "reused"
	planStatusSkipped  = "skipped"

	// maxPlanFiles bounds the local git work of the planner. Beyond it the
	// remaining changes stay in the plan without ranges or dependency edges.
	maxPlanFiles = 500
	// maxPlanUnitFiles keeps a grouped unit within one bounded conversation.
	maxPlanUnitFiles = 8
	// maxPlanRangesPerUnit bounds the serialized plan without dropping the
	// paths themselves.
	maxPlanRangesPerUnit = 12
	// maxPlanDependencyFiles and maxPlanDependencyBytes bound the head blobs
	// read to resolve cross-file dependencies.
	maxPlanDependencyFiles = 200
	maxPlanDependencyBytes = 256 << 10
)

// planRange is the new-side span one changed file contributes to a unit.
type planRange struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// planUnit is one review group. Paths that belong together (same directory or
// a dependency edge) share a unit, so the budget spends on related changes as
// a block. Status is reconciled with the final per-file coverage.
type planUnit struct {
	Key              string      `json:"key"`
	Kind             string      `json:"kind"`
	Paths            []string    `json:"paths"`
	Ranges           []planRange `json:"ranges,omitempty"`
	Dependencies     []string    `json:"dependencies,omitempty"`
	Priority         string      `json:"priority"`
	Evidence         []string    `json:"evidence,omitempty"`
	ContextBytes     int         `json:"context_bytes,omitempty"`
	TimeLimitSeconds int         `json:"time_limit_seconds,omitempty"`
	Status           string      `json:"status"`
	Reason           string      `json:"reason,omitempty"`
}

// reviewPlan is the priority order built before the first OCR group. It is
// serialized into the run coverage so the panel can explain which units were
// reviewed, reused or left pending and why.
type reviewPlan struct {
	Units                 []planUnit `json:"units"`
	CriticalTotal         int        `json:"critical_total"`
	CriticalPending       int        `json:"critical_pending"`
	ReviewSeconds         int        `json:"review_seconds,omitempty"`
	VerifyReserveSeconds  int        `json:"verify_reserve_seconds,omitempty"`
	PersistReserveSeconds int        `json:"persist_reserve_seconds,omitempty"`
}

// planInputs is everything the planner needs from the reviewed comparison.
// All reads are local: git trees, the policy and the contract comparison.
type planInputs struct {
	Dir             string
	BaseSHA         string
	HeadSHA         string
	Changed         []changedFile
	ExcludePatterns []string
	CriticalGlobs   []string
	Contracts       store.ArchitectureContracts
	ContextBytes    int
	ReviewTimeout   time.Duration
	VerifyReserve   time.Duration
}

// planFile is the per-path state gathered before grouping.
type planFile struct {
	path         string
	ranges       []planRange
	changedLines []string
	evidence     []string
	priority     string
}

// buildReviewPlan links every changed path to its ranges and dependencies,
// records the evidence that raises its priority, and groups related paths.
// A local failure never aborts the review: the affected path keeps a normal
// priority and the plan still orders the rest.
func buildReviewPlan(ctx context.Context, in planInputs) reviewPlan {
	plan := reviewPlan{
		ReviewSeconds:         int(in.ReviewTimeout.Round(time.Second).Seconds()),
		VerifyReserveSeconds:  int(in.VerifyReserve.Round(time.Second).Seconds()),
		PersistReserveSeconds: int(unitWriteTimeout.Round(time.Second).Seconds()),
	}
	if plan.ReviewSeconds < 0 {
		plan.ReviewSeconds = 0
	}

	contractPaths := make(map[string]bool)
	for _, contract := range in.Contracts.Contracts {
		if p := normalizeCoveragePath(contract.Path); p != "" {
			contractPaths[p] = true
		}
	}

	var files []planFile
	var skipped []planUnit
	seen := 0
	for _, entry := range in.Changed {
		p := normalizeCoveragePath(entry.Path)
		if p == "" {
			continue
		}
		switch entry.Kind {
		case changedDeleted, changedBinary, changedSubmodule:
			skipped = append(skipped, planUnit{
				Key: planUnitKey([]string{p}), Kind: "file", Paths: []string{p},
				Priority: planPriorityNormal, Status: planStatusSkipped, Reason: entry.Kind,
			})
			continue
		}
		if policyExcludesPath(in.ExcludePatterns, p) {
			skipped = append(skipped, planUnit{
				Key: planUnitKey([]string{p}), Kind: "file", Paths: []string{p},
				Priority: planPriorityNormal, Status: planStatusSkipped, Reason: "policy",
			})
			continue
		}
		if seen >= maxPlanFiles {
			files = append(files, planFile{path: p, priority: planPriorityNormal})
			continue
		}
		seen++
		ranges, changedLines := planFileDiff(ctx, in.Dir, in.BaseSHA, in.HeadSHA, p)
		file := planFile{path: p, ranges: ranges, changedLines: changedLines, priority: planPriorityNormal}
		file.evidence = planFileEvidence(p, changedLines, in.CriticalGlobs, contractPaths)
		if len(file.evidence) > 0 {
			file.priority = planPriorityCritical
		}
		files = append(files, file)
	}

	deps := planDependencies(ctx, in.Dir, in.HeadSHA, files)
	units := groupPlanFiles(files, deps, in.ContextBytes)
	units = append(units, skipped...)

	rank := map[string]int{planPriorityCritical: 0, planPriorityHigh: 1, planPriorityNormal: 2}
	sort.SliceStable(units, func(i, j int) bool {
		if rank[units[i].Priority] != rank[units[j].Priority] {
			return rank[units[i].Priority] < rank[units[j].Priority]
		}
		if (units[i].Status == planStatusSkipped) != (units[j].Status == planStatusSkipped) {
			return units[i].Status != planStatusSkipped
		}
		return units[i].Key < units[j].Key
	})

	plan.Units = units
	plan.CriticalTotal = countPlanPriority(units, planPriorityCritical)
	sharePlanTime(&plan, in.ReviewTimeout)
	return plan
}

func countPlanPriority(units []planUnit, priority string) int {
	count := 0
	for _, unit := range units {
		if unit.Priority == priority && unit.Status == planStatusPending {
			count++
		}
	}
	return count
}

// sharePlanTime gives every pending unit a slice of the review deadline. The
// verification and persistence reserves are already carved out by the caller,
// so a unit can never spend the time meant for checking its findings.
func sharePlanTime(plan *reviewPlan, timeout time.Duration) {
	pending := 0
	for _, unit := range plan.Units {
		if unit.Status == planStatusPending {
			pending++
		}
	}
	if pending == 0 {
		return
	}
	seconds := int(timeout.Round(time.Second).Seconds())
	if seconds <= 0 {
		return
	}
	share := seconds / pending
	if share < 1 {
		share = 1
	}
	for i := range plan.Units {
		if plan.Units[i].Status == planStatusPending {
			plan.Units[i].TimeLimitSeconds = share
		}
	}
}

// planFileDiff reads the new-side ranges and touched lines of one path. A rename
// or a read error contributes no ranges instead of failing the plan.
func planFileDiff(ctx context.Context, dir, baseSHA, headSHA, file string) ([]planRange, []string) {
	raw, err := gitRaw(ctx, dir, "plan diff", "diff", "-U0", "--no-color", "--no-renames",
		baseSHA, headSHA, "--", file)
	if err != nil {
		return nil, nil
	}
	hunks, err := parseFileDiff(raw)
	if err != nil {
		return nil, nil
	}
	var ranges []planRange
	var changedLines []string
	for _, hunk := range hunks {
		switch {
		case hunk.NewCount > 0:
			ranges = append(ranges, planRange{
				Path: file, Start: hunk.NewStart, End: hunk.NewStart + hunk.NewCount - 1,
			})
		default:
			// A pure deletion has no new lines; its insertion point is still the
			// range that explains where the change lands.
			ranges = append(ranges, planRange{Path: file, Start: hunk.NewStart, End: hunk.NewStart})
		}
		for _, line := range hunk.Lines {
			// Both sides count: removing the last lock or channel use is a
			// concurrency change too.
			if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
				changedLines = append(changedLines, line[1:])
			}
		}
	}
	return ranges, changedLines
}

// planFileEvidence records why a path is critical. The strings name the
// category and the concrete match, so the plan can justify the priority.
func planFileEvidence(file string, changedLines, criticalGlobs []string, contractPaths map[string]bool) []string {
	var evidence []string
	if match := matchPolicyGlob(criticalGlobs, file); match != "" {
		evidence = append(evidence, "policy:"+match)
	}
	if contractPaths[file] {
		evidence = append(evidence, "public_contract:changed")
	} else if kind := contractPathEvidence(file); kind != "" {
		evidence = append(evidence, "public_contract:"+kind)
	}
	if kind := persistencePathEvidence(file); kind != "" {
		evidence = append(evidence, "persistence:"+kind)
	}
	if marker := concurrencyMarker(changedLines); marker != "" {
		evidence = append(evidence, "concurrency:"+marker)
	}
	return evidence
}

// matchPolicyGlob returns the critical glob that matched file, if any.
func matchPolicyGlob(patterns []string, file string) string {
	file = normalizeCoveragePath(file)
	base := path.Base(file)
	for _, pattern := range patterns {
		p := strings.TrimSpace(strings.ReplaceAll(pattern, `\`, "/"))
		if p == "" {
			continue
		}
		if globMatch(p, file) || (!strings.Contains(p, "/") && globMatch(p, base)) {
			return p
		}
	}
	return ""
}

func contractPathEvidence(file string) string {
	lower := strings.ToLower(file)
	ext := strings.ToLower(path.Ext(lower))
	base := path.Base(lower)
	switch ext {
	case ".proto":
		return "proto"
	case ".graphql", ".gql":
		return "graphql"
	}
	if ext == ".json" || ext == ".yaml" || ext == ".yml" {
		if strings.Contains(base, "openapi") || strings.Contains(base, "swagger") {
			return "openapi"
		}
		if strings.Contains(lower, "/contracts/") || strings.HasPrefix(lower, "contracts/") {
			return "declared"
		}
	}
	return ""
}

func persistencePathEvidence(file string) string {
	lower := strings.ToLower(file)
	base := path.Base(lower)
	switch {
	case strings.Contains(lower, "/migrations/") || strings.HasPrefix(lower, "migrations/") ||
		strings.Contains(lower, "/migration/") || strings.HasPrefix(lower, "migration/"):
		return "migrations"
	case strings.HasSuffix(base, ".sql"):
		return "sql"
	case strings.Contains(base, "schema"):
		return "schema"
	case strings.Contains(lower, "/store/") || strings.Contains(lower, "/repository/") ||
		strings.Contains(lower, "/persistence/") || strings.Contains(lower, "/database/") ||
		strings.Contains(lower, "/migrations/") || strings.Contains(lower, "/db/"):
		return "data_layer"
	}
	return ""
}

var concurrencyMarkers = []string{
	"sync.", "sync/atomic", "atomic.", "go func", "make(chan", "chan ",
	"Mutex", "RWMutex", "WaitGroup", "errgroup.", "context.WithCancel",
}

func concurrencyMarker(changedLines []string) string {
	for _, line := range changedLines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, marker := range concurrencyMarkers {
			if strings.Contains(line, marker) {
				return marker
			}
		}
	}
	return ""
}

// planDependencies returns importer -> imported edges between changed Go files
// in the same module, and records them as evidence on the importer.
func planDependencies(ctx context.Context, dir, headSHA string, files []planFile) map[string][]string {
	edges := make(map[string][]string)
	if len(files) == 0 {
		return edges
	}
	module := goModulePath(ctx, dir, headSHA)
	if module == "" {
		return edges
	}

	byDir := make(map[string][]string)
	for _, file := range files {
		if path.Ext(file.path) != ".go" {
			continue
		}
		d := path.Dir(file.path)
		if d == "." {
			d = ""
		}
		byDir[d] = append(byDir[d], file.path)
	}
	if len(byDir) == 0 {
		return edges
	}

	read := 0
	bytesRead := 0
	for _, file := range files {
		if path.Ext(file.path) != ".go" || read >= maxPlanDependencyFiles || bytesRead >= maxPlanDependencyBytes {
			continue
		}
		content, err := gitRaw(ctx, dir, "show plan blob", "show", headSHA+":"+file.path)
		if err != nil {
			continue
		}
		read++
		bytesRead += len(content)
		for _, imported := range goImports(content) {
			rel := importRelativePath(module, imported)
			if rel == "" && imported != module {
				continue
			}
			for _, target := range changedFilesUnder(byDir, rel) {
				if target == file.path {
					continue
				}
				edges[file.path] = append(edges[file.path], target)
			}
		}
	}
	for importer := range edges {
		sort.Strings(edges[importer])
		edges[importer] = dedupeStrings(edges[importer])
	}
	return edges
}

func goModulePath(ctx context.Context, dir, headSHA string) string {
	raw, err := gitRaw(ctx, dir, "show go.mod", "show", headSHA+":go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func goImports(content []byte) []string {
	parsed, err := parser.ParseFile(token.NewFileSet(), "", content, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	imports := make([]string, 0, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		value := strings.Trim(spec.Path.Value, `"`)
		if value != "" {
			imports = append(imports, value)
		}
	}
	return imports
}

// importRelativePath maps a module import to the repository directory it
// points at. The module itself maps to the repository root.
func importRelativePath(module, imported string) string {
	if imported == module {
		return ""
	}
	prefix := module + "/"
	if !strings.HasPrefix(imported, prefix) {
		return ""
	}
	return strings.TrimPrefix(imported, prefix)
}

// changedFilesUnder returns the changed Go files below dir. An empty dir means
// the repository root.
func changedFilesUnder(byDir map[string][]string, dir string) []string {
	if dir == "" {
		return byDir[""]
	}
	var found []string
	for changedDir, files := range byDir {
		if changedDir == dir || strings.HasPrefix(changedDir, dir+"/") {
			found = append(found, files...)
		}
	}
	sort.Strings(found)
	return found
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

// groupPlanFiles merges paths of the same directory and dependency edges into
// units, splitting any unit larger than maxPlanUnitFiles.
func groupPlanFiles(files []planFile, deps map[string][]string, contextBytes int) []planUnit {
	if len(files) == 0 {
		return nil
	}
	byPath := make(map[string]planFile, len(files))
	parent := make(map[string]string, len(files))
	for _, file := range files {
		byPath[file.path] = file
		parent[file.path] = file.path
	}
	var find func(string) string
	find = func(p string) string {
		if parent[p] != p {
			parent[p] = find(parent[p])
		}
		return parent[p]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	byDir := make(map[string][]string)
	for _, file := range files {
		d := path.Dir(file.path)
		if d == "." {
			d = ""
		}
		byDir[d] = append(byDir[d], file.path)
	}
	for _, paths := range byDir {
		sort.Strings(paths)
		for i := 1; i < len(paths); i++ {
			union(paths[0], paths[i])
		}
	}
	for importer, imported := range deps {
		if _, ok := byPath[importer]; !ok {
			continue
		}
		for _, target := range imported {
			if _, ok := byPath[target]; ok {
				union(importer, target)
			}
		}
	}

	components := make(map[string][]string)
	rootKeys := make(map[string]bool)
	for _, file := range files {
		root := find(file.path)
		components[root] = append(components[root], file.path)
		rootKeys[root] = true
	}
	roots := make([]string, 0, len(rootKeys))
	for root := range rootKeys {
		roots = append(roots, root)
	}
	sort.Strings(roots)

	var units []planUnit
	for _, root := range roots {
		paths := components[root]
		sort.Strings(paths)
		for start := 0; start < len(paths); start += maxPlanUnitFiles {
			end := start + maxPlanUnitFiles
			if end > len(paths) {
				end = len(paths)
			}
			units = append(units, newPlanUnit(paths[start:end], byPath, deps, contextBytes))
		}
	}
	return units
}

func newPlanUnit(paths []string, byPath map[string]planFile, deps map[string][]string, contextBytes int) planUnit {
	unit := planUnit{
		Key:          planUnitKey(paths),
		Kind:         "group",
		Paths:        append([]string(nil), paths...),
		Priority:     planPriorityNormal,
		ContextBytes: contextBytes,
		Status:       planStatusPending,
	}
	evidence := make(map[string]bool)
	ranges := 0
	for _, p := range paths {
		file := byPath[p]
		if planPriorityRank(file.priority) < planPriorityRank(unit.Priority) {
			unit.Priority = file.priority
		}
		for _, item := range file.evidence {
			evidence[item] = true
		}
		for _, r := range file.ranges {
			if ranges >= maxPlanRangesPerUnit {
				break
			}
			unit.Ranges = append(unit.Ranges, r)
			ranges++
		}
	}
	for _, p := range paths {
		for _, target := range deps[p] {
			if _, ok := byPath[target]; ok && planContainsString(paths, target) {
				unit.Dependencies = append(unit.Dependencies, p+" -> "+target)
				evidence["dependency:"+p+" -> "+target] = true
			}
		}
	}
	sort.Strings(unit.Dependencies)
	unit.Dependencies = dedupeStrings(unit.Dependencies)
	if unit.Priority == planPriorityNormal && len(unit.Dependencies) > 0 {
		// A related change is reviewed right after its dependency instead of
		// waiting behind unrelated work.
		unit.Priority = planPriorityHigh
	}
	for item := range evidence {
		unit.Evidence = append(unit.Evidence, item)
	}
	sort.Strings(unit.Evidence)
	return unit
}

func planPriorityRank(priority string) int {
	switch priority {
	case planPriorityCritical:
		return 0
	case planPriorityHigh:
		return 1
	default:
		return 2
	}
}

func planUnitKey(paths []string) string {
	sum := sha256.Sum256([]byte(strings.Join(paths, "\x00")))
	return "plan:" + hex.EncodeToString(sum[:8])
}

func planContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// orderPaths sorts the paths of a route by the plan: critical units first,
// related paths together. Paths outside the plan keep a deterministic tail.
func (p *reviewPlan) orderPaths(paths []string) []string {
	if p == nil || len(p.Units) == 0 || len(paths) < 2 {
		return paths
	}
	order := make(map[string]int, len(paths))
	seq := 0
	units := append([]planUnit(nil), p.Units...)
	sort.SliceStable(units, func(i, j int) bool {
		ri, rj := planPriorityRank(units[i].Priority), planPriorityRank(units[j].Priority)
		if ri != rj {
			return ri < rj
		}
		return units[i].Key < units[j].Key
	})
	for _, unit := range units {
		if unit.Status == planStatusSkipped {
			continue
		}
		for _, file := range unit.Paths {
			if _, ok := order[file]; !ok {
				order[file] = seq
			}
		}
		seq++
	}
	unknown := seq
	out := append([]string(nil), paths...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, iok := order[normalizeCoveragePath(out[i])]
		oj, jok := order[normalizeCoveragePath(out[j])]
		if !iok {
			oi = unknown
		}
		if !jok {
			oj = unknown
		}
		if oi != oj {
			return oi < oj
		}
		return out[i] < out[j]
	})
	return out
}

// applyCoverage reconciles the plan with the final per-file coverage and the
// paths a previous attempt already paid for. A unit only becomes reviewed or
// reused when every one of its paths is covered.
func (p *reviewPlan) applyCoverage(files []coverageFile, reused map[string]bool) {
	if p == nil {
		return
	}
	index := make(map[string]coverageFile, len(files))
	for _, file := range files {
		index[file.Path] = file
	}
	p.CriticalPending = 0
	for i := range p.Units {
		unit := &p.Units[i]
		if unit.Status == planStatusSkipped {
			continue
		}
		covered, reusedPaths := 0, 0
		reason := ""
		for _, file := range unit.Paths {
			coverage, ok := index[file]
			if ok && fileCovered(coverage) {
				covered++
				if reused[file] {
					reusedPaths++
				}
				continue
			}
			if reason == "" {
				if ok {
					reason = strings.TrimSpace(coverage.Reason)
					if reason == "" {
						reason = coverage.Status
					}
				} else {
					reason = "not_reported"
				}
			}
		}
		switch {
		case covered == len(unit.Paths):
			unit.Reason = ""
			if reusedPaths == len(unit.Paths) {
				unit.Status = planStatusReused
			} else {
				unit.Status = planStatusReviewed
			}
		default:
			unit.Status = planStatusPending
			if reason == "" {
				reason = "not_reviewed"
			}
			unit.Reason = reason
			if unit.Priority == planPriorityCritical {
				p.CriticalPending++
			}
		}
	}
}

// applyPlanGate prevents a run with an unverified critical unit from claiming
// a conclusive result. The existing inconclusive gate already blocks an
// automatic approval, so no second gate is introduced.
func applyPlanGate(status, code string, plan *reviewPlan) (string, string) {
	if plan == nil || plan.CriticalPending == 0 {
		return status, code
	}
	if status == store.RunSucceeded {
		return store.RunInconclusive, "coverage_incomplete"
	}
	return status, code
}

// incompleteSuffix explains the critical gap in the run message.
func (p *reviewPlan) incompleteSuffix() string {
	if p == nil || p.CriticalPending == 0 {
		return ""
	}
	return fmt.Sprintf("; %d unidad(es) crítica(s) sin verificar", p.CriticalPending)
}

func (p *reviewPlan) jsonBytes() []byte {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return raw
}

// reusedCoveragePaths returns the paths the engine reported as reused from a
// previous session.
func reusedCoveragePaths(res nativeReview) map[string]bool {
	reused := make(map[string]bool)
	for _, item := range res.Manifest.Coverage.Reused {
		if p := normalizeCoveragePath(item.Path); p != "" {
			reused[p] = true
		}
	}
	return reused
}

// planCriticalGlobs decodes the policy list. A malformed list prioritises
// nothing instead of failing the review.
func planCriticalGlobs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var globs []string
	if err := json.Unmarshal(raw, &globs); err != nil {
		return nil
	}
	return globs
}
