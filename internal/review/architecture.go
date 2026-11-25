package review

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// architectureSignalChangeSize is the explicit size trigger. Smaller
	// changes are still analysed when a path signal fires.
	architectureSignalChangeSize  = "change_size"
	architectureSignalType        = "change_type"
	architectureSignalModule      = "module_boundary"
	architectureSignalInterface   = "public_interface"
	architectureSignalPersistence = "persistence"
	architectureSignalPermissions = "permissions"
	architectureSignalTopology    = "topology"
	architectureSignalConfig      = "configuration"
	architectureSignalDeps        = "dependencies"

	architectureFilesThreshold = 20
	architectureLinesThreshold = 400

	// architectureTimeMax bounds the analysis window even when the run is
	// much longer. OCR has no separate --analysis-timeout flag, so this budget
	// is recorded (and logged) rather than passed as its own flag.
	architectureTimeMax = 10 * time.Minute

	// maxArchitectureMarkdown bounds the report copy kept in the run row.
	maxArchitectureMarkdown = 64 << 10
)

// architectureChangeStats are the diff facts the signal detector needs. They
// are computed before the review so the engine can be asked for the analysis in
// the same invocation.
type architectureChangeStats struct {
	Files        int
	AddedLines   int
	RemovedLines int
	Renamed      int
}

// architectureMode resolves the policy mode. Unknown values fail closed to
// off: an unreadable policy must not spend tokens on an analysis nobody asked
// for.
func architectureMode(policy store.QAPolicy) string {
	switch strings.ToLower(strings.TrimSpace(policy.ArchitectureAnalysisMode)) {
	case store.ArchitectureAnalysisOff:
		return store.ArchitectureAnalysisOff
	case "", store.ArchitectureAnalysisObservation:
		return store.ArchitectureAnalysisObservation
	default:
		return store.ArchitectureAnalysisOff
	}
}

// architectureAnalysisRequested is the single activation rule: the policy must
// allow observation, the inventory must be available and an explicit signal
// must have fired. A small local change in one module is not analysed.
func architectureAnalysisRequested(mode string, inventoryErr error, signals []string) bool {
	return mode == store.ArchitectureAnalysisObservation && inventoryErr == nil && len(signals) > 0
}

// detectArchitectureSignals returns the explicit facts that justify an
// architectural analysis. It is intentionally conservative: a small change in
// one module with no interface, persistence, permission, topology, config or
// dependency effect produces no signal and therefore no analysis.
func detectArchitectureSignals(files []changedFile, stats architectureChangeStats) []string {
	signals := make([]string, 0, 5)
	add := func(signal string) {
		for _, existing := range signals {
			if existing == signal {
				return
			}
		}
		signals = append(signals, signal)
	}

	if stats.Files >= architectureFilesThreshold ||
		stats.AddedLines+stats.RemovedLines >= architectureLinesThreshold {
		add(architectureSignalChangeSize)
	}
	if stats.Renamed > 0 {
		add(architectureSignalType)
	}
	if multipleModuleRoots(files) {
		add(architectureSignalModule)
	}
	for _, file := range files {
		p := normalizeCoveragePath(file.Path)
		if p == "" {
			continue
		}
		base := strings.ToLower(baseName(p))
		switch {
		case matchesArchitecturePath(p, architectureInterfaceDirs) ||
			strings.HasSuffix(base, ".proto") || strings.HasSuffix(base, ".graphql") ||
			strings.HasSuffix(base, ".gql") || strings.Contains(base, "openapi") ||
			strings.Contains(base, "swagger"):
			add(architectureSignalInterface)
		}
		if strings.HasSuffix(base, ".sql") || matchesArchitecturePath(p, architecturePersistenceDirs) {
			add(architectureSignalPersistence)
		}
		if matchesArchitecturePath(p, architecturePermissionDirs) {
			add(architectureSignalPermissions)
		}
		if matchesArchitecturePath(p, architectureTopologyDirs) ||
			strings.HasPrefix(strings.ToLower(p), ".github/workflows/") ||
			strings.HasPrefix(base, "dockerfile") || strings.HasPrefix(base, "docker-compose") ||
			strings.HasPrefix(base, "compose.") {
			add(architectureSignalTopology)
		}
		if matchesArchitecturePath(p, architectureConfigDirs) ||
			strings.HasPrefix(base, "config.") || strings.HasPrefix(base, "settings.") ||
			strings.HasPrefix(base, ".env") || strings.HasPrefix(base, "application.") ||
			strings.HasPrefix(base, "values.") {
			add(architectureSignalConfig)
		}
		if architectureDependencyFiles[base] {
			add(architectureSignalDeps)
			// A dependency manifest is also a module boundary: it changes what
			// the packages can see and how they can be built.
			add(architectureSignalModule)
		}
	}
	return signals
}

