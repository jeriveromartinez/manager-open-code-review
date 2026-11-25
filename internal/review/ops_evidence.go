package review

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// opsEvidenceContentLimit bounds the text read per file. Operation evidence
	// is marker based, so a larger file cannot add evidence.
	opsEvidenceContentLimit = 1 << 20
	// opsEvidenceTotalLimit bounds the aggregate text retained for one change.
	// Beyond it every remaining evidence file is unknown, never silently absent.
	opsEvidenceTotalLimit = 4 << 20
)

// opsChangeFile is one changed path plus the head-side text used to look for
// evidence. Content is only read for paths that can affect a dimension.
// Unreadable turns a read failure into an explicit unknown.
type opsChangeFile struct {
	Path       string
	Content    string
	Unreadable bool
	ReadError  string
}

var (
	opsMigrationName = regexp.MustCompile(`^\d{2,}[_\-. ]`)

	// opsDestructiveMigration matches forward operations that cannot coexist
	// with the previous version while a staggered deployment still runs it.
	// It only looks at the Up section: the Down section may undo them.
	opsDestructiveMigration = regexp.MustCompile(`(?i)\b(` +
		`drop\s+(table|column|database|schema|index|constraint|type|view|sequence)|` +
		`truncate\s+(table\s+)?\w+|` +
		`delete\s+from|` +
		`rename\s+(column|table|constraint|to)|` +
		`set\s+not\s+null|` +
		`(?:alter|modify|change)\s+column\s+\S+\s+(type|set\s+data\s+type)|` +
		`(?:modify|change)\s+column\s+\S+\s+\S+` +
		`)\b`)

	opsSQLBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

	opsMigrationDownMarkers = []string{
		"+goose down", "-- +migrate down", "migrate:down", "migrate down",
		"down migration", "-- down",
	}
)

// evaluateOpsEvidence inspects the change and records, per applicable
// dimension, the evidence it carries and what is missing. It is a pure function
// of the change: the same input always yields the same verdict, and a dimension
// without structural evidence stays unknown instead of becoming satisfied.
func evaluateOpsEvidence(changes []opsChangeFile) store.OpsEvidence {
	ops := store.OpsEvidence{
		Status: store.OpsStatusNotApplicable,
		Controls: store.OpsControls{
			Observation:    true,
			WritesDecision: false,
		},
	}
	ops.Dimensions = []store.OpsDimension{
		opsSchemaDimension(changes),
		opsTopologyDimension(changes),
		opsPermissionsDimension(changes),
		opsLoadDimension(changes),
	}
	ops.Status = aggregateOpsStatus(ops.Dimensions)
	return ops
}

// evaluateRunOpsEvidence is the runner entry point. It is independent of the
// engine analysis signal: operational evidence is evaluated whenever the
// observation mode is on. A failed inventory yields an explicit unknown that
// carries the error instead of a nil verdict.
func evaluateRunOpsEvidence(mode string, invErr error, dir string, inventory []changedFile) (store.OpsEvidence, bool) {
	if mode != store.ArchitectureAnalysisObservation {
		return store.OpsEvidence{}, false
	}
	if invErr != nil {
		return store.OpsEvidence{
			Status:   store.OpsStatusUnknown,
			Error:    "diff inventory failed: " + invErr.Error(),
			Controls: store.OpsControls{Observation: true, WritesDecision: false},
		}, true
	}
	return evaluateOpsEvidence(collectOpsChanges(dir, inventory)), true
}

func aggregateOpsStatus(dimensions []store.OpsDimension) string {
	status := store.OpsStatusNotApplicable
	for _, dimension := range dimensions {
		if !dimension.Applies {
			continue
		}
		switch dimension.Status {
		case store.OpsStatusRisk:
			return store.OpsStatusRisk
		case store.OpsStatusUnknown:
			status = store.OpsStatusUnknown
		case store.OpsStatusSatisfied:
			if status != store.OpsStatusUnknown {
				status = store.OpsStatusSatisfied
			}
		}
	}
	return status
}

