// Package llmmodels lists the models a provider offers and probes it with one
// minimal chat request, using the caller's API key and a fixed URL per provider.
package llmmodels

import (
	"bytes"
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
	// pingPrompt is the whole conversation of a health probe. The request is
	// measured in dozens of tokens instead of the agent's ~5k.
	pingPrompt = "ping"
	// maxErrorBody bounds the provider error body read from a failed probe.
	maxErrorBody = 64 << 10
	// maxErrorMessage bounds the provider sentence carried to the profile row.
	maxErrorMessage = 300
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
// Message is the provider's own bounded sentence, when it sent one.
type UpstreamError struct {
	Status  int
	Message string
	Err     error
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

// Usage is the token accounting a provider reports for one chat request. A nil
// *Usage means the provider returned none, which is different from a request
// that cost zero tokens. MaxContext is the prompt size of the request, the
// figure that counts against the model context ceiling.
type Usage struct {
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Total      int64
	MaxContext int64
}

// Ping verifies that the provider accepts the key and the model with one
// minimal, tool-free chat request. baseURL overrides the closed table, which is
// how a custom connection is stored; a provider outside the table with no
// override is ErrUnknownProvider. The request is bounded only by ctx, so the
// caller must pass a deadline.
func (c *Client) Ping(ctx context.Context, providerID, baseURL, model, apiKey string) error {
	_, err := c.PingUsage(ctx, providerID, baseURL, model, apiKey)
	return err
}

// PingUsage is Ping plus the token usage the provider reported. Usage parsing
// never turns a successful probe into a failure: an absent or unreadable usage
// block yields a nil Usage and a nil error.
func (c *Client) PingUsage(ctx context.Context, providerID, baseURL, model, apiKey string) (*Usage, error) {
	providers := c.Providers
	if providers == nil {
		providers = DefaultProviders
	}
	provider, ok := providers[providerID]
	if !ok {
		if strings.TrimSpace(baseURL) == "" {
			return nil, ErrUnknownProvider
		}
		// An unknown provider with an explicit endpoint is expected to speak
		// the OpenAI-compatible dialect, like most custom connections.
		provider = Provider{Shape: OpenAICompatible}
	}
	if strings.TrimSpace(baseURL) != "" {
		provider.BaseURL = strings.TrimSpace(baseURL)
	}

	endpoint := strings.TrimRight(provider.BaseURL, "/")
	var body []byte
	switch provider.Shape {
	case Anthropic:
		endpoint += "/messages"
		body, _ = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": pingPrompt}},
		})
	case Google:
		endpoint += "/models/" + url.PathEscape(model) + ":generateContent?key=" + url.QueryEscape(apiKey)
		body, _ = json.Marshal(map[string]any{
			"contents":         []map[string]any{{"parts": []map[string]string{{"text": pingPrompt}}}},
			"generationConfig": map[string]int{"maxOutputTokens": 1},
		})
	default:
		endpoint += "/chat/completions"
		payload := map[string]any{
			"model":    model,
			"stream":   false,
			"messages": []map[string]string{{"role": "user", "content": pingPrompt}},
		}
		// Reasoning models on the OpenAI API reject the deprecated max_tokens.
		if providerID == "openai" {
			payload["max_completion_tokens"] = 16
		} else {
			payload["max_tokens"] = 1
		}
		body, _ = json.Marshal(payload)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &UpstreamError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
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
		return nil, &UpstreamError{Err: err}
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if readErr != nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil, &UpstreamError{Err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: providerErrorMessage(raw)}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: "el proveedor devolvió una respuesta vacía"}
	}
	return parseUsage(provider.Shape, raw), nil
}

// parseUsage reads the token accounting of a successful response. Each shape
// nests usage differently; an unknown or empty block returns nil.
func parseUsage(shape Shape, raw []byte) *Usage {
	switch shape {
	case Anthropic:
		return parseAnthropicUsage(raw)
	case Google:
		return parseGoogleUsage(raw)
	default:
		return parseOpenAIUsage(raw)
	}
}