var (
	architectureInterfaceDirs = []string{
		"api", "apis", "openapi", "swagger", "proto", "protos", "graphql",
		"sdk", "public", "contracts", "interfaces", "dto", "dtos",
	}
	architecturePersistenceDirs = []string{
		"migration", "migrations", "db", "database", "schema", "schemas",
		"repository", "repositories", "storage", "persistence", "entity",
		"entities", "models",
	}
	architecturePermissionDirs = []string{
		"auth", "authz", "authentication", "authorization", "permission",
		"permissions", "rbac", "acl", "scope", "scopes", "security",
	}
	architectureTopologyDirs = []string{
		"deploy", "deployment", "deployments", "infra", "infrastructure",
		"k8s", "kubernetes", "helm", "terraform", "ansible", "systemd", "docker",
	}
	architectureConfigDirs = []string{
		"config", "configs", "configuration", "settings",
	}
	architectureDependencyFiles = map[string]bool{
		"go.mod": true, "go.sum": true,
		"package.json": true, "package-lock.json": true, "yarn.lock": true,
		"pnpm-lock.yaml": true, "pnpm-workspace.yaml": true,
		"requirements.txt": true, "pyproject.toml": true, "poetry.lock": true,
		"cargo.toml": true, "cargo.lock": true,
		"pom.xml": true, "build.gradle": true, "build.gradle.kts": true,
		"composer.json": true, "composer.lock": true, "gemfile": true, "gemfile.lock": true,
	}
)

func matchesArchitecturePath(path string, segments []string) bool {
	lower := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	parts := strings.Split(lower, "/")
	for _, part := range parts {
		for _, segment := range segments {
			if part == segment {
				return true
			}
		}
	}
	return false
}

func baseName(path string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// multipleModuleRoots reports whether the change spans more than one module.
// Non-code roots (docs, assets, vendor) are ignored, and nested paths under
// the same source tree are one module: src/a.go and src/sub/b.go do not fire.
// Container roots that hold packages use their second segment so
// internal/review and internal/store are separate modules.
func multipleModuleRoots(files []changedFile) bool {
	roots := make(map[string]bool)
	for _, file := range files {
		root := architectureModuleRoot(file.Path)
		if root == "" {
			continue
		}
		roots[root] = true
	}
	return len(roots) > 1
}

var architectureContainerRoots = map[string]bool{
	"internal": true, "cmd": true, "pkg": true, "services": true,
	"apps": true, "packages": true, "modules": true,
}

var architectureNonCodeRoots = map[string]bool{
	"docs": true, "doc": true, "documentation": true, "assets": true,
	"vendor": true, "third_party": true, "testdata": true, "node_modules": true,
	"dist": true, "build": true, "generated": true,
}

var architectureCodeExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".py": true, ".rs": true, ".java": true,
	".kt": true, ".kts": true, ".rb": true, ".php": true, ".cs": true,
	".c": true, ".cc": true, ".cpp": true, ".h": true, ".hpp": true,
	".swift": true, ".scala": true, ".ex": true, ".exs": true, ".dart": true,
	".vue": true, ".svelte": true, ".sh": true,
}

func architectureModuleRoot(path string) string {
	p := normalizeCoveragePath(path)
	if p == "" || !architectureCodeExtensions[strings.ToLower(fileExtension(p))] {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return ""
	}
	root := strings.ToLower(parts[0])
	if architectureNonCodeRoots[root] {
		return ""
	}
	if architectureContainerRoots[root] {
		return root + "/" + parts[1]
	}
	return root
}

func fileExtension(path string) string {
	base := baseName(path)
	if i := strings.LastIndex(base, "."); i >= 0 {
		return base[i:]
	}
	return ""
}

