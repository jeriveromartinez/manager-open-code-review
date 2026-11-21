# Configuración en la base de datos y setup sin token

**Fecha:** 2026-11-20
**Estado:** aprobado

## Problema

`deploy/.env` acumula 14 variables: cadena de conexión, URL pública, dos claves
maestras, y doce ajustes de comportamiento (intervalos, concurrencia, timeouts,
rutas de binarios, alertas, retención de backups). Cambiar cualquiera de los
doce exige editar un archivo en el servidor y recrear contenedores.

El setup pide además un `setup_token` que el operador debe copiar de los logs,
más el nombre de la organización. Dos campos para un único dato útil.

## Objetivo

1. `.env` queda en lo imprescindible para arrancar: conexión y claves raíz.
2. El resto se configura desde la web y se guarda en PostgreSQL.
3. El setup pide solo el nombre de la App.

## Decisión: qué no puede salir del `.env`

`QA_MASTER_KEY` y `QA_WORKER_KEY` son raíces de confianza: descifran los
secretos que ya viven en la base (credenciales de la GitHub App, API keys de
perfiles LLM, tokens de clonado). Guardarlas en la misma base que cifran no
protege de nada y crea una dependencia circular.

`QA_DATABASE_URL` y `POSTGRES_PASSWORD` son la conexión; sin ellas no hay
arranque posible.

Por tanto `.env` queda en cuatro variables:

```sh
QA_DATABASE_URL=postgres://postgres:<pass>@postgres:5432/qa_control_center?sslmode=disable
POSTGRES_PASSWORD=<pass>
QA_MASTER_KEY=<32 bytes base64>
QA_WORKER_KEY=<32 bytes base64>
```

## Modelo de datos

```sql
CREATE TABLE app_settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL
);
```

Clave/valor sin tipos en SQL. El tipado y la validación viven en Go, en el
paquete `internal/settings`, que es también la única fuente de los defaults.
Una clave nueva no necesita migración.

`value` se guarda siempre como texto normalizado (duraciones en el formato de
`time.ParseDuration`, booleanos `true`/`false`, enteros en base 10), de modo que
la tabla sea legible y el panel pueda mostrar lo mismo que se guardó.

### Ajustes y sus defaults

Los defaults son exactamente los actuales, así que una instalación que no toque
nada se comporta igual que hoy.

| Grupo | Clave | Default |
|---|---|---|
| General | `public_url` | vacío hasta el setup; se detecta de la petición |
| General | `webhooks_enabled` | `true` |
| General | `reconcile_interval` | `5m` |
| General | `github_url` | `https://github.com` |
| General | `github_api_url` | `https://api.github.com` |
| General | `listen_addr` | `:8080` |
| Revisión | `review_concurrency` | `2` |
| Revisión | `review_agent_steps` | `40` |
| Revisión | `review_timeout` | `20m` |
| Revisión | `review_stale_after` | `45m` |
| Revisión | `llm_health_interval` | `30m` |
| Revisión | `fetch_depth_max` | `500` |
| Revisión | `git_base_url` | `https://github.com` |
| Worker | `workspace_dir` | `/tmp/qa` |
| Worker | `ocr_bin` | `ocr` |
| Worker | `opencode_bin` | `opencode` |
| Worker | `worker_health_addr` | `:9090` |
| Worker | `worker_queue_stale_after` | `10m` |
| Alertas | `worker_health_url` | `http://worker:9090` |
| Alertas | `monitor_interval` | `1m` |
| Alertas | `monitor_renotify_after` | `30m` |
| Alertas | `alert_webhook_url` | vacío (solo log) |
| Backups | `backup_retention` | `7` |

Validaciones, las mismas que hoy: duraciones > 0, enteros > 0, URLs absolutas
`http(s)` sin `/` final, `monitor_renotify_after >= monitor_interval`. Se añade
una que hoy solo estaba en la documentación: `review_stale_after` debe superar
`review_timeout`, o el watchdog mataría revisiones sanas.

## Carga

`config.Load` lee solo las cuatro variables y rellena el resto con los defaults.
Después de abrir el store, `runServe` y `runWorker` cargan `app_settings` y
aplican los valores sobre el `config.Config` ya existente:

```go
cfg, _ := config.Load(os.Getenv)   // esenciales + defaults
st, _ := store.Open(ctx, cfg.DatabaseURL)
st.Migrate(ctx)
settings.Load(ctx, st).Apply(&cfg) // sobrescribe con lo guardado
```

Esto mantiene intactos todos los call sites (`cfg.ReviewTimeout`,
`cfg.PublicURL`, …): cambia el origen del valor, no el tipo ni el nombre.

### Compatibilidad

