# Fase 2 — Motor de revisión con publicación en GitHub

Fecha: 2026-11-19 · Estado: borrador para aprobación

Spec de producto: [QA Control Center — Diseño](2026-11-18-qa-control-center-design.md) §8–§14.

## 1. Intención

Convertir el núcleo (repos, PRs, panel) en un revisor automático real: cuando un PR de un repo
habilitado cambia de SHA, un worker lo clona, ejecuta el motor `ocr` + `opencode` con el perfil LLM
del equipo y **publica el resultado en GitHub**: comentario de resumen, review con comentarios en
línea y sugerencias, check run por SHA y resolución de los hilos de hallazgos ya corregidos. El
panel muestra el mismo resultado en vivo.

La Fase 2 termina publicando en GitHub. No hay entregable intermedio “solo panel”.

## 2. Objetivos, no-objetivos, anti-objetivos

**Objetivos**

1. Disparar una revisión por `opened`, `synchronize`, `reopened` y `ready_for_review` en repos
   `enabled` con equipo asignado; un solo run activo por PR; un push nuevo marca el anterior como
   `superseded`.
2. Pipeline `ocr delegate preview` → `ocr delegate rule` → prompt → `opencode run --format json` →
   validación de esquema → fingerprints → cobertura completa o `inconclusive`.
3. Publicar en GitHub de forma idempotente: un comentario resumen por PR editado en cada revisión,
   una review con comentarios en línea solo para hallazgos nuevos, sugerencias aplicables, hilos
   resueltos y un check run por SHA.
4. Perfiles LLM con **API key** cifrada, concurrencia por perfil, prueba de salud y asignación por
   equipo.
5. Panel: pantalla **Agentes LLM** y **detalle de PR** (runs, hallazgos, cobertura, reintento).
6. Fallos de LLM, cuota, OCR o timeout nunca producen un resultado limpio: tras reintentos el run
   queda `inconclusive` con check `action_required`.
7. Endurecimiento: el agente no edita, no ejecuta shell y no sale del checkout; el worker no tiene
   la clave privada de la App; secretos y `@menciones` se redactan antes de publicar.

**No-objetivos (Fase 3/4)**

- Quality Gate con CI (`waiting_ci`), solicitud de revisión al QA/lead, check `success` como
  aprobación formal.
- Disputas, descartes, confirmaciones y comandos `/qa …`.
- PRs de forks y autores externos (`awaiting_approval`, límites).
- Perfiles de suscripción OpenCode (se difiere; el esquema queda preparado con `mode`).
- Pantallas de Configuración QA, Workers, Analítica y Auditoría.
- Sincronización con GitHub Teams y notificaciones fuera del panel.

**Anti-objetivos**

- Un comentario nuevo por push en lugar de editar el resumen.
- Repetir hallazgos ya resueltos o reabrir hilos cerrados sin motivo.
- Marcar un PR como listo sin haber revisado el SHA exacto actual.
- Tratar un fallo del LLM, una cuota agotada o una cobertura incompleta como aprobación.
- Filtrar la API key, el token de clonado o credenciales leídas del repo en un comentario público.
- Ejecutar código del PR o permitir que el agente modifique el checkout.

## 3. Restricciones

- Go 1.27, chi v5, GORM + goose, River sobre Postgres, Vue 3 + Vite + TypeScript, Vitest.
- La GitHub App **no se recrea**: el manifest ya declara `pull_requests: write`, `checks: write`,
  `contents: read`, `statuses: read`, `members: read`, `issues: write` y los eventos necesarios.
- `ocr` es **obligatorio** (decisión del usuario): el worker fija `@alibaba-group/open-code-review`
  en `1.13.13` y `opencode-ai` en `1.19.0`. Si `ocr` falta o falla, el run es `inconclusive`.
