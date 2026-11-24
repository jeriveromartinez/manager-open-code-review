package review

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const contextRadius = 3

// AnchorProvider returns the source lines of a repository path at the reviewed
// head, or nil when the file is unavailable. Reconcile and mergeNativeFindings
// use it to build the content anchor of a finding without depending on line
// numbers, which a push can shift.
type AnchorProvider func(path string) []string

// checkoutAnchors adapts the checked-out tree to an AnchorProvider. The lines
// are cached per path because a review asks for the same files repeatedly.
func checkoutAnchors(workDir string) AnchorProvider {
	cache := make(map[string][]string)
	return func(path string) []string {
		path = normalizeCoveragePath(path)
		if lines, ok := cache[path]; ok {
			return lines
		}
		content, err := readRepoFile(workDir, path)
		if err != nil {
			cache[path] = nil
			return nil
		}
		cache[path] = strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
		return cache[path]
	}
}

// Anchor returns the normalized content anchor of a finding: the code OCR
// cited when it provided it, otherwise the ±3 source lines around the range.
// Whitespace is collapsed so indentation, wrapping and line endings do not fork
// the identity of an unchanged finding.
func Anchor(f AgentFinding, lines []string) string {
	if code := normalizeAnchor(f.ExistingCode); code != "" {
		return code
	}
	return normalizeAnchor(contextWindow(f, lines))
}

// Fingerprint returns the version-2 identity of a finding: PR id, normalized
// path, category, normalized title, content anchor and the occurrence ordinal.
// Line numbers order occurrences but never enter the hash, so a finding that a
// push shifts keeps its identity.
func Fingerprint(prID uint, f AgentFinding, lines []string, ordinal int) string {
	return fingerprintFor(prID, f, Anchor(f, lines), ordinal)
}

