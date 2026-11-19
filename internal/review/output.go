package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// AgentFinding es un hallazgo propuesto por el agente de revisión.
type AgentFinding struct {
	Path       string `json:"path"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion"`
	Confidence string `json:"confidence"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
}

// AgentOutput es la salida estructurada esperada del agente de revisión.
type AgentOutput struct {
	Summary  string `json:"summary"`
	Coverage struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	} `json:"coverage"`
	PreviousFindings []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"previous_findings"`
	Findings []AgentFinding `json:"findings"`
}

var (
	validSeverity = map[string]bool{
		"blocker": true,
		"major":   true,
		"minor":   true,
		"nit":     true,
	}
	validCategory = map[string]bool{
		"bug":             true,
		"security":        true,
		"performance":     true,
		"maintainability": true,
		"style":           true,
		"test":            true,
	}
)

// ValidateOutput decodifica y valida la salida del agente. Rechaza campos
// desconocidos, resúmenes vacíos, enums inválidos, líneas inconsistentes y
// hallazgos con huellas dactilares duplicadas.
func ValidateOutput(raw string) (AgentOutput, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var out AgentOutput
	if err := dec.Decode(&out); err != nil {
		return AgentOutput{}, fmt.Errorf("decode output: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return AgentOutput{}, errors.New("output: datos sobrantes después del JSON")
	}

	if strings.TrimSpace(out.Summary) == "" {
		return AgentOutput{}, errors.New("output: summary requerido")
	}

	seen := make(map[string]bool, len(out.Findings))
	for i, f := range out.Findings {
		if !validSeverity[f.Severity] {
			return AgentOutput{}, fmt.Errorf("finding %d: severity inválido %q", i, f.Severity)
		}
		if !validCategory[f.Category] {
			return AgentOutput{}, fmt.Errorf("finding %d: category inválido %q", i, f.Category)
		}
		if f.StartLine < 1 {
			return AgentOutput{}, fmt.Errorf("finding %d: start_line debe ser >= 1", i)
		}
		if f.EndLine < f.StartLine {
			return AgentOutput{}, fmt.Errorf("finding %d: end_line debe ser >= start_line", i)
		}
		fp := Fingerprint(f, nil)
		if seen[fp] {
			return AgentOutput{}, fmt.Errorf("finding %d: fingerprint duplicado", i)
		}
		seen[fp] = true
	}
	return out, nil
}