- Solo API keys en esta fase. Ninguna credencial de suscripción.
- El worker no publica en GitHub ni posee la clave privada de la App; el `app` publica.
- **Separación de claves:** `QA_MASTER_KEY` cifra credenciales de la App y solo lo tiene `app`;
  `QA_WORKER_KEY` (32 bytes, base64, obligatoria) cifra API keys de perfiles y tokens de clonado y
  la comparten `app` y `worker`. El worker, aunque tenga acceso a la base, no puede descifrar
  `app_configs`.
- Un repo solo se revisa si está `enabled` **y** tiene `team_id`.
- Commits sin trailers de atribución.

## 4. Enfoques considerados

| Decisión | Alternativas | Elegido y por qué |
|---|---|---|
| Dependencia de `ocr` | (A) obligatorio; (B) opcional con fallback a `git diff`; (C) sin `ocr` | **A**, decisión del usuario: se mantiene la política de exclusiones y las reglas de `ocr`; el riesgo de formato se cubre con fixtures reales en la primera tarea del plan |
| Quién publica | worker publica; app publica | **app publica**: el token con escritura nunca convive con código ajeno |
| Transporte del resultado | payload del job; filas en Postgres | **Postgres**: el job solo lleva IDs, los resultados se pueden inspeccionar y reintentar |
| Comentarios en línea | un comentario por hallazgo; una review con N comentarios | **una review por run**: menos notificaciones y un solo evento en el PR |
| Check del resultado | Checks API; commit statuses | **Checks API**: ya hay permiso y permite anotaciones y `action_required` |
| Perfil LLM | por repo; por equipo con override por repo | **por equipo** con fila `qa_policies` creada al asignar; el override por repo llega en Fase 3 |
| Estado “limpio” | nuevo estado `reviewed`; reutilizar `ready_for_human` | **reutilizar `ready_for_human`**: el SHA ya está revisado; Fase 3 añadirá las condiciones de CI antes de llegar a él |
| Concurrencia por perfil | cola River por perfil; advisory lock | **advisory lock** de Postgres durante el proceso `opencode`, con slots de worker acotados |
| Token de clonado | token completo de instalación; token reducido | **reducido** (`contents: read`, `metadata: read`, solo el repo base): el worker nunca recibe escritura |
| PRs de fork | revisarlos; ignorarlos | **ignorarlos** en Fase 2: columna `is_fork` y fetch de `refs/pull/<n>/head` del repo base |
| Credenciales del worker | misma clave maestra; clave separada | **clave separada** (`QA_WORKER_KEY`): el worker no puede descifrar la clave privada de la App |

## 5. Arquitectura

```
GitHub ──webhook──▶ app ──(river: review)──▶ worker ──▶ ocr / opencode
   ▲                  │                          │
   │                  │◀── resultado en Postgres ┘
   │                  └──(river: publish)──▶ app ──▶ comentario, review, check
   └───────────────────────────────────────────────┘
```

- **app** (`qa serve`): webhooks, API, SSE, scheduler, colas `control` y `publish`. Tiene la clave
  de la App, crea el check `in_progress` al encolar y publica al terminar.
- **worker** (`qa worker`): cola `review`. Clona, ejecuta `ocr` y `opencode`, guarda el resultado.
  Tiene `git`, Node 24, `ocr` y `opencode` fijados, y `QA_WORKER_KEY` para descifrar el token de
  clonado.
- **postgres**: estado, cola River, resultados y locks.

Nuevos paquetes: `internal/review` (pipeline), `internal/publish` (publicación), `internal/llm`
(perfiles, proveedores, salud), `internal/schema` (validación del JSON del LLM). Se extienden
`internal/store`, `internal/jobs`, `internal/github`, `internal/api` y `web`.

## 6. Modelo de datos

Migración nueva `00002_phase2_review.sql` (goose), modelos GORM equivalentes.

**`llm_profiles`**
`id`, `name` (unique), `provider`, `model`, `variant` (nullable), `mode` (`api_key`),
`api_key_enc` (cifrada con `QA_WORKER_KEY`), `base_url` (nullable), `concurrency` (default 1),
`enabled`, `health_status` (`unknown|ok|error`), `health_checked_at`, `health_error`, timestamps.