// opsSchemaDimension answers: does the change alter the schema or the data, and
// does it carry an ordered, reversible migration that the old version can
// coexist with? A model or SQL change without a migration is unknown, never
// satisfied.
func opsSchemaDimension(changes []opsChangeFile) store.OpsDimension {
	dim := opsNotApplicable(store.OpsDimensionSchema)
	var sources, migrations []opsChangeFile
	unreadable := false
	for _, change := range changes {
		path := normalizeCoveragePath(change.Path)
		if path == "" {
			continue
		}
		schemaSource := opsSchemaSourcePath(path) || opsSchemaContent(path, change.Content)
		migration := opsMigrationPath(path)
		if change.Unreadable {
			if schemaSource {
				sources = append(sources, change)
			}
			if migration {
				migrations = append(migrations, change)
			}
			if schemaSource || migration {
				unreadable = true
				dim.Missing = appendUnique(dim.Missing, opsReadableReason(change))
			}
			continue
		}
		if schemaSource {
			sources = append(sources, change)
		}
		if migration {
			migrations = append(migrations, change)
		}
	}
	if len(sources) == 0 && len(migrations) == 0 {
		return dim
	}
	dim.Applies = true

	if len(migrations) == 0 {
		dim.Status = store.OpsStatusUnknown
		dim.Reason = "schema change without a migration in the change"
		dim.Missing = appendUnique(dim.Missing, "migration")
		return dim
	}

	dim.Status = store.OpsStatusSatisfied
	if !opsMigrationsOrdered(migrations) {
		dim.Status = store.OpsStatusUnknown
		dim.Reason = "migration order is not defined by a numeric prefix"
		dim.Missing = appendUnique(dim.Missing, "ordered migration")
	} else {
		dim.Evidence = append(dim.Evidence, "artifact: ordered migrations "+strings.Join(opsMigrationNames(migrations), ", "))
	}

	downFiles := make(map[string]bool, len(migrations))
	for _, migration := range migrations {
		downFiles[strings.ToLower(baseName(normalizeCoveragePath(migration.Path)))] = true
	}
	for _, migration := range migrations {
		path := normalizeCoveragePath(migration.Path)
		base := strings.ToLower(baseName(path))
		if migration.Unreadable {
			dim.Status = opsLowerStatus(dim.Status, store.OpsStatusUnknown)
			dim.Missing = appendUnique(dim.Missing, opsReadableReason(migration))
			continue
		}
		if strings.HasSuffix(base, ".down.sql") || strings.HasSuffix(base, "_down.sql") {
			dim.Evidence = append(dim.Evidence, "artifact: rollback migration "+path)
			continue
		}
		if opsMigrationDestructive(migration.Content) {
			dim.Status = store.OpsStatusRisk
			dim.Reason = "destructive migration " + path + " is incompatible with a staggered deployment"
			dim.Evidence = append(dim.Evidence, "artifact: destructive migration "+path)
			continue
		}
		dim.Evidence = append(dim.Evidence, "artifact: migration "+path)
		if !opsMigrationReversible(migration, downFiles) {
			dim.Status = opsLowerStatus(dim.Status, store.OpsStatusUnknown)
			dim.Missing = appendUnique(dim.Missing, "reversible migration "+path)
			if dim.Reason == "" {
				dim.Reason = "migration " + path + " has no rollback path"
			}
		}
	}
	if unreadable {
		dim.Status = opsLowerStatus(dim.Status, store.OpsStatusUnknown)
	}
	if dim.Reason == "" && dim.Status == store.OpsStatusUnknown {
		dim.Reason = "schema evidence is incomplete"
	}
	return dim
}

