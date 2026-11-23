package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// legacyEnvSet returns the variables that no longer configure anything. The
// startup warning is the only signal an operator gets that their .env stopped
// having an effect.
func legacyEnvSet(getenv func(string) string) []string {
	var found []string
	for _, key := range config.LegacyEnvKeys() {
		if getenv(key) != "" {
			found = append(found, key)
		}
	}
	return found
}

// loadSettings applies the stored overrides on top of the defaults. A value that
// does not parse is corruption, not a reason to keep running with a half-written
// config, so the error aborts startup. A pair an upgraded release tightened is
// only reported: the stored row was valid when it was saved, and refusing it
// would leave SQL as the only way to boot (#130).
func loadSettings(ctx context.Context, st *store.Store, cfg *config.Config) error {
	values, err := st.Settings(ctx)
	if err != nil {
		return fmt.Errorf("read app settings: %w", err)
	}
	adjusted, err := settings.ApplyStored(cfg, values)
	if err != nil {
		return fmt.Errorf("apply stored settings: %w", err)
	}
	if len(adjusted) > 0 {
		// The panel shows the stored row, so naming the keys here is what lets
		// an operator save the pair the process actually uses.
		slog.Warn("stored settings were raised to meet the current minimums; review them in the panel",
			"settings", adjusted)
	}
	if legacy := legacyEnvSet(getenvOrEmpty); len(legacy) > 0 {
		slog.Warn("these variables are ignored; configure them in the panel",
			"variables", legacy)
	}
	return nil
}

func getenvOrEmpty(key string) string { return os.Getenv(key) }