**`qa_policies`**
`id`, `team_id` (unique, FK), `llm_profile_id` (FK), `blocking_threshold` (default `major`),
`extra_instructions`, `exclude_globs` (jsonb, lista de patrones), `max_files_per_pass` (40),
`max_diff_lines_per_pass` (3000), `max_passes` (4), timestamps.

**`review_runs`**
`id`, `pull_request_id`, `profile_id`, `head_sha`, `base_sha` (merge-base), `trigger`
(`webhook|retry|reconcile`), `status` (`queued|running|succeeded|inconclusive|failed|superseded`),
`attempt`, `summary`, `coverage` (jsonb), `usage` (jsonb), `error_code`, `error_message`,
`clone_token_enc`, `started_at`, `finished_at`, timestamps.
Índice único parcial: `(pull_request_id) WHERE status IN ('queued','running')`.

**`pull_requests` (columnas nuevas en la misma migración)**
`is_fork` (bool, default false) y `head_repo_full_name` (text). El syncer y el webhook los rellenan
desde `head.repo.fork` y `head.repo.full_name`. Los PRs de fork no se revisan en Fase 2.

**`findings`**
`id` (uuid), `pull_request_id`, `fingerprint`, `path`, `start_line`, `end_line`, `severity`
(`blocker|major|minor|nit`), `category`, `title`, `body`, `suggestion`, `confidence`,
`status` (`open|resolved|withdrawn`), `first_seen_run_id`, `last_seen_run_id`,
`github_comment_id`, `github_comment_node_id`, `github_thread_id`, timestamps.
Único `(pull_request_id, fingerprint)`.

**`finding_events`**
`id`, `finding_id`, `review_run_id`, `kind` (`created|resolved|reopened|published|thread_resolved`),
`actor`, `data` (jsonb), `created_at`.

**`github_artifacts`**
`id`, `pull_request_id`, `kind` (`summary_comment|review|check_run`), `head_sha`, `external_id`,
`url`, timestamps. Único `(pull_request_id, kind)` para `summary_comment` y
`(pull_request_id, kind, head_sha)` para `check_run`.

No se añaden tablas de Fase 3 (`notifications`, `github_teams`).

## 7. Pipeline de revisión

### 7.1 Disparo y encolado (app)

- Webhook `pull_request` en `opened|synchronize|reopened|ready_for_review`: si el PR no es draft, no
  es de fork, el repo está `enabled` y tiene equipo **con política y perfil habilitados**, se marca
  `pending`, se marcan `superseded` los runs `queued|running` del PR y se inserta `ReviewRun` + job
  `review_run{run_id}`.
- El scheduler, tras sincronizar, encola reviews para PRs `pending` de repos habilitados sin run
  activo (máximo 50 por ciclo). Se salta los PRs sin política/perfil (no generan runs ni checks) y
  los que ya tienen un run `failed|inconclusive` para ese mismo `head_sha`: esos solo se reintentan
  a mano.
- Al encolar: se crea o actualiza el check run `QA Control Center` en `in_progress` para el SHA.
- Se mintea un **token de solo lectura** (`contents: read`, `metadata: read`) limitado al repo base
  mediante `POST /app/installations/{id}/access_tokens` con `repositories: [<repo>]`; nunca un token
  con escritura. Se cifra y se guarda en `review_runs.clone_token_enc`; se borra al terminar el run.

### 7.2 Worker

1. Carga run, PR, repo, instalación, política y perfil. Si el run ya no es el vigente, termina como
   `superseded` sin llamar a GitHub.
2. Descifra el token y clona en `$QA_WORKSPACE_DIR/<run_id>`: `git init`, remote con
   `x-access-token:<token>@github.com/...`, `fetch` del base y de `+refs/pull/<n>/head` con
   profundidad creciente hasta encontrar el merge-base (tope `QA_FETCH_DEPTH_MAX`, 500). Se usa la
   ref del PR, no la rama, para que funcione con ramas borradas y para no depender del fork.
   Checkout detached del head. Borra el remote con credenciales.
