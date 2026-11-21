# Modelos desde la API de cada plataforma — Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cargar la lista de modelos desde la API de cada plataforma con la clave que el usuario pega, y hacer que OpenCode ejecute esos identificadores aunque no estén en models.dev.

**Architecture:** Un paquete Go nuevo (`internal/llmmodels`) traduce tres formas de API (OpenAI-compatible, Anthropic, Google) a una lista común; un endpoint admin lo expone; la UI consulta al salir del campo de clave y cae en la lista curada si falla. `writeOpenCodeConfig` declara el modelo elegido en `provider.<id>.models` para que el motor lo acepte.

**Tech Stack:** Go 1.27 (net/http, httptest), Vue 3 + TypeScript + Vitest, OpenCode 1.19.

**Spec:** `docs/superpowers/specs/2026-11-21-modelos-desde-api-plataformas-design.md`

## Global Constraints

- Sin dependencias nuevas; solo biblioteca estándar y lo ya presente.
- La URL del proveedor es fija y sale de una tabla cerrada; el cliente nunca envía `base_url` a este endpoint (sin SSRF).
- La clave vive en memoria durante la petición: no se registra, no se devuelve, no se guarda y no aparece en mensajes de error.
- Solo administradores (`auth.RequireAdmin`), como el resto de perfiles LLM.
- Timeout de 10 s, respuesta del proveedor limitada a 2 MiB, máximo 500 modelos.
- Guardar un perfil nunca depende de la consulta de modelos; la lista curada sigue siendo el respaldo.
- OpenCode 1.19 rechaza un ID ausente de su catálogo; el config generado debe declarar el modelo del perfil.
- Pruebas, lint y typecheck solo en local; no se toca CI.
- Commits en inglés, Conventional Commits; documentación en español como el resto de `docs/superpowers`.
- Rama: `feat/dynamic-llm-models`, desde `main` actualizado. Si el PR #47 sigue abierto, esta rama no lo incluye y no debe incluirlo.

## Review Focus

1. Clave pegada con espacios o salto de línea al final → se recorta antes de llamar al proveedor (Task 2, `TestLLMProfileModelsTrimsInput`).
2. El proveedor devuelve HTML o JSON inválido → 502, sin pánico (Task 1, `TestUpstreamNonJSON`).
3. IDs de modelo duplicados en la respuesta → una sola entrada, se conserva el primer nombre (Task 1, `TestOpenAICompatibleModels`).
4. Editar un perfil cuyo modelo ya no aparece en la plataforma → sigue en «Otro modelo…» con el guardado (Task 4, `keeps the stored model in the custom option…`).
5. Guardar mientras la consulta de modelos sigue en vuelo → el perfil se crea con la selección actual (Task 4, `saves while the model list is still loading`).

---

### Task 1: Paquete `internal/llmmodels`

**Files:**
- Create: `internal/llmmodels/llmmodels.go`
- Test: `internal/llmmodels/llmmodels_test.go`

**Interfaces:**
- Produces:
  - `type Model struct { ID string; Name string }` con tags `id` y `name`.
  - `type Shape int`; constantes `OpenAICompatible`, `Anthropic`, `Google`.
  - `type Provider struct { BaseURL string; Shape Shape }`.
  - `var DefaultProviders map[string]Provider`.
  - `var ErrUnknownProvider = errors.New("unknown provider")`.
  - `type UpstreamError struct { Status int; Err error }` con `Error() string` y `Unwrap() error`.
  - `type Client struct { HTTP *http.Client; Timeout time.Duration; Providers map[string]Provider }`.
  - `func New(httpClient *http.Client) *Client`.
  - `func (c *Client) Models(ctx context.Context, providerID, apiKey string) ([]Model, error)`.

- [ ] **Step 1: Write the failing tests**

`internal/llmmodels/llmmodels_test.go`, package `llmmodels_test`:

