# Catálogo de plataformas LLM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Crear perfiles LLM contra diez plataformas conocidas pidiendo solo la clave API, manteniendo una pestaña de conexión personalizada.

**Architecture:** Un catálogo estático tipado en el frontend resuelve proveedor y modelos; el modal de `AgentsView` gana dos pestañas (plataforma conocida y conexión personalizada), con modelo preseleccionado y opciones avanzadas plegadas. El backend no cambia: se siguen enviando `provider`, `model` y `api_key`.

**Tech Stack:** Vue 3 `<script setup>`, TypeScript, Vitest + Vue Test Utils, tokens CSS del sistema Blueprint.

**Spec:** `docs/superpowers/specs/2026-11-21-catalogo-plataformas-llm-design.md`

## Global Constraints

- Rama: `feat/llm-platform-catalog`, creada desde `main` actualizado. Nunca commit directo a `main`.
- Sin dependencias nuevas y sin cambios en Go ni en `web/src/api.ts`.
- Copia de UI en español; identificadores, comentarios y mensajes de commit en inglés.
- Solo tokens CSS existentes (`--space-*`, `--border*`, `--text*`, `--surface*`, `--accent*`, `--rule-*`, `--dur-*`). Sin colores literales, sin `transition: all`, sin animar `width`, `height`, `top` ni `left`.
- Todo control interactivo mantiene 40 px mínimos, `:hover`, `:active` y `:focus-visible`.
- Pruebas locales: `cd web && npm test -- --run` (Vitest en modo CI). Build: `cd web && npm run build`.
- El campo `mode` deja de enviarse desde la UI; no se elimina del tipo `LLMProfile`.
- Commits Conventional Commits en inglés con trailer `Co-Authored-By: Claude Code <noreply@anthropic.com>`.

## Review Focus

- Alta sin plataforma elegida y sin clave: error inline y ninguna llamada a la API.
- Cambiar de pestaña conserva la clave escrita y no arrastra `base_url` a la pestaña de plataforma.
- Cambiar de plataforma reinicia el modelo al predeterminado de la nueva plataforma.
- Editar un perfil de plataforma cuyo modelo no está en la lista: el select arranca en «Otro modelo…» con el identificador actual.
- Editar un perfil con URL base o proveedor desconocido: abre conexión personalizada, conserva los valores y no envía `api_key` vacía.

---

### Task 1: Catálogo tipado de plataformas

**Files:**
- Create: `web/src/llmPlatforms.ts`
- Test: `web/src/llmPlatforms.test.ts`

**Interfaces:**
- Produces: `type LLMPlatformModel = { id: string; label: string }`, `type LLMPlatform = { id: string; name: string; models: LLMPlatformModel[] }`, `const llmPlatforms: LLMPlatform[]`. El primer elemento de `models` es el predeterminado.

- [ ] **Step 1: Write the failing test**

`web/src/llmPlatforms.test.ts`:

```ts
import { describe, expect, it } from 'vitest'
import { llmPlatforms } from './llmPlatforms'

describe('llmPlatforms', () => {
  it('lists ten platforms with unique provider ids', () => {
    expect(llmPlatforms).toHaveLength(10)
    const ids = llmPlatforms.map((platform) => platform.id)
    expect(new Set(ids).size).toBe(ids.length)
    for (const platform of llmPlatforms) {
      expect(platform.id.trim()).not.toBe('')
      expect(platform.name.trim()).not.toBe('')
    }
  })

  it('gives every platform labelled models with unique ids', () => {
    for (const platform of llmPlatforms) {
      expect(platform.models.length).toBeGreaterThan(0)
      const ids = platform.models.map((model) => model.id)
      expect(new Set(ids).size).toBe(ids.length)
      for (const model of platform.models) {
        expect(model.id.trim()).not.toBe('')
        expect(model.label.trim()).not.toBe('')
      }
    }
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run src/llmPlatforms.test.ts`
Expected: FAIL — cannot resolve `./llmPlatforms`.

- [ ] **Step 3: Implement the catalog**

`web/src/llmPlatforms.ts` with exact data; `id` is the OpenCode provider id:

| name | id | models (id → label), first is default |
|---|---|---|
| OpenAI | `openai` | `gpt-6.2-sol` → GPT-6.2 Sol · `gpt-6.2-astra` → GPT-6.2 Astra · `gpt-5.4-nano` → GPT-5.4 nano |
| Anthropic | `anthropic` | `claude-sonnet-5-5` → Claude Sonnet 5.5 · `claude-opus-5-5` → Claude Opus 5.5 · `claude-haiku-5-5` → Claude Haiku 5.5 |
| Google Gemini | `google` | `gemini-4-carbon` → Gemini 4 Carbon · `gemini-4-neon` → Gemini 4 Neon · `gemini-3.8-flash` → Gemini 3.8 Flash |
| OpenRouter | `openrouter` | `openrouter/auto` → Automático (OpenRouter elige) · `anthropic/claude-sonnet-5.5` → Claude Sonnet 5.5 · `google/gemini-3.7-flash` → Gemini 3.7 Flash |
| Groq | `groq` | `openai/gpt-oss-120b` → GPT-OSS 120B · `qwen/qwen3.8-27b` → Qwen3.8 27B · `llama-3.3-70b-versatile` → Llama 3.3 70B |
| Mistral | `mistral` | `mistral-large-latest` → Mistral Large · `devstral-medium-latest` → Devstral 2 · `mistral-medium-2604` → Mistral Medium 3.5 |
| DeepSeek | `deepseek` | `deepseek-v4-pro` → DeepSeek V4 Pro · `deepseek-v4-flash` → DeepSeek V4 Flash |
| xAI | `xai` | `grok-4.7` → Grok 4.7 · `grok-4.6` → Grok 4.6 · `grok-4.5` → Grok 4.5 |
| Together AI | `togetherai` | `moonshotai/Kimi-K3.1-Pro` → Kimi K3.1 Pro · `deepseek-ai/DeepSeek-V4-Pro-0813` → DeepSeek V4 Pro · `MiniMaxAI/MiniMax-M3` → MiniMax M3 |
| Fireworks | `fireworks-ai` | `accounts/fireworks/routers/deepseek-flash-latest` → DeepSeek Flash · `accounts/fireworks/routers/glm-latest` → GLM · `accounts/fireworks/routers/kimi-k3-fast` → Kimi K3 Fast |

Export the three symbols listed in Interfaces; no default export.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test -- --run src/llmPlatforms.test.ts`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/llmPlatforms.ts web/src/llmPlatforms.test.ts
git commit -m "feat(web): add curated LLM platform catalog"
```

---

### Task 2: Modal con pestañas, alta por plataforma y conexión personalizada

**Files:**
- Modify: `web/src/views/AgentsView.vue` (script 1–244, template 410–529, styles 736+)
- Modify: `web/src/views/AgentsView.test.ts`

**Interfaces:**
- Consumes: `llmPlatforms`, `LLMPlatform` from `../llmPlatforms`.
- Produces: `type ProfileTab = 'platform' | 'custom'`; `const CUSTOM_MODEL = '__custom__'`; component state `tab`, `selectedPlatform`, `modelSelect`, `modelOther`; functions `selectPlatform(platform: LLMPlatform): void`, `defaultProfileName(): string`; selector attributes used by Task 3: `[data-profile-tab-platform]`, `[data-profile-tab-custom]`, `[data-platform="<provider>"]`, `[data-profile-model]`, `[data-profile-model-custom]`.

**Behavior:**
- Al crear: pestaña `platform`, sin tarjeta seleccionada.
- Tarjeta seleccionada fija `form.provider` y `form.model` al primer modelo de la plataforma; cambiar de plataforma reinicia el modelo.
- La pestaña `custom` muestra los campos actuales (nombre, proveedor, modelo, clave, URL base, concurrencia, activado) **sin** campo modo.
- `submitProfile` construye `{ name, provider, model, concurrency, enabled, api_key }`; añade `base_url` solo en `custom`. No envía `mode` en ningún caso.
- Si `name` está vacío, envía `<platform.name> <model label>` (o `<provider> <model>` en personalizada).
- Al crear, exige clave API y plataforma/proveedor/modelo; el error se muestra en `[role="alert"]` sin llamar a la API.
- La clave sigue sin renderizarse fuera del input `type="password"`.

