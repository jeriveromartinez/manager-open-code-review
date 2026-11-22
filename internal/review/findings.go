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

// reviewedPaths devuelve el conjunto de rutas que OCR informó como completadas
// o reutilizadas. El manifiesto puede traer un "./" inicial; se recorta para
// que coincida con la ruta guardada en los hallazgos.
func reviewedPaths(coverage nativeCoverage) map[string]bool {
	paths := make(map[string]bool, len(coverage.Completed)+len(coverage.Reused))
	for _, group := range [][]nativeCoverageItem{coverage.Completed, coverage.Reused} {
		for _, item := range group {
			path := strings.TrimPrefix(strings.TrimSpace(item.Path), "./")
			if path != "" {
				paths[path] = true
			}
		}
	}
	return paths
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
			Status:              store.FindingOpen,
			FirstSeenRunID:      prev.FirstSeenRunID,
			LastSeenRunID:       runID,
			GithubCommentID:     prev.GithubCommentID,
			GithubCommentNodeID: prev.GithubCommentNodeID,
			GithubThreadID:      prev.GithubThreadID,
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
