package review

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const contextRadius = 3

// Fingerprint devuelve una huella estable para un hallazgo: normaliza el título
// (minúsculas y espacios colapsados) y hashea ruta + categoría + título + las
// ±3 líneas de contexto alrededor del hallazgo. El rango se recorta a los
// límites de lines. El resultado es hexadecimal SHA-256 en minúsculas.
func Fingerprint(f AgentFinding, lines []string) string {
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

	h := sha256.New()
	parts := []string{
		f.Path,
		f.Category,
		normalizeTitle(f.Title),
		strings.Join(lines[start:end], "\n"),
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func normalizeTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}

// normalizeCoveragePath quita el "./" inicial que el manifiesto de OCR añade a
// veces, para que la ruta coincida con la guardada en los hallazgos.
func normalizeCoveragePath(path string) string {
	return strings.TrimPrefix(strings.TrimSpace(path), "./")
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

// dedupeNearbyFindings fusiona hallazgos que describen el mismo problema desde
// dos trozos contiguos del mismo archivo. La huella exacta ya los uniría cuando
// coinciden ruta, categoría y título, pero dos trozos pueden separar el rango
// unas líneas; aquí se acepta un margen de contextRadius líneas. Se conserva el
// rango que empieza primero y el cuerpo de la primera aparición.
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
	if a.Path != b.Path || a.Category != b.Category {
		return false
	}
	if normalizeTitle(a.Title) != normalizeTitle(b.Title) {
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
		if paths[f.Path] {
			kept = append(kept, f)
		}
	}
	return kept
}

// Reconcile concilia los hallazgos de una corrida con los abiertos. Los que
// coinciden por huella se actualizan conservando su ID; los nuevos reciben un
// ID determinista derivado de la huella. Solo con complete=true se devuelven
// los IDs abiertos que ya no aparecen.
func Reconcile(open []store.Finding, out AgentOutput, runID uint, complete bool) (upserts []store.Finding, resolvedIDs []string) {
	byFingerprint := make(map[string]store.Finding, len(open))
	var pullRequestID uint
	for _, f := range open {
		byFingerprint[f.Fingerprint] = f
		if pullRequestID == 0 {
			pullRequestID = f.PullRequestID
		}
	}

	seen := make(map[string]bool, len(out.Findings))
	upserts = make([]store.Finding, 0, len(out.Findings))
	for _, af := range out.Findings {
		fp := Fingerprint(af, nil)
		seen[fp] = true
		prev, exists := byFingerprint[fp]

		nf := store.Finding{
			ID:                  prev.ID,
			PullRequestID:       prev.PullRequestID,
			Fingerprint:         fp,
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
			nf.ID = "f_" + fp[:16]
			nf.PullRequestID = pullRequestID
			nf.FirstSeenRunID = runID
		}
		upserts = append(upserts, nf)
	}

	if complete {
		for _, f := range open {
			if !seen[f.Fingerprint] {
				resolvedIDs = append(resolvedIDs, f.ID)
			}
		}
	}
	return upserts, resolvedIDs
}