```go
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

	client := &Client{
		HTTP:    server.Client(),
		Timeout: time.Second,
		Providers: map[string]Provider{
			"openai": {BaseURL: server.URL, Shape: OpenAICompatible},
		},
	}
	models, err := client.Models(context.Background(), "openai", "sk-test")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotAuth != "Bearer sk-test" || gotPath != "/models" {
		t.Fatalf("request auth=%q path=%q", gotAuth, gotPath)
	}
	want := []Model{{ID: "gpt-a", Name: "GPT A"}, {ID: "gpt-b", Name: "gpt-b"}}
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

	client := &Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]Provider{
		"anthropic": {BaseURL: server.URL, Shape: Anthropic},
	}}
	models, err := client.Models(context.Background(), "anthropic", "sk-ant")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotKey != "sk-ant" || gotVersion != "2023-06-01" || gotLimit != "1000" {
		t.Fatalf("headers key=%q version=%q limit=%q", gotKey, gotVersion, gotLimit)
	}
	if !reflect.DeepEqual(models, []Model{{ID: "claude-x", Name: "Claude X"}}) {
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

	client := &Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]Provider{
		"google": {BaseURL: server.URL, Shape: Google},
	}}
	models, err := client.Models(context.Background(), "google", "sk-google")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotKey != "sk-google" || gotSize != "1000" {
		t.Fatalf("query key=%q pageSize=%q", gotKey, gotSize)
	}
	want := []Model{{ID: "gemini-4-carbon", Name: "Gemini 4 Carbon"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestUnknownProvider(t *testing.T) {
	client := &Client{HTTP: http.DefaultClient, Timeout: time.Second}
	if _, err := client.Models(context.Background(), "nope", "sk"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]Provider{
		"openai": {BaseURL: server.URL, Shape: OpenAICompatible},
	}}
	_, err := client.Models(context.Background(), "openai", "sk-secret")
	var upstream *UpstreamError
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
	client := &Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]Provider{
		"openai": {BaseURL: server.URL, Shape: OpenAICompatible},
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
	client := &Client{HTTP: server.Client(), Timeout: 50 * time.Millisecond, Providers: map[string]Provider{
		"openai": {BaseURL: server.URL, Shape: OpenAICompatible},
	}}
	_, err := client.Models(context.Background(), "openai", "sk")
	var upstream *UpstreamError
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
	client := &Client{HTTP: server.Client(), Timeout: time.Second, Providers: map[string]Provider{
		"openai": {BaseURL: server.URL, Shape: OpenAICompatible},
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
	for id := range DefaultProviders {
		got = append(got, id)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/llmmodels/`
Expected: FAIL — `undefined: Client`, `undefined: DefaultProviders`.

- [ ] **Step 3: Implement `internal/llmmodels/llmmodels.go`**

Constants: `defaultTimeout = 10 * time.Second`, `maxResponseBytes = 2 << 20`, `maxModels = 500`.

`DefaultProviders` exactly:

| ID | BaseURL | Shape |
|---|---|---|
| `openai` | `https://api.openai.com/v1` | OpenAICompatible |
| `anthropic` | `https://api.anthropic.com/v1` | Anthropic |
| `opencode` | `https://opencode.ai/inference/openai/v1` | OpenAICompatible |
| `google` | `https://generativelanguage.googleapis.com/v1beta` | Google |
| `openrouter` | `https://openrouter.ai/api/v1` | OpenAICompatible |
| `commandcode` | `https://api.commandcode.ai/provider/v1` | OpenAICompatible |
| `groq` | `https://api.groq.com/openai/v1` | OpenAICompatible |
| `mistral` | `https://api.mistral.ai/v1` | OpenAICompatible |
| `deepseek` | `https://api.deepseek.com` | OpenAICompatible |
| `moonshotai` | `https://api.moonshot.ai/v1` | OpenAICompatible |
| `zai` | `https://api.z.ai/api/paas/v4` | OpenAICompatible |
| `xai` | `https://api.x.ai/v1` | OpenAICompatible |
| `togetherai` | `https://api.together.xyz/v1` | OpenAICompatible |
| `fireworks-ai` | `https://api.fireworks.ai/inference/v1` | OpenAICompatible |

`Models` behaviour:

1. Look up the provider in `c.Providers`; if absent return `ErrUnknownProvider`. Nil map and nil `HTTP` fall back to `DefaultProviders` and `http.DefaultClient`.
2. `context.WithTimeout(ctx, c.Timeout)`, defaulting to `defaultTimeout` when zero.
3. Build the request:
   - `OpenAICompatible`: `GET {BaseURL}/models`, header `Authorization: Bearer <key>`.
   - `Anthropic`: `GET {BaseURL}/models?limit=1000`, headers `x-api-key: <key>` y `anthropic-version: 2023-06-01`.
   - `Google`: `GET {BaseURL}/models?key=<url.QueryEscape(key)>&pageSize=1000`.
