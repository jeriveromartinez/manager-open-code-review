package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

	got, err := checkBinary(context.Background(), bin)
	if err != nil {
		t.Fatalf("checkBinary: %v", err)
	}
	if got != "open-code-review v1.14.0 (d29ca204) linux/amd64" {
		t.Errorf("version = %q, want the pinned OCR version", got)
	}
}

func TestCheckBinaryFailsWithStderr(t *testing.T) {
	bin := writeScript(t, "echo 'missing git >= 2.41' >&2\nexit 1\n")

	_, err := checkBinary(context.Background(), bin)
	if err == nil {
		t.Fatal("checkBinary error = nil, want the startup check to fail")
	}
	if !strings.Contains(err.Error(), "missing git >= 2.41") {
		t.Errorf("error = %q, want the stderr in the message", err)
	}
}
