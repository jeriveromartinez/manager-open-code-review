package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeProvider is an OpenAI-compatible Chat Completions endpoint for tests. It
// records every request and lets the test script the reply, which is enough to
// measure how an agent loop spends tokens without calling a real provider.
type FakeProvider struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []ProviderRequest
	respond func(ProviderRequest) ProviderReply
}

// ProviderRequest is one Chat Completions request observed by FakeProvider.
type ProviderRequest struct {
	Index                 int
	Path                  string
	Model                 string
	Stream                bool
	Messages              []ProviderMessage
	Tools                 []string
	System                string
	PromptCacheKey        string
	HasCacheControl       bool
	BodyBytes             int
	EstimatedInputTokens  int
	EstimatedOutputTokens int
	TotalTokens           int
}

// ProviderMessage is the subset of a chat message the bank needs to inspect.
type ProviderMessage struct {
	Role       string
	Content    string
	ToolCalls  []ProviderToolCall
	ToolCallID string
}

// ProviderToolCall is one function call requested by the fake model.
type ProviderToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResultCount counts the tool messages already in the conversation: one per
// executed tool call.
func (r ProviderRequest) ToolResultCount() int {
	count := 0
	for _, message := range r.Messages {
		if message.Role == "tool" || message.ToolCallID != "" {
			count++
		}
	}
	return count
}

// ProviderUsage is the token accounting included in the fake response.
type ProviderUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	ReasoningTokens  int
}

// ProviderReply is what the fake provider returns for one request. Set Text for
// a final answer or ToolCalls for another loop iteration. Status defaults to
// 200; Body is used as the error message for non-2xx replies.
type ProviderReply struct {
	Text      string
	ToolCalls []ProviderToolCall
	Usage     ProviderUsage
	Status    int
	Body      string
}

// NewFakeProvider starts an HTTP server and stops it when the test finishes.
func NewFakeProvider(t *testing.T, respond func(ProviderRequest) ProviderReply) *FakeProvider {
	t.Helper()
	if respond == nil {
		respond = func(ProviderRequest) ProviderReply {
			return ProviderReply{Text: `{"ok":true}`}
		}
	}
	p := &FakeProvider{t: t, respond: respond}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

// URL is the base URL to store in a profile; the provider appends
// /chat/completions to it.
func (p *FakeProvider) URL() string {
	return p.srv.URL + "/v1"
}

// Requests returns a snapshot of the chat requests observed so far.
func (p *FakeProvider) Requests() []ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProviderRequest(nil), p.reqs...)
}

// Reset forgets the recorded requests.
func (p *FakeProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = nil
}

// TotalEstimatedTokens is the sum of the estimated input and output tokens of
// every chat request. It is an estimate, but it tracks the diagnosis figures
// closely enough to catch a regression in loop size.
func (p *FakeProvider) TotalEstimatedTokens() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, req := range p.reqs {
		total += req.TotalTokens
	}
	return total
}

func (p *FakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "fake-model", "object": "model", "created": time.Now().Unix(), "owned_by": "test"},
				{"id": "gpt-4o-mini", "object": "model", "created": time.Now().Unix(), "owned_by": "test"},
			},
		})
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		p.t.Logf("fake provider: unsupported request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	var wire struct {
		Model          string `json:"model"`
		Stream         bool   `json:"stream"`
		PromptCacheKey string `json:"prompt_cache_key"`
		Messages       []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		ExtraBody struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		} `json:"extra_body"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid JSON: " + err.Error()}})
		return
	}

	p.mu.Lock()
	index := len(p.reqs)
	p.reqs = append(p.reqs, ProviderRequest{})
	p.mu.Unlock()

	req := ProviderRequest{
		Index:           index,
		Path:            r.URL.Path,
		Model:           wire.Model,
		Stream:          wire.Stream,
		PromptCacheKey:  wire.PromptCacheKey,
		HasCacheControl: bytes.Contains(raw, []byte(`"cache_control"`)),
		BodyBytes:       len(raw),
	}
	if req.PromptCacheKey == "" {
		req.PromptCacheKey = wire.ExtraBody.PromptCacheKey
	}
	for _, message := range wire.Messages {
		text := contentText(message.Content)
		item := ProviderMessage{Role: message.Role, Content: text, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			item.ToolCalls = append(item.ToolCalls, ProviderToolCall{
				ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
			})
		}
		if message.Role == "system" {
			req.System += text
		}
		req.Messages = append(req.Messages, item)
	}
	for _, tool := range wire.Tools {
		req.Tools = append(req.Tools, tool.Function.Name)
	}
	req.EstimatedInputTokens = estimateTokens(len(raw))

	reply := p.respond(req)
	if reply.Status == 0 {
		reply.Status = http.StatusOK
	}
	if reply.Status >= http.StatusBadRequest {
		message := reply.Body
		if message == "" {
			message = "fake provider error"
		}
		writeFakeJSON(w, reply.Status, map[string]any{"error": map[string]string{"message": message}})
		p.storeRequest(index, p.withOutput(req, reply))
		return
	}

	usage := reply.Usage
	if usage.PromptTokens == 0 {
		usage.PromptTokens = req.EstimatedInputTokens
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = estimateReplyTokens(reply)
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	req.EstimatedOutputTokens = usage.CompletionTokens
	req.TotalTokens = usage.TotalTokens

	id := fmt.Sprintf("chatcmpl-fake-%d", index)
	if wire.Stream {
		p.writeStream(w, id, wire.Model, reply, usage)
	} else {
		p.writeCompletion(w, id, wire.Model, reply, usage)
	}
	p.storeRequest(index, req)
}

func (p *FakeProvider) withOutput(req ProviderRequest, reply ProviderReply) ProviderRequest {
	req.EstimatedOutputTokens = estimateReplyTokens(reply)
	req.TotalTokens = req.EstimatedInputTokens + req.EstimatedOutputTokens
	return req
}

func (p *FakeProvider) storeRequest(index int, req ProviderRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs[index] = req
}

func (p *FakeProvider) writeCompletion(w http.ResponseWriter, id, model string, reply ProviderReply, usage ProviderUsage) {
	message := map[string]any{"role": "assistant", "content": reply.Text}
	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		message["content"] = nil
		message["tool_calls"] = openAIToolCalls(reply.ToolCalls)
		finish = "tool_calls"
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": openAIUsage(usage),
	})
}

func (p *FakeProvider) writeStream(w http.ResponseWriter, id, model string, reply ProviderReply, usage ProviderUsage) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			p.t.Errorf("marshal fake stream event: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	created := time.Now().Unix()
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
	}

	if len(reply.ToolCalls) > 0 {
		calls := openAIToolCalls(reply.ToolCalls)
		for i := range calls {
			calls[i]["index"] = i
		}
		write(chunk(map[string]any{"role": "assistant", "tool_calls": calls}, nil))
		write(chunk(map[string]any{}, "tool_calls"))
	} else {
		write(chunk(map[string]any{"role": "assistant", "content": reply.Text}, nil))
		write(chunk(map[string]any{}, "stop"))
	}
	write(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   openAIUsage(usage),
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func openAIToolCalls(calls []ProviderToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		out = append(out, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

func openAIUsage(usage ProviderUsage) map[string]any {
	return map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Text != "" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func estimateReplyTokens(reply ProviderReply) int {
	size := len(reply.Text)
	for _, call := range reply.ToolCalls {
		size += len(call.Name) + len(call.Arguments)
	}
	return estimateTokens(size)
}

func estimateTokens(size int) int {
	if size <= 0 {
		return 0
	}
	return (size + 3) / 4
}

func writeFakeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