3. `ocr delegate preview --format json --repo <checkout> --from <base_ref> --to <head_sha>` →
   `merge_base`, `reviewable_files` (path, status, inserciones/borrados) y `excluded_files` con
   motivo. El `merge_base` devuelto se guarda como `review_runs.base_sha`.
4. `ocr delegate rule --format json --repo <checkout> --from <base_ref> --to <head_sha> <archivos>`
   → reglas agrupadas por contenido; se llama por lote, con los mismos flags que el preview.
5. Planifica pasadas: agrupa archivos por grupo de reglas y presupuesto
   (`max_files_per_pass`, `max_diff_lines_per_pass`), hasta `max_passes`.
6. Por pasada: genera el diff con
   `git --no-pager diff --no-ext-diff --no-textconv --no-color <merge_base>..<head_sha> -- <paths>`
   y ejecuta `opencode` con un HOME temporal, `--pure`, `--format json`, `--agent qa-reviewer`,
   `--model <provider>/<model>` y `--variant` si existe. El mensaje lleva instrucciones, reglas,
   contexto del PR como datos no confiables, hallazgos abiertos de la revisión anterior y el
   esquema; el diff se adjunta con `--file` para no exceder el tamaño de argumentos.
7. La configuración de OpenCode se genera en el HOME temporal: agente `qa-reviewer` con `edit`,
   `bash`, `webfetch` y `external_directory` en `deny`; lectura, `grep` y `glob` permitidos;
   sin plugins. Cada run usa `HOME` y `XDG_*` propios y un servidor de OpenCode privado (nunca el
   servicio compartido del usuario), y se ejecuta en su propio grupo de procesos para poder
   terminarlo completo al acabar o al vencer el timeout.
8. Extrae el último mensaje del stream JSON, lo valida contra el esquema (decodificación estricta
   con campos desconocidos prohibidos). Si no valida, un reintento pidiendo corrección; si vuelve a
   fallar, `inconclusive`.
9. Fusiona hallazgos de todas las pasadas, calcula fingerprints (`sha256` de ruta + categoría +
   título normalizado + hash del contexto de ±3 líneas) y reconcilia con los abiertos del PR:
   `resolved` para los que el LLM marca resueltos o ya no aparecen **solo si el run termina
   `succeeded` con cobertura completa**; en `inconclusive` no se resuelve nada.
10. Si `ocr` reportó archivos revisables que ninguna pasada cubrió, una pasada extra; si siguen
    faltando, `inconclusive` con `coverage_incomplete`.
11. Guarda run, hallazgos, eventos y cobertura en una transacción; actualiza `qa_status`; encola
    `publish_review{run_id}`.

**Contrato con `ocr` (modo delegación, verificado en la documentación de la versión fijada).**
`preview` devuelve `mode`, `merge_base`, `reviewable_files` y `excluded_files` con motivo;
`rule` agrupa los archivos por contenido de reglas. `ocr` usa `severity` `critical|high|medium|low`
y un catálogo de categorías más amplio que el nuestro: se traducen a nuestro esquema
(`critical→blocker`, `high→major`, `medium→minor`, `low→nit`; `documentation|other` →
`maintainability`). No se usa `--background`: el contexto del PR es no confiable y viaja en nuestro
prompt, no como “requisito”. La cobertura se cuenta sobre `reviewable_files` (identidad
`path+status`), y cada archivo termina `reviewed` o `skipped` con motivo.

### 7.3 Fallos y reintentos

| Causa | Reintentos | Resultado final |
|---|---|---|
| Error de red/git | 3 con backoff | `failed` + check `action_required` |
| `ocr` no disponible o JSON inválido | 2 | `inconclusive` (`ocr_error`) |
| LLM: cuota, timeout, 5xx | 3 con backoff | `inconclusive` (`llm_error`) |
| Salida que no valida el esquema | 1 interno + 2 de job | `inconclusive` (`schema_invalid`) |
| Token expirado | 1 | `inconclusive` (`token_expired`), reintento manual |
| Cobertura incompleta | 1 pasada extra | `inconclusive` (`coverage_incomplete`) |
| Configuración inválida (solo en reintento manual) | 0 | `failed` (`config`) + check `action_required` |

