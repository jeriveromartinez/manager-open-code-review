# QA Control Center

Revisor automático de pull requests, auto-hospedado. Crea su propia GitHub App desde un manifiesto, recibe webhooks (o funciona solo con polling), encola la sincronización de PRs en PostgreSQL y muestra el resultado en un panel Vue en vivo. Una misma instancia puede gestionar repositorios de varias cuentas personales u organizaciones de GitHub.

## Arquitectura

Se despliega con **una sola imagen** en tres contenedores:

| Servicio | Proceso | Función |
|---|---|---|
| `app` | `qa serve` | API REST + SSE, webhooks, web Vue embebida con `go:embed`, scheduler de reconciliación, disparo de revisiones y publicación en GitHub |
| `worker` | `qa worker` | Cola `review`: clona el PR, ejecuta `ocr` + `opencode` y guarda el resultado |
| `postgres` | `postgres:17` | Estado, cola River y eventos `LISTEN/NOTIFY` |

El Dockerfile produce dos targets: `app` (mínimo) y `worker` (con `git`, Node 24, `ocr` 1.13.13 y `opencode` 1.19.0). `app` publica en GitHub con la GitHub App; el worker nunca tiene su clave privada ni tokens de escritura, y clona con un token de solo lectura. `QA_MASTER_KEY` cifra la configuración de la App y la usa solo `app`; `QA_WORKER_KEY` cifra las API keys y los tokens de clonado, y la comparten `app` y `worker`.

## Inicio rápido

```sh
cp deploy/.env.example deploy/.env
# rellena las dos claves con valores distintos:
openssl rand -base64 32   # QA_MASTER_KEY (solo app)
openssl rand -base64 32   # QA_WORKER_KEY (app y worker)
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs app | grep setup_token
```

Abre `http://localhost:8080/setup`, pega el token y crea la GitHub App (en tu cuenta personal o en una organización). Después instálala en los repositorios y habilítalos desde el panel. La guía completa está en [docs/install.md](docs/install.md).