4. Any transport error or non-2xx is `*UpstreamError`; set `Status` to the upstream code (0 for transport), keep the cause in `Err`, and never put the key, URL or body in the message.
5. Decode with `io.LimitReader(resp.Body, maxResponseBytes)`; decode failures are `*UpstreamError{Status: 0}`.
6. Parse per shape: OpenAI-compatible reads `data[].id` and `data[].name`; Anthropic reads `data[].id` and `data[].display_name`; Google reads `models[].name`, `models[].displayName`, keeps only entries whose `supportedGenerationMethods` contains `generateContent`, and strips the `models/` prefix.
7. For OpenAI-compatible and Anthropic, drop IDs whose lower-case value starts with any of: `text-embedding`, `embedding`, `whisper`, `tts`, `dall-e`, `gpt-image`, `omni-moderation`, `moderation`, `rerank`, `transcribe`, `speech`, `audio`, `realtime`, `sora`, `image`.
8. Trim IDs, drop empty IDs, dedupe by ID keeping the first non-empty name, fall back to the ID when the name is empty, sort case-insensitively by name then ID, cap at `maxModels`, and return a non-nil slice.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/llmmodels/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/llmmodels
git commit -m "feat(llm): load provider models from three API shapes" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: Endpoint admin `POST /api/llm-profiles/models`

**Files:**
- Create: `internal/api/llmmodels.go`
- Test: `internal/api/llmmodels_test.go`
- Modify: `internal/api/router.go:41-57` (Deps), `internal/api/router.go:139-143` (route)
- Modify: `cmd/qa/main.go:176-192` (wire the client)

**Interfaces:**
- Consumes: `llmmodels.Client`, `llmmodels.Model`, `llmmodels.ErrUnknownProvider`, `llmmodels.UpstreamError`.
- Produces:
  - `type LLMModelLister interface { Models(ctx context.Context, provider, apiKey string) ([]llmmodels.Model, error) }` en `internal/api/llmmodels.go`.
  - Campo `Models LLMModelLister` en `api.Deps`.
  - `func (s *server) llmModels(w http.ResponseWriter, r *http.Request)`.
  - JSON de respuesta: `{"models":[{"id":"…","name":"…"}]}`.

- [ ] **Step 1: Write the failing tests**

`internal/api/llmmodels_test.go`, package `api_test`:

```go
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
```

Imports: `context`, `net/http`, `strings`, `testing`, y `github.com/jeriveromartinez/qa-control-center/internal/llmmodels`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run TestLLMProfileModels`
Expected: FAIL — 404/405 or `undefined: llmmodels`.

- [ ] **Step 3: Implement**

`internal/api/llmmodels.go`:

- Interface `LLMModelLister` as declared above.
- Handler:
  1. `decodeRawBody`; unmarshal into `struct { Provider string; APIKey string }`; invalid JSON → 400 `"invalid JSON body"`.
  2. `provider := strings.TrimSpace(...)`, `key := strings.TrimSpace(...)`; empty provider or key → 400 `"provider and api_key are required"`; `len(key) > 8192` → 400 `"api_key is too long"`.
  3. `s.Models == nil` → 503 `"model catalog unavailable"`.
  4. `models, err := s.Models.Models(r.Context(), provider, key)`.
  5. `errors.Is(err, llmmodels.ErrUnknownProvider)` → 400 `"unknown provider"`.
  6. `errors.As(err, &upstream)`: status > 0 → 502 `fmt.Sprintf("provider returned HTTP %d", upstream.Status)`; status 0 → 502 `"provider did not answer"`. Any other error → 502 `"provider did not answer"`.
  7. `writeJSON(w, http.StatusOK, map[string]any{"models": models})`; if `models == nil` use `[]llmmodels.Model{}`.

`router.go`: add `Models LLMModelLister` to `Deps` and `r.Post("/llm-profiles/models", s.llmModels)` next to `r.Post("/llm-profiles", ...)`.

`cmd/qa/main.go`: add `Models: llmmodels.New(&http.Client{Timeout: 15 * time.Second}),` to `api.Deps`, importing `internal/llmmodels`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run TestLLMProfileModels`
Expected: PASS.

