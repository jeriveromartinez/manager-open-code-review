package review

import (
	"reflect"
	"testing"
)

func TestPolicyExcludesBuildsCommaSeparatedFlag(t *testing.T) {
	arg, dropped, err := policyExcludes([]byte(`[" **/*.pb.go ", "", "vendor/**", "  ", "**/*_test.go"]`))
	if err != nil {
		t.Fatalf("policyExcludes: %v", err)
	}
	if want := "**/*.pb.go,vendor/**,**/*_test.go"; arg != want {
		t.Errorf("arg = %q, want %q", arg, want)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
}

// OCR splits --exclude on commas without escaping, so a brace pattern would
// arrive as two broken globs. Dropping it is the only safe outcome.
func TestPolicyExcludesDropsPatternsWithCommas(t *testing.T) {
	arg, dropped, err := policyExcludes([]byte(`["**/*.pb.go", "**/*.{go,js}", "docs/{a,b}.md"]`))
	if err != nil {
		t.Fatalf("policyExcludes: %v", err)
	}
	if arg != "**/*.pb.go" {
		t.Errorf("arg = %q, want only the comma-free pattern", arg)
	}
	want := []string{"**/*.{go,js}", "docs/{a,b}.md"}
	if !reflect.DeepEqual(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

func TestPolicyExcludesHandlesEmptyValues(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":       nil,
		"blank":     []byte("  "),
		"empty":     []byte(`[]`),
		"blanks in": []byte(`["", "   "]`),
	} {
		t.Run(name, func(t *testing.T) {
			arg, dropped, err := policyExcludes(raw)
			if err != nil {
				t.Fatalf("policyExcludes: %v", err)
			}
			if arg != "" || len(dropped) != 0 {
				t.Errorf("arg = %q dropped = %v, want empty", arg, dropped)
			}
		})
	}
}

func TestPolicyExcludesRejectsANonList(t *testing.T) {
	if _, _, err := policyExcludes([]byte(`{"glob":"*.go"}`)); err == nil {
		t.Fatal("policyExcludes accepted an object instead of a list")
	}
}