`QA_RUN_TIMEOUT` (20m) mata el proceso y clasifica `timeout`. River reintenta el job; el run
conserva `attempt`.

### 7.4 Perfil LLM y contrato con OpenCode

- `provider` es un ID del catálogo de models.dev, limitado en la UI a una lista curada:
  `anthropic`, `openai`, `openrouter`, `google`, `deepseek`, `xai`, `mistral`, `groq`. El modelo es
  texto libre (ID del catálogo). `base_url` es opcional y solo se admite para esos proveedores.
- El worker genera un `opencode.jsonc` por run con:

  ```jsonc
  {
    "providers": {
      "<provider>": {
        "env": ["QA_LLM_API_KEY"],
        "settings": { "baseURL": "<base_url si existe>" }
      }
    }
  }
  ```

  Así el nombre de la variable no depende del proveedor: la API key descifrada se inyecta como
  `QA_LLM_API_KEY` **solo** en el proceso `opencode`, nunca en el entorno del worker ni en logs.
  El modelo se referencia como `<provider>/<model>`.
- La prueba de salud encola `llm_health{profile_id}` en la cola `review`: el worker ejecuta el mismo
  binario y configuración con un prompt mínimo (`responde OK`), timeout de 60 s, y guarda
  `health_status`, `health_checked_at` y `health_error`. La API responde 202 y la UI recibe el
  cambio por SSE.
- Los proveedores que necesitan más que una API key (Azure, Vertex, Bedrock, Copilot) y los
  endpoints personalizados fuera del catálogo quedan fuera de esta fase; el aviso aparece en la UI.

### 7.5 Concurrencia

`QA_REVIEW_CONCURRENCY` slots por worker (default 2). Antes de lanzar `opencode`, el worker toma
`pg_advisory_lock` con clave derivada de `profile_id` y lo libera al terminar; así un perfil con
`concurrency=1` no se solapa aunque haya varios workers. Sin timeout propio: los slots acotan la
espera.

## 8. Publicación en GitHub (app)

Job `publish_review{run_id}` idempotente (si ya existe artefacto para la clave, se actualiza).

**Qué publica cada estado del run**

| Estado | Resumen | Comentarios nuevos | Resoluciones | Check | `qa_status` |
|---|---|---|---|---|---|
| `succeeded` (cobertura completa) | edita | sí | sí | `success`/`failure` | `ready_for_human`/`changes_requested` |
| `inconclusive` | edita con aviso | sí, los de pasadas completas | **no** | `action_required` | `inconclusive` |
| `failed` | no toca | no | no | `action_required` con el error | `inconclusive` |
| `superseded` | no toca | no | no | cierra el check anterior como `cancelled` | sin cambio |

Un hallazgo solo pasa a `resolved` cuando el run fue `succeeded` y la cobertura fue completa: una
revisión parcial no es prueba de que lo ausente esté corregido.

1. **Comentario resumen** único por PR, marcador `<!-- qa-control-center:summary -->`, editado en
   cada run: número de revisión, SHA corto, conteo por severidad, resueltos desde la anterior,
   resumen, cobertura (`revisados/omitidos`) y enlace al panel. Si el run es `inconclusive` o
   `failed`, encabeza un aviso con el motivo. Los hallazgos sin línea en el diff se listan aquí.
