package settings

import (
	"strings"
	"testing"
)

// The panel used to render raw keys. Every field shown to a person needs a
// human label, an explanation and an input kind, or the settings screen is
// just a database view.
func TestFieldsCarryHumanMetadata(t *testing.T) {
	kinds := map[string]bool{
		"duration": true,
		"int":      true,
		"bool":     true,
		"url":      true,
		"text":     true,
		"path":     true,
		"secret":   true,
	}
	units := map[string]bool{"s": true, "m": true, "h": true}

	for _, field := range Fields() {
		if strings.TrimSpace(field.Label) == "" {
			t.Errorf("%s: missing label", field.Key)
		}
		if strings.TrimSpace(field.Help) == "" {
			t.Errorf("%s: missing help", field.Key)
		}
		if field.Label == field.Key {
			t.Errorf("%s: label repeats the raw key", field.Key)
		}
		if !kinds[field.Kind] {
			t.Errorf("%s: kind = %q, want one of duration, int, bool, url, text, path, secret", field.Key, field.Kind)
		}
		if field.Kind == "duration" && !units[field.Unit] {
			t.Errorf("%s: duration unit = %q, want s, m or h", field.Key, field.Unit)
		}
	}
}

// Reviews no longer run the OpenCode agent, so the historical key now bounds
// only the health probe fallback. The panel must say that instead of promising
// a per-pass review budget.
func TestReviewAgentStepsLabelDescribesHealthProbe(t *testing.T) {
	for _, field := range Fields() {
		if field.Key != KeyReviewAgentSteps {
			continue
		}
		if !strings.Contains(strings.ToLower(field.Label), "salud") {
			t.Errorf("label = %q, want it to name the health check", field.Label)
		}
		help := strings.ToLower(field.Help)
		if !strings.Contains(help, "salud") || !strings.Contains(help, "opencode") {
			t.Errorf("help = %q, want it to explain the opencode health probe limit", field.Help)
		}
		if strings.Contains(strings.ToLower(field.Label), "pasada") || strings.Contains(help, "max_passes") {
			t.Errorf("label/help still describe a per-pass review budget: %q / %q", field.Label, field.Help)
		}
		return
	}
	t.Fatalf("%s is missing from Fields()", KeyReviewAgentSteps)
}
