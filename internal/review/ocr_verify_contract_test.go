package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyRuleLoadsInOCRV1141 is the contract check for the rule file handed
// to `ocr scan --rule`. OCR 1.14.1 reads a ProjectRule ({"rules":[...]}) and
// silently falls back to the default rule when the schema does not match, so
// the file would produce answers without QA-VERIFY instructions. The test runs
// the real CLI and skips cleanly when no OCR binary is installed.
func TestVerifyRuleLoadsInOCRV1141(t *testing.T) {
	bin, err := exec.LookPath("ocr")
	if err != nil {
		t.Skip("ocr binary not installed; skipping the OCR 1.14.1 rule contract")
	}
	version, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("ocr --version failed: %v; run the contract inside the worker image", err)
	}
	pinned := strings.TrimSpace(string(version))
	if !strings.Contains(pinned, "1.14.") {
		t.Skipf("ocr %q is not 1.14.x; the contract targets the worker image", pinned)
	}

	dir := t.TempDir()
	initGit := exec.Command("git", "init")
	initGit.Dir = dir
	if out, err := initGit.CombinedOutput(); err != nil {
		t.Skipf("git init: %v; run the contract inside the worker image\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatalf("write sample file: %v", err)
	}
	rulePath := filepath.Join(dir, "verify-rule.json")
	if err := os.WriteFile(rulePath, verifyRuleJSON(), 0o600); err != nil {
		t.Fatalf("write rule: %v", err)
	}

	out, err := exec.Command(bin, "rules", "check",
		"--repo", dir, "--rule", rulePath, "sample.go").CombinedOutput()
	if err != nil {
		t.Fatalf("ocr %q rules check: %v\n%s", pinned, err, out)
	}
	text := string(out)
	for _, want := range []string{"Custom (--rule)", "Pattern: **/*", "QA-VERIFY", "EVIDENCE:"} {
		if !strings.Contains(text, want) {
			t.Errorf("ocr rules check output does not contain %q:\n%s", want, text)
		}
	}
}
