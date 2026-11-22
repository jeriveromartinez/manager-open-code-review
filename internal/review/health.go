package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// healthPrompt is only used by the agent fallback for providers outside the
	// closed table. The normal probe sends a short ping through llmmodels.Ping.
	healthPrompt         = `Responde solo con {"ok":true}`
	defaultHealthTimeout = 2 * time.Minute
	maxHealthMessage     = 500
)

// ProfileChecker verifies that a stored LLM profile can actually answer and
// persists the outcome on the profile row.
type ProfileChecker struct {
	Store *store.Store
	Box   *crypto.Box
	// Bin runs the agent fallback for providers the closed table does not know
	// and that have no base URL override. Empty means "opencode".
	Bin string
	// Timeout bounds one check. Zero means defaultHealthTimeout.
	Timeout time.Duration
	Events  events.Publisher
}

// Check decrypts the profile key, sends one minimal tool-free request and
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
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	usage, cause := c.probe(checkCtx, profile, key)
	if cause != "" {
		message := healthFailureMessage(profileID, profile, cause)
		// A failed probe records no usage: NULL, never zero.
		if saveErr := c.Store.RecordLLMHealth(ctx, profileID, store.HealthError, message, nil); saveErr != nil {
			return fmt.Errorf("review: health check profile %d: %s; record failure: %w", profileID, cause, saveErr)
		}
		c.notify(ctx, profileID)
		return fmt.Errorf("review: health check profile %d: %s", profileID, cause)
	}

	if err := c.Store.RecordLLMHealth(ctx, profileID, store.HealthOK, "", usage); err != nil {
		return fmt.Errorf("review: record ok health for profile %d: %w", profileID, err)
	}
	c.notify(ctx, profileID)
	return nil
}

// probe returns the token usage of the request and an operator-facing cause, or
// an empty cause when the provider answered. The direct request is the normal
// path; the agent is only kept for providers OpenCode knows by name but this
// package's closed table does not.
func (c *ProfileChecker) probe(ctx context.Context, profile store.LLMProfile, key []byte) (*store.HealthUsage, string) {
	client := llmmodels.New(nil)
	baseURL := ""
	if profile.BaseURL != nil {
		baseURL = *profile.BaseURL
	}

	usage, err := client.PingUsage(ctx, profile.Provider, baseURL, profile.Model, string(key))
	switch {
	case err == nil:
		return healthUsage(usage), ""
	case errors.Is(err, llmmodels.ErrUnknownProvider):
		slog.Debug("health check falls back to opencode", "profile_id", profile.ID, "provider", profile.Provider)
		return c.probeAgent(ctx, profile, key)
	default:
		return nil, describeProbeError(profile, err)
	}
}

func (c *ProfileChecker) probeAgent(ctx context.Context, profile store.LLMProfile, key []byte) (*store.HealthUsage, string) {
	work, err := os.MkdirTemp("", "qa-health-")
	if err != nil {
		return nil, fmt.Sprintf("no se pudo preparar el entorno de OpenCode: %v", err)
	}
	defer os.RemoveAll(work)

	agent := &Agent{
		Bin:     c.Bin,
		Dir:     work,
		Home:    filepath.Join(work, "home"),
		Profile: profile,
		APIKey:  string(key),
	}
	text, raw, err := agent.Run(ctx, healthPrompt, nil)
	if err != nil {
		return nil, err.Error()
	}
	if strings.TrimSpace(text) == "" {
		return nil, "opencode returned no text"
	}
	return parseOpenCodeUsage(raw), ""
}

// healthUsage maps the provider usage to the store shape. A nil result means
// the provider reported none, which the history stores as NULL.
func healthUsage(usage *llmmodels.Usage) *store.HealthUsage {
	if usage == nil {
		return nil
	}
	return &store.HealthUsage{
		Input:      usage.Input,
		Output:     usage.Output,
		Reasoning:  usage.Reasoning,
		CacheRead:  usage.CacheRead,
		CacheWrite: usage.CacheWrite,
		Total:      usage.Total,
		MaxContext: usage.MaxContext,
	}
}

// parseOpenCodeUsage reads the aggregated JSON the agent emits. Unparseable or
// empty usage is unknown, never a failed health check.
func parseOpenCodeUsage(raw []byte) *store.HealthUsage {
	if len(raw) == 0 {
		return nil
	}
	var wire struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
		Total      int64 `json:"total"`
		MaxContext int64 `json:"max_context"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil
	}
	usage := &store.HealthUsage{
		Input: wire.Input, Output: wire.Output, Reasoning: wire.Reasoning,
		CacheRead: wire.Cache.Read, CacheWrite: wire.Cache.Write,
		Total: wire.Total, MaxContext: wire.MaxContext,
	}
	if usage.MaxContext == 0 {
		usage.MaxContext = usage.Input + usage.CacheRead + usage.CacheWrite
	}
	// OpenCode can omit the total while still reporting its parts. Deriving it
	// keeps the check out of the average as a zero-token one.
	if usage.Total == 0 {
		usage.Total = usage.Input + usage.Output + usage.Reasoning
	}
	if usage.Total == 0 && usage.Input == 0 && usage.Output == 0 && usage.Reasoning == 0 &&
		usage.CacheRead == 0 && usage.CacheWrite == 0 {
		return nil
	}
	return usage
}

// describeProbeError reuses the actionable sentences of describeAgentError so a
// profile shows the same message whichever path checked it. Transport errors are
// never echoed: a Google URL carries the key in the query string.
func describeProbeError(profile store.LLMProfile, err error) string {
	var upstream *llmmodels.UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Status == 0 {
			if errors.Is(upstream.Err, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				return "tiempo de espera agotado al contactar con el proveedor"
			}
			if upstream.Err != nil && isNetworkFailure(upstream.Err.Error()) {
				return "fallo de red/DNS al conectar con el proveedor"
			}
			return "no se pudo contactar con el proveedor"
		}
		data := agentErrorData{Status: upstream.Status, Message: upstream.Message}
		if upstream.Status == http.StatusNotFound {
			data.ProviderID = profile.Provider
			data.ModelID = profile.Model
		}
		return describeAgentError("", data)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "tiempo de espera agotado al contactar con el proveedor"
	}
	return err.Error()
}

// notify announces a stored verdict. The verdict is the check's result, so a
// lost notification is only logged.
func (c *ProfileChecker) notify(ctx context.Context, profileID uint) {
	if c.Events == nil {
		return
	}
	ev := events.Event{Type: "llm_profile.updated", ID: strconv.FormatUint(uint64(profileID), 10)}
	if err := c.Events.Publish(ctx, ev); err != nil {
		slog.Warn("publish event", "type", ev.Type, "id", ev.ID, "error", err)
	}
}

// healthFailureMessage persists the endpoint alongside the cause. A provider
// can fail generically, and the profile row alone does not say which
// provider/model was being checked.
func healthFailureMessage(profileID uint, profile store.LLMProfile, cause string) string {
	message := fmt.Sprintf("review: health check profile %d (%s/%s): %s",
		profileID, profile.Provider, profile.Model, cause)
	return truncateHealthMessage(message)
}

func truncateHealthMessage(message string) string {
	if len(message) <= maxHealthMessage {
		return message
	}
	return message[:maxHealthMessage]
}
