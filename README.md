# QA Control Center

Revisor automático de pull requests, auto-hospedado. Crea su propia GitHub App desde un manifiesto, recibe webhooks (o funciona solo con polling), encola la sincronización de PRs en PostgreSQL y muestra el resultado en un panel Vue en vivo.

## Arquitectura (Fase 1 — núcleo)

Se despliega con **una sola imagen** en tres contenedores:

| Servicio | Proceso | Función |
|---|---|---|
| `app` | `qa serve` | API REST + SSE, webhooks, web Vue embebida con `go:embed` y scheduler de reconciliación |
| `worker` | `qa worker` | Arranca inactivo; la cola `review` llega en Fase 2 |
| `postgres` | `postgres:17` | Estado, cola River y eventos `LISTEN/NOTIFY` |

## Inicio rápido

```sh
cp deploy/.env.example deploy/.env
# rellena QA_MASTER_KEY con: openssl rand -base64 32
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs app | grep setup_token
```

Abre `http://localhost:8080/setup`, pega el token y crea la GitHub App (en tu cuenta personal o en una organización). Después instálala en los repositorios y habilítalos desde el panel. La guía completa está en [docs/install.md](docs/install.md).

Si no tienes una URL pública, consulta la sección [modo solo polling](docs/install.md#modo-solo-polling-sin-url-pública).

## Variables de entorno

| Variable | Descripción | Valor por defecto |
|---|---|---|
| `QA_DATABASE_URL` | Cadena de conexión a PostgreSQL (obligatoria) | — |
| `QA_PUBLIC_URL` | URL pública sin `/` final (obligatoria) | — |
| `QA_MASTER_KEY` | Base64 de 32 bytes para cifrar credenciales (obligatoria; `openssl rand -base64 32`) | — |
| `QA_LISTEN_ADDR` | Dirección de escucha de `qa serve` | `:8080` |
| `QA_WEBHOOKS_ENABLED` | Habilita los webhooks de GitHub | `true` |
| `QA_RECONCILE_INTERVAL` | Intervalo de reconciliación (duration de Go) | `5m` |
| `QA_GITHUB_URL` | Base web de GitHub | `https://github.com` |
| `QA_GITHUB_API_URL` | Base de la API de GitHub | `https://api.github.com` |
| `POSTGRES_PASSWORD` | Contraseña del contenedor `postgres` | — |

## Desarrollo

```sh
make web        # compila la SPA y la copia a internal/web/dist
make build      # go build -o bin/qa ./cmd/qa
make test       # go test ./... + Vitest
```

El binario admite `qa serve`, `qa worker` y `qa migrate`.

## Rutas principales

`GET /healthz` · `GET /api/setup/status` · `POST /api/setup/manifest` · `/setup` · `/auth/login` · `/auth/callback` · `/api/me` · `/api/stats` · `/api/prs` · `/api/teams` · `GET /api/events` (SSE) · `POST /webhooks/github`. Cualquier otra ruta sirve la SPA.

## Hojas de ruta

- **Fase 1 (este repo):** binario único, GitHub App por manifiesto, login, equipos y repos, Dashboard en vivo.
- **Fase 2:** motor de revisión con `ocr` y `opencode`, perfiles LLM, hallazgos con fingerprints y publicación de comentarios/checks.
- **Fase 3:** Quality Gate, disputas, comandos `/qa` y políticas QA.
- **Fase 4:** operación, analítica, auditoría y sincronización con GitHub Teams.

## Licencia

[MIT](LICENSE).