// architectureScope records what the analysis covers and what stays out. The
// out-of-scope reasons are the ones the review itself cannot inspect: deleted,
// binary and submodule entries, plus the team's policy exclusions.
func architectureScope(files []changedFile, patterns []string) store.ArchitectureScope {
	scope := store.ArchitectureScope{}
	reasons := make(map[string]bool)
	for _, file := range files {
		p := normalizeCoveragePath(file.Path)
		if p == "" {
			continue
		}
		reason := ""
		switch file.Kind {
		case changedDeleted:
			reason = "deleted"
		case changedBinary:
			reason = "binary"
		case changedSubmodule:
			reason = "submodule"
		default:
			if policyExcludesPath(patterns, p) {
				reason = "policy"
			}
		}
		if reason == "" {
			scope.Included = append(scope.Included, store.ArchitectureScopeFile{Path: p, Kind: file.Kind})
			continue
		}
		scope.Excluded = append(scope.Excluded, store.ArchitectureScopeFile{Path: p, Kind: file.Kind, Reason: reason})
		reasons[reason] = true
	}
	for _, reason := range []string{"deleted", "binary", "submodule", "policy"} {
		if reasons[reason] {
			scope.OutOfScope = append(scope.OutOfScope, reason)
		}
	}
	return scope
}

// architectureBudget derives the analysis budget that is separate from the
// review and the verifier. The token cap is the one OCR accepts through
// --analysis-max-tokens-budget; the caller carves it out of the run budget so
// the aggregate cannot exceed it. The time window is recorded in the verdict
// because OCR has no separate analysis timeout flag; the analysis runs
// concurrently, so the window is a share of the run clock, not extra time.
func architectureBudget(policy store.QAPolicy, remaining time.Duration, reviewTokens int64) store.ArchitectureBudget {
	tokenShare := policy.ArchitectureTokenShare
	if tokenShare <= 0 {
		tokenShare = store.DefaultArchitectureTokenShare
	}
	timeShare := policy.ArchitectureTimeShare
	if timeShare <= 0 {
		timeShare = store.DefaultArchitectureTimeShare
	}
	tokenMax := policy.ArchitectureTokenMax
	if tokenMax <= 0 {
		tokenMax = store.DefaultArchitectureTokenMax
	}

	budget := tokenMax
	if reviewTokens > 0 {
		if share := reviewTokens * int64(tokenShare) / 100; share < budget {
			budget = share
		}
	}
	if budget > tokenMax {
		budget = tokenMax
	}
	if budget < 1 {
		budget = 1
	}

	seconds := int64(0)
	if remaining > 0 {
		window := remaining * time.Duration(timeShare) / 100
		if maxReserve := remaining / 2; window > maxReserve {
			window = maxReserve
		}
		if window > architectureTimeMax {
			window = architectureTimeMax
		}
		seconds = int64(window / time.Second)
	}
	return store.ArchitectureBudget{
		TokenBudget:       budget,
		TokenShare:        tokenShare,
		TimeBudgetSeconds: seconds,
		TimeShare:         timeShare,
	}
}

// newArchitectureVerdict seeds the verdict before the engine runs. A requested
// analysis starts inconclusive: only a validated report can promote it.
func newArchitectureVerdict(mode string, requested bool, signals []string, scope store.ArchitectureScope,
	comparison store.ArchitectureComparison, budget store.ArchitectureBudget) store.ArchitectureVerdict {
	status := store.ArchitectureNotRequested
	if requested {
		status = store.ArchitectureInconclusive
	}
	return store.ArchitectureVerdict{
		Status:     status,
		Requested:  requested,
		Mode:       mode,
		Signals:    signals,
		Comparison: comparison,
		Scope:      scope,
		Controls: store.ArchitectureControls{
			Observation:    mode == store.ArchitectureAnalysisObservation,
			WritesDecision: false,
		},
		Budget: budget,
	}
}

