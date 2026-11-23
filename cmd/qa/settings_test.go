package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestLegacyEnvSetReportsOnlyPresentVariables(t *testing.T) {
	env := map[string]string{
		"QA_REVIEW_TIMEOUT": "40m",
		"QA_OCR_BIN":        "/usr/bin/ocr",
		"QA_DATABASE_URL":   "postgres://x/y",
	}
	got := legacyEnvSet(func(key string) string { return env[key] })

	want := map[string]bool{"QA_REVIEW_TIMEOUT": true, "QA_OCR_BIN": true}
	if len(got) != len(want) {
		t.Fatalf("legacyEnvSet = %v, want exactly %v", got, want)
	}
	seen := make(map[string]bool, len(got))
	for _, key := range got {
		if !want[key] {
			t.Errorf("legacyEnvSet returned %q, want only the two legacy variables", key)
		}
		if seen[key] {
			t.Errorf("legacyEnvSet returned %q twice", key)
		}
		seen[key] = true
	}
}

func TestLoadSettingsAppliesStoredOverrides(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	const storedBin = "/usr/local/bin/ocr"
	if err := st.SaveSettings(ctx, nil, map[string]string{settings.KeyOCRBin: storedBin}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg := config.Default()
	wantOpencodeBin := cfg.OpencodeBin
	if err := loadSettings(ctx, st, &cfg); err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	if cfg.OCRBin != storedBin {
		t.Errorf("OCRBin = %q, want the stored %q", cfg.OCRBin, storedBin)
	}
	if cfg.OpencodeBin != wantOpencodeBin {
		t.Errorf("OpencodeBin = %q, want the default %q", cfg.OpencodeBin, wantOpencodeBin)
	}
}

func TestLoadSettingsRejectsACorruptRow(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	if err := st.SaveSettings(ctx, nil, map[string]string{settings.KeyReviewTimeout: "0s"}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg := config.Default()
	if err := loadSettings(ctx, st, &cfg); err == nil {
		t.Fatal("loadSettings accepted review_timeout 0s; a corrupt row must abort startup")
	}
}

// An install upgraded from a release that accepted any stale_after above the
// timeout must still start: 45m/48m was valid when it was saved, and aborting
// here leaves SQL as the only way to boot (#130).
func TestLoadSettingsRaisesAStoredPairBelowTheMinimum(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := context.Background()
	st := testutil.NewStore(t)

	if err := st.SaveSettings(ctx, nil, map[string]string{
		settings.KeyReviewTimeout:    "45m",
		settings.KeyReviewStaleAfter: "48m",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	cfg := config.Default()
	if err := loadSettings(ctx, st, &cfg); err != nil {
		t.Fatalf("loadSettings(45m/48m) = %v, want the upgraded install to start", err)
	}
	if cfg.ReviewTimeout != 45*time.Minute {
		t.Errorf("ReviewTimeout = %s, want the stored 45m", cfg.ReviewTimeout)
	}
	if cfg.ReviewStaleAfter != 50*time.Minute {
		t.Errorf("ReviewStaleAfter = %s, want the repaired 50m", cfg.ReviewStaleAfter)
	}
	if logged := logs.String(); !strings.Contains(logged, settings.KeyReviewStaleAfter) ||
		!strings.Contains(logged, "48m0s") || !strings.Contains(logged, "50m0s") {
		t.Errorf("warning does not name the repaired key and both values; log was %q", logged)
	}
}

func TestLoadSettingsWarnsAboutLegacyVariables(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	t.Setenv("QA_REVIEW_TIMEOUT", "40m")
	t.Setenv("QA_OCR_BIN", "/usr/bin/ocr")

	ctx := context.Background()
	st := testutil.NewStore(t)

	cfg := config.Default()
	if err := loadSettings(ctx, st, &cfg); err != nil {
		t.Fatalf("loadSettings: %v", err)
	}

	for _, key := range []string{"QA_REVIEW_TIMEOUT", "QA_OCR_BIN"} {
		if !strings.Contains(logs.String(), key) {
			t.Errorf("warning does not name %s; log was %q", key, logs.String())
		}
	}
}
