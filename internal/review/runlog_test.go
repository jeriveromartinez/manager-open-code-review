package review

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestRedactRunLogLine(t *testing.T) {
	line := `clonando https://x-access-token:ghs_secret@example.invalid/acme/origin.git con sk-test`
	got := redactRunLogLine(line, "sk-test")
	if strings.Contains(got, "ghs_secret") || strings.Contains(got, "sk-test") {
		t.Fatalf("redacted line still carries a secret: %q", got)
	}
	if !strings.Contains(got, "https://[redactado]@example.invalid/acme/origin.git") {
		t.Errorf("URL userinfo was not redacted: %q", got)
	}
	if !strings.Contains(got, "[redactado]") {
		t.Errorf("secret was not replaced: %q", got)
	}
}

func TestClassifyOCRLevel(t *testing.T) {
	cases := map[string]string{
		"fase: leyendo archivos":       "info",
		"warning: archivo muy grande":  "warn",
		"ERROR: proveedor no responde": "error",
		"request failed after 3 tries": "error",
	}
	for line, want := range cases {
		if got := classifyOCRLevel(line); got != want {
			t.Errorf("classifyOCRLevel(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestRunnerStreamsOCRProgressIntoReviewLog(t *testing.T) {
	f := newRunTestFixture(t)
	t.Setenv("RUN_FAKE_OCR_PROGRESS", "1")

	if err := f.runner().Run(f.ctx, f.runID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	logs, err := f.s.ReviewRunLogs(f.ctx, f.runID, 0, 1000)
	if err != nil {
		t.Fatalf("ReviewRunLogs() error = %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("run log is empty")
	}

	phases := make(map[string]bool)
	var ocrLines []string
	for _, entry := range logs {
		phases[entry.Phase] = true
		if strings.Contains(entry.Line, "sk-test") || strings.Contains(entry.Line, "secreto-en-url") {
			t.Fatalf("run log leaked a secret: %q", entry.Line)
		}
		if entry.Source == "ocr" {
			ocrLines = append(ocrLines, entry.Line)
		}
	}
	for _, phase := range []string{logPhaseClaim, logPhaseClone, logPhasePolicy, logPhaseOCR, logPhaseFinish} {
		if !phases[phase] {
			t.Errorf("run log has no %q phase: %+v", phase, logs)
		}
	}
	if len(ocrLines) == 0 {
		t.Fatal("run log has no OCR progress lines")
	}
	joined := strings.Join(ocrLines, "\n")
	for _, want := range []string{
		"fase: recopilando archivos",
		"clave en uso: [redactado]",
		"https://[redactado]@example.invalid/acme/origin.git",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("OCR log lines missing %q:\n%s", want, joined)
		}
	}

	var last store.ReviewRunLog
	for _, entry := range logs {
		if entry.Phase == logPhaseFinish {
			last = entry
		}
	}
	if !strings.Contains(last.Line, "succeeded") {
		t.Errorf("finish line = %q, want the succeeded status", last.Line)
	}

	if ev := (events.Event{Type: "run.log", ID: strconv.FormatUint(uint64(f.runID), 10)}); !f.events.has(ev) {
		t.Errorf("run.log event not published; got %+v", f.events.snapshot())
	}
}

func TestRunLogSinkTruncatesAtCap(t *testing.T) {
	f := newRunTestFixture(t)
	sink := newRunLogSink(f.s, nil, f.runID, 120)

	for i := 0; i < 20; i++ {
		sink.Log(f.ctx, "worker", "", "info", logPhasePrepare, fmt.Sprintf("línea %d %s", i, strings.Repeat("x", 40)))
	}

	logs, err := f.s.ReviewRunLogs(f.ctx, f.runID, 0, 1000)
	if err != nil {
		t.Fatalf("ReviewRunLogs() error = %v", err)
	}
	if len(logs) >= 20 {
		t.Fatalf("log was not truncated: %d lines", len(logs))
	}
	last := logs[len(logs)-1]
	if last.Level != "warn" || !strings.Contains(last.Line, "truncado") {
		t.Fatalf("last line = %+v, want the truncation warning", last)
	}
}
