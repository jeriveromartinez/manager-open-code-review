package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	healthPrompt         = `Responde solo con {"ok":true}`
	defaultHealthTimeout = 60 * time.Second
	maxHealthMessage     = 500
)

// ProfileChecker verifies that a stored LLM profile can actually answer through
// OpenCode and persists the outcome on the profile row.
type ProfileChecker struct {
	Store   *store.Store
	Box     *crypto.Box
	Bin     string
	Timeout time.Duration
}

// Check decrypts the profile key, runs a minimal prompt in an isolated agent and
// records HealthOK or HealthError with a bounded message. A failed check returns
// an error after the failed health status has been persisted.
func (c *ProfileChecker) Check(ctx context.Context, profileID uint) error {
	if c == nil || c.Store == nil {
		return fmt.Errorf("review: health check profile %d: nil store", profileID)
	}
	if c.Box == nil {
		return fmt.Errorf("review: health check profile %d: nil crypto box", profileID)
	}

	profile, err := c.Store.LLMProfile(ctx, profileID)
	if err != nil {
		return fmt.Errorf("review: load llm profile %d: %w", profileID, err)
	}
	key, err := c.Box.Open(profile.APIKeyEnc)
	if err != nil {
		return fmt.Errorf("review: decrypt key for llm profile %d: %w", profileID, err)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}

	work, err := os.MkdirTemp("", "qa-health-")
	if err != nil {
		return fmt.Errorf("review: create health workspace: %w", err)
	}
	defer os.RemoveAll(work)

	agent := &Agent{
		Bin:     c.Bin,
		Dir:     work,
		Home:    filepath.Join(work, "home"),
		Profile: profile,
		APIKey:  string(key),
		Timeout: timeout,
	}

	text, _, err := agent.Run(ctx, healthPrompt, nil)
	if err != nil {
		message := truncateHealthMessage(err.Error())
		if saveErr := c.Store.SetLLMHealth(ctx, profileID, store.HealthError, message); saveErr != nil {
			return fmt.Errorf("review: health check profile %d: %v; record failure: %w", profileID, err, saveErr)
		}
		return fmt.Errorf("review: health check profile %d: %w", profileID, err)
	}
	if strings.TrimSpace(text) == "" {
		message := "opencode returned no text"
		if saveErr := c.Store.SetLLMHealth(ctx, profileID, store.HealthError, message); saveErr != nil {
			return fmt.Errorf("review: empty answer, record failure: %w", saveErr)
		}
		return fmt.Errorf("review: health check profile %d: %s", profileID, message)
	}

	if err := c.Store.SetLLMHealth(ctx, profileID, store.HealthOK, ""); err != nil {
		return fmt.Errorf("review: record ok health for profile %d: %w", profileID, err)
	}
	return nil
}

func truncateHealthMessage(message string) string {
	if len(message) <= maxHealthMessage {
		return message
	}
	return message[:maxHealthMessage]
}