// applyNativeArchitectureAnalysis validates the analysis section of the OCR
// result and turns it into the persisted verdict. It never returns an error:
// an analysis failure is recorded as inconclusive evidence and must not fail
// the review.
func applyNativeArchitectureAnalysis(verdict store.ArchitectureVerdict, res nativeReview) store.ArchitectureVerdict {
	if !verdict.Requested {
		return verdict
	}
	verdict.ReviewStatus = strings.TrimSpace(res.ReviewStatus)
	meta, err := parseNativeArchitectureMeta(res.AnalysisMeta)
	if err != nil {
		verdict.Status = store.ArchitectureInconclusive
		verdict.Errors = append(verdict.Errors, err.Error())
		return verdict
	}
	if meta == nil {
		verdict.Status = store.ArchitectureInconclusive
		verdict.Errors = append(verdict.Errors, "analysis metadata missing")
		return verdict
	}

	markdown := strings.TrimSpace(nativeAnalysisMarkdown(res))
	if markdown == "" {
		verdict.Status = store.ArchitectureInconclusive
		verdict.Errors = append(verdict.Errors, "analysis report empty")
		return verdict
	}
	// A report from another comparison must never be published as evidence of
	// this run: the base and head have to match the SHAs the worker reviewed.
	if mismatch := architectureComparisonMismatch(verdict.Comparison, meta.Comparison); mismatch != "" {
		verdict.ExecutionStatus = strings.ToLower(strings.TrimSpace(meta.ExecutionStatus))
		verdict.Errors = append(verdict.Errors, mismatch)
		verdict.Status = store.ArchitectureInconclusive
		return verdict
	}
	if meta.Comparison.Mode != "" {
		verdict.Comparison.Mode = meta.Comparison.Mode
	}
	verdict.Comparison.BaseIsEmpty = meta.Comparison.BaseIsEmpty
	evidenceMarkdown := truncateArchitectureMarkdown(markdown)
	verdict.Evidence = store.ArchitectureEvidence{
		SchemaVersion:       meta.SchemaVersion,
		Markdown:            evidenceMarkdown,
		ArtifactDir:         meta.ArtifactDir,
		ObservationIDs:      meta.ObservationIDs,
		CitedObservationIDs: meta.CitedObservationIDs,
		EngineScope:         nativeArchitectureScope(meta.Scope),
	}
	// Parse the capped text so every persisted dimension is backed by the
	// evidence that is actually stored.
	verdict.Dimensions = parseArchitectureDimensions(evidenceMarkdown)
	verdict.BehaviorChanges = architectureSectionItems(verdict.Dimensions, architectureBehaviorHeadings)
	verdict.Risks = architectureSectionItems(verdict.Dimensions, architectureRiskHeadings)
	verdict.Alternatives = architectureSectionItems(verdict.Dimensions, architectureAlternativeHeadings)
	verdict.AcceptanceConditions = architectureSectionItems(verdict.Dimensions, architectureAcceptanceHeadings)
	verdict.Uncertainties = architectureSectionItems(verdict.Dimensions, architectureUncertaintyHeadings)
	// Recommendation keeps its own source. Deriving it from the acceptance
	// conditions printed the same list twice and mislabelled maintainer
	// decisions as the original recommendation.
	if items := architectureSectionItems(verdict.Dimensions, architectureRecommendationHeadings); len(items) > 0 {
		verdict.Recommendation = strings.Join(items, "\n")
	}
	verdict.Usage = store.ArchitectureUsage{
		Known:      meta.Usage.TotalTokens > 0 || meta.Usage.InputTokens > 0 || meta.Usage.OutputTokens > 0,
		Input:      meta.Usage.InputTokens,
		Output:     meta.Usage.OutputTokens,
		CacheRead:  meta.Usage.CacheReadTokens,
		CacheWrite: meta.Usage.CacheWriteTokens,
		Total:      meta.Usage.TotalTokens,
	}
	if verdict.Usage.Total == 0 {
		verdict.Usage.Total = verdict.Usage.Input + verdict.Usage.Output +
			verdict.Usage.CacheRead + verdict.Usage.CacheWrite
	}
	verdict.ExecutionStatus = strings.ToLower(strings.TrimSpace(meta.ExecutionStatus))
	verdict.DeclaredCompletion = strings.ToLower(strings.TrimSpace(meta.DeclaredCompletion))
	verdict.StopReason = strings.TrimSpace(meta.StopReason)
	verdict.Model = meta.Model
	verdict.SessionID = meta.SessionID
	verdict.Errors = append(verdict.Errors, meta.Errors...)
	verdict.Status = architectureStatusFromMeta(meta, evidenceMarkdown)
	return verdict
}

// architectureComparisonMismatch reports the first SHA disagreement between
// the comparison the worker reviewed and the one the report claims.
func architectureComparisonMismatch(expected store.ArchitectureComparison, reported nativeArchitectureCompare) string {
	if expected.BaseSHA != "" && reported.Old != "" && !strings.EqualFold(expected.BaseSHA, reported.Old) {
		return fmt.Sprintf("analysis base %s does not match reviewed base %s", reported.Old, expected.BaseSHA)
	}
	if expected.HeadSHA != "" && reported.Head != "" && !strings.EqualFold(expected.HeadSHA, reported.Head) {
		return fmt.Sprintf("analysis head %s does not match reviewed head %s", reported.Head, expected.HeadSHA)
	}
	return ""
}

