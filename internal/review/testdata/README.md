# Fixtures del motor de revisión

Salidas reales de `ocr` y `opencode` usadas para fijar los `json` tags de
`PreviewResult`, `RuleGroup` y del extractor de eventos del agente. Este directorio
solo contiene datos: no incluye parsers ni código de producto.

## Versiones

| Herramienta | Versión / detalle |
| --- | --- |
| `@alibaba-group/open-code-review` (binario `ocr`) | `v1.14.0 (d29ca204) linux/amd64`, compilado el `2026-11-21T07:02:25Z` |
| Imagen `qa-worker-ocr114:local` | `ocr 1.14.0 (d29ca204)`; usada para las capturas de `ocr review` |
| `opencode` | `1.19.0` |
| Node / npm | `v24.20.0` / `12.0.2` |
| `jq` | `1.7.1` |

Instalación (prefijo temporal, fuera del repo):

```bash
npm install --prefix /tmp/qa-fixtures @alibaba-group/open-code-review@1.14.0
/tmp/qa-fixtures/node_modules/.bin/ocr --version
# open-code-review v1.14.0 (d29ca204) linux/amd64
```

npm avisó de que el `postinstall` del paquete quedó bloqueado por la política de
scripts; el binario incluido funciona igualmente y no fue necesario aprobarlo.

`ocr delegate` no cambió respecto de la versión anterior, así que los fixtures
conservan el mismo esquema; se validaron con 1.14.0 sin recapturarlos.

Los fixtures de `ocr review` que se describen más abajo sí se capturaron con el
binario 1.14.0 de `qa-worker-ocr114:local`; no se reutilizaron salidas de
versiones anteriores.

## Repo de prueba

Los fixtures de `ocr` se capturaron de un repo git **temporal** (no del repo del
proyecto): `/private/var/folders/8d/3l5hglbj02123b9ftz8w6sj40000gn/T/opencode/ocr-fixture-repo`.

- Commit base `bac725e283dbec23985186cbcc9894bec7981351` (`base: initial fixture`):
  `main.go` y `README.md`.
- Commit HEAD `f567dae75c8e95396f5107dd36e2c6d03beb7c67`
  (`feat: add auth with naive password comparison`): modifica ambos archivos y añade
  `auth.go`, donde `CheckPassword` compara contraseñas con `==` (bug evidente).

## Fixtures y comandos de captura

| Archivo | Contenido |
| --- | --- |
| `ocr-preview.json` | Resultado de `ocr delegate preview`: 3 archivos totales, 2 revisables (`auth.go` añadido, `main.go` modificado) y `README.md` excluido por `unsupported_ext`. |
| `ocr-rule.json` | Resultado de `ocr delegate rule`: 2 grupos (`**/*.go` con `main.go`, `auth.go`; `default` con `README.md`), reglas de sistema en texto. |
| `opencode-events.jsonl` | 3 eventos reales de `opencode run --format json`: `step_start`, `text` (texto `{"ok":true}`) y `step_finish`. |

Comandos ejecutados (con `OCR=/tmp/qa-fixtures/node_modules/.bin/ocr` y
`BASE=bac725e283dbec23985186cbcc9894bec7981351`):

```bash
$OCR delegate preview --format json --repo <repo> --from "$BASE" --to HEAD \
  > internal/review/testdata/ocr-preview.json
$OCR delegate rule --format json --repo <repo> --from "$BASE" --to HEAD main.go auth.go README.md \
  > internal/review/testdata/ocr-rule.json

opencode run --pure --format json \
  --model nvidia/nvidia/nemotron-3.5-lightning-30b-a3b \
  'Responde solo con {"ok":true}' > opencode-events.jsonl
```

La salida de OpenCode se redirigió primero a un archivo temporal y luego se copió a
este directorio; el contenido es el stream completo, sin editar.

### Credencial de OpenCode

La captura de OpenCode **sí usó una credencial real**: la configuración `nvidia` de
`~/.local/share/opencode/auth.json`. Por eso no se aplicó el `HOME=$(mktemp -d)`
del brief: un HOME vacío oculta esa credencial y la ejecución falla. No se necesita
recapturar antes de la Task 12.

## Fixtures de `ocr review` (1.14.0)

Estas capturas ejercitan el comando nativo `ocr review` de 1.14.0, incluidas
las flags `--max-tokens-budget` y `--exclude`, y el esquema JSON que consumirá
el parser de #71. Se generaron con el binario real de `qa-worker-ocr114:local`
y un proveedor LLM falso: no se usó ninguna credencial real ni se hizo ninguna
llamada externa.

### Proveedor falso

Dentro del mismo contenedor se ejecutó un servidor Node.js compatible con
`/v1/chat/completions` (`/tmp/fake.js`, fuera de este repo). Responde con
`tool_calls` de `code_comment`, `task_done` y `approve_all_comments` según el
caso. En el escenario de caché devuelve `usage` con `prompt_tokens_details`;
en el de timeout duerme 20 s sin responder; en el de fallo responde HTTP 500.
OCR se invocó con un entorno limpio para que no heredara claves del host:

```bash
env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/tmp/ocrhome \
  OCR_LLM_URL=http://127.0.0.1:18080/v1 \
  OCR_LLM_TOKEN=test \
  OCR_LLM_MODEL=fake-model \
  OCR_LLM_PROTOCOL=openai \
  OCR_LLM_TIMEOUT=60 \
  ocr review --repo <repo> --from <base> --to <head> --format json --audience agent
```

Para el escenario de timeout se cambió `OCR_LLM_TIMEOUT` a `2` (segundos); el
resto del entorno fue idéntico.

Los repos de prueba se crearon con `git init` dentro del contenedor (`/tmp/repo`,
`/tmp/repo-multi` y `/tmp/repo-skipped`), nunca dentro del worktree. Los dos
fixtures nuevos reutilizan `/tmp/repo` con un rango reducido (`696a494..d529080`).

### Escenarios

| Archivo | Repo / comando | Resultado |
| --- | --- | --- |
| `ocr-review-complete.json` | `/tmp/repo`, `--from 5375f11 --to 9bb6edc` | `terminal_state: complete`, 2 comentarios (`security/high` y `maintainability/low`), 4960 tokens, exit 0. |
| `ocr-review-complete-cache.json` | `/tmp/repo`, `--from 696a494 --to d529080`; `usage` por respuesta: 5000 `prompt_tokens`, 3000 `prompt_tokens_details.cached_tokens`, 1000 `prompt_tokens_details.cache_creation_tokens` | `terminal_state: complete`, 2 comentarios, `total_tokens: 20640`, `cache_read_tokens: 12000`, `cache_write_tokens: 4000`, exit 0. |
| `ocr-review-partial-budget.json` | `/tmp/repo-multi`, `--from 6877354 --to b8453d9 --concurrency 1 --max-tokens-budget 30000` | `terminal_state: partial`, 3 comentarios, 3 ítems `completed` y 9 `failed` con `classification: budget`, 14880 tokens, exit 0. |
| `ocr-review-failed-provider.json` | `/tmp/repo`, mismo rango que el caso completo; el servidor falso responde HTTP 500 | `terminal_state: failed`, `status: failed`, 2 ítems `failed` con `classification: provider`, `retry_report` con 6 intentos y `status_code: 500`, exit 1. El JSON se capturó igualmente. |
| `ocr-review-skipped.json` | `/tmp/repo-skipped`, `--from c3f6044 --to 5bce0db --exclude '**/*.go'` | `terminal_state: skipped`, `selected: []`, `excluded` con `exclude_reason: user_exclude`, 0 tokens, exit 0. |
| `ocr-review-timeout.json` | `/tmp/repo`, mismo rango que el caso de caché; el servidor falso duerme 20 s y `OCR_LLM_TIMEOUT=2` | `terminal_state: failed`, `status: failed`, 2 ítems `failed` con `classification: timeout`, `retry_report` con 1 request, 0 reintentos y `failure_phase: context`, exit 1. El JSON se capturó igualmente. |

En los casos de fallo de proveedor y de timeout el comando se ejecutó con
`|| true` para conservar la salida y el código de salida real (1).

### Caché de tokens

OCR 1.14.0 sí expone los tokens de caché en `summary`: mapea
`usage.prompt_tokens_details.cached_tokens` a `summary.cache_read_tokens` y
`usage.prompt_tokens_details.cache_creation_tokens` a
`summary.cache_write_tokens`. Los valores se suman entre las peticiones de la
revisión: 4 peticiones × 3000 / 1000 dieron `12000` / `4000`, mientras que
`input_tokens` sumó 4 × 5000 = 20000. Cuando el proveedor no envía
`prompt_tokens_details` (caso `ocr-review-complete.json`), ambos campos se
omiten del `summary`, que es lo que hizo pensar que solo existían en el
esquema inferido. El fixture de caché fija los campos con valores no cero.

### Timeout frente a fallo de proveedor

El timeout de una petición no se confunde con un HTTP 500. En
`ocr-review-timeout.json` cada ítem falla con `classification: timeout` y
`reason: "LLM request timed out; configure OCR_LLM_TIMEOUT or provider timeout_sec"`;
el `retry_report` tiene un único request con un único intento
(`outcome: error`, `error_class: timeout`, `failure_phase: context`,
`duration_to_headers_ms` sanitizado) y sin `status_code` ni reintentos, porque
el timeout no se reintenta. En `ocr-review-failed-provider.json`, en cambio,
`classification` es `provider`, `failure_phase` es `http`, hay `status_code: 500`
y 5 reintentos. Ambos terminan en `terminal_state: failed` y exit 1, de modo que
la distinción para el mapeo de estados de #71 está en `classification`,
`error_class`, `failure_phase` y el número de intentos.

### Sanitización

Los seis JSON son salidas reales del binario, con estos valores sustituidos
para que sean deterministas:

- `session_id` y `manifest.run_id`: UUIDs fijos
  `00000000-0000-4000-8000-000000000001` a `...0006`, uno por escenario.
- `summary.elapsed`: `"0s"`; `manifest.elapsed_ms`: `0`.
- `retry_report.requests[*].logical_request_id`: hash fijo terminado en `3`.
- `retry_report.requests[*].attempts[*].duration_to_headers_ms` y
  `observed_backoff_ms`: `0`.
