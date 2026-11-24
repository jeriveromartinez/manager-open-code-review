package review

import (
	"reflect"
	"testing"
)

func TestPolicyExcludePatternsKeepsValidGlobs(t *testing.T) {
	kept, dropped, err := policyExcludePatterns([]byte(`[" **/*.pb.go ", "", "vendor/**", "  ", "**/*_test.go"]`))
	if err != nil {
		t.Fatalf("policyExcludePatterns: %v", err)
	}
	if want := []string{"**/*.pb.go", "vendor/**", "**/*_test.go"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want %v", kept, want)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
}

// OCR splits --exclude on commas without escaping, so a brace pattern would
// arrive as two broken globs. Dropping it is the only safe outcome.
func TestPolicyExcludePatternsDropsPatternsWithCommas(t *testing.T) {
	kept, dropped, err := policyExcludePatterns([]byte(`["**/*.pb.go", "**/*.{go,js}", "docs/{a,b}.md"]`))
	if err != nil {
		t.Fatalf("policyExcludePatterns: %v", err)
	}
	if want := []string{"**/*.pb.go"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept = %v, want only the comma-free pattern", kept)
	}
	want := []string{"**/*.{go,js}", "docs/{a,b}.md"}
	if !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

func TestPolicyExcludePatternsHandlesEmptyValues(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":       nil,
		"blank":     []byte("  "),
		"empty":     []byte(`[]`),
		"blanks in": []byte(`["", "   "]`),
	} {
		t.Run(name, func(t *testing.T) {
			kept, dropped, err := policyExcludePatterns(raw)
			if err != nil {
				t.Fatalf("policyExcludePatterns: %v", err)
			}
			if len(kept) != 0 || len(dropped) != 0 {
				t.Errorf("kept = %v dropped = %v, want empty", kept, dropped)
			}
		})
	}
}

func TestPolicyExcludePatternsRejectsANonList(t *testing.T) {
	if _, _, err := policyExcludePatterns([]byte(`{"glob":"*.go"}`)); err == nil {
		t.Fatal("policyExcludePatterns accepted an object instead of a list")
	}
}