func parseOpenAIUsage(raw []byte) *Usage {
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || noUsageValue(payload.Usage) {
		return nil
	}
	var wire struct {
		PromptTokens             int64 `json:"prompt_tokens"`
		CompletionTokens         int64 `json:"completion_tokens"`
		TotalTokens              int64 `json:"total_tokens"`
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		PromptTokensDetails      struct {
			CachedTokens        int64 `json:"cached_tokens"`
			CacheWriteTokens    int64 `json:"cache_write_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(payload.Usage, &wire); err != nil {
		return nil
	}
	usage := &Usage{
		Input:      wire.PromptTokens,
		Output:     wire.CompletionTokens,
		Reasoning:  firstNonZero(wire.CompletionTokensDetails.ReasoningTokens, wire.ReasoningTokens),
		CacheRead:  firstNonZero(wire.PromptTokensDetails.CachedTokens, wire.CacheReadInputTokens),
		CacheWrite: firstNonZero(wire.PromptTokensDetails.CacheWriteTokens, wire.PromptTokensDetails.CacheCreationTokens, wire.CacheCreationInputTokens),
		Total:      wire.TotalTokens,
		// prompt_tokens already includes the cached part.
		MaxContext: wire.PromptTokens,
	}
	if usage.Total == 0 {
		usage.Total = usage.Input + usage.Output
	}
	if usage.MaxContext == 0 {
		usage.MaxContext = usage.Input + usage.CacheRead + usage.CacheWrite
	}
	return nonEmptyUsage(usage)
}

func parseAnthropicUsage(raw []byte) *Usage {
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || noUsageValue(payload.Usage) {
		return nil
	}
	var wire struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	}
	if err := json.Unmarshal(payload.Usage, &wire); err != nil {
		return nil
	}
	usage := &Usage{
		Input:      wire.InputTokens,
		Output:     wire.OutputTokens,
		CacheRead:  wire.CacheReadInputTokens,
		CacheWrite: wire.CacheCreationInputTokens,
	}
	// Anthropic reports cached input outside input_tokens and omits a total.
	usage.Total = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	usage.MaxContext = usage.Input + usage.CacheRead + usage.CacheWrite
	return nonEmptyUsage(usage)
}

func parseGoogleUsage(raw []byte) *Usage {
	var payload struct {
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || noUsageValue(payload.UsageMetadata) {
		return nil
	}
	var wire struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	}
	if err := json.Unmarshal(payload.UsageMetadata, &wire); err != nil {
		return nil
	}
	usage := &Usage{
		Input:     wire.PromptTokenCount,
		Output:    wire.CandidatesTokenCount,
		Reasoning: wire.ThoughtsTokenCount,
		CacheRead: wire.CachedContentTokenCount,
		Total:     wire.TotalTokenCount,
		// promptTokenCount already includes the cached content.
		MaxContext: wire.PromptTokenCount,
	}
	if usage.Total == 0 {
		usage.Total = usage.Input + usage.Output
	}
	return nonEmptyUsage(usage)
}

func noUsageValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// nonEmptyUsage treats an empty usage object as no data so it never dilutes an
// average as a zero-token check.
func nonEmptyUsage(usage *Usage) *Usage {
	if usage.Input == 0 && usage.Output == 0 && usage.CacheRead == 0 &&
		usage.CacheWrite == 0 && usage.Total == 0 {
		return nil
	}
	return usage
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// providerErrorMessage extracts the provider's own sentence from an error body
// written in the OpenAI, Anthropic or Google shape.
func providerErrorMessage(raw []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		if message := strings.TrimSpace(payload.Error.Message); message != "" {
			return truncateMessage(message)
		}
		if message := strings.TrimSpace(payload.Message); message != "" {
			return truncateMessage(message)
		}
	}
	return truncateMessage(strings.TrimSpace(string(raw)))
}

func truncateMessage(message string) string {
	if len(message) <= maxErrorMessage {
		return message
	}
	return message[:maxErrorMessage]
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