// opsTopologyDimension answers: does the change alter deployment topology, and
// does it carry an explicit rollout strategy, a rollback/restore path and
// observability for the new shape? Incidental words in comments or fixtures do
// not count, and missing evidence is unknown, not a pass.
func opsTopologyDimension(changes []opsChangeFile) store.OpsDimension {
	dim := opsNotApplicable(store.OpsDimensionTopology)
	var files []opsChangeFile
	for _, change := range changes {
		if opsTopologyPath(change.Path) {
			files = append(files, change)
		}
	}
	if len(files) == 0 {
		return dim
	}
	dim.Applies = true

	var rollout, rollback, observability bool
	unreadable := false
	for _, file := range files {
		path := normalizeCoveragePath(file.Path)
		if file.Unreadable {
			unreadable = true
			dim.Missing = appendUnique(dim.Missing, opsReadableReason(file))
			continue
		}
		if opsFixturePath(path) || opsTestPath(path) {
			continue
		}
		content := strings.ToLower(opsStripComments(file.Content))
		if opsContainsAny(content, opsRolloutMarkers) {
			rollout = true
			dim.Evidence = appendUnique(dim.Evidence, "artifact: rollout strategy in "+path)
		}
		if opsContainsAny(content, opsRollbackMarkers) {
			rollback = true
			dim.Evidence = appendUnique(dim.Evidence, "artifact: rollback/restore in "+path)
		}
		if opsContainsAny(content, opsObservabilityMarkers) {
			observability = true
			dim.Evidence = appendUnique(dim.Evidence, "artifact: observability in "+path)
		}
	}
	if !rollout {
		dim.Missing = appendUnique(dim.Missing, "rollout strategy")
	}
	if !rollback {
		dim.Missing = appendUnique(dim.Missing, "rollback/restore")
	}
	if !observability {
		dim.Missing = appendUnique(dim.Missing, "observability/alerts")
	}
	if unreadable || len(dim.Missing) > 0 {
		dim.Status = store.OpsStatusUnknown
		dim.Reason = "topology change without evidence for " + strings.Join(dim.Missing, ", ")
		if unreadable && len(dim.Missing) == 0 {
			dim.Reason = "topology evidence is unreadable"
		}
		return dim
	}
	dim.Status = store.OpsStatusSatisfied
	return dim
}

// opsPermissionsDimension answers: does the change alter authorization, and do
// the tests in the same change carry a structural allow/deny assertion? A
// comment or an incidental word such as "scope" is not proof.
func opsPermissionsDimension(changes []opsChangeFile) store.OpsDimension {
	dim := opsNotApplicable(store.OpsDimensionPermissions)
	touched := false
	for _, change := range changes {
		if opsPermissionPath(change.Path) {
			touched = true
			break
		}
	}
	if !touched {
		return dim
	}
	dim.Applies = true

	evidence := false
	unreadable := false
	for _, change := range changes {
		path := normalizeCoveragePath(change.Path)
		if change.Unreadable {
			if opsPermissionPath(path) {
				unreadable = true
				dim.Missing = appendUnique(dim.Missing, opsReadableReason(change))
			}
			continue
		}
		if opsFixturePath(path) || !opsTestPath(path) {
			continue
		}
		content := strings.ToLower(opsStripComments(change.Content))
		if !strings.Contains(content, "func test") && !strings.Contains(content, "func benchmark") {
			continue
		}
		if opsPermissionAssertion(content) {
			evidence = true
			dim.Evidence = appendUnique(dim.Evidence, "test: permission assertions in "+path)
		}
	}
	if evidence && !unreadable {
		dim.Status = store.OpsStatusSatisfied
		return dim
	}
	dim.Status = store.OpsStatusUnknown
	dim.Reason = "permission change without test evidence"
	dim.Missing = appendUnique(dim.Missing, "permission test assertion")
	return dim
}

// opsLoadDimension answers: does the change touch a high-load path, and is
// there a benchmark or load test that measures it? An incidental "metrics" word
// is not a measurement.
func opsLoadDimension(changes []opsChangeFile) store.OpsDimension {
	dim := opsNotApplicable(store.OpsDimensionLoad)
	touched := false
	for _, change := range changes {
		if opsLoadPath(change.Path) {
			touched = true
			break
		}
	}
	if !touched {
		return dim
	}
	dim.Applies = true

	evidence := false
	unreadable := false
	for _, change := range changes {
		path := normalizeCoveragePath(change.Path)
		if change.Unreadable {
			if opsLoadPath(path) {
				unreadable = true
				dim.Missing = appendUnique(dim.Missing, opsReadableReason(change))
			}
			continue
		}
		if opsFixturePath(path) {
			continue
		}
		found, detail := opsLoadEvidence(change)
		if found {
			evidence = true
			dim.Evidence = appendUnique(dim.Evidence, "measurement: "+detail+" in "+path)
		}
	}
	if evidence && !unreadable {
		dim.Status = store.OpsStatusSatisfied
		return dim
	}
	dim.Status = store.OpsStatusUnknown
	dim.Reason = "high-load change without load or capacity evidence"
	dim.Missing = appendUnique(dim.Missing, "load or capacity evidence")
	return dim
}

func opsLoadEvidence(change opsChangeFile) (bool, string) {
	path := normalizeCoveragePath(change.Path)
	if opsLoadEvidencePath(path) {
		return true, "load test artifacts"
	}
	if !opsTestPath(path) {
		return false, ""
	}
	content := strings.ToLower(opsStripComments(change.Content))
	if strings.Contains(content, "func benchmark") {
		return true, "benchmark"
	}
	for _, marker := range []string{"k6 run", "vegeta attack", "locust -f"} {
		if strings.Contains(content, marker) {
			return true, "load test"
		}
	}
	return false, ""
}

