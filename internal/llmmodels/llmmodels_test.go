package llmmodels_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
)

func TestOpenAICompatibleModels(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		io.WriteString(w, `{"data":[
			{"id":"gpt-b"},
			{"id":"text-embedding-3-small"},
			{"id":"gpt-a","name":"GPT A"},
			{"id":"gpt-a","name":"Duplicate"}
		]}`)
	}))
	defer server.Close()

	client := &llmmodels.Client{
		HTTP:    server.Client(),
		Timeout: time.Second,
		Providers: map[string]llmmodels.Provider{
			"openai": {BaseURL: server.URL, Shape: llmmodels.OpenAICompatible},
		},
	}
	models, err := client.Models(context.Background(), "openai", "sk-test")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotAuth != "Bearer sk-test" || gotPath != "/models" {
		t.Fatalf("request auth=%q path=%q", gotAuth, gotPath)
	}
	want := []llmmodels.Model{{ID: "gpt-a", Name: "GPT A"}, {ID: "gpt-b", Name: "gpt-b"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestAnthropicModels(t *testing.T) {
	var gotKey, gotVersion, gotLimit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		gotLimit = r.URL.Query().Get("limit")
		io.WriteString(w, `{"data":[{"id":"claude-x","display_name":"Claude X"}]}`)
	}))
	defer server.Close()

	client := &llmmodels.Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]llmmodels.Provider{
		"anthropic": {BaseURL: server.URL, Shape: llmmodels.Anthropic},
	}}
	models, err := client.Models(context.Background(), "anthropic", "sk-ant")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotKey != "sk-ant" || gotVersion != "2023-06-01" || gotLimit != "1000" {
		t.Fatalf("headers key=%q version=%q limit=%q", gotKey, gotVersion, gotLimit)
	}
	if !reflect.DeepEqual(models, []llmmodels.Model{{ID: "claude-x", Name: "Claude X"}}) {
		t.Fatalf("models = %#v", models)
	}
}

func TestGoogleModels(t *testing.T) {
	var gotKey, gotSize string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.URL.Query().Get("key")
		gotSize = r.URL.Query().Get("pageSize")
		io.WriteString(w, `{"models":[
			{"name":"models/gemini-4-carbon","displayName":"Gemini 4 Carbon","supportedGenerationMethods":["generateContent"]},
			{"name":"models/embedding-1","displayName":"Embedding","supportedGenerationMethods":["embedContent"]}
		]}`)
	}))
	defer server.Close()

	client := &llmmodels.Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]llmmodels.Provider{
		"google": {BaseURL: server.URL, Shape: llmmodels.Google},
	}}
	models, err := client.Models(context.Background(), "google", "sk-google")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotKey != "sk-google" || gotSize != "1000" {
		t.Fatalf("query key=%q pageSize=%q", gotKey, gotSize)
	}
	want := []llmmodels.Model{{ID: "gemini-4-carbon", Name: "Gemini 4 Carbon"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestUnknownProvider(t *testing.T) {
	client := &llmmodels.Client{HTTP: http.DefaultClient, Timeout: time.Second}
	if _, err := client.Models(context.Background(), "nope", "sk"); !errors.Is(err, llmmodels.ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := &llmmodels.Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]llmmodels.Provider{
		"openai": {BaseURL: server.URL, Shape: llmmodels.OpenAICompatible},
	}}
	_, err := client.Models(context.Background(), "openai", "sk-secret")
	var upstream *llmmodels.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want UpstreamError 401", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error leaks the key: %v", err)
	}
}

func TestUpstreamNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>gateway error</html>")
	}))
	defer server.Close()
	client := &llmmodels.Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]llmmodels.Provider{
		"openai": {BaseURL: server.URL, Shape: llmmodels.OpenAICompatible},
	}}
	if _, err := client.Models(context.Background(), "openai", "sk"); err == nil {
		t.Fatal("Models accepted a non-JSON body")
	}
}

func TestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	client := &llmmodels.Client{HTTP: server.Client(), Timeout: 50 * time.Millisecond, Providers: map[string]llmmodels.Provider{
		"openai": {BaseURL: server.URL, Shape: llmmodels.OpenAICompatible},
	}}
	_, err := client.Models(context.Background(), "openai", "sk")
	var upstream *llmmodels.UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("err = %v, want UpstreamError", err)
	}
}

func TestModelsCap(t *testing.T) {
	var items []string
	for i := 0; i < 501; i++ {
		items = append(items, fmt.Sprintf(`{"id":"model-%03d"}`, i))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[`+strings.Join(items, ",")+`]}`)
	}))
	defer server.Close()
	client := &llmmodels.Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]llmmodels.Provider{
		"openai": {BaseURL: server.URL, Shape: llmmodels.OpenAICompatible},
	}}
	models, err := client.Models(context.Background(), "openai", "sk")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 500 {
		t.Fatalf("len(models) = %d, want 500", len(models))
	}
}

func TestDefaultProviderTable(t *testing.T) {
	want := []string{
		"anthropic", "commandcode", "deepseek", "fireworks-ai", "google", "groq",
		"mistral", "moonshotai", "openai", "opencode", "openrouter", "togetherai", "xai", "zai",
	}
	var got []string
	for id := range llmmodels.DefaultProviders {
		got = append(got, id)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}