Si no tienes una URL pública, consulta la sección [modo solo polling](docs/install.md#modo-solo-polling-sin-url-pública).

Los backups programados y la restauración verificada están en [docs/backup.md](docs/backup.md).

## Variables de entorno

| Variable | Descripción | Valor por defecto |
|---|---|---|
| `QA_DATABASE_URL` | Cadena de conexión a PostgreSQL (obligatoria) | — |
| `QA_PUBLIC_URL` | URL pública sin `/` final (obligatoria) | — |
| `QA_MASTER_KEY` | Base64 de 32 bytes para cifrar la configuración de la GitHub App (obligatoria en `app`; `openssl rand -base64 32`) | — |
| `QA_WORKER_KEY` | Base64 de 32 bytes para cifrar API keys y tokens de clonado (obligatoria en `app` y `worker`; clave distinta de `QA_MASTER_KEY`) | — |
| `QA_LISTEN_ADDR` | Dirección de escucha de `qa serve` | `:8080` |
| `QA_WEBHOOKS_ENABLED` | Habilita los webhooks de GitHub | `true` |
| `QA_RECONCILE_INTERVAL` | Intervalo de reconciliación (duration de Go) | `5m` |
| `QA_GITHUB_URL` | Base web de GitHub | `https://github.com` |
| `QA_GITHUB_API_URL` | Base de la API de GitHub | `https://api.github.com` |
| `POSTGRES_PASSWORD` | Contraseña del contenedor `postgres` | — |

Ajustes opcionales del worker: `QA_REVIEW_CONCURRENCY` (`2`), `QA_REVIEW_AGENT_STEPS` (`40`, iteraciones del agente por pasada), `QA_REVIEW_TIMEOUT` (`20m`, presupuesto total del run), `QA_REVIEW_STALE_AFTER` (`45m`), `QA_WORKSPACE_DIR` (`/tmp/qa`), `QA_OCR_BIN` (`ocr`), `QA_OPENCODE_BIN` (`opencode`), `QA_FETCH_DEPTH_MAX` (`500`) y `QA_GIT_BASE_URL` (`https://github.com`).

`QA_REVIEW_STALE_AFTER` es el umbral del vigilante de runs huérfanos: al arrancar y cada 5 minutos, los runs en `running` cuya hora de inicio supera ese umbral pasan a `failed` con código `interrupted`, se publica el check como `action_required` y el PR queda `inconclusive`. Debe superar `QA_REVIEW_TIMEOUT` (más el margen de reintentos), o matará revisiones sanas.

## Revisión automática (Fase 2)

Se revisa un PR cuando no es borrador ni de un fork, su repositorio está habilitado y tiene equipo asignado, y la política de ese equipo apunta a un perfil LLM activo. Los eventos `opened`, `synchronize`, `reopened` y `ready_for_review` y la reconciliación encolan la revisión del SHA actual; un push nuevo marca `superseded` el run anterior.

### Perfiles LLM

En **Agentes LLM** (solo admins), «Nuevo perfil» permite dar nombre, proveedor, modelo, concurrencia (1 por defecto), activación y clave API. La clave es write-only: se cifra con `QA_WORKER_KEY` y nunca se devuelve ni se muestra. «Probar» encola una comprobación de salud en el worker, que ejecuta el mismo `opencode` con un prompt mínimo; la tabla muestra `Saludable` o `Con error` con el motivo. La respuesta es asíncrona (202): vuelve a cargar la pantalla para ver el resultado final.

Para asignar el perfil, en **Política por equipo** elige el equipo y el perfil y pulsa «Guardar política». Esa política es la que usa el worker para cada PR del equipo; el perfil debe estar activo para que se generen runs.

### Artefactos tras la revisión

En el PR aparecen:

- **Un comentario resumen** por PR (marcador `<!-- qa-control-center:summary -->`): número de revisión, SHA corto, conteo por severidad, resueltos desde la revisión anterior, resumen, cobertura y enlace al panel. Se edita en cada run; un push no crea un comentario nuevo.
- **Una review** con comentarios en línea solo para hallazgos nuevos cuya línea cae en el diff. Si la sugerencia cubre solo líneas añadidas se emite un bloque `suggestion`. El evento de la review es siempre `COMMENT`: no aprueba ni pide cambios por sí mismo.
- **Un check run `QA Control Center`** por SHA: `success` sin hallazgos a partir del umbral, `failure` si los hay (umbral `major` por defecto), `action_required` si el run es `inconclusive` o `failed`, y `cancelled` si el run queda `superseded`.
- **El estado del PR en el panel**: `ready_for_human` («Listo para ti») o `changes_requested` («Con defectos») según los hallazgos abiertos; `inconclusive` («Inconcluso») cuando la revisión no terminó con cobertura completa.

En `/prs/:id` se consultan los runs, el motivo del error, la cobertura y los hallazgos por severidad. Tras push de una corrección, el resumen se edita, el hallazgo pasa a `resolved` y su hilo se cierra; esto último solo ocurre en runs `succeeded` con cobertura completa.

### `inconclusive` y reintento

`inconclusive` significa que la revisión no pudo completarse con cobertura completa: fallo de `ocr` o JSON inválido, error del LLM (cuota, timeout, 5xx), salida que no valida el esquema, cobertura incompleta, token de clonado caducado, timeout o presupuesto agotado (`budget_exhausted`, con la pasada y los archivos pendientes en el mensaje). **Nunca resuelve hallazgos**: puede publicar los hallazgos de pasadas completas como comentarios nuevos, pero no cierra hilos. El check queda `action_required` y el PR pasa a `inconclusive`.

Para reintentar, abre el detalle del PR y pulsa **Reintentar** (admin o lead del equipo). Se crea un run con trigger `retry` para el mismo SHA, se reactiva el check y se encola de nuevo. Comprueba antes la causa (por ejemplo, un modelo inválido en el perfil): la reconciliación no reencola automáticamente un SHA que ya terminó en `failed` o `inconclusive`.

## Desarrollo

```sh
make web        # compila la SPA y la copia a internal/web/dist
make build      # go build -o bin/qa ./cmd/qa
make test       # go test ./... + Vitest
```

El binario admite `qa serve`, `qa worker` y `qa migrate`.

## Rutas principales

`GET /healthz` · `GET /api/setup/status` · `POST /api/setup/manifest` · `/setup` · `/auth/login` · `/auth/callback` · `/api/me` · `/api/stats` · `/api/prs` · `GET /api/prs/{id}` · `POST /api/prs/{id}/retry` · `/api/teams` · `GET|PUT /api/teams/{id}/policy` · `GET|POST /api/llm-profiles` · `POST /api/llm-profiles/{id}/health` · `/agents` · `/prs/:id` · `GET /api/events` (SSE) · `POST /webhooks/github`. Cualquier otra ruta sirve la SPA.

## Hojas de ruta

- **Fase 1 (este repo):** binario único, GitHub App por manifiesto, login, equipos y repos, Dashboard en vivo.
- **Fase 2 (este repo):** motor de revisión con `ocr` y `opencode`, perfiles LLM, hallazgos con fingerprints y publicación de comentarios, reviews y checks.
- **Fase 3:** Quality Gate con CI, disputas y comandos `/qa`, PRs de forks y override de política por repositorio.
- **Fase 4:** operación, analítica, auditoría y sincronización con GitHub Teams.

## Licencia

[MIT](LICENSE).
