package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/settings"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the worker dependency check runs inside a Linux image")
	}
	bin := filepath.Join(t.TempDir(), "ocr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return bin
}

func TestCheckBinaryReportsVersion(t *testing.T) {
	bin := writeScript(t, "echo 'open-code-review v1.14.0 (d29ca204) linux/amd64'\n")

	got, err := checkBinary(context.Background(), settings.KeyOCRBin, bin)
	if err != nil {
		t.Fatalf("checkBinary: %v", err)
	}
	if got != "open-code-review v1.14.0 (d29ca204) linux/amd64" {
		t.Errorf("version = %q, want the pinned OCR version", got)
	}
}

func TestCheckBinaryFailsWithStderr(t *testing.T) {
	bin := writeScript(t, "echo 'missing git >= 2.41' >&2\nexit 1\n")

	_, err := checkBinary(context.Background(), settings.KeyOCRBin, bin)
	if err == nil {
		t.Fatal("checkBinary error = nil, want the startup check to fail")
	}
	if !strings.Contains(err.Error(), "missing git >= 2.41") {
		t.Errorf("error = %q, want the stderr in the message", err)
	}
	if !strings.Contains(err.Error(), settings.KeyOCRBin) {
		t.Errorf("error = %q, want the setting named so the panel points at the field", err)
	}
}

// A hung binary must not block the worker startup forever: the probe has its
// own deadline and names the setting and the phase.
func TestCheckBinaryProbeIsBounded(t *testing.T) {
	bin := writeScript(t, "sleep 5\n")
	previous := binaryProbeTimeout
	binaryProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { binaryProbeTimeout = previous })

	started := time.Now()
	_, err := checkBinary(context.Background(), settings.KeyOCRBin, bin)
	if err == nil {
		t.Fatal("checkBinary error = nil, want the probe to time out")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("probe took %s, want it bounded by the configured timeout", elapsed)
	}
	for _, want := range []string{settings.KeyOCRBin, "did not finish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	}
}