2. **Review con comentarios en línea** (`COMMENT`, nunca `APPROVE` ni `REQUEST_CHANGES`) solo para
   hallazgos nuevos cuya línea cae en un hunk del diff. Si `suggestion` existe y el rango completo
   son líneas añadidas, se emite bloque ```` ```suggestion ````; si no, texto.
3. **Hilos resueltos:** solo en runs `succeeded`; respuesta breve en el hilo y
   `resolveReviewThread` por GraphQL, usando `github_comment_node_id` para obtener el `thread_id`
   (se guarda en el hallazgo).
4. **Check run** por SHA: `success` si no hay hallazgos abiertos con severidad ≥ umbral; `failure`
   si los hay; `action_required` si el run es `inconclusive` o `failed`. Se adjuntan hasta 50
   anotaciones por petición (el resto se pagina).
5. **Estado del PR:** con bloqueantes → `changes_requested`; sin bloqueantes → `ready_for_human`;
   `inconclusive`/`failed` → `inconclusive`. Nunca se solicita revisión humana (eso es Fase 3).

**Saneamiento:** se escapan `@menciones` (`@` → `` `@` ``), se redactan los valores secretos
conocidos (API key del perfil, token de clonado) y patrones (`ghp_`, `github_pat_`, `sk-`, `AKIA`,
`-----BEGIN`). Si hay coincidencia, el run se marca `redacted` y se emite alerta SSE.

## 9. API y panel

**Endpoints nuevos**

- `GET/POST /api/llm-profiles`, `PATCH/DELETE /api/llm-profiles/{id}` — admin. La API key nunca se
  devuelve; se muestra `api_key_set: true`.
- `POST /api/llm-profiles/{id}/health` — encola prueba de salud en el worker y devuelve 202.
- `GET/PUT /api/teams/{id}/policy` — admin o lead del equipo: perfil, umbral, instrucciones,
  exclusiones y presupuestos.
- `GET /api/prs/{id}` — PR, runs, hallazgos y cobertura.
- `POST /api/prs/{id}/retry` — admin o lead del equipo; crea run y encola.
- SSE: `run.updated`, `finding.updated`, `pr.updated`.

**Pantallas**

- `/agents` — tabla de perfiles (nombre, proveedor/modelo, concurrencia, salud), alta/edición con
  API key write-only, botón “Probar”, y asignación equipo → perfil.
- `/prs/:id` — cabecera (`repo #n`, título, SHA, estado, check), timeline de runs con duración y
  motivo de fallo, hallazgos por severidad con `ruta:línea` y fragmento, cobertura, botón
  “Reintentar”. Actualización en vivo por SSE; enlace al PR en GitHub.
- Dashboard: las tarjetas enlazan al detalle; se añade el estado del check cuando exista.

## 10. Seguridad

- Worker sin clave de la App ni tokens de escritura; el token de clonado pide solo
  `contents: read` + `metadata: read` sobre el repo base, dura 1 h, se guarda cifrado con
  `QA_WORKER_KEY` y se borra al terminar; el remote con credenciales se elimina tras el fetch.
- Cada run en directorio temporal propio con HOME temporal; `external_directory` en `deny`; sin
  `--auto`; sin plugins.
- Salida del LLM validada y saneada antes de publicar; contenido del PR tratado como datos.
- API keys cifradas en reposo con `QA_WORKER_KEY`; nunca en logs, jobs ni respuestas.
- Directorios de trabajo borrados en `defer`, incluso en error.
- Firma HMAC de webhooks ya existente; sin cambios de auth ni RBAC.

## 11. Configuración y operación

| Variable | Defecto | Uso |
|---|---|---|
| `QA_REVIEW_CONCURRENCY` | `2` | slots de revisión por worker |
| `QA_REVIEW_TIMEOUT` | `20m` | tope por run |
| `QA_WORKSPACE_DIR` | `/tmp/qa` | checkouts efímeros |
| `QA_OCR_BIN` | `ocr` | binario OCR (tests usan uno falso) |
| `QA_OPENCODE_BIN` | `opencode` | binario OpenCode |
| `QA_FETCH_DEPTH_MAX` | `500` | profundidad máxima de fetch |
| `QA_WORKER_KEY` | — (obligatoria) | cifra API keys de perfiles y tokens de clonado; no descifra la App |

Imagen: nuevo target `worker` en `deploy/Dockerfile` con Node 24 y
`npm i -g @alibaba-group/open-code-review@1.13.13 opencode-ai@1.19.0`; el target `app` sigue
mínimo. `docker-compose.yml` compila `worker` con ese target. Al arrancar, el worker valida que
`ocr` y `opencode` existen y registra sus versiones; si faltan, falla rápido.