- [ ] **Step 1: Write the failing tests**

Replace the existing `creates a profile, never renders the api key and shows api_key_set` test and add the new ones in `AgentsView.test.ts`:

```ts
it('creates a profile from a platform with only the api key', async () => {
  const secret = 'sk-ant-super-secret-456'
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue(secret)
  expect(wrapper.html()).not.toContain(secret)

  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).toHaveBeenCalledWith({
    name: 'Anthropic Claude Sonnet 5.5',
    provider: 'anthropic',
    model: 'claude-sonnet-5-5',
    concurrency: 1,
    enabled: true,
    api_key: secret,
  })
  expect(wrapper.html()).not.toContain(secret)
  expect(wrapper.find('[data-profile-api-key]').exists()).toBe(false)

  wrapper.unmount()
})

it('refuses to create a platform profile without an api key', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="openai"]').trigger('click')
  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).not.toHaveBeenCalled()
  expect(wrapper.get('[role="alert"]').text()).toContain('clave API')

  wrapper.unmount()
})

it('resets the model when the platform changes', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="openai"]').trigger('click')
  expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
    'gpt-6.2-sol',
  )

  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
    'claude-sonnet-5-5',
  )

  wrapper.unmount()
})

it('keeps the typed api key and platform when switching tabs', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="anthropic"]').trigger('click')
  await wrapper.get('[data-profile-api-key]').setValue('sk-keep-me')

  await wrapper.get('[data-profile-tab-custom]').trigger('click')
  expect((wrapper.get('[data-profile-api-key]').element as HTMLInputElement).value).toBe(
    'sk-keep-me',
  )

  await wrapper.get('[data-profile-tab-platform]').trigger('click')
  expect(wrapper.get('[data-platform="anthropic"]').attributes('aria-pressed')).toBe('true')
  expect((wrapper.get('[data-profile-api-key]').element as HTMLInputElement).value).toBe(
    'sk-keep-me',
  )

  wrapper.unmount()
})

it('creates a profile through the custom connection tab', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-profile-tab-custom]').trigger('click')
  await wrapper.get('[data-profile-name]').setValue('Local llama')
  await wrapper.get('[data-profile-provider]').setValue('ollama')
  await wrapper.get('[data-profile-model]').setValue('llama3')
  await wrapper.get('[data-profile-api-key]').setValue('local-key')
  await wrapper.get('[data-profile-base-url]').setValue('http://localhost:11434')
  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).toHaveBeenCalledWith({
    name: 'Local llama',
    provider: 'ollama',
    model: 'llama3',
    concurrency: 1,
    enabled: true,
    api_key: 'local-key',
    base_url: 'http://localhost:11434',
  })

  wrapper.unmount()
})
```

