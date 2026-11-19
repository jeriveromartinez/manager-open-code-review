package review

import (
	"fmt"
	"strings"
	"testing"
)

func validOutputJSON() string {
	return `{"summary":"ok","coverage":{"reviewed_files":1,"skipped_files":0},` +
		`"previous_findings":[{"id":"f_a","status":"open"}],` +
		`"findings":[{"path":"src/a.go","severity":"major","category":"bug",` +
		`"title":"x","body":"b","suggestion":"s","confidence":"high",` +
		`"start_line":10,"end_line":12}]}`
}

func TestValidateOutputRejectsUnknownField(t *testing.T) {
	if _, err := ValidateOutput(`{"summary":"x","extra":1}`); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestValidateOutputRejectsBadEnum(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"severity", "urgent"},
		{"category", "urgent"},
	} {
		raw := strings.Replace(validOutputJSON(),
			fmt.Sprintf(`"%s":"%s"`, tc.field, map[string]string{"severity": "major", "category": "bug"}[tc.field]),
			fmt.Sprintf(`"%s":"%s"`, tc.field, tc.value), 1)
		if _, err := ValidateOutput(raw); err == nil {
			t.Fatalf("expected error for bad %s", tc.field)
		}
	}
}

func TestValidateOutputAcceptsValid(t *testing.T) {
	out, err := ValidateOutput(validOutputJSON())
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary != "ok" || out.Coverage.ReviewedFiles != 1 || out.Coverage.SkippedFiles != 0 {
		t.Fatalf("bad summary/coverage: %+v", out)
	}
	if len(out.PreviousFindings) != 1 || out.PreviousFindings[0].ID != "f_a" {
		t.Fatalf("bad previous findings: %+v", out)
	}
	if len(out.Findings) != 1 || out.Findings[0].StartLine != 10 || out.Findings[0].EndLine != 12 {
		t.Fatalf("bad findings: %+v", out)
	}
}

func TestValidateOutputRejectsEmptySummary(t *testing.T) {
	for _, raw := range []string{
		`{"summary":""}`,
		`{"summary":"   "}`,
		`{}`,
	} {
		if _, err := ValidateOutput(raw); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}

func TestValidateOutputRejectsBadLines(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(validOutputJSON(), `"start_line":10`, `"start_line":0`, 1),
		strings.Replace(validOutputJSON(), `"start_line":10`, `"start_line":-1`, 1),
		strings.Replace(validOutputJSON(), `"end_line":12`, `"end_line":9`, 1),
	} {
		if _, err := ValidateOutput(raw); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}

func TestValidateOutputRejectsDuplicateFingerprints(t *testing.T) {
	f := `{"path":"src/a.go","severity":"major","category":"bug",` +
		`"title":"Igual","body":"","suggestion":"","confidence":"",` +
		`"start_line":5,"end_line":5}`
	fNorm := strings.Replace(f, `"title":"Igual"`, `"title":"  igual   "`, 1)
	for _, raw := range []string{
		`{"summary":"ok","findings":[` + f + `,` + f + `]}`,
		`{"summary":"ok","findings":[` + f + `,` + fNorm + `]}`,
	} {
		if _, err := ValidateOutput(raw); err == nil {
			t.Fatal("expected error for duplicate fingerprints")
		}
	}
}