El entorno deja de leerse para esos doce ajustes: gana la BD, y si falta la
fila, el default. Al arrancar, cada variable legada presente produce un
`slog.Warn` que dice que se ignora y que el valor se configura en la web. No se
importa ninguno: una instalación con `QA_REVIEW_TIMEOUT=40m` vuelve a `20m`, y
el aviso es la única señal, tal y como se decidió.

### Perfiles opcionales

`qa monitor` y `backup.sh` también leen de la BD.

- **Monitor**: resuelve su configuración al arrancar. Si la BD no responde en
  ese momento, usa los defaults de `internal/settings` y registra un aviso, en
  vez de no arrancar: su propósito es avisar de que algo se cayó, y negarse a
  arrancar cuando la BD está caída lo inutiliza justo entonces. Una vez
  arrancado no vuelve a consultar la BD, así que un fallo posterior de la base
  no lo detiene.
- **Backups**: `backup.sh` lee `backup_retention` con `psql` al empezar. Si no
  hay fila o la consulta falla, usa `7`. El script ya necesita la base para
  hacer `pg_dump`, así que no añade una dependencia nueva.

## API

`GET /api/settings` y `PUT /api/settings`, solo administradores.

`GET` devuelve los valores guardados, los defaults y, por clave, si el cambio
requiere reiniciar la app o el worker y el rango válido. `PUT` valida cada valor
enviado, escribe solo las claves presentes y devuelve el estado resultante. Un
valor inválido responde 400 con la clave culpable, nunca un 500.

`PUT` es un reemplazo parcial a propósito: el panel envía solo el grupo que se
guardó, de modo que dos administradores editando grupos distintos no se pisan.

## Panel

`SettingsView.vue` nueva, en la navegación principal, solo visible para
administradores. Cinco secciones (General, Revisión, Worker, Alertas, Backups) y
campos explícitos, no un formulario generado: se lee igual que el resto del
panel y el texto de ayuda cabe donde hace falta.

Cada sección lleva la marca de qué hay que reiniciar para que el cambio surta
efecto: «requiere reiniciar app», «requiere reiniciar worker» o ninguna. Al
guardar se muestra el resultado y, si hay algo pendiente de reinicio, se dice
explícitamente en lugar de fingir que ya está aplicado.

## Setup sin token

- El formulario pasa a un único campo: **nombre de la App**. Viaja como
  `name` en el cuerpo de `POST /api/setup/manifest` y se usa tal cual en el
  manifiesto de GitHub.
- La App se registra en la cuenta personal de quien hace el setup. GitHub
  permite instalar después esa App en cualquier organización donde el usuario
  tenga permiso, así que desaparece el campo de organización sin perder la
  capacidad.
- La URL pública se detecta de la petición en el momento de crear el
  manifiesto: esquema desde `X-Forwarded-Proto` (o TLS directo) y host desde
  `X-Forwarded-Host` (o `Host`). Se guarda en `app_settings.public_url` para que
  el callback, el hook y las redirecciones de OAuth apunten al sitio correcto
  aunque el `.env` ya no la tenga.

### Seguridad

Sin token, cualquiera que alcance una instancia **aún sin configurar** puede
crear una App y quedarse con ella. Los endpoints siguen devolviendo 409 en
cuanto existe configuración, así que la ventana se abre solo antes del setup.

`QA_SETUP_TOKEN` se mantiene como endurecimiento **opcional**: si está definido,
se exige como hoy; si no, el formulario no lo pide. No aparece en
`.env.example`; se documenta en el README junto a la recomendación de terminar
el setup antes de exponer la instancia a internet.

## Fuera de alcance

- Recarga en caliente: los ajustes se aplican al reiniciar, y el panel lo dice.
- Editar desde la web las dos claves raíz o la cadena de conexión.
- Historial de cambios en `app_settings` más allá de `updated_by`/`updated_at`.
- Migrar automáticamente los valores del `.env`; se decidió que gane la BD.

## Pruebas

- **store**: ida y vuelta de valores, fila ausente → defaults, `updated_by`
  registrado, escritura parcial que no borra las demás claves.
- **settings**: parseo de cada tipo, límites (`<= 0`, URL no absoluta,
  `renotify < interval`) rechazados con la clave en el error, `Apply` sobre un
  `config.Config`.
- **API**: 401 sin sesión, 403 sin ser admin, 400 con la clave culpable, 200 y
  persistencia, `GET` que refleja lo guardado.
- **UI**: carga de valores, guardado del grupo, marca de reinicio visible, error
  de validación mostrado sin perder lo tecleado.
- **setup**: nombre obligatorio (400 si falta), sin token por defecto, token
  exigido cuando la variable está definida, URL pública detectada con y sin
  `X-Forwarded-Proto`, 409 cuando ya hay configuración.

Cada una se escribe antes del cambio y se comprueba que falla.
