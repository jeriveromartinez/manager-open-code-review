# Instalación

QA Control Center se entrega como **una sola imagen** con dos roles (`serve` y `worker`) junto a PostgreSQL. La instalación inicial tarda unos 15 minutos.

## Requisitos

- Docker con Compose v2 (`docker compose version`).
- OpenSSL, para generar la clave maestra y la clave del worker.
- Permisos de administrador sobre la cuenta personal o la organización de GitHub donde se creará la GitHub App.

## 1. Preparar el entorno

```sh
git clone <url-del-repositorio>
cd qa-control-center
cp deploy/.env.example deploy/.env
```

Genera dos claves distintas y pégalas en `deploy/.env`:

```sh
openssl rand -base64 32   # QA_MASTER_KEY
openssl rand -base64 32   # QA_WORKER_KEY
```

`QA_MASTER_KEY` cifra la configuración de la GitHub App y la usa solo el servicio `app`. `QA_WORKER_KEY` cifra las API keys de los perfiles LLM y los tokens de clonado, y la comparten `app` y `worker`. Ambas deben decodificar exactamente a 32 bytes; si falta alguna, `serve` o `worker` se detienen al arrancar con un mensaje que indica cómo generarla.

Revisa también estos valores de `deploy/.env`:

- `POSTGRES_PASSWORD`: contraseña de PostgreSQL; debe repetirse en `QA_DATABASE_URL`.
- `QA_PUBLIC_URL`: URL pública de la app **sin `/` final** (por ejemplo `https://qa.example.com`). Si no hay entrada pública, consulta el modo solo polling más abajo.
- `QA_WEBHOOKS_ENABLED` y `QA_RECONCILE_INTERVAL`: ajusta según el modo de operación.

`QA_DATABASE_URL` ya apunta al servicio `postgres` de Compose; no hace falta cambiar el host.

## 2. Levantar el stack

```sh
docker compose -f deploy/docker-compose.yml up -d --build
```

El primer arranque construye la web y el binario con los targets `app` y `worker` de `deploy/Dockerfile`, aplica las migraciones y levanta ambos servicios. El `worker` incluye `git`, Node 24 y las herramientas fijadas `@alibaba-group/open-code-review@1.13.13` (`ocr`) y `opencode-ai@1.19.0` (`opencode`), y valida que estén disponibles al arrancar. Comprueba que la API responde:

```sh
curl -s localhost:8080/healthz
# {"status":"ok"}
```

## 3. Crear la GitHub App

Lee el token de setup de los logs del contenedor `app` (aparece una vez, mientras la app no esté configurada):

```sh
docker compose -f deploy/docker-compose.yml logs app | grep setup_token
```

Abre `http://localhost:8080/setup` (o `$QA_PUBLIC_URL/setup`), pega el token y elige la creación en tu cuenta **personal** o en una **organización**. GitHub mostrará el manifiesto de la App; confirma la creación y, a continuación, instala la App en los repositorios que quieras revisar.

## 4. Poner en marcha el panel

Entra con el login de GitHub y, desde la interfaz:

1. Habilita los repositorios que se van a revisar (solo se procesan los activos).
2. Crea equipos y asigna roles a sus miembros.
3. Como admin, abre **Agentes LLM** y pulsa «Nuevo perfil»: nombre, proveedor, modelo, concurrencia (1 por defecto), activación y clave API. La clave se guarda cifrada con `QA_WORKER_KEY` y nunca se muestra de nuevo; dejar el campo vacío al editar conserva la actual.
4. Pulsa «Probar» en el perfil. La comprobación se encola en el worker, usa el mismo `opencode` con un prompt mínimo y actualiza la columna Salud a `Saludable` o `Con error` (con el motivo). El botón devuelve 202 y la comprobación continúa en segundo plano: vuelve a cargar **Agentes LLM** para ver el resultado final.
5. En **Política por equipo**, selecciona el equipo y el perfil y guarda. Solo se revisan los PRs de repositorios habilitados con equipo y política con un perfil activo; se excluyen borradores y PRs de forks.

Los pull requests abiertos aparecen en el Dashboard. La reconciliación periódica incorpora los que llegaron sin webhook.

### Eventos de webhook y qué queda fuera en modo polling

La App creada desde el manifiesto se suscribe a estos eventos. Sin webhooks solo funcionan las acciones marcadas como «también en polling»:

| Evento | Para qué sirve | ¿Funciona en modo polling? |
| --- | --- | --- |
| `installation`, `installation_repositories` | Alta, baja, suspensión y repositorios de la instalación | Sí: la reconciliación descubre instalaciones y repositorios nuevos en cada ciclo. |
| `pull_request` | Sincronizar el PR y lanzar la revisión de `opened`, `synchronize`, `reopened` y `ready_for_review` | Sí: la reconciliación encola los PRs abiertos sin run para su SHA. |
| `pull_request_review_thread` | Resolver o reabrir un hallazgo cuando alguien marca el hilo como resuelto o sin resolver en GitHub | No. En polling el hallazgo conserva el estado del último run. |
| `issue_comment` | Comandos `/qa retry`, `/qa dismiss` y `/qa confirm` en el PR | No. En polling, los descartes y las confirmaciones se hacen desde el panel. |
| `check_suite`, `check_run`, `status` | Reevaluar el gate cuando termina el CI | La App los recibe, pero esta fase todavía no los procesa: el gate se recalcula al publicar un run, al resolver o reabrir un hilo y al aplicar un comando `/qa`. |