func architectureStatusFromMeta(meta *nativeArchitectureMeta, markdown string) string {
	switch strings.ToLower(strings.TrimSpace(meta.ExecutionStatus)) {
	case "completed":
		switch {
		case strings.TrimSpace(markdown) == "":
			return store.ArchitectureInconclusive
		case strings.EqualFold(strings.TrimSpace(meta.DeclaredCompletion), "partial"),
			len(meta.Errors) > 0:
			return store.ArchitecturePartial
		default:
			return store.ArchitectureComplete
		}
	case "partial":
		return store.ArchitecturePartial
	default:
		return store.ArchitectureInconclusive
	}
}

func nativeAnalysisMarkdown(res nativeReview) string {
	if res.AnalysisRaw != nil && strings.TrimSpace(*res.AnalysisRaw) != "" {
		return *res.AnalysisRaw
	}
	if res.Analysis != nil {
		return *res.Analysis
	}
	return ""
}

func truncateArchitectureMarkdown(markdown string) string {
	if len(markdown) <= maxArchitectureMarkdown {
		return markdown
	}
	return strings.ToValidUTF8(markdown[:maxArchitectureMarkdown], "") + "\n\n[truncated]"
}

// nativeArchitectureMeta mirrors the ocr.review-analysis/v2 report emitted in
// analysis_meta.
type nativeArchitectureMeta struct {
	SchemaVersion       string                     `json:"schema_version"`
	ExecutionStatus     string                     `json:"execution_status"`
	Comparison          nativeArchitectureCompare  `json:"comparison"`
	Scope               []nativeArchitectureChange `json:"scope"`
	SubmissionAccepted  bool                       `json:"submission_accepted"`
	DeclaredCompletion  string                     `json:"declared_completion"`
	StopReason          string                     `json:"stop_reason"`
	SubmissionStatus    string                     `json:"submission_status"`
	Errors              []string                   `json:"errors"`
	Usage               nativeArchitectureUsage    `json:"usage"`
	Model               string                     `json:"model"`
	SessionID           string                     `json:"session_id"`
	ArtifactDir         string                     `json:"artifact_dir"`
	ObservationIDs      []string                   `json:"observation_ids"`
	CitedObservationIDs []string                   `json:"cited_observation_ids"`
}

type nativeArchitectureCompare struct {
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	Mode        string `json:"mode"`
	Old         string `json:"old"`
	Head        string `json:"head"`
	BaseIsEmpty bool   `json:"base_is_empty"`
}

type nativeArchitectureChange struct {
	OldPath        string `json:"old_path"`
	NewPath        string `json:"new_path"`
	Deleted        bool   `json:"deleted"`
	Added          bool   `json:"added"`
	Renamed        bool   `json:"renamed"`
	ExcludedReason string `json:"excluded_reason"`
}

type nativeArchitectureUsage struct {
	TotalTokens      int64 `json:"total_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

func parseNativeArchitectureMeta(raw json.RawMessage) (*nativeArchitectureMeta, error) {
	if len(strings.TrimSpace(string(raw))) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var meta nativeArchitectureMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func nativeArchitectureScope(changes []nativeArchitectureChange) []store.ArchitectureScopeFile {
	scope := make([]store.ArchitectureScopeFile, 0, len(changes))
	for _, change := range changes {
		path := change.NewPath
		if path == "" {
			path = change.OldPath
		}
		kind := "modified"
		switch {
		case change.Deleted:
			kind = "deleted"
		case change.Added:
			kind = "added"
		case change.Renamed:
			kind = "renamed"
		}
		scope = append(scope, store.ArchitectureScopeFile{
			Path: normalizeCoveragePath(path), Kind: kind, Reason: change.ExcludedReason,
		})
	}
	return scope
}

var architectureHeadingPattern = regexp.MustCompile(`^#{2,3}[ \t]+(.+?)[ \t]*$`)

// parseArchitectureDimensions splits the accepted report into its Markdown
// sections. The report is prose by design, so sections are the structured unit
// the panel can show without reinterpreting the model's wording.
func parseArchitectureDimensions(markdown string) []store.ArchitectureDimension {
	var dimensions []store.ArchitectureDimension
	var current *store.ArchitectureDimension
	inFence := false
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			if current != nil {
				current.Content += line + "\n"
			}
			continue
		}
		if !inFence {
			if match := architectureHeadingPattern.FindStringSubmatch(line); match != nil {
				if current != nil {
					current.Content = strings.TrimSpace(current.Content)
					dimensions = append(dimensions, *current)
				}
				current = &store.ArchitectureDimension{Name: strings.TrimSpace(match[1])}
				continue
			}
		}
		if current == nil {
			continue
		}
		current.Content += line + "\n"
	}
	if current != nil {
		current.Content = strings.TrimSpace(current.Content)
		dimensions = append(dimensions, *current)
	}
	if len(dimensions) == 0 && strings.TrimSpace(markdown) != "" {
		dimensions = append(dimensions, store.ArchitectureDimension{
			Name: "Resumen", Content: strings.TrimSpace(markdown),
		})
	}
	return dimensions
}