func opsNotApplicable(name string) store.OpsDimension {
	return store.OpsDimension{Name: name, Status: store.OpsStatusNotApplicable}
}

// opsLowerStatus keeps the more severe of two states. Risk always wins, then
// unknown, then satisfied.
func opsLowerStatus(current, next string) string {
	rank := func(status string) int {
		switch status {
		case store.OpsStatusRisk:
			return 3
		case store.OpsStatusUnknown:
			return 2
		case store.OpsStatusSatisfied:
			return 1
		default:
			return 0
		}
	}
	if rank(next) > rank(current) {
		return next
	}
	return current
}

var (
	// The markers below are structural keys or values, not bare words. A
	// comment can mention "rollback" without providing a rollback strategy.
	opsRolloutMarkers = []string{
		"strategy:", "rollingupdate", "maxsurge", "maxunavailable",
		"stop_grace_period", "terminationgraceperiodseconds", "deployment_strategy",
		"update_config:", "order: start-first", "order: stop-first", "canary:",
		"bluegreen", "blue-green",
	}
	opsRollbackMarkers = []string{
		"rollback:", "rollback_", "automatic_rollback", "rollback_on_failure",
		"restore:", "restore_", "pg_restore", "backup:", "backup_", "backup.sh",
	}
	opsObservabilityMarkers = []string{
		"metrics:", "metrics_", "prometheus", "alertmanager", "alerts:", "alert:",
		"monitor:", "healthz", "healthcheck:", "readinessprobe", "livenessprobe",
		"observability:", "tracing:",
	}
	opsLoadDirs = []string{
		"queue", "queues", "scheduler", "scheduled", "batch", "stream",
		"streaming", "cache", "caching", "index", "indexes", "worker",
		"workers", "concurrency",
	}
	opsBenchmarkDirs = []string{
		"bench", "benchmark", "benchmarks", "loadtest", "loadtests",
		"k6", "vegeta", "locust",
	}
)

func opsSchemaSourcePath(path string) bool {
	path = normalizeCoveragePath(path)
	if path == "" || opsTestPath(path) {
		return false
	}
	base := strings.ToLower(baseName(path))
	if strings.HasSuffix(base, ".sql") {
		return true
	}
	if matchesArchitecturePath(path, architecturePersistenceDirs) {
		return true
	}
	return opsModelPath(path)
}

// opsSchemaContent only treats an ORM tag or DDL as a schema change in a model
// file. A gorm tag quoted in another package is not proof that the table
// changed, so ordinary edits do not demand a migration.
func opsSchemaContent(path, content string) bool {
	if opsTestPath(path) || !opsModelPath(path) {
		return false
	}
	stripped := opsStripComments(content)
	if strings.Contains(stripped, `gorm:"`) || strings.Contains(stripped, "gorm.Model") {
		return true
	}
	upper := strings.ToUpper(stripped)
	return strings.Contains(upper, "CREATE TABLE") || strings.Contains(upper, "ALTER TABLE")
}

func opsModelPath(path string) bool {
	base := strings.ToLower(baseName(normalizeCoveragePath(path)))
	if strings.HasSuffix(base, "_model.go") || strings.HasSuffix(base, ".model.go") {
		return true
	}
	switch base {
	case "models.go", "model.go", "schema.go", "entities.go", "entity.go",
		"database.go", "db.go":
		return true
	}
	return matchesArchitecturePath(path, []string{"models", "model", "entities", "entity"})
}

func opsMigrationPath(path string) bool {
	path = normalizeCoveragePath(path)
	if path == "" || opsTestPath(path) {
		return false
	}
	base := strings.ToLower(baseName(path))
	if !strings.HasSuffix(base, ".sql") {
		return false
	}
	if matchesArchitecturePath(path, []string{"migration", "migrations", "migrate"}) {
		return true
	}
	return opsMigrationName.MatchString(base)
}

func opsMigrationsOrdered(migrations []opsChangeFile) bool {
	if len(migrations) == 0 {
		return false
	}
	for _, migration := range migrations {
		base := strings.ToLower(baseName(normalizeCoveragePath(migration.Path)))
		if !opsMigrationName.MatchString(base) {
			return false
		}
	}
	return true
}

