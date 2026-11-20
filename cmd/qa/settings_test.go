package main

import "testing"

func TestLegacyEnvSetReportsOnlyPresentVariables(t *testing.T) {
	env := map[string]string{
		"QA_REVIEW_TIMEOUT": "40m",
		"QA_OCR_BIN":        "/usr/bin/ocr",
		"QA_DATABASE_URL":   "postgres://x/y",
	}
	got := legacyEnvSet(func(key string) string { return env[key] })
	if len(got) != 2 {
		t.Fatalf("legacyEnvSet = %v, want the two legacy variables", got)
	}
	for _, key := range got {
		if key == "QA_DATABASE_URL" {
			t.Error("QA_DATABASE_URL is essential, not legacy")
		}
	}
}