func fingerprintFor(prID uint, f AgentFinding, anchor string, ordinal int) string {
	h := sha256.New()
	parts := []string{
		strconv.FormatUint(uint64(prID), 10),
		normalizeCoveragePath(f.Path),
		strings.TrimSpace(f.Category),
		normalizeTitle(f.Title),
		anchor,
		strconv.Itoa(ordinal),
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FindingID derives the stored ID from the pull request and its fingerprint, so
// two pull requests that report the same finding never share a primary key.
func FindingID(prID uint, fingerprint string) string {
	sum := sha256.Sum256([]byte(strconv.FormatUint(uint64(prID), 10) + "|" + fingerprint))
	return "f_" + hex.EncodeToString(sum[:])[:16]
}

// LegacyFingerprint reproduces the version-1 fingerprint. It is only used to
// match rows written before the identity change; the old pipeline always passed
// nil lines, so the stored value is reproducible from the row's content.
func LegacyFingerprint(f AgentFinding, lines []string) string {
	h := sha256.New()
	parts := []string{
		f.Path,
		f.Category,
		normalizeTitle(f.Title),
		contextWindow(f, lines),
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func contextWindow(f AgentFinding, lines []string) string {
	start := f.StartLine - 1 - contextRadius
	if start < 0 {
		start = 0
	}
	end := f.EndLine + contextRadius
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		start = end
	}
	return strings.Join(lines[start:end], "\n")
}

func normalizeAnchor(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}

// normalizeCoveragePath makes the several spellings OCR and the store use for
// one file collapse to a single identity: separators are unified and the path
// is cleaned, so "./x", "x" and "dir\\x" hash the same on every platform.
func normalizeCoveragePath(value string) string {
	cleaned := path.Clean(filepath.ToSlash(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")))
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// reviewedPaths devuelve el conjunto de rutas que OCR informó como completadas
// o reutilizadas.
func reviewedPaths(coverage nativeCoverage) map[string]bool {
	paths := make(map[string]bool, len(coverage.Completed)+len(coverage.Reused))
	for _, group := range [][]nativeCoverageItem{coverage.Completed, coverage.Reused} {
		for _, item := range group {
			if path := normalizeCoveragePath(item.Path); path != "" {
				paths[path] = true
			}
		}
	}
	return paths
}

// identifiedFinding is a finding with the identity Reconcile must persist. The
// ordinal separates occurrences that share every other identity field.
type identifiedFinding struct {
	finding     AgentFinding
	fingerprint string
}

// identityGroupKey is the identity without its ordinal. Occurrences that share
// it are ordered by (StartLine, EndLine, Body) to assign the ordinal.
type identityGroupKey struct {
	path, category, title, anchor string
}

type identityEntry struct {
	finding AgentFinding
	anchor  string
}

func identityKey(f AgentFinding, anchor string) identityGroupKey {
	return identityGroupKey{
		path:     normalizeCoveragePath(f.Path),
		category: strings.TrimSpace(f.Category),
		title:    normalizeTitle(f.Title),
		anchor:   anchor,
	}
}

// identifyFindings assigns the occurrence ordinal to each input. The ordinal
// continues across runs: an occurrence that already had a row keeps that row's
// position, so removing one of two identical occurrences does not renumber the
// survivor and repoint its comment. New occurrences take the lowest free
// ordinal. Within a group the order is (StartLine, EndLine, Body), which makes
// IDs independent of the order in which OCR reported the findings.
func identifyFindings(prID uint, findings []AgentFinding, open []store.Finding, anchors AnchorProvider) []identifiedFinding {
	groups := make(map[identityGroupKey][]identityEntry, len(findings))
	order := make([]identityGroupKey, 0, len(findings))
	for _, f := range findings {
		anchor := findingAnchor(f, anchors)
		key := identityKey(f, anchor)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], identityEntry{finding: f, anchor: anchor})
	}

	previous := make(map[identityGroupKey][]store.Finding, len(open))
	for _, row := range open {
		af := findingFromStore(row)
		key := identityKey(af, findingAnchor(af, anchors))
		previous[key] = append(previous[key], row)
	}

	out := make([]identifiedFinding, 0, len(findings))
	for _, key := range order {
		entries := groups[key]
		sort.SliceStable(entries, func(i, j int) bool {
			return occurrenceLess(entries[i].finding, entries[j].finding)
		})
		prevs := previous[key]
		sort.SliceStable(prevs, func(i, j int) bool {
			return occurrenceLess(findingFromStore(prevs[i]), findingFromStore(prevs[j]))
		})
		ordinals := stableOrdinals(prID, entries, prevs, anchors)
		for i, entry := range entries {
			out = append(out, identifiedFinding{
				finding:     entry.finding,
				fingerprint: fingerprintFor(prID, entry.finding, entry.anchor, ordinals[i]),
			})
		}
	}
	return out
}

// stableOrdinals keeps each occurrence on the ordinal it already had when its
// row still exists. An occurrence that moved beyond the context radius falls
// back to the nearest unmatched previous row (preferring a matching body), so a
// far survivor keeps its ID and, for example, its GitHub comment. Occurrences
// with no predecessor left take an ordinal above every previous row instead of
// the lowest free one, which would reuse a removed occurrence's row.
func stableOrdinals(prID uint, entries []identityEntry, prevs []store.Finding, anchors AnchorProvider) []int {
	ordinals := make([]int, len(entries))
	if len(prevs) == 0 {
		for i := range ordinals {
			ordinals[i] = i
		}
		return ordinals
	}
	for i := range ordinals {
		ordinals[i] = -1
	}

	prevOrdinals := previousOrdinals(prID, prevs, anchors, len(prevs)+len(entries))
	matched := make([]bool, len(prevs))

	// First pass: keep nearby occurrences on the row they already had.
	for i := range entries {
		if best := bestPrevious(entries[i].finding, prevs, matched, true); best >= 0 {
			matched[best] = true
			ordinals[i] = prevOrdinals[best]
		}
	}

	// Second pass: a survivor that moved beyond the context radius still
	// continues the nearest previous row rather than becoming a new finding.
	for i := range ordinals {
		if ordinals[i] >= 0 {
			continue
		}
		best := bestPrevious(entries[i].finding, prevs, matched, false)
		if best < 0 {
			break
		}
		matched[best] = true
		ordinals[i] = prevOrdinals[best]
	}

	next := 0
	for _, ordinal := range prevOrdinals {
		if ordinal >= next {
			next = ordinal + 1
		}
	}
	for i := range ordinals {
		if ordinals[i] >= 0 {
			continue
		}
		ordinals[i], next = next, next+1
	}
	return ordinals
}

// bestPrevious returns the unmatched row an occurrence continues: the nearest
// one, preferring a row with the same body. nearbyOnly discards rows farther
// than the context radius.
func bestPrevious(f AgentFinding, prevs []store.Finding, matched []bool, nearbyOnly bool) int {
	best, bestDist, bestBody := -1, 0, false
	for j := range prevs {
		if matched[j] {
			continue
		}
		prev := findingFromStore(prevs[j])
		if nearbyOnly && !rangesNearby(prev, f, contextRadius) {
			continue
		}
		dist := rangeDistance(prev, f)
		body := prevs[j].Body != "" && prevs[j].Body == f.Body
		better := best < 0 ||
			(body && !bestBody) ||
			(body == bestBody && dist < bestDist)
		if better {
			best, bestDist, bestBody = j, dist, body
		}
	}
	return best
}

// previousOrdinals recovers the ordinal each row was stored with by recomputing
// the fingerprint for the candidate ordinals. The sorted position is only a
// fallback for rows whose fingerprint cannot be reproduced (legacy rows or an
// anchor that changed).
func previousOrdinals(prID uint, prevs []store.Finding, anchors AnchorProvider, bound int) []int {
	ordinals := make([]int, len(prevs))
	used := make(map[int]bool, len(prevs))
	for j := range prevs {
		af := findingFromStore(prevs[j])
		anchor := findingAnchor(af, anchors)
		ordinal := -1
		if prevs[j].Fingerprint != "" {
			for candidate := 0; candidate <= bound; candidate++ {
				if fingerprintFor(prID, af, anchor, candidate) == prevs[j].Fingerprint {
					ordinal = candidate
					break
				}
			}
		}
		if used[ordinal] {
			ordinal = -1
		}
		if ordinal >= 0 {
			used[ordinal] = true
		}
		ordinals[j] = ordinal
	}

	next := 0
	for j := range ordinals {
		if ordinals[j] >= 0 {
			continue
		}
		for used[next] {
			next++
		}
		ordinals[j], used[next] = next, true
	}
	return ordinals
}

// rangeDistance is the number of lines between two ranges; zero means they
// overlap.
func rangeDistance(a, b AgentFinding) int {
	if b.EndLine < a.StartLine {
		return a.StartLine - b.EndLine
	}
	if a.EndLine < b.StartLine {
		return b.StartLine - a.EndLine
	}
	return 0
}

func occurrenceLess(a, b AgentFinding) bool {
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	if a.EndLine != b.EndLine {
		return a.EndLine < b.EndLine
	}
	if a.Body != b.Body {
		return a.Body < b.Body
	}
	// Deterministic tie-breaks for rows that share the documented order keys.
	if a.Severity != b.Severity {
		return a.Severity < b.Severity
	}
	if a.Suggestion != b.Suggestion {
		return a.Suggestion < b.Suggestion
	}
	return a.Confidence < b.Confidence
}

func findingAnchor(f AgentFinding, anchors AnchorProvider) string {
	if code := normalizeAnchor(f.ExistingCode); code != "" {
		return code
	}
	var lines []string
	if anchors != nil {
		lines = anchors(f.Path)
	}
	return normalizeAnchor(contextWindow(f, lines))
}

func findingFromStore(f store.Finding) AgentFinding {
	return AgentFinding{
		Path:         f.Path,
		Category:     f.Category,
		Title:        f.Title,
		Body:         f.Body,
		ExistingCode: f.ExistingCode,
		StartLine:    f.StartLine,
		EndLine:      f.EndLine,
	}
}

// dedupeNearbyFindings fusiona hallazgos que describen el mismo problema desde
// dos trozos contiguos del mismo archivo. Dos trozos pueden separar el rango
// unas líneas, así que aquí se acepta un margen de contextRadius líneas. El
// ancla solo se exige cuando ambas apariciones citan código: dos títulos iguales
// sobre código citado distinto no son el mismo hallazgo, pero si OCR no citó
// código el rango es la única señal disponible. Se conserva el rango que empieza
// primero y el cuerpo de la primera aparición.
func dedupeNearbyFindings(findings []AgentFinding) []AgentFinding {
	kept := make([]AgentFinding, 0, len(findings))
	for _, finding := range findings {
		merged := false
		for i := range kept {
			if !sameNearbyFinding(kept[i], finding) {
				continue
			}
			// Preferir el rango que empieza primero sin perder el primer cuerpo.
			if finding.StartLine < kept[i].StartLine {
				kept[i].StartLine = finding.StartLine
				kept[i].EndLine = finding.EndLine
			} else if finding.StartLine == kept[i].StartLine && finding.EndLine > kept[i].EndLine {
				kept[i].EndLine = finding.EndLine
			}
			merged = true
			break
		}
		if !merged {
			kept = append(kept, finding)
		}
	}
	return kept
}

func sameNearbyFinding(a, b AgentFinding) bool {
	if normalizeCoveragePath(a.Path) != normalizeCoveragePath(b.Path) ||
		strings.TrimSpace(a.Category) != strings.TrimSpace(b.Category) {
		return false
	}
	if normalizeTitle(a.Title) != normalizeTitle(b.Title) {
		return false
	}
	aCode, bCode := normalizeAnchor(a.ExistingCode), normalizeAnchor(b.ExistingCode)
	if aCode != "" && bCode != "" && aCode != bCode {
		return false
	}
	return rangesNearby(a, b, contextRadius)
}

// rangesNearby indica si dos rangos se solapan o distan como mucho gap líneas.
func rangesNearby(a, b AgentFinding, gap int) bool {
	return b.StartLine <= a.EndLine+gap+1 && a.StartLine <= b.EndLine+gap+1
}

// findingsInPaths conserva solo los hallazgos abiertos de archivos revisados.
// Un conjunto vacío no conserva ninguno: la resolución nunca se amplía a los
// hallazgos fuera del rango revisado.
func findingsInPaths(open []store.Finding, paths map[string]bool) []store.Finding {
	kept := make([]store.Finding, 0, len(open))
	for _, f := range open {
		if paths[normalizeCoveragePath(f.Path)] {
			kept = append(kept, f)
		}
	}
	return kept
}

// Reconcile concilia los hallazgos de una corrida con los conocidos del PR. Los
// que coinciden por identidad se actualizan conservando su ID; los nuevos
// reciben un ID determinista que incluye el PR. Las filas anteriores a #144 se
// buscan por su huella antigua y, cuando coinciden, se migran en sitio.
// Solo con complete=true se devuelven los IDs abiertos que ya no aparecen.
func Reconcile(open []store.Finding, out AgentOutput, runID uint, complete bool, prID uint, anchors AnchorProvider) (upserts []store.Finding, resolvedIDs []string) {
	return ReconcileScoped(open, out, runID, complete, prID, anchors, nil)
}

// ReconcileScoped is Reconcile with an explicit resolution scope. The full
// known list is still used to match identities, so a re-reported finding keeps
// its ID and triage even on a path this run did not cover; only open findings
// whose path is in resolvable may be resolved. A nil map resolves every
// candidate, which is the behaviour of Reconcile.
func ReconcileScoped(open []store.Finding, out AgentOutput, runID uint, complete bool, prID uint, anchors AnchorProvider, resolvable map[string]bool) (upserts []store.Finding, resolvedIDs []string) {
	byFingerprint := make(map[string]store.Finding, len(open))
	legacyByFingerprint := make(map[string]store.Finding, len(open))
	for _, f := range open {
		byFingerprint[f.Fingerprint] = f
		if f.IdentityVersion < store.IdentityVersionCurrent {
			legacyByFingerprint[LegacyFingerprint(findingFromStore(f), nil)] = f
		}
	}
	if prID == 0 {
		for _, f := range open {
			if f.PullRequestID != 0 {
				prID = f.PullRequestID
				break
			}
		}
	}

	identified := identifyFindings(prID, out.Findings, open, anchors)
	seen := make(map[string]bool, len(identified))
	upserts = make([]store.Finding, 0, len(identified))
	for _, identified := range identified {
		af := identified.finding
		prev, exists := byFingerprint[identified.fingerprint]
		if !exists {
			if legacy, ok := legacyByFingerprint[LegacyFingerprint(af, nil)]; ok && !seen[legacy.ID] {
				prev, exists = legacy, true
			}
		}

		nf := store.Finding{
			ID:                  prev.ID,
			PullRequestID:       prev.PullRequestID,
			Fingerprint:         identified.fingerprint,
			IdentityVersion:     store.IdentityVersionCurrent,
			Path:                af.Path,
			StartLine:           af.StartLine,
			EndLine:             af.EndLine,
			Severity:            af.Severity,
			Category:            af.Category,
			Title:               af.Title,
			Body:                af.Body,
			Suggestion:          af.Suggestion,
			Confidence:          af.Confidence,
			ExistingCode:        af.ExistingCode,
			Status:              store.FindingOpen,
			FirstSeenRunID:      prev.FirstSeenRunID,
			LastSeenRunID:       runID,
			GithubCommentID:     prev.GithubCommentID,
			GithubCommentNodeID: prev.GithubCommentNodeID,
			GithubThreadID:      prev.GithubThreadID,
			TriageVerdict:       prev.TriageVerdict,
			TriageSource:        prev.TriageSource,
			TriagedBy:           prev.TriagedBy,
			TriagedAt:           prev.TriagedAt,
			Verification:        prev.Verification,
		}
		if !exists {
			nf.ID = FindingID(prID, identified.fingerprint)
			nf.PullRequestID = prID
			nf.FirstSeenRunID = runID
		}
		seen[nf.ID] = true
		upserts = append(upserts, nf)
	}

	if complete {
		candidates := open
		if resolvable != nil {
			candidates = findingsInPaths(open, resolvable)
		}
		for _, f := range candidates {
			// Only an open finding can be resolved by its absence: withdrawn and
			// resolved rows are not part of the review's open set.
			if f.Status == store.FindingOpen && !seen[f.ID] {
				resolvedIDs = append(resolvedIDs, f.ID)
			}
		}
	}
	return upserts, resolvedIDs
}