Reparto de secretos en Compose: `app` recibe `QA_MASTER_KEY` y `QA_WORKER_KEY`; `worker` solo
`QA_WORKER_KEY` (más base de datos y las variables de revisión). Sin `QA_WORKER_KEY`, `serve` y
`worker` fallan al arrancar con un mensaje que indica cómo generarla.

## 12. Pruebas

- **Unitarias Go:** fingerprints y reconciliación de hallazgos (incluido “no resolver en
  inconclusive”); validación estricta del esquema; constructor de prompt (incluye contexto no
  confiable y hallazgos previos); planificador de pasadas y cobertura; redacción de secretos y
  menciones; transiciones de estado por estado de run; cifrado con `QA_WORKER_KEY` y rechazo de
  descifrado con la clave maestra; token de clonado con permisos mínimos; filtrado de PRs de fork;
  resolución de política por equipo; payloads de publicación con `go-github` falso; idempotencia de
  artefactos; el scheduler no reencola un SHA con run `failed|inconclusive`.
- **Integración:** `httptest` de GitHub (token, contents, pulls, reviews, checks, GraphQL) + Postgres
  de `testutil`, con binarios falsos `fakeocr` y `fakeopencode` (fixtures JSON reales capturados en
  la primera tarea). Casos: review feliz, push que supersede, OCR caído, esquema inválido,
  cobertura incompleta, reintento manual, publicación idempotente.
- **Vue:** Vitest para `AgentsView` (alta, key write-only, salud) y `PrDetailView` (runs, hallazgos,
  reintento).
- **Manual:** un PR real en un repo de prueba con una API key real; se verifica comentario, review,
  check y re-revisión tras push.

## 13. Despliegue

1. Generar `QA_WORKER_KEY` (`openssl rand -base64 32`) y añadirla a `deploy/.env` para `app` y
   `worker`; `app` conserva además `QA_MASTER_KEY`.
2. `qa migrate` aplica `00002` (lo hace el arranque de `app`).
3. Se publican las pantallas y endpoints nuevos.
4. Los repos sin equipo o deshabilitados no generan runs ni check.
5. Rollback: desplegar la imagen anterior; las tablas nuevas quedan inertes y los webhooks siguen
   funcionando como Fase 1.

## 14. Riesgos y verificaciones pendientes

- **Formato de `ocr delegate preview/rule`**: los campos y flags están verificados en la
  documentación de la versión fijada; la primera tarea del plan ejecuta el binario real contra un
  repo de prueba y guarda fixtures para fijar el parser.
- **Eventos de `opencode run --format json`**: se captura una ejecución real y se documenta cómo
  extraer el mensaje final y el uso de tokens; si no hay evento final fiable, se usa el último
  `text` completo y se valida.
- **Permiso de `resolveReviewThread`**: se valida contra la App real en la tarea de publicación;
  si fallara, el hilo se responde y se deja abierto, registrando la limitación.
- **Tamaño de contexto**: PRs grandes dependen del planificador de pasadas; el límite por defecto
  es conservador y configurable.
- **Coste**: un PR con muchos pushes consume runs; el supersede evita trabajo duplicado, pero no
  hay presupuesto por repo hasta Fase 3.
- **Variables de proveedor del catálogo**: se valida en la prueba de salud que
  `providers.<provider>.env` funciona para los proveedores de la lista curada; si alguno no lo
  admitiera, se usa su variable estándar como caso especial documentado.
- **PRs de fork**: excluidos por `is_fork`; si un PR cambia de fork a mismo repo (o al revés), el
  webhook actualiza la columna y el siguiente SHA se dispara o se ignora en consecuencia.

## 15. Decisiones del constructor

Librería de validación (se elige decodificación estricta propia), nombres exactos de eventos SSE,
diseño visual de las pantallas, redacción literal de los prompts, formato del bloque de resumen y
valores finos de backoff no fijados aquí.
