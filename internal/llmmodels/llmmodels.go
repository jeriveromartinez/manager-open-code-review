// Package llmmodels lists the models a provider offers, using the caller's API
// key and a fixed URL per provider.
package llmmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultTimeout   = 10 * time.Second
	maxResponseBytes = 2 << 20
	maxModels        = 500
)

// Model is one entry a provider exposes for chat completions.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Shape identifies how a provider answers its model-list request.
type Shape int

const (
	OpenAICompatible Shape = iota
	Anthropic
	Google
)

// Provider is a closed entry of the supported platform table. The base URL is
// never taken from the client, so the endpoint cannot be pointed elsewhere.
type Provider struct {
	BaseURL string
	Shape   Shape
}

// DefaultProviders mirrors the platform catalog in web/src/llmPlatforms.ts.
var DefaultProviders = map[string]Provider{
	"openai":       {BaseURL: "https://api.openai.com/v1", Shape: OpenAICompatible},
	"anthropic":    {BaseURL: "https://api.anthropic.com/v1", Shape: Anthropic},
	"opencode":     {BaseURL: "https://opencode.ai/inference/openai/v1", Shape: OpenAICompatible},
	"google":       {BaseURL: "https://generativelanguage.googleapis.com/v1beta", Shape: Google},
	"openrouter":   {BaseURL: "https://openrouter.ai/api/v1", Shape: OpenAICompatible},
	"commandcode":  {BaseURL: "https://api.commandcode.ai/provider/v1", Shape: OpenAICompatible},
	"groq":         {BaseURL: "https://api.groq.com/openai/v1", Shape: OpenAICompatible},
	"mistral":      {BaseURL: "https://api.mistral.ai/v1", Shape: OpenAICompatible},
	"deepseek":     {BaseURL: "https://api.deepseek.com", Shape: OpenAICompatible},
	"moonshotai":   {BaseURL: "https://api.moonshot.ai/v1", Shape: OpenAICompatible},
	"zai":          {BaseURL: "https://api.z.ai/api/paas/v4", Shape: OpenAICompatible},
	"xai":          {BaseURL: "https://api.x.ai/v1", Shape: OpenAICompatible},
	"togetherai":   {BaseURL: "https://api.together.xyz/v1", Shape: OpenAICompatible},
	"fireworks-ai": {BaseURL: "https://api.fireworks.ai/inference/v1", Shape: OpenAICompatible},
}

// ErrUnknownProvider means the caller asked for a provider outside the table.
var ErrUnknownProvider = errors.New("unknown provider")

// ProviderBaseURL returns the fixed endpoint of a platform in the closed table.
// The review agent uses it for providers OpenCode does not know by name.
func ProviderBaseURL(providerID string) (string, bool) {
	provider, ok := DefaultProviders[providerID]
	return provider.BaseURL, ok
}

// UpstreamError reports a provider that failed or did not answer. Error() is
// safe to log and return: it never includes the key, the URL or the body.
type UpstreamError struct {
	Status int
	Err    error
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("provider returned HTTP %d", e.Status)
	}
	return "provider request failed"
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// Client calls the provider model endpoints. Providers and HTTP are injectable
// so tests can point a shape at an httptest server.
type Client struct {
	HTTP      *http.Client
	Timeout   time.Duration
	Providers map[string]Provider
}

// New returns a client with the real provider table and the given HTTP client.
func New(httpClient *http.Client) *Client {
	return &Client{HTTP: httpClient, Providers: DefaultProviders}
}

// Models returns the chat-capable models the provider offers for this key.
func (c *Client) Models(ctx context.Context, providerID, apiKey string) ([]Model, error) {
	providers := c.Providers
	if providers == nil {
		providers = DefaultProviders
	}
	provider, ok := providers[providerID]
	if !ok {
		return nil, ErrUnknownProvider
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := strings.TrimRight(provider.BaseURL, "/") + "/models"
	switch provider.Shape {
	case Anthropic:
		endpoint += "?limit=1000"
	case Google:
		endpoint += "?key=" + url.QueryEscape(apiKey) + "&pageSize=1000"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &UpstreamError{Status: 0, Err: err}
	}
	switch provider.Shape {
	case OpenAICompatible:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	case Anthropic:
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &UpstreamError{Status: 0, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &UpstreamError{Status: resp.StatusCode}
	}

	body := io.LimitReader(resp.Body, maxResponseBytes)
	switch provider.Shape {
	case Anthropic:
		var payload struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
		}
		if err := json.NewDecoder(body).Decode(&payload); err != nil {
			return nil, &UpstreamError{Status: 0, Err: err}
		}
		models := make([]Model, 0, len(payload.Data))
		for _, entry := range payload.Data {
			models = append(models, Model{ID: entry.ID, Name: entry.DisplayName})
		}
		return finalize(models, provider.Shape), nil
	case Google:
		var payload struct {
			Models []struct {
				Name                       string   `json:"name"`
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.NewDecoder(body).Decode(&payload); err != nil {
			return nil, &UpstreamError{Status: 0, Err: err}
		}
		models := make([]Model, 0, len(payload.Models))
		for _, entry := range payload.Models {
			if !supportsGenerateContent(entry.SupportedGenerationMethods) {
				continue
			}
			models = append(models, Model{
				ID:   strings.TrimPrefix(entry.Name, "models/"),
				Name: entry.DisplayName,
			})
		}
		return finalize(models, provider.Shape), nil
	default:
		var payload struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.NewDecoder(body).Decode(&payload); err != nil {
			return nil, &UpstreamError{Status: 0, Err: err}
		}
		models := make([]Model, 0, len(payload.Data))
		for _, entry := range payload.Data {
			models = append(models, Model{ID: entry.ID, Name: entry.Name})
		}
		return finalize(models, provider.Shape), nil
	}
}

func supportsGenerateContent(methods []string) bool {
	for _, method := range methods {
		if method == "generateContent" {
			return true
		}
	}
	return false
}

var nonChatPrefixes = []string{
	"text-embedding", "embedding", "whisper", "tts", "dall-e", "gpt-image",
	"omni-moderation", "moderation", "rerank", "transcribe", "speech",
	"audio", "realtime", "sora", "image",
}

func isNonChat(id string) bool {
	lower := strings.ToLower(id)
	for _, prefix := range nonChatPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// finalize trims, filters, dedupes, names, sorts and caps the raw entries.
func finalize(models []Model, shape Shape) []Model {
	seen := make(map[string]int, len(models))
	result := make([]Model, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if shape != Google && isNonChat(id) {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if index, ok := seen[id]; ok {
			if result[index].Name == "" && name != "" {
				result[index].Name = name
			}
			continue
		}
		seen[id] = len(result)
		result = append(result, Model{ID: id, Name: name})
	}
	for index := range result {
		if result[index].Name == "" {
			result[index].Name = result[index].ID
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].Name), strings.ToLower(result[j].Name)
		if left == right {
			return result[i].ID < result[j].ID
		}
		return left < right
	})
	if len(result) > maxModels {
		result = result[:maxModels]
	}
	return result
}
