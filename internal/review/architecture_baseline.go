package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	baselineRefBase = "base"
	baselineRefHead = "head"

	architectureBaselineDimensionName = "Línea base de arquitectura"
)

// baselineTree reads repository files at the trusted base commit and at the
// reviewed head. The worker backs it with git; tests use an in-memory map so
// the baseline logic stays deterministic and independent from the shell.
type baselineTree interface {
	Read(ref, file string) ([]byte, bool, error)
	Blob(ref, file string) (string, bool, error)
}

// gitBaselineTree resolves content from an existing checkout. Both refs are
// commit SHAs: base is the reviewed base, never the pull request head.
type gitBaselineTree struct {
	ctx  context.Context
	dir  string
	base string
	head string
}

func (g gitBaselineTree) commit(ref string) string {
	if ref == baselineRefHead {
		return g.head
	}
	return g.base
}

func (g gitBaselineTree) Blob(ref, file string) (string, bool, error) {
	sha, err := git(g.ctx, g.dir, "rev-parse --verify", "rev-parse", "--verify", "--quiet",
		g.commit(ref)+":"+file)
	if err != nil {
		// rev-parse --verify exits 1 for a path that does not exist at the
		// commit. Any other failure means the repository or the commit could
		// not be read, which must not be reported as a missing baseline.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return sha, true, nil
}

func (g gitBaselineTree) Read(ref, file string) ([]byte, bool, error) {
	sha, ok, err := g.Blob(ref, file)
	if err != nil || !ok {
		return nil, ok, err
	}
	content, err := git(g.ctx, g.dir, "show", "--no-pager", "show", sha)
	if err != nil {
		return nil, true, err
	}
	return []byte(content), true, nil
}

// architectureBaselineFile is the on-disk rule contract. It is JSON so the
// worker needs no new dependency and the file stays byte-reproducible.
type architectureBaselineFile struct {
	Version    string                              `json:"version"`
	Decisions  []architectureBaselineDecisionSpec  `json:"decisions"`
	Rules      []architectureBaselineRuleSpec      `json:"rules"`
	Exceptions []architectureBaselineExceptionSpec `json:"exceptions"`
}

type architectureBaselineDecisionSpec struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

type architectureBaselineRuleSpec struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	From    string `json:"from"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
}

type architectureBaselineExceptionSpec struct {
	ID         string `json:"id"`
	Rule       string `json:"rule"`
	From       string `json:"from"`
	To         string `json:"to"`
	Reason     string `json:"reason"`
	ApprovedBy string `json:"approved_by"`
}

// checkArchitectureBaseline resolves ADRs and dependency rules from the trusted
// base commit, compares them with the head, and checks the imports this change
// adds. It never returns an error: missing, unreadable or self-modified rules
// degrade to unknown instead of passing.
func checkArchitectureBaseline(tree baselineTree, policy store.QAPolicy, changes []changedFile) store.ArchitectureBaseline {
	rulesPath := strings.TrimSpace(policy.ArchitectureBaselinePath)
	if rulesPath == "" {
		rulesPath = store.DefaultArchitectureBaselinePath
	}
	exceptionsPolicy := strings.TrimSpace(policy.ArchitectureExceptionPolicy)
	if exceptionsPolicy == "" {
		exceptionsPolicy = store.DefaultArchitectureExceptionPolicy
	}

	baseline := store.ArchitectureBaseline{
		Status:    store.ArchitectureBaselineUnknown,
		RulesPath: rulesPath,
		Controls: store.ArchitectureBaselineControls{
			BaseOnly:         true,
			ExceptionsPolicy: exceptionsPolicy,
		},
	}
	unknown := false
	note := func(format string, args ...any) {
		unknown = true
		baseline.Errors = append(baseline.Errors, fmt.Sprintf(format, args...))
	}

	baseSHA, baseExists, baseErr := tree.Blob(baselineRefBase, rulesPath)
	if baseErr != nil {
		note("no se pudo comprobar la línea base %s: %v", rulesPath, baseErr)
		return baseline
	}
	headSHA, headExists, headErr := tree.Blob(baselineRefHead, rulesPath)
	if headErr != nil {
		note("no se pudo comprobar la regla propuesta %s: %v", rulesPath, headErr)
		return baseline
	}
	baseline.RulesSHA = baseSHA
	if headExists {
		baseline.RulesHeadSHA = headSHA
	}
	baseline.ChangedInPR = (baseExists && (!headExists || !strings.EqualFold(headSHA, baseSHA))) ||
		(!baseExists && headExists)
	baseline.Controls.RuleFileChanged = baseline.ChangedInPR

	if !baseExists {
		baseline.Controls.BaselineMissing = true
		note("no existe una línea base en %s; la comprobación no puede aprobarse", rulesPath)
		return baseline
	}
	raw, ok, err := tree.Read(baselineRefBase, rulesPath)
	if err != nil {
		note("la línea base %s no se pudo leer: %v; la comprobación no puede aprobarse", rulesPath, err)
		return baseline
	}
	if !ok {
		note("la línea base %s desapareció de la base; la comprobación no puede aprobarse", rulesPath)
		return baseline
	}
	var file architectureBaselineFile
	trimmedRaw := strings.TrimSpace(string(raw))
	if trimmedRaw == "" || trimmedRaw == "null" {
		note("la línea base %s está vacía; la comprobación no puede aprobarse", rulesPath)
		return baseline
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		note("la línea base %s no es JSON válido: %v; la comprobación no puede aprobarse", rulesPath, err)
		return baseline
	}
	baseline.RulesVersion = strings.TrimSpace(file.Version)
	if baseline.RulesVersion == "" {
		note("la línea base %s no declara versión; la comprobación no puede aprobarse", rulesPath)
	}
	if len(file.Rules) == 0 && len(file.Decisions) == 0 {
		note("la línea base %s no declara reglas ni decisiones; la comprobación no puede aprobarse", rulesPath)
	}

	if baseline.ChangedInPR {
		note("la regla cambió en el PR; se evalúa la versión de la base %s", baseSHA)
	}

	ruleIndex := make(map[string]architectureBaselineRuleSpec, len(file.Rules))
	for _, rule := range file.Rules {
		id := strings.TrimSpace(rule.ID)
		from := normalizeBaselinePrefix(rule.From)
		to := normalizeBaselinePrefix(rule.To)
		if id == "" || from == "" || to == "" {
			note("regla de dependencia inválida en %s: id, from y to son obligatorios", rulesPath)
			continue
		}
		if _, duplicate := ruleIndex[id]; duplicate {
			note("regla de dependencia duplicada %s; se ignora", id)
			continue
		}
		ruleIndex[id] = rule
		baseline.Rules = append(baseline.Rules, store.ArchitectureBaselineRule{
			ID: id, Version: strings.TrimSpace(rule.Version), From: from, To: to,
			Reason: strings.TrimSpace(rule.Reason),
		})
	}
	sort.SliceStable(baseline.Rules, func(i, j int) bool { return baseline.Rules[i].ID < baseline.Rules[j].ID })
	baseline.Controls.RulesChecked = len(baseline.Rules)

	for _, decision := range file.Decisions {
		baseline.Decisions = append(baseline.Decisions,
			resolveBaselineDecision(tree, decision, note))
	}
	sort.SliceStable(baseline.Decisions, func(i, j int) bool { return baseline.Decisions[i].ID < baseline.Decisions[j].ID })
	for _, decision := range baseline.Decisions {
		if decision.Status == store.ArchitectureDecisionResolved {
			baseline.Controls.DecisionsChecked++
		}
	}

	baseModule := readGoModule(tree, baselineRefBase, note)
	headModule := readGoModule(tree, baselineRefHead, note)
	module := headModule
	if module == "" {
		module = baseModule
	}
	moduleChanged := baseModule != "" && headModule != "" && baseModule != headModule
	if moduleChanged {
		// The head imports live under a different module path, so a rule
		// resolved from the base cannot be compared with them reliably.
		note("el módulo Go cambió en el PR (%s -> %s); las reglas de dependencia no son verificables",
			baseModule, headModule)
	}
	if len(baseline.Rules) > 0 && module == "" {
		note("no se pudo resolver el módulo Go; las reglas de dependencia no son verificables")
	}
	if module != "" && !moduleChanged {
		baseline.Checks, baseline.Controls.FilesChecked = architectureDependencyChecks(tree, changes,
			module, ruleIndex, file.Exceptions, exceptionsPolicy, note)
		for _, check := range baseline.Checks {
			if check.Status == store.ArchitectureDependencyException {
				baseline.Controls.ExceptionsApplied++
			}
		}
	}

	violated := false
	for _, check := range baseline.Checks {
		if check.Status == store.ArchitectureDependencyViolation {
			violated = true
			break
		}
	}
	switch {
	case violated:
		// A violation found under the trusted base wins over every degradation:
		// the change cannot approve the rule it breaks.
		baseline.Status = store.ArchitectureBaselineViolation
	case unknown:
		baseline.Status = store.ArchitectureBaselineUnknown
	default:
		baseline.Status = store.ArchitectureBaselinePass
	}
	return baseline
}

func resolveBaselineDecision(tree baselineTree, spec architectureBaselineDecisionSpec,
	note func(string, ...any)) store.ArchitectureBaselineDecision {
	decision := store.ArchitectureBaselineDecision{
		ID:      strings.TrimSpace(spec.ID),
		Path:    strings.TrimSpace(spec.Path),
		Version: strings.TrimSpace(spec.Version),
	}
	if decision.ID == "" || decision.Path == "" {
		decision.Status = store.ArchitectureDecisionMissing
		note("decisión de arquitectura inválida: id y path son obligatorios")
		return decision
	}
	sha, exists, err := tree.Blob(baselineRefBase, decision.Path)
	if err != nil {
		decision.Status = store.ArchitectureDecisionUnreadable
		note("el ADR %s no se pudo comprobar (%s): %v", decision.ID, decision.Path, err)
		return decision
	}
	decision.SHA = sha
	headSHA, headExists, err := tree.Blob(baselineRefHead, decision.Path)
	if err != nil {
		decision.Status = store.ArchitectureDecisionUnreadable
		note("no se pudo comprobar la versión propuesta del ADR %s (%s): %v", decision.ID, decision.Path, err)
		return decision
	}
	switch {
	case !exists:
		decision.Status = store.ArchitectureDecisionMissing
		note("el ADR %s no existe en la base (%s)", decision.ID, decision.Path)
	case !headExists || !strings.EqualFold(headSHA, sha):
		decision.Status = store.ArchitectureDecisionReplaced
		note("el ADR %s se reemplazó en el PR; no se puede validar la decisión contra sí misma", decision.ID)
	default:
		if _, ok, err := tree.Read(baselineRefBase, decision.Path); err != nil || !ok {
			decision.Status = store.ArchitectureDecisionUnreadable
			note("el ADR %s no se pudo leer (%s)", decision.ID, decision.Path)
			break
		}
		decision.Status = store.ArchitectureDecisionResolved
	}
	return decision
}

// architectureDependencyChecks compares the imports of every changed Go file
// with its base version and checks only the imports this change adds.
func architectureDependencyChecks(tree baselineTree, changes []changedFile, module string,
	rules map[string]architectureBaselineRuleSpec, exceptions []architectureBaselineExceptionSpec,
	exceptionsPolicy string, note func(string, ...any)) ([]store.ArchitectureDependencyCheck, int) {
	var checks []store.ArchitectureDependencyCheck
	filesChecked := 0
	// Rules and exceptions may declare a repository-relative module or the
	// fully-qualified import path; both describe the same boundary.
	normalizedRules := make(map[string]architectureBaselineRuleSpec, len(rules))
	for id, rule := range rules {
		rule.From = baselineRelative(module, rule.From)
		rule.To = baselineRelative(module, rule.To)
		normalizedRules[id] = rule
	}
	normalizedExceptions := make([]architectureBaselineExceptionSpec, len(exceptions))
	for i, exception := range exceptions {
		exception.From = baselineRelative(module, exception.From)
		exception.To = baselineRelative(module, exception.To)
		normalizedExceptions[i] = exception
	}
	ruleIDs := make([]string, 0, len(normalizedRules))
	for id := range normalizedRules {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)

	for _, change := range changes {
		file := normalizeCoveragePath(change.Path)
		if file == "" || change.Kind == changedDeleted || strings.ToLower(fileExtension(file)) != ".go" {
			continue
		}
		headImports, err := baselineGoImports(tree, baselineRefHead, file)
		if err != nil {
			note("no se pudieron leer los imports de %s: %v", file, err)
			continue
		}
		filesChecked++
		baseImports := make(map[string]bool)
		if change.Kind != changedAdded {
			basePath := file
			if change.Renamed {
				if change.OldPath == "" {
					note("el renombrado de %s no trae su ruta de origen; no se puede atribuir el cambio", file)
					continue
				}
				basePath = change.OldPath
			}
			rawBase, hasBase, readErr := tree.Read(baselineRefBase, basePath)
			switch {
			case readErr != nil:
				note("no se pudo leer la base de %s (%s): %v", file, basePath, readErr)
				continue
			case !hasBase && change.Renamed:
				note("no se encontró el origen del renombrado %s (%s); no se puede atribuir el cambio", file, basePath)
				continue
			case hasBase:
				baseImports, err = parseGoImports(rawBase)
				if err != nil {
					note("no se pudieron leer los imports base de %s: %v", file, err)
					continue
				}
			}
		}
		packagePath := goPackagePath(file)
		headChecks := architectureEdges(file, packagePath, module, headImports,
			normalizedRules, ruleIDs, normalizedExceptions, exceptionsPolicy)
		if change.Kind == changedAdded {
			checks = append(checks, headChecks...)
			continue
		}
		basePackage := packagePath
		if change.Renamed {
			basePackage = goPackagePath(change.OldPath)
		}
		baseChecks := architectureEdges(file, basePackage, module, baseImports,
			normalizedRules, ruleIDs, normalizedExceptions, exceptionsPolicy)
		existing := make(map[string]bool, len(baseChecks))
		for _, check := range baseChecks {
			existing[architectureCheckKey(check)] = true
		}
		// An edge is attributable to the change only when the base file did not
		// already have it. That includes a rename that moves a file into a
		// forbidden module without touching its imports.
		for _, check := range headChecks {
			if !existing[architectureCheckKey(check)] {
				checks = append(checks, check)
			}
		}
	}
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].RuleID != checks[j].RuleID {
			return checks[i].RuleID < checks[j].RuleID
		}
		if checks[i].File != checks[j].File {
			return checks[i].File < checks[j].File
		}
		return checks[i].Import < checks[j].Import
	})
	return checks, filesChecked
}

// architectureEdges expands one file's imports into the dependency edges the
// rules forbid, including whether an authorized exception applies.
func architectureEdges(file, packagePath, module string, imports map[string]bool,
	rules map[string]architectureBaselineRuleSpec, ruleIDs []string,
	exceptions []architectureBaselineExceptionSpec, exceptionsPolicy string) []store.ArchitectureDependencyCheck {
	names := make([]string, 0, len(imports))
	for imported := range imports {
		names = append(names, imported)
	}
	sort.Strings(names)

	var checks []store.ArchitectureDependencyCheck
	for _, imported := range names {
		target, internal := baselineInternalImport(module, imported)
		if !internal {
			continue
		}
		for _, ruleID := range ruleIDs {
			rule := rules[ruleID]
			if !baselinePrefixMatches(rule.From, packagePath) || !baselinePrefixMatches(rule.To, target) {
				continue
			}
			check := store.ArchitectureDependencyCheck{
				RuleID:     ruleID,
				FromModule: packagePath,
				ToModule:   target,
				File:       file,
				Import:     imported,
				Status:     store.ArchitectureDependencyViolation,
			}
			if exceptionID, ok := authorizedBaselineException(exceptions, ruleID, packagePath, target, exceptionsPolicy); ok {
				check.Status = store.ArchitectureDependencyException
				check.ExceptionID = exceptionID
			}
			checks = append(checks, check)
		}
	}
	return checks
}

func architectureCheckKey(check store.ArchitectureDependencyCheck) string {
	return check.RuleID + "\x00" + check.FromModule + "\x00" + check.ToModule + "\x00" + check.Import
}

func baselineGoImports(tree baselineTree, ref, file string) (map[string]bool, error) {
	raw, ok, err := tree.Read(ref, file)
	if err != nil {
		return nil, err
	}
	if !ok {
		return make(map[string]bool), nil
	}
	return parseGoImports(raw)
}

func parseGoImports(raw []byte) (map[string]bool, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "", raw, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make(map[string]bool, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imports[imported] = true
	}
	return imports, nil
}

func parseGoModulePath(gomod string) string {
	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
	}
	return ""
}

func readGoModule(tree baselineTree, ref string, note func(string, ...any)) string {
	raw, ok, err := tree.Read(ref, "go.mod")
	if err != nil {
		note("no se pudo leer go.mod de %s: %v", ref, err)
		return ""
	}
	if !ok {
		return ""
	}
	return parseGoModulePath(string(raw))
}

// baselineInternalImport maps an import to the repository-relative package.
// Imports outside the module are not covered by module rules.
func baselineInternalImport(module, imported string) (string, bool) {
	imported = strings.TrimSpace(imported)
	if module == "" || imported == "" {
		return "", false
	}
	if imported == module {
		return "", true
	}
	if strings.HasPrefix(imported, module+"/") {
		return strings.TrimPrefix(imported, module+"/"), true
	}
	return "", false
}

func goPackagePath(file string) string {
	dir := path.Dir(normalizeCoveragePath(file))
	if dir == "." {
		return ""
	}
	return dir
}

func normalizeBaselinePrefix(prefix string) string {
	return strings.Trim(strings.TrimSpace(strings.ReplaceAll(prefix, `\`, "/")), "/")
}

func baselineRelative(module, value string) string {
	value = normalizeBaselinePrefix(value)
	if module != "" && strings.HasPrefix(value, module+"/") {
		return strings.TrimPrefix(value, module+"/")
	}
	return value
}

func baselinePrefixMatches(prefix, value string) bool {
	prefix = normalizeBaselinePrefix(prefix)
	value = normalizeBaselinePrefix(value)
	if prefix == "" {
		return false
	}
	return value == prefix || strings.HasPrefix(value, prefix+"/")
}

func authorizedBaselineException(exceptions []architectureBaselineExceptionSpec, ruleID, from, to, policy string) (string, bool) {
	if policy != store.ArchitectureExceptionsBaseline {
		return "", false
	}
	for _, exception := range exceptions {
		id := strings.TrimSpace(exception.ID)
		if id == "" || strings.TrimSpace(exception.Rule) != ruleID {
			continue
		}
		if strings.TrimSpace(exception.ApprovedBy) == "" || strings.TrimSpace(exception.Reason) == "" {
			continue
		}
		if exception.From != "" && !baselinePrefixMatches(exception.From, from) {
			continue
		}
		if exception.To != "" && !baselinePrefixMatches(exception.To, to) {
			continue
		}
		return id, true
	}
	return "", false
}

// architectureBaselineDimension turns the baseline result into the verdict
// dimension the panel and the summary render.
func architectureBaselineDimension(baseline store.ArchitectureBaseline) store.ArchitectureDimension {
	lines := []string{fmt.Sprintf("Estado: %s", architectureBaselineStatusLabel(baseline.Status))}
	reference := baseline.RulesPath
	if baseline.RulesSHA != "" {
		reference += "@" + baseline.RulesSHA
	}
	if reference != "" {
		lines = append(lines, "Reglas: "+reference)
	}
	if baseline.RulesVersion != "" {
		lines = append(lines, "Versión: "+baseline.RulesVersion)
	}
	if baseline.ChangedInPR {
		lines = append(lines, "La regla cambió en el PR: se evaluó la versión de la base.")
		if baseline.RulesHeadSHA != "" {
			lines = append(lines, "Regla propuesta (evidencia, no aplicada): "+baseline.RulesPath+"@"+baseline.RulesHeadSHA)
		}
	}
	if baseline.Controls.BaselineMissing {
		lines = append(lines, "Sin línea base en la base: no se puede aprobar la comprobación.")
	}
	for _, decision := range baseline.Decisions {
		entry := fmt.Sprintf("ADR %s (%s)", decision.ID, decision.Path)
		if decision.Version != "" {
			entry += " v" + decision.Version
		}
		if decision.SHA != "" {
			entry += " sha " + decision.SHA
		}
		lines = append(lines, entry+": "+decision.Status)
	}
	for _, check := range baseline.Checks {
		entry := fmt.Sprintf("%s %s: %s → %s en %s (import %s)",
			architectureDependencyStatusLabel(check.Status), check.RuleID, check.FromModule,
			check.ToModule, check.File, check.Import)
		if check.ExceptionID != "" {
			entry += " [" + check.ExceptionID + "]"
		}
		lines = append(lines, entry)
	}
	for _, err := range baseline.Errors {
		lines = append(lines, "Aviso: "+err)
	}
	return store.ArchitectureDimension{Name: architectureBaselineDimensionName, Content: strings.Join(lines, "\n")}
}

func architectureBaselineStatusLabel(status string) string {
	switch status {
	case store.ArchitectureBaselinePass:
		return "correcta"
	case store.ArchitectureBaselineViolation:
		return "violación"
	case store.ArchitectureBaselineUnknown:
		return "desconocida"
	default:
		return status
	}
}

func architectureDependencyStatusLabel(status string) string {
	switch status {
	case store.ArchitectureDependencyViolation:
		return "violación"
	case store.ArchitectureDependencyException:
		return "excepción autorizada"
	default:
		return status
	}
}

// applyArchitectureBaseline integrates the baseline result into the verdict as
// a dimension and raises the verdict status. A violation wins; an unknown
// baseline never leaves the verdict looking complete.
func applyArchitectureBaseline(verdict store.ArchitectureVerdict, baseline store.ArchitectureBaseline) store.ArchitectureVerdict {
	stored := baseline
	verdict.Baseline = &stored
	dimension := architectureBaselineDimension(baseline)
	dimensions := make([]store.ArchitectureDimension, 0, len(verdict.Dimensions)+1)
	dimensions = append(dimensions, dimension)
	for _, existing := range verdict.Dimensions {
		if existing.Name == architectureBaselineDimensionName {
			continue
		}
		dimensions = append(dimensions, existing)
	}
	verdict.Dimensions = dimensions
	switch baseline.Status {
	case store.ArchitectureBaselineViolation:
		verdict.Status = store.ArchitectureViolation
	case store.ArchitectureBaselineUnknown:
		if verdict.Status != store.ArchitectureViolation {
			verdict.Status = store.ArchitectureInconclusive
		}
	}
	return verdict
}