Keep the profile-list assertions (`Clave configurada` / `Sin clave`) in a separate test if they were only in the replaced test.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npm test -- --run src/views/AgentsView.test.ts`
Expected: FAIL — no `[data-platform="anthropic"]`, no `[data-profile-tab-custom]`.

- [ ] **Step 3: Implement**

In `AgentsView.vue`:
- Import the catalog and remove `mode` from `ProfileForm`, `emptyForm`, `openEdit` and the submission payload.
- Add the tab and platform state and the two helper functions from Interfaces.
- Template: tab strip before `modal__fields`; in `platform` tab render the 10-card grid (`button` with `aria-pressed`), the API key field, and the model select; in `custom` tab render the existing fields without modo. Move name, concurrencia and activado into `<details class="modal__advanced" data-profile-advanced>` with `<summary>Opciones avanzadas</summary>` in both tabs; key and model stay outside.
- Add the catalog-driven model select options and a `CUSTOM_MODEL` option labelled `Otro modelo…` that reveals `[data-profile-model-custom]` (bound to `modelOther`); Task 3 wires the edit case.
- Keep all existing selectors used by other tests: `[data-policy-*]`, `[data-health-*]`, `[data-edit-profile]`.
- Styles: `.profile-tabs`, `.profile-tabs__tab` (40 px, selected with heavy bottom keyline), `.platform-grid` (2 columns, 1 below 560 px), `.platform-card` (hairline border, `--surface-2`, selected with `--border-strong` + `--accent-soft`), `.modal__advanced` summary (40 px, `:focus-visible`), and `:hover`/`:active`/`:focus-visible` for cards and tabs.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npm test -- --run src/views/AgentsView.test.ts`
Expected: PASS, including the untouched policy and health tests.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/AgentsView.vue web/src/views/AgentsView.test.ts
git commit -m "feat(web): create LLM profiles from a platform catalog"
```

---

### Task 3: Modelo libre y edición de perfiles

**Files:**
- Modify: `web/src/views/AgentsView.vue`
- Modify: `web/src/views/AgentsView.test.ts`

**Interfaces:**
- Consumes: everything produced by Task 2.
- Produces: `function platformFor(provider: string): LLMPlatform | undefined` and `function syncPlatformFromForm(): void`, both used only inside the view; edit behavior described below.

**Behavior:**
- `openEdit(profile)`: abre `platform` si `platformFor(profile.provider)` existe y `profile.base_url` está vacío; si no, `custom`. Precarga nombre, concurrencia, activado, clave vacía, `provider` y `model`.
- En `platform`, si el modelo guardado está en el catálogo se selecciona; si no, el select queda en `CUSTOM_MODEL` y `[data-profile-model-custom]` muestra el identificador.
- Cambiar el select a un modelo del catálogo actualiza `form.model`; elegir `CUSTOM_MODEL` no borra el último modelo, solo revela el campo de texto.
- En `custom`, `[data-profile-model]` es un input de texto con el modelo guardado.
- Al guardar una edición, el PATCH omite `api_key` si el campo está vacío y omite `base_url` en la pestaña `platform`.

- [ ] **Step 1: Write the failing tests**

Add fixtures to `AgentsView.test.ts`: profile `13` (`anthropic` / `claude-sonnet-5-5`, sin `base_url`) y profile `14` (`azure` / `gpt-4o`, sin `base_url`). Then:

```ts
it('sends the model chosen from the catalog select', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="openai"]').trigger('click')
  await wrapper.get('[data-profile-model]').setValue('gpt-6.2-astra')
  await wrapper.get('[data-profile-api-key]').setValue('sk-test')
  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
    expect.objectContaining({ provider: 'openai', model: 'gpt-6.2-astra' }),
  )

  wrapper.unmount()
})

it('sends a model typed through the other-model option', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-open-profile]').trigger('click')
  await wrapper.get('[data-platform="openai"]').trigger('click')
  await wrapper.get('[data-profile-model]').setValue('__custom__')
  await wrapper.get('[data-profile-model-custom]').setValue('gpt-preview-x')
  await wrapper.get('[data-profile-api-key]').setValue('sk-test')
  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
    expect.objectContaining({ provider: 'openai', model: 'gpt-preview-x' }),
  )

  wrapper.unmount()
})

it('edits a catalog profile keeping its model and omitting the api key', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
  expect(wrapper.get('[data-platform="anthropic"]').attributes('aria-pressed')).toBe('true')
  expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
    'claude-sonnet-5-5',
  )

  await wrapper.get('[data-profile-submit]').trigger('click')
  await flushPromises()

  expect(mocks.api.updateLLMProfile).toHaveBeenCalledWith(
    13,
    expect.objectContaining({ provider: 'anthropic', model: 'claude-sonnet-5-5' }),
  )
  expect(mocks.api.updateLLMProfile.mock.calls[0][1]).not.toHaveProperty('api_key')
  expect(mocks.api.updateLLMProfile.mock.calls[0][1]).not.toHaveProperty('base_url')

  wrapper.unmount()
})

it('edits a catalog profile with an unknown model through the other-model field', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-profile-row="7"] [data-edit-profile]').trigger('click')
  expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
    '__custom__',
  )
  expect((wrapper.get('[data-profile-model-custom]').element as HTMLInputElement).value).toBe(
    'gpt-5',
  )

  wrapper.unmount()
})