La App necesita además la suscripción a `pull_request_review_thread`: una App creada antes de que este evento estuviera en el manifiesto debe añadirlo a mano en **Settings → GitHub Apps → Permissions & events**, o resolver los hilos solo se reflejará en el panel tras la siguiente revisión.

## Disputas en hilos y comandos `/qa`

Cuando alguien marca un hilo de revisión como **resuelto** en GitHub, el hallazgo correspondiente pasa a `resolved` y su check se recalcula; si alguien lo **vuelve a abrir**, el hallazgo vuelve a `open`. El enlace se hace por el comentario en línea publicado, así que solo aplica a hallazgos que llegaron a GitHub.

En un PR puedes escribir un comentario cuyo **primer** renglón sea un comando:

- `/qa retry`: encola una revisión nueva de este PR. Útil cuando el run anterior quedó `inconclusive` o `failed`.
- `/qa dismiss <motivo>`: marca los hallazgos abiertos como `withdrawn` y cierra sus hilos en GitHub. El motivo queda en la auditoría.
- `/qa confirm`: devuelve a `open` los hallazgos descartados y recalcula el check; úsalo cuando un descarte fue un error.

Solo un **admin** o el **lead del equipo** al que pertenece el repositorio pueden ejecutar comandos. El rol se resuelve con el usuario que envió el comentario contra los datos de la instalación; el comando se ignora con una respuesta corta cuando el autor no tiene permiso. Cada cambio queda en la auditoría con el login del autor.

## Revisión de un pull request

Al abrir o actualizar un PR elegible (`opened`, `synchronize`, `reopened`, `ready_for_review`) se crea un run para su SHA. Si llega un push nuevo, el run anterior queda `superseded` y se cancela su check.

En GitHub aparecen estos artefactos:

- **Comentario resumen**: uno por PR, con el marcador `<!-- qa-control-center:summary -->`. Se edita en cada revisión (número de revisión, SHA, conteo por severidad, resueltos, resumen, cobertura y enlace al panel); un push no añade un comentario nuevo.
- **Review con comentarios en línea**: una por run, solo para hallazgos nuevos cuya línea esté en el diff. Si la sugerencia cubre únicamente líneas añadidas se publica como bloque `suggestion`. Es una review `COMMENT`: no aprueba ni solicita cambios por sí misma.
- **Check `QA Control Center`**: por SHA, `success` sin hallazgos a partir del umbral de la política (`major` por defecto), `failure` si los hay, `action_required` si el run es `inconclusive` o `failed`, y `cancelled` para runs `superseded`.
- **Estado del PR**: `ready_for_human` («Listo para ti») o `changes_requested` («Con defectos») según los hallazgos abiertos; `inconclusive` («Inconcluso») si la revisión no terminó con cobertura completa.

En el panel, `/prs/:id` muestra el historial de runs (con disparador, intento y motivo de error), la cobertura y los hallazgos agrupados por severidad. Tras una corrección, el resumen se edita, el hallazgo se marca `resolved` y su hilo se cierra; esto último solo en runs `succeeded` con cobertura completa.

### `inconclusive` y reintento

Un run `inconclusive` indica que no se completó la revisión con cobertura completa: `ocr` caído o con JSON inválido, error del LLM (cuota, timeout o 5xx), salida que no valida el esquema, cobertura incompleta, token de clonado caducado, timeout del run o presupuesto agotado (`budget_exhausted`: el mensaje dice en qué pasada paró y cuántos archivos quedaron sin revisar; súbelo con `QA_REVIEW_TIMEOUT`). Puede publicar hallazgos de pasadas completas como comentarios nuevos, pero **no resuelve ningún hallazgo ni cierra hilos**. El check queda `action_required` y el PR pasa a `inconclusive`.

Para reintentar, abre el detalle del PR y pulsa **Reintentar** (admin o lead del equipo). Se crea un run `retry` para el mismo SHA, se reactiva el check y se encola de nuevo. Revisa antes la causa: un modelo inválido, por ejemplo, seguirá fallando hasta corregir el perfil. La reconciliación no reencola automáticamente un SHA que ya terminó en `failed` o `inconclusive`.

## Verificación manual de extremo a extremo

Con una API key real y un repositorio de prueba:

1. Crea el perfil LLM en **Agentes LLM** y comprueba que «Probar» devuelve `Saludable`.
2. Asigna el perfil al equipo del repo en **Política por equipo** y verifica que el repositorio está habilitado.
3. Abre un PR de prueba (ni borrador ni fork) y espera a que termine el run.
4. Verifica en GitHub el comentario resumen, la review con comentarios en línea y el check `QA Control Center`; en el panel, comprueba runs, cobertura y hallazgos en `/prs/:id`.
5. Haz push de una corrección: debe aparecer un run nuevo, el resumen debe editarse sin crear otro comentario, el hallazgo debe pasar a `resolved` y su hilo debe cerrarse.
6. Provoca un fallo con un modelo inválido y reintenta desde el detalle del PR: el check debe quedar `action_required`, el PR `Inconcluso` y no debe resolverse ningún hallazgo. Restaura el modelo al terminar.