func opsMigrationNames(migrations []opsChangeFile) []string {
	names := make([]string, 0, len(migrations))
	for _, migration := range migrations {
		names = append(names, strings.ToLower(baseName(normalizeCoveragePath(migration.Path))))
	}
	sort.Strings(names)
	return names
}

func opsMigrationReversible(migration opsChangeFile, downFiles map[string]bool) bool {
	base := strings.ToLower(baseName(normalizeCoveragePath(migration.Path)))
	stem := strings.TrimSuffix(base, ".sql")
	stem = strings.TrimSuffix(stem, ".up")
	stem = strings.TrimSuffix(stem, "_up")
	if downFiles[stem+".down.sql"] || downFiles[stem+"_down.sql"] {
		return true
	}
	content := strings.ToLower(migration.Content)
	for _, marker := range opsMigrationDownMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func opsMigrationDestructive(content string) bool {
	return opsDestructiveMigration.MatchString(opsStripSQLComments(opsMigrationUpSection(content)))
}

// opsMigrationUpSection cuts the text at the first Down marker so a rollback
// that drops a column is not mistaken for a destructive forward migration.
func opsMigrationUpSection(content string) string {
	lower := strings.ToLower(content)
	cut := -1
	for _, marker := range opsMigrationDownMarkers {
		if index := strings.Index(lower, marker); index >= 0 && (cut < 0 || index < cut) {
			cut = index
		}
	}
	if cut < 0 {
		return content
	}
	return content[:cut]
}

func opsTopologyPath(path string) bool {
	path = normalizeCoveragePath(path)
	if path == "" {
		return false
	}
	base := strings.ToLower(baseName(path))
	return matchesArchitecturePath(path, architectureTopologyDirs) ||
		strings.HasPrefix(strings.ToLower(path), ".github/workflows/") ||
		strings.HasPrefix(base, "dockerfile") ||
		strings.HasPrefix(base, "docker-compose") ||
		strings.HasPrefix(base, "compose.")
}

func opsPermissionPath(path string) bool {
	path = normalizeCoveragePath(path)
	if path == "" {
		return false
	}
	return matchesArchitecturePath(path, architecturePermissionDirs)
}

func opsLoadPath(path string) bool {
	path = normalizeCoveragePath(path)
	if path == "" {
		return false
	}
	return matchesArchitecturePath(path, opsLoadDirs)
}

func opsLoadEvidencePath(path string) bool {
	return matchesArchitecturePath(path, opsBenchmarkDirs)
}

func opsTestPath(path string) bool {
	base := strings.ToLower(baseName(normalizeCoveragePath(path)))
	return strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, "_test.") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_")
}

func opsFixturePath(path string) bool {
	path = strings.ToLower(normalizeCoveragePath(path))
	return matchesArchitecturePath(path, []string{"testdata", "fixtures", "fixture", "mocks", "mock"}) ||
		strings.Contains(path, "/testdata/") || strings.Contains(path, "/fixtures/")
}

