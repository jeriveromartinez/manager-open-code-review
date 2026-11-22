# QA Control Center

Revisor automático de pull requests, auto-hospedado. Crea su propia GitHub App desde un manifiesto, recibe webhooks (o funciona solo con polling), encola la sincronización de PRs en PostgreSQL y muestra el resultado en un panel Vue en vivo. Una misma instancia puede gestionar repositorios de varias cuentas personales u organizaciones de GitHub.

## Arquitectura

Se despliega con **una sola imagen** en tres contenedores, más un cuarto opcional con las alertas:

| Servicio | Proceso | Función |
|---|---|---|
| `app` | `qa serve` | API REST + SSE, webhooks, web Vue embebida con `go:embed`, scheduler de reconciliación, disparo de revisiones y publicación en GitHub |
| `worker` | `qa worker` | Cola `review`: clona el PR, ejecuta `ocr` + `opencode` y guarda el resultado |
| `monitor` | `qa monitor` | Opcional (perfil `monitor`): sondea `/healthz` y `/metrics` del worker y avisa por log y webhook |
| `postgres` | `postgres:17` | Estado, cola River y eventos `LISTEN/NOTIFY` |

El Dockerfile produce dos targets: `app` (mínimo) y `worker` (con `git`, Node 24, `ocr` 1.13.13 y `opencode` 1.19.0). `app` publica en GitHub con la GitHub App; el worker nunca tiene su clave privada ni tokens de escritura, y clona con un token de solo lectura. `QA_MASTER_KEY` cifra la configuración de la App y la usa solo `app`; `QA_WORKER_KEY` cifra las API keys y los tokens de clonado, y la comparten `app` y `worker`.

## Inicio rápido

```sh
cp deploy/.env.example deploy/.env
# rellena las dos claves con valores distintos:
openssl rand -base64 32   # QA_MASTER_KEY (solo app)
openssl rand -base64 32   # QA_WORKER_KEY (app y worker)
docker compose -f deploy/docker-compose.yml up -d --build
```

Abre `http://localhost:8080/setup`, escribe el **nombre de la App** y crea la GitHub App; se registra en tu cuenta personal, y después puedes instalarla en cualquier organización donde tengas permiso. En el mismo paso se detecta la URL pública desde la petición y se guarda en la base. Tras instalar la App en los repositorios, habilítalos desde el panel. La guía completa está en [docs/install.md](docs/install.md).

Si una cuenta nueva no aparece al instante, en **Repositorios** los administradores tienen **Sincronizar** por cuenta, **Sincronizar todo** y **Actualizar**. La sincronización recupera la instalación desde GitHub aunque el webhook `installation.created` llegue tarde o se pierda, así que no hace falta esperar al siguiente ciclo de reconciliación.

`QA_SETUP_TOKEN` es un endurecimiento opcional: si lo defines en `deploy/.env`, el setup exige ese valor; si no, el formulario solo pide el nombre. Termina el setup antes de exponer la instancia a internet: hasta que exista configuración, la ventana de setup sigue abierta. Si la URL detectada no es la correcta, corrígela en **Ajustes → General** (eso arregla los enlaces del panel y los redirects de OAuth del panel) y actualiza también `redirect_url`, `callback_urls` y `setup_url` en la GitHub App desde GitHub, o repite el setup desde la URL correcta antes de configurarla.