## Añadir otra cuenta u organización

La GitHub App es pública, por lo que cualquier cuenta personal u organización puede instalarla.

> **Nota:** si la instancia se creó antes con una App **privada**, un operador debe hacer pública esa App en GitHub o recrearla desde `/setup` antes de que “Añadir cuenta” pueda instalarla en otra cuenta u organización. Recrear la App implica reinstalarla: los IDs de instalación cambian.

Desde una misma instancia puedes gestionar repositorios de varias cuentas:

1. Entra como admin → Repositorios → “Añadir cuenta”.
2. Instala la App en la otra cuenta u organización.
3. Vuelve al panel; los repositorios aparecen deshabilitados. Habilita los que quieras revisar y asígnalos a un equipo.
4. Con webhooks, la instalación entra al instante; en modo solo polling se descubre en la próxima reconciliación (≤5 min).

Los repositorios de una cuenta recién instalada quedan deshabilitados hasta que un admin los active.

## Modo solo polling (sin URL pública)

Cuando el servicio no se expone a Internet:

```dotenv
QA_PUBLIC_URL=http://localhost:8080
QA_WEBHOOKS_ENABLED=false
QA_RECONCILE_INTERVAL=5m
```

`app` sigue sirviendo la interfaz en `localhost:8080`. El scheduler consulta cada `QA_RECONCILE_INTERVAL` los PRs abiertos de los repos activos, de modo que la sincronización de PRs y las revisiones pendientes siguen funcionando. No hace falta registrar una URL de webhook en la GitHub App.

Las disputas en hilos (`pull_request_review_thread`) y los comandos `/qa` (`issue_comment`) requieren webhooks; la tabla de eventos de la sección anterior detalla el resto. En modo polling, los descartes y las confirmaciones se hacen desde el panel.

## Túnel público (opcional)

`deploy/docker-compose.yml` incluye un servicio `tunnel` con Cloudflare Tunnel que solo se levanta con el perfil `tunnel`:

```dotenv
# deploy/.env
CLOUDFLARE_TUNNEL_TOKEN=<token del túnel>
```

```sh
docker compose -f deploy/docker-compose.yml --profile tunnel up -d --build
```

El servicio vive en la red del compose y resuelve la app por DNS (`http://app:8080`), no comparte su network namespace: recrear o reiniciar `app` deja el túnel intacto. En el ingress del túnel (panel de Cloudflare o `cloudflared tunnel route`) la URL de origen debe ser `http://app:8080`.

La imagen de `cloudflared` es distroless (sin `sh`, `wget` ni `curl`), así que el contenedor no puede comprobarse a sí mismo con un healthcheck. Por eso publica su endpoint de métricas en `127.0.0.1:2000`: `GET /ready` devuelve 200 cuando hay al menos una conexión al edge y 503 mientras reconecta. Un **502** en el dominio público con `/ready` en 200 apunta a la app; con `/ready` en 503 apunta al túnel.

Para el reinicio seguro de cada pieza:

```sh
# La app sola: el túnel sigue conectado.
docker compose -f deploy/docker-compose.yml restart app

# La app con un build o un cambio de .env: recrea el túnel en la misma orden.
docker compose -f deploy/docker-compose.yml --profile tunnel up -d --build app

# El worker no afecta al túnel.
docker compose -f deploy/docker-compose.yml restart worker
```

### Alertas mínimas

Dos comprobaciones externas (o el monitor que ya uses) cubren el issue:

- `$QA_PUBLIC_URL/healthz` cada minuto: la app responde 200 con `{"status":"ok"}`; un 502 o un timeout indica app o túnel caídos.
- `http://127.0.0.1:2000/ready` cada minuto en el host: 200 significa túnel conectado; 503 sostenido, túnel caído o sin salida al edge.

`docker compose ps` distingue cuál de las dos piezas falla cuando alguna alerta salta.

## Comandos útiles

```sh
# Seguir los logs de la API y del worker
docker compose -f deploy/docker-compose.yml logs -f app worker

# Reconstruir tras cambiar código o la web
docker compose -f deploy/docker-compose.yml up -d --build

# Parar conservando los datos
docker compose -f deploy/docker-compose.yml down

# Parar y borrar base de datos y volúmenes
docker compose -f deploy/docker-compose.yml down -v
```

## Desarrollo local (sin Docker)

```sh
make web                        # npm ci + build + copia a internal/web/dist
make build                      # bin/qa con la web embebida
make test                       # go test ./... + pruebas de la web (Vitest)
npm --prefix web run dev        # Vite con proxy /api, /auth, /setup y /webhooks
```

> En Fase 2 el worker se construye con el target `worker`: incorpora Node 24, `ocr` 1.13.13 y `opencode` 1.19.0, ejecuta los perfiles LLM y conserva la autenticación en el volumen `opencode-data`.
