package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
)

type fakeModelLister struct {
	models      []llmmodels.Model
	err         error
	gotProvider string
	gotKey      string
	calls       int
}

func (f *fakeModelLister) Models(_ context.Context, provider, apiKey string) ([]llmmodels.Model, error) {
	f.calls++
	f.gotProvider, f.gotKey = provider, apiKey
	return f.models, f.err
}

func TestLLMProfileModelsRequiresAdmin(t *testing.T) {
	f := newSecureFixture(t)
	dev := f.user("dev", false)
	rec := f.request(http.MethodPost, "/api/llm-profiles/models", map[string]string{
		"provider": "anthropic", "api_key": "sk-ant",
	}, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)
}

func TestLLMProfileModelsValidatesInput(t *testing.T) {
	f := newSecureFixture(t)
	lister := &fakeModelLister{}
	f.deps.Models = lister
	f.rebuildRouter()
	admin := f.session(f.user("admin", true))

	cases := []map[string]string{
		{"provider": "", "api_key": "sk"},
		{"provider": "anthropic", "api_key": ""},
		{"provider": "anthropic", "api_key": "   "},
		{"provider": "anthropic", "api_key": strings.Repeat("k", 8193)},
	}
	for _, body := range cases {
		rec := f.request(http.MethodPost, "/api/llm-profiles/models", body, admin)
		assertStatus(t, rec, http.StatusBadRequest)
	}

	if lister.calls != 0 {
		t.Fatalf("lister called %d times for invalid bodies", lister.calls)
	}
}

func TestLLMProfileModelsTrimsInput(t *testing.T) {
	f := newSecureFixture(t)
	lister := &fakeModelLister{models: []llmmodels.Model{{ID: "claude-x", Name: "Claude X"}}}
	f.deps.Models = lister
	f.rebuildRouter()
	admin := f.session(f.user("admin", true))

	rec := f.request(http.MethodPost, "/api/llm-profiles/models", map[string]string{
		"provider": " anthropic ", "api_key": " sk-ant\n",
	}, admin)
	assertStatus(t, rec, http.StatusOK)
	if lister.gotProvider != "anthropic" || lister.gotKey != "sk-ant" {
		t.Fatalf("lister got provider=%q key=%q", lister.gotProvider, lister.gotKey)
	}
	var body struct {
		Models []struct{ ID, Name string }
	}
	decodeJSON(t, rec, &body)
	if len(body.Models) != 1 || body.Models[0].ID != "claude-x" || body.Models[0].Name != "Claude X" {
		t.Fatalf("body = %#v", body)
	}
}

func TestLLMProfileModelsUnknownProvider(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.Models = &fakeModelLister{err: llmmodels.ErrUnknownProvider}
	f.rebuildRouter()
	rec := f.request(http.MethodPost, "/api/llm-profiles/models", map[string]string{
		"provider": "nope", "api_key": "sk",
	}, f.session(f.user("admin", true)))
	assertStatus(t, rec, http.StatusBadRequest)
}

func TestLLMProfileModelsUpstreamError(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.Models = &fakeModelLister{err: &llmmodels.UpstreamError{Status: http.StatusUnauthorized}}
	f.rebuildRouter()
	rec := f.request(http.MethodPost, "/api/llm-profiles/models", map[string]string{
		"provider": "anthropic", "api_key": "sk-secret",
	}, f.session(f.user("admin", true)))
	assertStatus(t, rec, http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("response leaks the key: %s", rec.Body.String())
	}
}

func TestLLMProfileModelsUnavailable(t *testing.T) {
	f := newSecureFixture(t)
	rec := f.request(http.MethodPost, "/api/llm-profiles/models", map[string]string{
		"provider": "anthropic", "api_key": "sk",
	}, f.session(f.user("admin", true)))
	assertStatus(t, rec, http.StatusServiceUnavailable)
}
