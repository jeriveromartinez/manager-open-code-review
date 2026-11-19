# Instalación

QA Control Center se entrega como **una sola imagen** con dos roles (`serve` y `worker`) junto a PostgreSQL. La instalación inicial tarda unos 15 minutos.

## Requisitos

- Docker con Compose v2 (`docker compose version`).
- OpenSSL, para generar la clave maestra.
- Permisos de administrador sobre la cuenta personal o la organización de GitHub donde se creará la GitHub App.

## 1. Preparar el entorno

```sh
git clone <url-del-repositorio>
cd qa-control-center
cp deploy/.env.example deploy/.env
```

Genera la clave maestra y pégala en `QA_MASTER_KEY`:

```sh
openssl rand -base64 32
```

Revisa también estos valores de `deploy/.env`:

- `POSTGRES_PASSWORD`: contraseña de PostgreSQL; debe repetirse en `QA_DATABASE_URL`.
- `QA_PUBLIC_URL`: URL pública de la app **sin `/` final** (por ejemplo `https://qa.example.com`). Si no hay entrada pública, consulta el modo solo polling más abajo.
- `QA_WEBHOOKS_ENABLED` y `QA_RECONCILE_INTERVAL`: ajusta según el modo de operación.

`QA_DATABASE_URL` ya apunta al servicio `postgres` de Compose; no hace falta cambiar el host.

## 2. Levantar el stack

```sh
docker compose -f deploy/docker-compose.yml up -d --build
```

El primer arranque construye la web y el binario y aplica las migraciones. Comprueba que la API responde:

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
3. Opcionalmente, ajusta la política QA por equipo o por repositorio.

Los pull requests abiertos aparecen en el Dashboard. La reconciliación periódica incorpora los que llegaron sin webhook.

## Añadir otra cuenta u organización

La GitHub App es pública, por lo que cualquier cuenta personal u organización puede instalarla. Desde una misma instancia puedes gestionar repositorios de varias cuentas:

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

`app` sigue sirviendo la interfaz en `localhost:8080`. El scheduler consulta cada `QA_RECONCILE_INTERVAL` los PRs abiertos de los repos activos y los checks de CI de los PRs en espera, de modo que las revisiones y el estado de CI siguen funcionando. No hace falta registrar una URL de webhook en la GitHub App.

Las disputas en hilos y los comandos `/qa` requieren webhooks. En modo polling, los descartes y las confirmaciones se hacen desde el panel.

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

> En Fase 2 el worker incorpora Node, `ocr` y `opencode` con versiones fijadas en la imagen, además de los perfiles LLM y la autenticación persistida en el volumen `opencode-data`.