var (
	architectureBehaviorHeadings = []string{
		"behavior", "behaviour", "comportamiento", "impact", "impacto",
		"before", "after", "antes", "después", "despues",
	}
	architectureRiskHeadings = []string{
		"risk", "riesgo", "compat", "compatibilidad",
	}
	architectureAlternativeHeadings = []string{
		"alternative", "alternativa", "tradeoff", "trade-off", "opción", "opcion", "opciones",
	}
	architectureAcceptanceHeadings = []string{
		"pre-merge", "maintainer", "mantenedor", "acceptance", "aceptación", "aceptacion",
		"decision", "decisión", "decisiones", "required fix", "closing", "cierre",
	}
	architectureUncertaintyHeadings = []string{
		"limit", "límite", "limite", "uncertain", "incert", "verification",
		"verificación", "verificacion", "evidence", "evidencia", "open question", "pregunta",
	}
	architectureRecommendationHeadings = []string{
		"recommendation", "recommendations", "recomendación", "recomendacion", "recomendar",
	}
)

func architectureSectionItems(dimensions []store.ArchitectureDimension, headings []string) []string {
	var items []string
	seen := make(map[string]bool)
	for _, dimension := range dimensions {
		if !containsAny(strings.ToLower(dimension.Name), headings) {
			continue
		}
		for _, line := range strings.Split(dimension.Content, "\n") {
			item := cleanArchitectureItem(line)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			items = append(items, item)
		}
	}
	return items
}

func cleanArchitectureItem(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "-")
	line = strings.TrimPrefix(line, "*")
	line = strings.TrimPrefix(line, "+")
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "```") || line == "---" {
		return ""
	}
	return line
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// gitArchitectureStats reads the numstat totals of the reviewed range. A binary
// file contributes no line counts, matching git's "-" markers.
func gitArchitectureStats(ctx context.Context, dir, baseRef, headRef string) (architectureChangeStats, error) {
	raw, err := git(ctx, dir, "diff --numstat", "--no-pager", "diff", "--numstat", "-z", baseRef, headRef)
	if err != nil {
		return architectureChangeStats{}, err
	}
	var stats architectureChangeStats
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
		if parts[2] == "" {
			// Rename: the next two tokens are old and new paths.
			i += 2
			stats.Renamed++
			continue
		}
		added, addedErr := strconv.Atoi(parts[0])
		removed, removedErr := strconv.Atoi(parts[1])
		if addedErr != nil || removedErr != nil {
			continue
		}
		stats.AddedLines += added
		stats.RemovedLines += removed
	}
	return stats, nil
}

func architectureStatsFromFiles(files []changedFile, stats architectureChangeStats) architectureChangeStats {
	stats.Files = 0
	stats.Renamed = 0
	for _, file := range files {
		if normalizeCoveragePath(file.Path) == "" {
			continue
		}
		stats.Files++
		if file.Renamed {
			stats.Renamed++
		}
	}
	return stats
}

// sortArchitectureSignals keeps the persisted order stable for tests and for
// the published summary.
func sortArchitectureSignals(signals []string) {
	sort.Strings(signals)
}

// architectureLogLevel colours the analysis line in the run log without
// conflating it with the review's terminal state.
func architectureLogLevel(status string) string {
	switch status {
	case store.ArchitectureComplete:
		return "info"
	case store.ArchitectureNotRequested:
		return "info"
	default:
		return "warn"
	}
}
