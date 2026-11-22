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

// The value is applied inside the pass loop, so the panel must not present it
// as a per-review limit.
func TestReviewAgentStepsLabelSaysPerPass(t *testing.T) {
	for _, field := range Fields() {
		if field.Key != KeyReviewAgentSteps {
			continue
		}
		if !strings.Contains(strings.ToLower(field.Label), "pasada") {
			t.Errorf("label = %q, want it to say the limit applies per pass", field.Label)
		}
		if !strings.Contains(field.Help, "max_passes") {
			t.Errorf("help = %q, want it to explain the relationship with max_passes", field.Help)
		}
		return
	}
	t.Fatalf("%s is missing from Fields()", KeyReviewAgentSteps)
}
