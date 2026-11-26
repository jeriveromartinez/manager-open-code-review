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

	// architectureTimeMax bounds the analysis phase even when the run is much
	// longer. OCR has no separate --analysis-timeout flag, so the worker
	// enforces this share as that phase's invocation deadline.
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
// the aggregate cannot exceed it. The time window is the share enforced as the
// analysis phase's invocation deadline; the run context still caps the total.
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

// markArchitectureTimedOut records that the analysis phase did not produce a
// trustworthy report, because it exhausted its share or the invocation failed.
// The review outcome, conclusion, QA and inline publication never depend on it.
func markArchitectureTimedOut(verdict store.ArchitectureVerdict, reason string) store.ArchitectureVerdict {
	if !verdict.Requested {
		return verdict
	}
	verdict.Status = store.ArchitectureInconclusive
	if strings.TrimSpace(reason) != "" {
		verdict.Errors = append(verdict.Errors, reason)
	}
	return verdict
}

// applyAnalysisDeadline downgrades a requested observation when the analysis
// share, not the run, closed the invocation. It reports whether the deadline
// applied so the caller can log it. The review outcome is untouched.
func applyAnalysisDeadline(verdict store.ArchitectureVerdict, res nativeReview, budget store.ArchitectureBudget) (store.ArchitectureVerdict, bool) {
	if !res.analysisTimedOut {
		return verdict, false
	}
	reason := fmt.Sprintf("analysis deadline exceeded: %ds share", budget.TimeBudgetSeconds)
	return markArchitectureTimedOut(verdict, reason), true
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

// architectureReportCitation is one source location parsed from the report's
// prose before the analysed scope decides which side it belongs to. The report
// has no structured citation field, so its own wording is the source; a side
// marker in the same clause (old/new, before/after, base/head) keeps a
// previous-side line from being published as current code.
type architectureReportCitation struct {
	Path      string
	StartLine int
	EndLine   int
	Side      string
}

var (
	architectureCitationExtensions = []string{
		"go", "ts", "tsx", "js", "jsx", "mjs", "cjs", "vue", "svelte", "py",
		"rs", "java", "kt", "kts", "rb", "php", "cs", "c", "cc", "cpp", "h",
		"hpp", "swift", "scala", "ex", "exs", "dart", "sql", "yaml", "yml",
		"json", "toml", "mod", "sum", "proto", "graphql", "gql", "md", "sh",
	}
	architectureCitationPathPattern = `[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*\.(?:` +
		strings.Join(architectureCitationExtensions, "|") + `)`
	architectureCitationQuotedPathPattern = `[\x60"'\[]?` + architectureCitationPathPattern + `[\x60"'\]]?`
	architectureCitationSideWords         = `previous|prev|old|older|base|before|deleted|removed|left|` +
		`antes|anteriores|anterior|previos|previas|previo|previa|` +
		`eliminados|eliminadas|eliminado|eliminada|borrados|borradas|borrado|borrada|` +
		`new|newer|current|head|after|now|right|added|` +
		`ahora|actuales|actual|nuevos|nuevas|nuevo|nueva|despu[eé]s|despues|` +
		`agregados|agregadas|agregado|agregada`
	architectureCitationSidePattern = `(?:` + architectureCitationSideWords + `)`
	// Side qualifiers sit next to the location they qualify. A qualifier that
	// is not immediately followed by a path ("the old behavior ...") is prose,
	// not a side marker, and must not relabel a citation.
	architectureCitationPrefixPattern = `(?:(?:(?:en|in|on)\s+)?(?:(?:el|la|the)\s+)?` +
		`(?:(?:lado|side)\s+)?` + architectureCitationSidePattern +
		`(?:\s+(?:side|lado))?\s*(?:\s*[:,]\s*|\s+))`
	architectureCitationSuffixPattern = `(?:\s*[\(\[]\s*` + architectureCitationSidePattern +
		`(?:\s+(?:side|lado))?\s*[\)\]])?`
	architectureCitationLocationPattern = `(?:\s*:\s*\d+(?:\s*[-–—]\s*\d+)?(?::\s*\d+)?` +
		`|#L\d+(?:-L?\d+)?` +
		`|[ \t]*[\(\[]\s*(?:lines?|l[ií]neas?|ln)\s*[:#]?\s*\d+(?:\s*[-–—]\s*\d+)?\s*[\)\]])`
	architectureCitationPathSeparator = `(?:[ \t]*,[ \t]*|[ \t]+(?:y|and)[ \t]+)`

	// A leading boundary keeps URLs, absolute paths and prose tokens out. The
	// grouped form exists because one citation can cover several files that
	// share a single location.
	architectureCitationPattern = regexp.MustCompile(
		`(?i)(?:^|[^\w/.-])(?P<prefix>` + architectureCitationPrefixPattern + `)?` +
			`[\x60"'\[]?(?P<path>` + architectureCitationPathPattern + `)[\x60"'\]]?[ \t]*` +
			`(?P<loc>` + architectureCitationLocationPattern + `)[\x60"'\]]?` +
			`(?P<suffix>` + architectureCitationSuffixPattern + `)`)
	architectureGroupedCitationPattern = regexp.MustCompile(
		`(?i)(?:^|[^\w/.-])(?P<prefix>` + architectureCitationPrefixPattern + `)?` +
			`(?P<paths>` + architectureCitationQuotedPathPattern +
			`(?:` + architectureCitationPathSeparator + architectureCitationQuotedPathPattern + `)+)` +
			`[ \t]*(?P<loc>` + architectureCitationLocationPattern + `)[\x60"'\]]?` +
			`(?P<suffix>` + architectureCitationSuffixPattern + `)`)
	architectureCitationPathSplitPattern      = regexp.MustCompile(architectureCitationPathSeparator)
	architectureCitationSideWordPattern       = regexp.MustCompile(`(?i)\b(?:` + architectureCitationSideWords + `)\b`)
	architectureCitationStandaloneSidePattern = regexp.MustCompile(
		`(?i)^(?:(?:en|in|on)\s+)?(?:(?:el|la|the)\s+)?(?:(?:lado|side)\s+)?(` +
			architectureCitationSideWords + `)(?:\s+(?:side|lado|version|versión|revision|revisión))?$`)

	architectureCitationColonLocation  = regexp.MustCompile(`^\s*:\s*(\d+)(?:\s*[-–—]\s*(\d+))?(?::\s*\d+)?$`)
	architectureCitationAnchorLocation = regexp.MustCompile(`^#L(\d+)(?:-L?(\d+))?$`)
	architectureCitationWordsLocation  = regexp.MustCompile(
		`(?i)^[ \t]*[\(\[]\s*(?:lines?|l[ií]neas?|ln)\s*[:#]?\s*(\d+)(?:\s*[-–—]\s*(\d+))?\s*[\)\]]$`)

	architectureCitationBaseWords = map[string]bool{
		"previous": true, "prev": true, "old": true, "older": true, "base": true,
		"before": true, "deleted": true, "removed": true, "left": true,
		"antes": true, "anteriores": true, "anterior": true, "previos": true,
		"previas": true, "previo": true, "previa": true, "eliminados": true,
		"eliminadas": true, "eliminado": true, "eliminada": true, "borrados": true,
		"borradas": true, "borrado": true, "borrada": true,
	}
	architectureCitationHeadWords = map[string]bool{
		"new": true, "newer": true, "current": true, "head": true, "after": true,
		"now": true, "right": true, "added": true, "ahora": true, "actuales": true,
		"actual": true, "nuevos": true, "nuevas": true, "nuevo": true, "nueva": true,
		"después": true, "despues": true, "agregados": true, "agregadas": true,
		"agregado": true, "agregada": true,
	}
)

// architectureEvidenceCitations extracts the structured citations from the
// accepted report. Each citation is anchored to the analysed scope and to its
// own side, so a previous-side line and a current line of the same path stay
// distinct. Citations outside the compared scope are evidence about code this
// verdict cannot vouch for and are dropped.
func architectureEvidenceCitations(verdict store.ArchitectureVerdict) []store.FindingCitation {
	sides := architectureCitationSides(verdict)
	added := architectureAddedScopePaths(verdict)
	var citations []store.FindingCitation
	for _, text := range architectureCitationTexts(verdict) {
		for _, parsed := range parseArchitectureReportCitations(text) {
			defaultSide, ok := sides[parsed.Path]
			if !ok {
				continue
			}
			side := defaultSide
			switch parsed.Side {
			case store.FindingCitationBase:
				// The previous revision of a path that still exists at head is
				// real evidence, even though the default side is the head. An
				// added file has no previous revision to cite.
				if !added[parsed.Path] {
					side = store.FindingCitationBase
				}
			case store.FindingCitationHead:
				// A deleted path has no head side; its only comparable code is
				// the previous revision.
				if defaultSide != store.FindingCitationBase {
					side = store.FindingCitationHead
				}
			}
			citations = append(citations, store.FindingCitation{
				Path: parsed.Path, StartLine: parsed.StartLine, EndLine: parsed.EndLine, Side: side,
			})
		}
	}
	return store.NormalizeFindingCitations(citations)
}

// architectureAddedScopePaths records the files that only exist at head, so a
// previous-side qualifier cannot invent a base citation for them.
func architectureAddedScopePaths(verdict store.ArchitectureVerdict) map[string]bool {
	added := make(map[string]bool)
	add := func(file store.ArchitectureScopeFile) {
		if !strings.EqualFold(file.Kind, "added") {
			return
		}
		if path := normalizeCoveragePath(file.Path); path != "" {
			added[path] = true
		}
	}
	for _, file := range verdict.Scope.Included {
		add(file)
	}
	for _, file := range verdict.Scope.Excluded {
		add(file)
	}
	for _, file := range verdict.Evidence.EngineScope {
		add(file)
	}
	return added
}

// architectureCitationTexts returns each report unit separately so a path list
// never joins the end of one unit with the start of the next. Dimensions hold
// the full section prose; the extracted items are only a fallback for verdicts
// that carry no dimensions, because losing their surrounding qualifier would
// publish the same line as both sides.
func architectureCitationTexts(verdict store.ArchitectureVerdict) []string {
	texts := []string{verdict.Evidence.Markdown}
	if len(verdict.Dimensions) > 0 {
		for _, dimension := range verdict.Dimensions {
			texts = append(texts, dimension.Content)
		}
		return texts
	}
	for _, items := range [][]string{
		verdict.BehaviorChanges, verdict.Risks, verdict.Alternatives,
		verdict.AcceptanceConditions, verdict.Uncertainties,
	} {
		texts = append(texts, items...)
	}
	return texts
}

func parseArchitectureReportCitations(text string) []architectureReportCitation {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var citations []architectureReportCitation
	state := ""
	for _, line := range strings.Split(text, "\n") {
		if architectureCitationSectionBreak(line) {
			state = ""
		}
		if side := architectureStandaloneSide(line); side != "" {
			// "Antes:" on its own line qualifies the list that follows.
			state = side
			continue
		}
		parsed, next := parseArchitectureCitationLine(line, state)
		citations = append(citations, parsed...)
		state = next
	}
	return citations
}

// architectureCitationSectionBreak starts a new unit whose prose must not
// inherit the previous section's side.
func architectureCitationSectionBreak(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// architectureStandaloneSide returns the side of a line that only holds a
// qualifier ("Antes:", "**Old side:**", "- Base version:"). The pattern is
// anchored, so prose that merely contains a side word does not qualify.
func architectureStandaloneSide(line string) string {
	text := strings.TrimSpace(line)
	if text == "" {
		return ""
	}
	text = strings.TrimLeft(text, "#-+>*_ \t")
	text = strings.Trim(text, "*_ \t")
	text = strings.TrimRight(text, ":：-–— \t")
	match := architectureCitationStandaloneSidePattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return ""
	}
	return architectureCitationSide(match[1])
}

type architectureCitationMatch struct {
	start, end int
	paths      []string
	loc        string
	side       string
}

// parseArchitectureCitationLine keeps the side state of a clause: in "Antes
// a.go:5 y b.go:7; ahora c.go:9" both paths before the semicolon are previous
// side even though only the first one carries the qualifier.
func parseArchitectureCitationLine(line, state string) ([]architectureReportCitation, string) {
	if strings.TrimSpace(line) == "" {
		return nil, state
	}
	matches := architectureCitationLineMatches(line)
	var citations []architectureReportCitation
	lastEnd := 0
	for _, match := range matches {
		// The leading boundary is consumed by the match, so include it in the
		// gap: otherwise a ';' immediately before the next citation is unseen
		// and the previous clause's side leaks into it.
		if lastEnd <= match.start {
			gapEnd := match.start + 1
			if gapEnd > len(line) {
				gapEnd = len(line)
			}
			if architectureCitationClauseBreak(line[lastEnd:gapEnd]) {
				state = ""
			}
		}
		if match.side != "" {
			state = match.side
		}
		side := match.side
		if side == "" {
			side = state
		}
		for _, rawPath := range match.paths {
			path := normalizeArchitectureCitationPath(rawPath)
			if path == "" {
				continue
			}
			start, end, ok := parseArchitectureCitationLocation(match.loc)
			if !ok {
				continue
			}
			citations = append(citations, architectureReportCitation{
				Path: path, StartLine: start, EndLine: end, Side: side,
			})
		}
		if match.end > lastEnd {
			lastEnd = match.end
		}
	}
	return citations, state
}

func architectureCitationLineMatches(line string) []architectureCitationMatch {
	var matches []architectureCitationMatch
	for _, index := range architectureGroupedCitationPattern.FindAllStringSubmatchIndex(line, -1) {
		paths := architectureCitationSubmatch(architectureGroupedCitationPattern, line, index, "paths")
		if paths == "" {
			continue
		}
		matches = append(matches, architectureCitationMatch{
			start: index[0], end: index[1],
			paths: splitArchitectureCitationPaths(paths),
			loc:   architectureCitationSubmatch(architectureGroupedCitationPattern, line, index, "loc"),
			side:  architectureCitationMatchSide(architectureGroupedCitationPattern, line, index),
		})
	}
	for _, index := range architectureCitationPattern.FindAllStringSubmatchIndex(line, -1) {
		path := architectureCitationSubmatch(architectureCitationPattern, line, index, "path")
		if path == "" {
			continue
		}
		matches = append(matches, architectureCitationMatch{
			start: index[0], end: index[1],
			paths: []string{path},
			loc:   architectureCitationSubmatch(architectureCitationPattern, line, index, "loc"),
			side:  architectureCitationMatchSide(architectureCitationPattern, line, index),
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].end > matches[j].end
	})
	return matches
}

func architectureCitationSubmatch(re *regexp.Regexp, line string, index []int, name string) string {
	position := re.SubexpIndex(name)
	if position < 0 {
		return ""
	}
	start, end := index[2*position], index[2*position+1]
	if start < 0 || end < 0 || end > len(line) {
		return ""
	}
	return line[start:end]
}

func architectureCitationMatchSide(re *regexp.Regexp, line string, index []int) string {
	for _, name := range []string{"prefix", "suffix"} {
		raw := architectureCitationSubmatch(re, line, index, name)
		if raw == "" {
			continue
		}
		if side := architectureCitationSide(raw); side != "" {
			return side
		}
	}
	return ""
}

func architectureCitationSide(text string) string {
	word := architectureCitationSideWordPattern.FindString(text)
	if word == "" {
		return ""
	}
	lower := strings.ToLower(word)
	switch {
	case architectureCitationBaseWords[lower]:
		return store.FindingCitationBase
	case architectureCitationHeadWords[lower]:
		return store.FindingCitationHead
	default:
		return ""
	}
}

func architectureCitationClauseBreak(text string) bool {
	return strings.ContainsAny(text, ";!?") ||
		strings.Contains(text, ". ") ||
		strings.HasSuffix(strings.TrimSpace(text), ".")
}

func splitArchitectureCitationPaths(raw string) []string {
	parts := architectureCitationPathSplitPattern.Split(raw, -1)
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}

func normalizeArchitectureCitationPath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\x60'\"()[]{}<>,.;:")
	if raw == "" || strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "~") || strings.ContainsAny(raw, " \t") {
		return ""
	}
	// Reject parent segments before path.Clean can resolve them away.
	raw = strings.ReplaceAll(raw, `\`, "/")
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return ""
		}
	}
	path := normalizeCoveragePath(raw)
	if path == "" {
		return ""
	}
	extension := strings.ToLower(fileExtension(path))
	for _, allowed := range architectureCitationExtensions {
		if extension == "."+allowed {
			return path
		}
	}
	return ""
}

func parseArchitectureCitationLocation(raw string) (int, int, bool) {
	raw = strings.TrimSpace(raw)
	for _, pattern := range []*regexp.Regexp{
		architectureCitationColonLocation,
		architectureCitationAnchorLocation,
		architectureCitationWordsLocation,
	} {
		match := pattern.FindStringSubmatch(raw)
		if match == nil {
			continue
		}
		start, err := strconv.Atoi(match[1])
		if err != nil || start <= 0 {
			return 0, 0, false
		}
		end := start
		if match[2] != "" {
			parsed, parseErr := strconv.Atoi(match[2])
			if parseErr != nil {
				return 0, 0, false
			}
			end = parsed
		}
		// A reversed range is malformed: publishing its start line would claim
		// a location the report never stated.
		if end < start {
			return 0, 0, false
		}
		return start, end, true
	}
	return 0, 0, false
}

// architectureCitationSides maps every comparable path of the verdict to the
// side its lines belong to. A deleted file exists only in the base, so its
// evidence is a LEFT citation; binary, submodule and policy exclusions are not
// current code and are left out entirely.
func architectureCitationSides(verdict store.ArchitectureVerdict) map[string]string {
	sides := make(map[string]string)
	add := func(file store.ArchitectureScopeFile, includeHead bool) {
		path := normalizeCoveragePath(file.Path)
		if path == "" {
			return
		}
		if isBaseOnlyScopeFile(file) {
			sides[path] = store.FindingCitationBase
			return
		}
		if includeHead {
			sides[path] = store.FindingCitationHead
		}
	}
	for _, file := range verdict.Scope.Included {
		add(file, true)
	}
	for _, file := range verdict.Scope.Excluded {
		add(file, false)
	}
	for _, file := range verdict.Evidence.EngineScope {
		add(file, file.Reason == "")
	}
	return sides
}

func isBaseOnlyScopeFile(file store.ArchitectureScopeFile) bool {
	return strings.EqualFold(file.Kind, "deleted") || strings.EqualFold(file.Reason, "deleted")
}

// ArchitectureFindings turns the accepted architectural verdict into at most
// one advisory design finding (#162). It is a producer only: the finding goes
// through the normal identity and publication path, but its advisory flag
// keeps it out of the check, the inline comments and the verifier decisions.
func ArchitectureFindings(verdict store.ArchitectureVerdict) []AgentFinding {
	if !verdict.Requested ||
		(verdict.Status != store.ArchitectureComplete && verdict.Status != store.ArchitecturePartial) {
		return nil
	}
	title := architectureFindingTitle(verdict)
	if title == "" {
		return nil
	}
	citations := architectureEvidenceCitations(verdict)

	finding := AgentFinding{
		Title:     title,
		Body:      architectureFindingBody(verdict),
		Severity:  "minor",
		Category:  "design",
		Scope:     store.FindingScopePR,
		Citations: citations,
		Advisory:  true,
	}
	if len(citations) > 0 {
		first := citations[0]
		finding.Path = first.Path
		finding.StartLine, finding.EndLine = first.StartLine, first.EndLine
		finding.Scope = store.NormalizeFindingScope("", citations)
	}
	return []AgentFinding{finding}
}

func architectureFindingTitle(verdict store.ArchitectureVerdict) string {
	items := []string{}
	for _, group := range [][]string{
		verdict.Risks, verdict.AcceptanceConditions, verdict.BehaviorChanges,
		verdict.Uncertainties,
	} {
		items = append(items, group...)
	}
	if len(items) == 0 && strings.TrimSpace(verdict.Recommendation) != "" {
		items = strings.Split(verdict.Recommendation, "\n")
	}
	for _, item := range items {
		if title := strings.TrimSpace(cleanArchitectureItem(item)); title != "" {
			return "Arquitectura: " + truncateArchitectureTitle(title)
		}
	}
	return ""
}

func truncateArchitectureTitle(title string) string {
	const limit = 110
	if len([]rune(title)) <= limit {
		return title
	}
	return string([]rune(title)[:limit-1]) + "…"
}

func architectureFindingBody(verdict store.ArchitectureVerdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Análisis arquitectónico %s en %s.",
		architectureFindingStatus(verdict.Status), architectureFindingMode(verdict.Mode))
	if len(verdict.Signals) > 0 {
		fmt.Fprintf(&b, " Señales: %s.", strings.Join(verdict.Signals, ", "))
	}
	if len(verdict.Dimensions) > 0 {
		names := make([]string, 0, len(verdict.Dimensions))
		for _, dimension := range verdict.Dimensions {
			names = append(names, dimension.Name)
		}
		fmt.Fprintf(&b, "\n\nDimensiones: %s.", strings.Join(names, " · "))
	}
	for _, group := range []struct {
		label string
		items []string
	}{
		{"Comportamiento antes/después", verdict.BehaviorChanges},
		{"Riesgos", verdict.Risks},
		{"Alternativas", verdict.Alternatives},
		{"Condiciones de aceptación", verdict.AcceptanceConditions},
		{"Incertidumbres", verdict.Uncertainties},
	} {
		writeArchitectureFindingItems(&b, group.label, group.items)
	}
	if recommendation := strings.TrimSpace(verdict.Recommendation); recommendation != "" {
		fmt.Fprintf(&b, "\n\nRecomendación: %s", recommendation)
	}
	return b.String()
}

func writeArchitectureFindingItems(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\n%s:\n", label)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}

func architectureFindingStatus(status string) string {
	switch status {
	case store.ArchitectureComplete:
		return "completo"
	case store.ArchitecturePartial:
		return "parcial"
	case store.ArchitectureInconclusive:
		return "inconcluso"
	default:
		return status
	}
}

func architectureFindingMode(mode string) string {
	if mode == store.ArchitectureAnalysisObservation {
		return "modo observación"
	}
	return mode
}

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
	case store.ArchitectureStale:
		return "warn"
	default:
		return "warn"
	}
}