Si no tienes una URL pública, consulta la sección [modo solo polling](docs/install.md#modo-solo-polling-sin-url-pública).

Los backups programados y la restauración verificada están en [docs/backup.md](docs/backup.md).

## Variables de entorno

| Variable | Descripción | Valor por defecto |
|---|---|---|
| `QA_DATABASE_URL` | Cadena de conexión a PostgreSQL (obligatoria) | — |
| `QA_MASTER_KEY` | Base64 de 32 bytes para cifrar la configuración de la GitHub App (obligatoria en `app`; `openssl rand -base64 32`) | — |
| `QA_WORKER_KEY` | Base64 de 32 bytes para cifrar API keys y tokens de clonado (obligatoria en `app` y `worker`; clave distinta de `QA_MASTER_KEY`) | — |
| `QA_SETUP_TOKEN` | Opcional: si se define, el asistente de `/setup` exige ese valor para crear la App | vacío (setup abierto) |

La URL pública no se declara en el entorno: el asistente de `/setup` la detecta de la petición y la guarda en `app_settings`, y se puede corregir en **Ajustes → General**.

`POSTGRES_PASSWORD` no la lee la app: solo configura el contenedor `postgres` y debe coincidir con la contraseña de `QA_DATABASE_URL`.

Todo lo demás —escucha, reconciliación, revisión, worker, alertas y backups— se edita en el panel, en **Ajustes** (solo administradores). La base manda sobre el entorno: las variables retiradas se ignoran y el arranque avisa de las que sigan definidas. Cada sección del panel indica si el cambio necesita reiniciar la app, el worker o el monitor.

`listen_addr` y `worker_health_addr` deben seguir coincidiendo con los `healthcheck` de [deploy/docker-compose.yml](deploy/docker-compose.yml): el worker solo arranca cuando el healthcheck de `app` (que sondea `127.0.0.1:8080`) está sano, y el suyo sondea `127.0.0.1:9090`. Si cambias una dirección sin actualizar el compose, el contenedor queda unhealthy y, con `listen_addr`, el worker y el monitor no llegan a arrancar.

`llm_health_interval` es la cadencia del barrido de salud de los perfiles LLM: al arrancar el worker y cada intervalo, encola una comprobación para cada perfil activo cuyo último resultado supera dos veces ese intervalo, y para los que nunca se comprobaron. Un perfil en error espera `15m` antes de reintentarse, para no gastar cuota en una clave rota. `GET /api/llm-profiles` devuelve `health_age_seconds` y `health_stale` (marcado con el mismo umbral de dos intervalos), de modo que un barrido detenido se ve en el panel en lugar de parecer un perfil sano.

`review_stale_after` es el umbral del vigilante de runs huérfanos: al arrancar y cada 5 minutos, los runs en `running` cuya hora de inicio supera ese umbral pasan a `failed` con código `interrupted`, se publica el check como `action_required` y el PR queda `inconclusive`. Debe superar `review_timeout` (más el margen de reintentos), o matará revisiones sanas.

El worker expone salud y métricas en `worker_health_addr` (`:9090` por defecto):

| Ruta | Contenido |
|---|---|
| `GET /healthz` | Estado del proceso, `uptime_seconds`, ping a Postgres y versión de `ocr` y `opencode`. Devuelve **503** y `status: degraded` si la base o cualquiera de los binarios falla. |
| `GET /metrics` | Cola de River (`available`, `running`, `scheduled`, `discarded`, `oldest_available_seconds`), runs de la última hora por estado, percentil 95 de duración y `stalled`. |

`stalled` es `true` cuando hay jobs esperando, ninguno en ejecución y el más antiguo lleva más de `worker_queue_stale_after` (`10m`) esperando: la cola no avanza porque el worker no consume. Una cola con trabajo reciente no se marca, para que un pico legítimo no dispare alertas.

### Alertas: `qa monitor`

Un worker caído no puede avisar de que está caído, así que las alertas viven fuera de él. `qa monitor` sondea `/healthz` y `/metrics`, compara con el ciclo anterior y avisa solo en las transiciones. Es un contenedor más, opcional, bajo el perfil `monitor`:

```sh
docker compose -f deploy/docker-compose.yml --profile monitor up -d
```

No necesita claves, así que se levanta con la misma imagen `qa-control-center:local` sin `QA_MASTER_KEY` ni `QA_WORKER_KEY`. Al arrancar lee su configuración de la base de datos, como el resto de procesos; si la base no responde o los valores guardados no validan, usa los defaults y registra un aviso en lugar de no arrancar: un monitor que se niega a arrancar cuando PostgreSQL está caído no puede avisar de que PostgreSQL está caído. Una vez arrancado no vuelve a consultar la base.

Las cuatro claves se editan en **Ajustes → Alertas** y un cambio pide reiniciar el monitor:

| Ajuste | Descripción | Valor por defecto |
|---|---|---|
| `worker_health_url` | Base del endpoint de salud del worker, sin ruta final | `http://worker:9090` |
| `monitor_interval` | Cadencia de sondeo (debe ser > 0) | `1m` |
| `monitor_renotify_after` | Cada cuánto se recuerda un problema que sigue abierto (>= `monitor_interval`) | `30m` |
| `alert_webhook_url` | Endpoint `POST` opcional para las alertas; vacío = solo log | — |

Cada alerta se escribe siempre en el log JSON; si además hay `alert_webhook_url`, se envía un `POST`.

Códigos estables de alerta:

| Código | Cuándo |
|---|---|
| `worker_unreachable` | Conexión rechazada, timeout, o `/healthz` no devuelve el JSON del contrato |
| `worker_degraded` | `/healthz` responde 503, `status != "ok"`, o `/metrics` no se puede leer |
| `database_down` | El ping a PostgreSQL del worker falla; incluye el error observado |
| `dependency_down` | Un binario del worker (`ocr`, `opencode`) no responde; el `subject` lleva su nombre |
| `queue_stalled` | `/metrics` devuelve `stalled: true`; el mensaje lleva los conteos observados |

Un problema nuevo avisa una vez; si persiste, se recuerda cada `monitor_renotify_after`; cuando desaparece se emite **una** recuperación. Un problema que se resuelve y vuelve a aparecer cuenta como nuevo. Un webhook que responde 5xx se registra como error y el bucle sigue: la alerta siguiente se entrega igual.

El cuerpo del webhook es un JSON pequeño y estable, sin envoltorio de ningún proveedor:

```json
{
  "severity": "warning",
  "code": "queue_stalled",
  "message": "review queue stalled: 5 waiting jobs, 0 running, oldest waiting 3600s",
  "values": { "available": 5, "running": 0, "scheduled": 0, "discarded": 0, "oldest_available_seconds": 3600 },
  "timestamp": "2026-11-20T10:00:00Z"
}
```

`severity` es `warning` al abrir o recordar un problema y `recovered` al cerrarlo; `code` es el mismo en la recuperación, así un receptor puede cerrar el incidente por código sin interpretar el texto.

`app` y `worker` tienen `healthcheck` en compose, de modo que `docker compose ps` refleja la realidad. La imagen del worker es `node:24-trixie-slim` (sin `curl` ni `wget`), así que usa `node -e 'fetch(...)'`; la de `app` es `debian:bookworm-slim` con solo `ca-certificates` y `git`, así que el propio binario hace de cliente con `qa healthcheck` (0 si `/healthz` responde 200, 1 en cualquier otro caso).

`GET /healthz` de la app refleja el estado de PostgreSQL: devuelve 200 con `{"status":"ok"}` solo si la base responde, y 503 con `{"status":"degraded"}` cuando no. Si el contenedor de PostgreSQL desaparece (no solo se para), los datos siguen en el volumen y el procedimiento de recuperación está en [Si PostgreSQL desaparece](docs/install.md#si-postgresql-desaparece).

El check `QA Control Center` en `in_progress` más de `review_stale_after` lo cubre el watchdog, no este monitor; conviene vigilarlo desde fuera.

## Revisión automática (Fase 2)

Se revisa un PR cuando no es borrador ni de un fork, su repositorio está habilitado y tiene equipo asignado, y la política de ese equipo apunta a un perfil LLM activo. Los eventos `opened`, `synchronize`, `reopened` y `ready_for_review` y la reconciliación encolan la revisión del SHA actual; un push nuevo marca `superseded` el run anterior.

El Dashboard avisa a los administradores de los eslabones que faltan para que se ejecute una revisión —sin equipos, sin perfiles LLM, repositorios habilitados sin equipo, equipos sin política y perfiles sin salud reciente— y cada aviso enlaza con la pantalla donde se resuelve. Un PR en `pending` indica además si la revisión está programada o bloqueada, y en ese caso el motivo concreto (borrador, fork, sin equipo, sin política o perfil deshabilitado).

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

### Reinicio y despliegue del worker

Un `docker compose up -d` o un `docker stop` del worker inicia un apagado ordenado: deja de reclamar runs nuevos, espera hasta 45 s (`jobs.ReviewDrainTimeout`) a que terminen las revisiones en curso y, si alguna no termina, cancela el trabajo y la devuelve a la cola antes de salir. El run vuelve a `queued` con código `interrupted` («el worker se detuvo durante la revisión; el run vuelve a la cola») y el worker nuevo lo retoma al arrancar, sin esperar los 45 min del watchdog ni tocar el PR. Los tokens consumidos por el intento interrumpido no se recuperan: esa revisión se repite desde el principio.

`deploy/docker-compose.yml` fija `stop_grace_period: 90s` en el worker: el margen por defecto de Docker (10 s) manda `SIGKILL` antes de que el apagado ordenado llegue a completarse. Si despliegas el worker fuera de compose, dale al proceso ese mismo margen entre `SIGTERM` y `SIGKILL`.

## Desarrollo

```sh
make web        # compila la SPA y la copia a internal/web/dist
make build      # go build -o bin/qa ./cmd/qa
make test       # go test ./... + Vitest
go test ./internal/review -run '^TestE2EFlow$' -count=1   # flujo E2E contra dobles (docs/e2e.md)
```

Los pull requests usan la plantilla de `.github/pull_request_template.md`. Conserva la línea `Closes #N`: GitHub cierra ese issue automáticamente al fusionar.

El binario admite `qa serve`, `qa worker`, `qa migrate`, `qa monitor` y `qa healthcheck [url]`.

## Rutas principales

`GET /healthz` · `GET /api/setup/status` · `POST /api/setup/manifest` · `/setup` · `/auth/login` · `/auth/callback` · `/api/me` · `/api/stats` · `/api/prs` · `GET /api/prs/{id}` · `POST /api/prs/{id}/retry` · `/api/teams` · `GET|PUT /api/teams/{id}/policy` · `GET|POST /api/llm-profiles` · `POST /api/llm-profiles/{id}/health` · `GET /api/installations` · `POST /api/installations/{id}/sync` · `/agents` · `/prs/:id` · `GET /api/events` (SSE) · `POST /webhooks/github`. Cualquier otra ruta sirve la SPA.

## Hojas de ruta

- **Fase 1 (este repo):** binario único, GitHub App por manifiesto, login, equipos y repos, Dashboard en vivo.
- **Fase 2 (este repo):** motor de revisión con `ocr` y `opencode`, perfiles LLM, hallazgos con fingerprints y publicación de comentarios, reviews y checks.
- **Fase 3:** Quality Gate con CI, disputas y comandos `/qa`, PRs de forks y override de política por repositorio.
- **Fase 4:** operación, analítica, auditoría y sincronización con GitHub Teams.

## Licencia

[MIT](LICENSE).