- No se encontraron rutas absolutas, hosts, URLs ni timestamps en la salida.

Se conservan el esquema completo, `manifest.coverage`, `manifest.terminal_state`,
`status`, los campos de tokens de `summary`, `groups`, `comments`, los hashes de
commit, `item_id`/`fingerprint` y el nombre `fake-model` (identificador explícito
del proveedor falso). Los comentarios los genera el proveedor falso; estos
fixtures fijan la forma y los estados de `ocr review`, no la calidad del texto.

## Verificación

```bash
jq . internal/review/testdata/ocr-preview.json > /dev/null
jq . internal/review/testdata/ocr-rule.json > /dev/null
jq -c . internal/review/testdata/opencode-events.jsonl > /dev/null
jq -s 'any(.[]; .type == "text" and (.part.text // "") != "")' \
  internal/review/testdata/opencode-events.jsonl

for f in internal/review/testdata/ocr-review-*.json; do
  jq -e . "$f" > /dev/null
done
jq -e '.summary.cache_read_tokens == 12000 and .summary.cache_write_tokens == 4000' \
  internal/review/testdata/ocr-review-complete-cache.json
jq -e '[.manifest.coverage.failed[].classification] | length == 2 and all(. == "timeout")' \
  internal/review/testdata/ocr-review-timeout.json
jq -e '.retry_report.requests[0].attempts[0] |
  .error_class == "timeout" and .failure_phase == "context"' \
  internal/review/testdata/ocr-review-timeout.json
jq -r '[input_filename | split("/")[-1], .manifest.terminal_state,
  (.comments // [] | length), .summary.total_tokens] | @tsv' \
  internal/review/testdata/ocr-review-*.json
```

Resultado esperado de la última orden (orden alfabético):

```text
ocr-review-complete-cache.json	complete	2	20640
ocr-review-complete.json	complete	2	4960
ocr-review-failed-provider.json	failed	0	0
ocr-review-partial-budget.json	partial	3	14880
ocr-review-skipped.json	skipped	0	0
ocr-review-timeout.json	failed	0	0
```

Resultado del resto: los tres archivos parsean; `jq 'keys'` devuelve
`excluded_count, excluded_files, from, merge_base, mode, repository,
reviewable_count, reviewable_files, schema_version, to, total_deletions,
total_files, total_insertions` para `ocr-preview.json` y `groups, schema_version`
para `ocr-rule.json`; el stream tiene 3 eventos y 1 evento `text` con texto.

## Notas para los tests

- `ocr-preview.json` contiene la ruta absoluta del repo temporal y los hashes del
  commit base; son datos de la captura, no valores que los tests deban fijar (salvo
  los `json` tags y la forma de los objetos).
- `opencode-events.jsonl` contiene IDs de sesión/parte y timestamps reales de la
  ejecución; los tests deben comprobar la estructura (`type`, `part.text`), no esos
  valores.
- `fakeopencode-review.sh` acepta `RUN_FAKE_AGENT_ARGV` para añadir a ese archivo
  la línea de argumentos de cada invocación. Los tests la usan para comprobar que
  el reintento por esquema usa `--continue` y no vuelve a adjuntar el diff con
  `-f`.
- `ocr-review-complete.json`: fija `manifest.terminal_state == "complete"`, un
  array de 2 comentarios y las parejas `security/high` y
  `maintainability/low`; `comments` es un array. No incluye campos de caché
  porque el proveedor no devolvió `prompt_tokens_details`.
- `ocr-review-complete-cache.json`: fija `manifest.terminal_state == "complete"`,
  `summary.cache_read_tokens == 12000` y `summary.cache_write_tokens == 4000`
  con `input_tokens == 20000`; los campos de caché solo aparecen cuando el
  proveedor envía `prompt_tokens_details`.
- `ocr-review-timeout.json`: fija `status` y `manifest.terminal_state` en
  `failed`, `comments: null`, los 2 ítems de `manifest.coverage.failed` con
  `classification: timeout`, y `retry_report` con un intento
  `error_class: timeout` / `failure_phase: context` sin reintentos.
- `ocr-review-partial-budget.json`: fija `manifest.terminal_state == "partial"`,
  `summary.budget_exceeded == true`, `manifest.coverage.failed` con
  `classification: budget` y 3 comentarios utilizables.
- `ocr-review-failed-provider.json`: fija `status` y `manifest.terminal_state`
  en `failed`, `comments: null`, `manifest.coverage.failed` con
  `classification: provider` y `retry_report` con intentos HTTP 500.
- `ocr-review-skipped.json`: fija `manifest.terminal_state == "skipped"`,
  `comments: []`, `manifest.coverage.selected` vacío y `excluded` con
  `exclude_reason: user_exclude`.
- Los valores sanitizados (`session_id`, `run_id`, `elapsed`, `elapsed_ms`,
  `logical_request_id`, duraciones y backoff) no deben usarse como aserciones;
  los tests deben comprobar estados, cobertura, tokens y forma de los comentarios.