Then: `go build ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/api/llmmodels.go internal/api/llmmodels_test.go internal/api/router.go cmd/qa/main.go
git commit -m "feat(api): list platform models with the caller's api key" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: OpenCode config declares the selected model

**Files:**
- Modify: `internal/review/agent.go:169-203`
- Test: `internal/review/agent_test.go`

**Interfaces:**
- Consumes: `store.LLMProfile.Provider`, `store.LLMProfile.Model`.
- Produces: generated `opencode.jsonc` with `provider.<provider>.models.<model>.name`.

- [ ] **Step 1: Write the failing test**

Append to `internal/review/agent_test.go`:

```go
func TestAgentConfigDeclaresSelectedModel(t *testing.T) {
	home := t.TempDir()
	a := testAgent(t, home)
	if _, _, err := a.Run(context.Background(), "__FAKE_ARGS__", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var cfg struct {
		Provider map[string]struct {
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(readAgentConfigRaw(t, home), &cfg); err != nil {
		t.Fatalf("decode generated config: %v", err)
	}
	model, ok := cfg.Provider["nvidia"].Models["nemotron"]
	if !ok || model.Name != "nemotron" {
		t.Fatalf("provider.nvidia.models = %#v, want nemotron", cfg.Provider["nvidia"].Models)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/review/ -run TestAgentConfigDeclaresSelectedModel`
Expected: FAIL — `provider.nvidia.models` is nil.

- [ ] **Step 3: Implement**

In `writeOpenCodeConfig`, extend the provider object:

```go
provider := map[string]any{
	"options": options,
	// OpenCode 1.19 only runs model ids present in its catalog; declaring the
	// selected one keeps provider-api models usable before models.dev catches up.
	"models": map[string]any{
		profile.Model: map[string]string{"name": profile.Model},
	},
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/review/`
Expected: PASS, including the existing config tests.

- [ ] **Step 5: Commit**

```bash
git add internal/review/agent.go internal/review/agent_test.go
git commit -m "fix(review): declare the selected model in the opencode config" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: UI asíncrona con respaldo curado

**Files:**
- Modify: `web/src/api.ts:194-226` (types) y `:329-335` (client)
- Create: `web/src/api.test.ts`
- Modify: `web/src/views/AgentsView.vue`
- Modify: `web/src/views/AgentsView.test.ts`

**Interfaces:**
- Consumes: `POST /api/llm-profiles/models` de Task 2.
- Produces:
  - `export type LLMModelOption = { id: string; name: string }`.
  - `export type LLMModelsResponse = { models: LLMModelOption[] }`.
  - `api.llmModels(provider: string, api_key: string): Promise<LLMModelsResponse>`.
  - Selectores nuevos: `[data-profile-models-loading]`, `[data-profile-models-notice]`.

- [ ] **Step 1: Write the failing tests**

Add `llmModels` to the hoisted `mocks.api` object and default it in `beforeEach`:

```ts
llmModels: vi.fn(),
// beforeEach:
mocks.api.llmModels.mockResolvedValue({
  models: [
    { id: 'claude-sonnet-5-5', name: 'Claude Sonnet 5.5' },
    { id: 'claude-new-7', name: 'Claude New 7' },
  ],
})
```

Add to `AgentsView.test.ts`:

```ts
it('loads platform models on key blur with curated ids first', async () => {
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  expect(mocks.api.llmModels).toHaveBeenCalledWith('anthropic', 'sk-ant')
  const values = wrapper
    .findAll('[data-profile-model] option')
    .map((option) => (option.element as HTMLOptionElement).value)
  expect(values).toEqual(['claude-sonnet-5-5', 'claude-new-7', '__custom__'])
  wrapper.unmount()
})

it('keeps curated models and shows a notice when the platform call fails', async () => {
  mocks.api.llmModels.mockRejectedValue(new Error('upstream'))
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  expect(wrapper.get('[data-profile-models-notice]').text()).toContain('se muestran los recomendados')
  const values = wrapper
    .findAll('[data-profile-model] option')
    .map((option) => (option.element as HTMLOptionElement).value)
  expect(values).toContain('claude-opus-5-5')
  wrapper.unmount()
})

it('treats an empty model list as a failure', async () => {
  mocks.api.llmModels.mockResolvedValue({ models: [] })
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  expect(wrapper.find('[data-profile-models-notice]').exists()).toBe(true)
  expect(wrapper.findAll('[data-profile-model] option')).toHaveLength(4) // 3 curados + «Otro modelo…»
  wrapper.unmount()
})

it('ignores a late response for a previous provider and key', async () => {
  let resolveFirst: (value: { models: { id: string; name: string }[] }) => void = () => {}
  mocks.api.llmModels
    .mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve }))
    .mockResolvedValueOnce({ models: [{ id: 'fresh-model', name: 'Fresh' }] })

  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')

  await wrapper.get('[data-platform="openai"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-openai')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  resolveFirst({ models: [{ id: 'claude-old', name: 'Claude Old' }] })
  await flushPromises()

  const values = wrapper
    .findAll('[data-profile-model] option')
    .map((option) => (option.element as HTMLOptionElement).value)
  expect(values).toEqual(['fresh-model', '__custom__'])
  wrapper.unmount()
})

it('saves while the model list is still loading', async () => {
  mocks.api.llmModels.mockImplementation(() => new Promise(() => {}))
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
    expect.objectContaining({ provider: 'anthropic', api_key: 'sk-ant' }),
  )
  wrapper.unmount()
})

it('does not query models when editing with an empty key', async () => {
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  expect(mocks.api.llmModels).not.toHaveBeenCalled()
  wrapper.unmount()
})

it('keeps the stored model in the custom option when the platform no longer lists it', async () => {
  mocks.api.llmModels.mockResolvedValue({ models: [{ id: 'other-model', name: 'Other' }] })
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-new')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe('__custom__')
  expect((wrapper.get('[data-profile-model-custom]').element as HTMLInputElement).value).toBe(
    'claude-sonnet-5-5',
  )
  wrapper.unmount()
})

it('resets to the curated list when the platform changes', async () => {
  const wrapper = mountAgents()
  await flushPromises()
  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
  await wrapper.get('[data-profile-api-key]').trigger('blur')
  await flushPromises()

  await wrapper.get('[data-platform="openai"]').trigger('click')
  const values = wrapper
    .findAll('[data-profile-model] option')
    .map((option) => (option.element as HTMLOptionElement).value)
  expect(values).toEqual(['gpt-6.2-sol', 'gpt-6.2-astra', 'gpt-5.4-nano', '__custom__'])
  wrapper.unmount()
})
```

`web/src/api.test.ts`:

```ts
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('api.llmModels', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('posts the provider and key to the models endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ models: [{ id: 'm', name: 'M' }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.llmModels('anthropic', 'sk-ant')).resolves.toEqual({
      models: [{ id: 'm', name: 'M' }],
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/llm-profiles/models',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ provider: 'anthropic', api_key: 'sk-ant' }),
      }),
    )
  })
})
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npm test -- --run src/api.test.ts src/views/AgentsView.test.ts`
Expected: FAIL — `api.llmModels` no existe; no hay opciones dinámicas ni aviso.

- [ ] **Step 3: Implement**

`api.ts`: add the two types after `LLMProfilePatch` and the client method after `checkLLMHealth`:

```ts
export type LLMModelOption = { id: string; name: string }
export type LLMModelsResponse = { models: LLMModelOption[] }

llmModels: (provider: string, api_key: string) =>
  request<LLMModelsResponse>('/api/llm-profiles/models', {
    method: 'POST',
    body: { provider, api_key },
  }),
```

`AgentsView.vue` script:

- Import `LLMPlatformModel` from `../llmPlatforms`.
- State: `remoteModels = ref<LLMPlatformModel[]>([])`, `modelsLoading = ref(false)`, `modelsNotice = ref<string | null>(null)`, module-level `let modelsTag = ''`.
- `platformModelOptions` computed:
  - no platform → `[]`;
  - `remoteModels` empty → `platform.models`;
  - otherwise curated models whose ID is in `remoteModels`, in catalog order, followed by remote models not in the catalog.
- `resetRemoteModels()`: clears all three refs and `modelsTag`.
- `onAPIKeyInput()`: `resetRemoteModels()`.
- `loadPlatformModels()`:
  - return unless `tab === 'platform'`, a platform is selected and `form.api_key.trim() !== ''`;
  - `tag = platform.id + '\n' + key`, set `modelsTag`, `modelsLoading = true`, `modelsNotice = null`;
  - await `api.llmModels(platform.id, key)`; if `modelsTag !== tag` ignore the reply;
  - map `{id, name}` to `{id, label: name || id}`, trim, drop empty; empty result throws;
  - assign `remoteModels`;
  - if the current model is in the list keep `modelSelect`; else if creating select the first model; else set `modelSelect = CUSTOM_MODEL` and `modelOther = form.model`;
  - on error (or empty), if the tag is still current: `remoteModels = []`, notice `«No se pudieron cargar los modelos de la plataforma; se muestran los recomendados»`;
  - `finally` clears `modelsLoading` only when the tag is current.
- `selectPlatform`, `openCreate`, `openEdit`, `closeModal`: call `resetRemoteModels()` before setting their own state.
- `defaultProfileName()`: look the label up in `platformModelOptions.value`, not only in the curated list.

Template in the platform tab:

```html
<input
  v-model="form.api_key"
  ...
  data-profile-api-key
  @input="onAPIKeyInput"
  @blur="loadPlatformModels"
/>
...
<p v-if="modelsLoading" class="field__hint" role="status" data-profile-models-loading>
  Cargando modelos…
</p>
<p v-else-if="modelsNotice" class="field__hint" role="status" data-profile-models-notice>
  {{ modelsNotice }}
</p>
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npm test -- --run src/api.test.ts src/views/AgentsView.test.ts`
Expected: PASS (existing tests unchanged).

- [ ] **Step 5: Commit**

```bash
git add web/src/api.ts web/src/api.test.ts web/src/views/AgentsView.vue web/src/views/AgentsView.test.ts
git commit -m "feat(web): load llm models from the selected platform" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: Verificación completa, documentación y PR

**Files:**
- Modify: `docs/superpowers/specs/2026-11-21-modelos-desde-api-plataformas-design.md` (estado → `aprobado`)
- Add: `docs/superpowers/plans/2026-11-21-modelos-desde-api-plataformas.md` (este plan)

- [ ] **Step 1: Run the full Go suite**

Run: `go test ./...`
Expected: all packages `ok`.

- [ ] **Step 2: Run the full web suite and the typecheck**

Run: `cd web && npm test -- --run`
Expected: all test files PASS.

Run: `cd web && npm run build`
Expected: `vue-tsc` without errors and Vite build completes.

- [ ] **Step 3: Run the design lint**

Run: `node "C:/Users/jeriv/.claude/skills/hallmark/scripts/sloplint.mjs" web/src/views/AgentsView.vue --scope app`
Expected: `0 FAIL`. Fix every FAIL; fix or waive with a stated reason every REFLEX/WARN that comes from this change. Re-run after fixes.

- [ ] **Step 4: Commit the docs**

Si la ejecución va en un worktree, copia antes los dos documentos desde el checkout donde se escribieron; están sin seguimiento.

```bash
git add docs/superpowers/specs/2026-11-21-modelos-desde-api-plataformas-design.md docs/superpowers/plans/2026-11-21-modelos-desde-api-plataformas.md
git commit -m "docs: add provider model loading design and plan" -m "Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

Expected: working tree clean; `git log --oneline main..HEAD` shows five commits.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin feat/dynamic-llm-models
gh pr create --base main --title "feat: load LLM models from each platform API" --body "<what changed, why, how it was tested with real output, follow-ups deliberately left out>"
```

The PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. If the active GitHub account cannot push to the repository, switch to the repo owner account, push, and restore the previous account.

Expected: PR URL reported. Do not merge without review.

## Self-Review

- **Cobertura del spec:** tabla de proveedores y adaptadores (Task 1); contrato HTTP, validaciones, 502 y timeout (Task 1/2); compatibilidad de OpenCode y `provider.models` (Task 3); consulta al `blur`, orden curado-primero, aviso, respuesta vacía, descarte de respuestas tardías, guardado no bloqueante y reinicio al cambiar de plataforma (Task 4); documentación y PR (Task 5).
- **Firmas y tipos:** `llmmodels.Model{ID,Name}`, `UpstreamError{Status,Err}`, `LLMModelLister.Models`, `api.llmModels(provider, api_key)`, `[data-profile-models-loading]` y `[data-profile-models-notice]` coinciden entre tareas.
- **Review Focus:** las cinco líneas tienen prueba en la tarea que posee el código.
- **Límites:** sin dependencias nuevas; la tabla Go cubre catorce proveedores y se verifica sola; el endpoint no acepta URL del cliente.