func opsPermissionAssertion(content string) bool {
	for _, marker := range []string{
		"testpermission", "testauth", "testauthoriz", "testrbac", "testscope",
		"testforbidden", "testunauthorized",
		"statusforbidden", "statusunauthorized", "errforbidden", "errunauthorized",
		"requirepermission",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

// opsStripComments removes whole-line and trailing # / // comments so an
// incidental word in prose cannot satisfy a dimension.
func opsStripComments(content string) string {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if index := strings.Index(line, " #"); index >= 0 {
			line = line[:index]
		}
		if index := strings.Index(line, " //"); index >= 0 {
			line = line[:index]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func opsStripSQLComments(content string) string {
	content = opsSQLBlockComment.ReplaceAllString(content, "")
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if index := strings.Index(line, "--"); index >= 0 {
			line = line[:index]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func opsContainsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		if addition == "" {
			continue
		}
		seen := false
		for _, existing := range values {
			if existing == addition {
				seen = true
				break
			}
		}
		if !seen {
			values = append(values, addition)
		}
	}
	return values
}

func opsReadableReason(change opsChangeFile) string {
	reason := "readable " + normalizeCoveragePath(change.Path)
	if change.ReadError != "" {
		reason += " (" + change.ReadError + ")"
	}
	return reason
}

// collectOpsChanges reads the head side of the changed files that can affect a
// dimension. Read failures are recorded as unreadable evidence so the evaluator
// degrades to unknown instead of assuming the evidence is absent. Deleted,
// binary and submodule entries carry no head text but still count for path-based
// detection.
func collectOpsChanges(dir string, inventory []changedFile) []opsChangeFile {
	root := dir
	if abs, err := filepath.Abs(dir); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	changes := make([]opsChangeFile, 0, len(inventory))
	used := 0
	for _, file := range inventory {
		path := normalizeCoveragePath(file.Path)
		if path == "" {
			continue
		}
		change := opsChangeFile{Path: path}
		switch file.Kind {
		case changedDeleted, changedBinary, changedSubmodule:
			changes = append(changes, change)
			continue
		}
		if !opsNeedsContent(path) {
			changes = append(changes, change)
			continue
		}
		full, reason, ok := opsSafePathFromRoot(root, path)
		if !ok {
			change.Unreadable = true
			change.ReadError = reason
			changes = append(changes, change)
			continue
		}
		info, err := os.Lstat(full)
		if err != nil {
			change.Unreadable = true
			change.ReadError = "stat " + path + ": " + err.Error()
			changes = append(changes, change)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			change.Unreadable = true
			change.ReadError = path + " is a symbolic link"
			changes = append(changes, change)
			continue
		}
		if info.Size() > opsEvidenceContentLimit {
			change.Unreadable = true
			change.ReadError = path + " exceeds the evidence size limit"
			changes = append(changes, change)
			continue
		}
		if used+int(info.Size()) > opsEvidenceTotalLimit {
			change.Unreadable = true
			change.ReadError = "total evidence budget exhausted"
			changes = append(changes, change)
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			change.Unreadable = true
			change.ReadError = "read " + path + ": " + err.Error()
			changes = append(changes, change)
			continue
		}
		used += len(content)
		change.Content = string(content)
		changes = append(changes, change)
	}
	return changes
}

// opsNeedsContent limits reads to the paths that can affect a dimension. An
// ordinary source file carries no operational evidence.
func opsNeedsContent(path string) bool {
	return opsSchemaSourcePath(path) || opsMigrationPath(path) || opsTopologyPath(path) ||
		opsPermissionPath(path) || opsLoadPath(path) || opsTestPath(path) ||
		opsLoadEvidencePath(path)
}

// opsSafePathFromRoot resolves the parent directory and re-checks containment,
// so a symlinked parent cannot redirect the read outside the checkout. The
// caller still Lstats the final component and refuses a symlink. The reason
// explains a rejected path without assuming it was an escape attempt.
func opsSafePathFromRoot(root, path string) (string, string, bool) {
	full := filepath.Join(root, filepath.FromSlash(path))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "path escapes the review checkout", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return "", "resolve parent of " + path + ": " + err.Error(), false
	}
	rel, err = filepath.Rel(root, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "parent of " + path + " escapes the review checkout", false
	}
	return filepath.Join(parent, filepath.Base(full)), "", true
}

// applyOpsEvidence attaches the deterministic operational verdict to the
// architectural one and folds its material risks and unknowns into the same
// lists the engine report uses. It is idempotent: re-applying after the engine
// report is parsed does not duplicate entries.
func applyOpsEvidence(verdict store.ArchitectureVerdict, ops store.OpsEvidence) store.ArchitectureVerdict {
	verdict.OpsEvidence = &ops
	verdict.Risks = appendUnique(verdict.Risks, ops.RiskItems()...)
	verdict.Uncertainties = appendUnique(verdict.Uncertainties, ops.UncertaintyItems()...)
	return verdict
}

func opsEvidenceSummary(ops store.OpsEvidence) string {
	if strings.TrimSpace(ops.Error) != "" {
		return "inventario no disponible"
	}
	parts := make([]string, 0, len(ops.Dimensions))
	for _, dimension := range ops.Dimensions {
		if dimension.Applies {
			parts = append(parts, dimension.Name+"="+dimension.Status)
		}
	}
	if len(parts) == 0 {
		return "sin dimensiones aplicables"
	}
	return strings.Join(parts, ", ")
}

// opsLogLevel marks an unproven or risky dimension without turning it into a
// failed analysis line.
func opsLogLevel(status string) string {
	switch status {
	case store.OpsStatusRisk, store.OpsStatusUnknown:
		return "warn"
	default:
		return "info"
	}
}