it('opens a profile with a base URL in the custom tab', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
  expect((wrapper.get('[data-profile-provider]').element as HTMLInputElement).value).toBe('ollama')
  expect((wrapper.get('[data-profile-base-url]').element as HTMLInputElement).value).toBe(
    'http://localhost:11434',
  )

  wrapper.unmount()
})

it('opens a profile with an unknown provider in the custom tab', async () => {
  const wrapper = mountAgents()
  await flushPromises()

  await wrapper.get('[data-profile-row="14"] [data-edit-profile]').trigger('click')
  expect((wrapper.get('[data-profile-provider]').element as HTMLInputElement).value).toBe('azure')

  wrapper.unmount()
})
```

Adapt the existing edit test (`edits a profile without sending an api_key…`) to open profile 11 through the custom tab and keep asserting the PATCH has no `api_key`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd web && npm test -- --run src/views/AgentsView.test.ts`
Expected: FAIL — edit still opens the old form, no `[data-platform]` preselection, no custom-model input.

- [ ] **Step 3: Implement**

Add `platformFor` and `syncPlatformFromForm`, rewrite `openEdit` and the model-select handlers, and split the template's model control: platform tab renders `select[data-profile-model]` with catalog options plus the `Otro modelo…` option and a conditional `input[data-profile-model-custom]`; custom tab renders `input[data-profile-model]`. Update `submitProfile` to omit `base_url` in platform mode and `api_key` when editing with an empty key.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd web && npm test -- --run src/views/AgentsView.test.ts`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/views/AgentsView.vue web/src/views/AgentsView.test.ts
git commit -m "feat(web): edit LLM profiles with catalog-aware model picker"
```

---

### Task 4: Verificación completa y PR

**Files:**
- Modify: `docs/superpowers/specs/2026-11-21-catalogo-plataformas-llm-design.md` (estado ya aprobado, sin cambios)
- Add: `docs/superpowers/plans/2026-11-21-catalogo-plataformas-llm.md` (este plan)

- [ ] **Step 1: Run the full web suite**

Run: `cd web && npm test -- --run`
Expected: all test files PASS.

- [ ] **Step 2: Typecheck and build**

Run: `cd web && npm run build`
Expected: `vue-tsc` without errors and Vite build completes.

- [ ] **Step 3: Run the design lint**

Run: `node "C:/Users/jeriv/.claude/skills/hallmark/scripts/sloplint.mjs" web/src/views/AgentsView.vue --scope app`
Expected: `0 FAIL`. Fix every FAIL; fix or waive with a stated reason every REFLEX/WARN that comes from this change. Re-run after fixes.

- [ ] **Step 4: Commit docs and verify the branch**

```bash
git add docs/superpowers/specs/2026-11-21-catalogo-plataformas-llm-design.md docs/superpowers/plans/2026-11-21-catalogo-plataformas-llm.md
git commit -m "docs: add LLM platform catalog design and plan"
git status
git log --oneline main..HEAD
```

Expected: working tree clean; four commits on the branch.

- [ ] **Step 5: Push and open the PR**

```bash
git push -u origin feat/llm-platform-catalog
gh pr create --base main --title "feat(web): simplify LLM profile creation with a platform catalog" --body "<what, why, how tested with real output, follow-ups left out>"
```

The PR body ends with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. If the active GitHub account cannot push to the repository, switch to the repo owner account, push, and restore the previous account.

Expected: PR URL reported. Do not merge without review.

## Self-Review

- Spec coverage: catálogo (Task 1), alta por plataforma y personalizada (Task 2), modelo libre y edición (Task 3), verificación y PR (Task 4). Sin cambios de backend, `mode` fuera de la UI y defaults del catálogo quedan en Task 1/2.
- Los cinco puntos de Review Focus tienen prueba: alta sin clave, reinicio de modelo y clave conservada al cambiar de pestaña en Task 2; modelo fuera de lista y URL base/proveedor desconocido en Task 3. El envío de `base_url` solo en personalizada se prueba con el objeto exacto de Task 2.
- Firmas y selectores coinciden entre tareas: `llmPlatforms`, `LLMPlatform`, `CUSTOM_MODEL`, `[data-platform]`, `[data-profile-model]`, `[data-profile-model-custom]`.
