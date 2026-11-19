package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// PreviewFile es un archivo revisable informado por `ocr delegate preview`.
type PreviewFile struct {
	Path       string `json:"path"`
	Status     string `json:"status"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
}

// ExcludedFile es un archivo descartado por `ocr delegate preview`.
type ExcludedFile struct {
	Path   string `json:"path"`
	Reason string `json:"exclude_reason"`
}

// PreviewResult es el subconjunto de la salida de preview que usa el motor.
type PreviewResult struct {
	Mode            string         `json:"mode"`
	MergeBase       string         `json:"merge_base"`
	ReviewableFiles []PreviewFile  `json:"reviewable_files"`
	ExcludedFiles   []ExcludedFile `json:"excluded_files"`
}

// RuleGroup asocia el texto de una regla con los archivos a los que aplica.
type RuleGroup struct {
	Rules string   `json:"rule"`
	Files []string `json:"files"`
}

// OCR ejecuta el binario `ocr` contra un repositorio.
type OCR struct {
	Bin     string
	RepoDir string
}

// Preview invoca `ocr delegate preview` para el rango [baseRef, head].
func (o *OCR) Preview(ctx context.Context, baseRef, head string) (PreviewResult, error) {
	out, err := o.run(ctx, "preview", baseRef, head, nil)
	if err != nil {
		return PreviewResult{}, err
	}
	return parsePreview(out)
}

// Rules invoca `ocr delegate rule` para los paths indicados.
func (o *OCR) Rules(ctx context.Context, baseRef, head string, paths []string) ([]RuleGroup, error) {
	out, err := o.run(ctx, "rule", baseRef, head, paths)
	if err != nil {
		return nil, err
	}
	return parseRules(out)
}

func (o *OCR) run(ctx context.Context, subcommand, baseRef, head string, paths []string) ([]byte, error) {
	args := []string{
		"delegate", subcommand,
		"--format", "json",
		"--repo", o.RepoDir,
		"--from", baseRef,
		"--to", head,
	}
	args = append(args, paths...)

	cmd := exec.CommandContext(ctx, o.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ocr %s: %w: %s", subcommand, err, stderr.String())
	}
	return out, nil
}

func parsePreview(b []byte) (PreviewResult, error) {
	var got PreviewResult
	if err := json.Unmarshal(b, &got); err != nil {
		return PreviewResult{}, fmt.Errorf("parse preview: %w", err)
	}
	if got.MergeBase == "" {
		return PreviewResult{}, fmt.Errorf("parse preview: falta merge_base")
	}
	if got.ReviewableFiles == nil {
		return PreviewResult{}, fmt.Errorf("parse preview: falta reviewable_files")
	}
	return got, nil
}

func parseRules(b []byte) ([]RuleGroup, error) {
	var envelope struct {
		Groups []RuleGroup `json:"groups"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	if envelope.Groups == nil {
		return nil, fmt.Errorf("parse rules: falta groups")
	}
	return envelope.Groups, nil
}
