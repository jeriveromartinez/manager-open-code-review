package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	defaultHealthTimeout = 2 * time.Minute
	maxHealthMessage     = 500
)

// ProfileChecker verifies that a stored LLM profile can actually answer and
// persists the outcome on the profile row.
type ProfileChecker struct {
	Store *store.Store
	Box   *crypto.Box
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
	// Capture the configuration the probe measures. If the profile is rotated
	// while the request is in flight, the verdict describes a destination that
	// no longer exists and must be discarded instead of published.
	revision := store.ProfileConfigRevision(profile)
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
		if saveErr := c.Store.RecordLLMHealthAt(ctx, profileID, revision, store.HealthError, message, nil); saveErr != nil {
			if errors.Is(saveErr, store.ErrStaleHealthCheck) {
				slog.Info("health check superseded by a profile change",
					"profile_id", profileID)
				return nil
			}
			return fmt.Errorf("review: health check profile %d: %s; record failure: %w", profileID, cause, saveErr)
		}
		c.notify(ctx, profileID)
		return fmt.Errorf("review: health check profile %d: %s", profileID, cause)
	}

	if err := c.Store.RecordLLMHealthAt(ctx, profileID, revision, store.HealthOK, "", usage); err != nil {
		if errors.Is(err, store.ErrStaleHealthCheck) {
			slog.Info("health check superseded by a profile change", "profile_id", profileID)
			return nil
		}
		return fmt.Errorf("review: record ok health for profile %d: %w", profileID, err)
	}
	c.notify(ctx, profileID)
	return nil
}

// probe sends the profile through the same endpoint OCR resolves for a review,
// so a profile the native engine would reject can never be marked healthy. The
// probe speaks the protocol OCR configured: OpenAI-compatible for unknown or
// Google-compatible destinations, Anthropic for the native API.
func (c *ProfileChecker) probe(ctx context.Context, profile store.LLMProfile, key []byte) (*store.HealthUsage, string) {
	endpoint, protocol, err := nativeEndpoint(profile)
	if err != nil {
		return nil, "OCR no puede usar este perfil: " + err.Error()
	}
	providerID := ""
	if protocol == "anthropic" {
		providerID = "anthropic"
	} else if provider, ok := llmmodels.DefaultProviders[profile.Provider]; ok &&
		provider.Shape == llmmodels.OpenAICompatible {
		// Keep the provider's own request shape (OpenAI rejects max_tokens on
		// reasoning models); the endpoint is the one OCR will use.
		providerID = profile.Provider
	}
	usage, err := llmmodels.New(nil).PingUsage(ctx, providerID, endpoint, profile.Model, string(key))
	if err != nil {
		return nil, describeProbeError(profile, err)
	}
	return healthUsage(usage), ""
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
