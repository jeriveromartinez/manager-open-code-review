# QA Control Center — Multi-cuenta de GitHub (repositorios)

Fecha: 2026-11-19 · Estado: propuesta para aprobación

## 1. Intención

Hoy una instancia sirve a los repositorios de **una sola cuenta/org**: la GitHub App se crea con `public=false`, y una App privada solo puede instalarse en la cuenta que la posee. Queremos que la misma instancia muestre y gestione repositorios de **varias cuentas personales y organizaciones** de GitHub, sin levantar una instancia por cuenta.

La decisión es: **una GitHub App pública, instalada en N cuentas**. El modelo de datos ya guarda varias `installations` y el dashboard de admin ya ve todos los repos; faltan la App pública, el flujo para añadir cuentas y el descubrimiento de instalaciones nuevas.

## 2. Estado actual y huecos

Ya existe:
- `installations` con `AccountLogin`, `AccountType`, `SuspendedAt`; `repositories` con `InstallationID`.
- `SyncInstallation` por instalación y `ReconcileAll` sobre las instalaciones guardadas.
- Webhooks `installation` / `installation_repositories` que dan de alta/baja cuentas y repos.
- `/api/setup/status` devuelve `install_url`; `/setup/installed` encola la sincronización.

Falta:
1. `BuildManifest` crea la App con `public=false`; una App privada no se puede instalar en otras cuentas.
2. No hay ningún control en la web para “instalar en otra cuenta” cuando la instancia ya está configurada.
3. En modo polling, `ReconcileAll` solo recorre las instalaciones de la BD: no descubre instalaciones nuevas con `GET /app/installations`.
4. La lista de repos no muestra a qué cuenta pertenece cada uno; no hay una vista de cuentas conectadas.

## 3. Objetivos / no-objetivos

**Objetivos**
- Instalar la misma App en varias cuentas personales/orgs y ver todos sus repos en el panel.
- Descubrir instalaciones nuevas aunque se pierda el webhook o el callback, también en modo polling.
- Mostrar la cuenta de cada repo y un botón de admin para añadir otra cuenta.
- Asignar repos de cualquier cuenta a los equipos existentes.

**No-objetivos**
- Varias Apps privadas, una por cuenta, con credenciales separadas (otra decisión, mucho mayor).
- Roles de admin por cuenta/org; los equipos siguen siendo de la instancia.
- Sincronización con GitHub Teams, políticas QA, motor de revisión (Fases 2–4).
- Aislamiento por cuenta de datos o de webhooks.

**Anti-objetivos**
- Que una cuenta instale la App y sus repos aparezcan habilitados sin que un admin los habilite.
- Que un fallo de paginación borre instalaciones/repos existentes.
- Que una instalación suspendida bloquee la reconciliación de las demás.

## 4. Diseño

### 4.1 Manifest público

`BuildManifest` pasa a `Public: true`. El resto de permisos/eventos/URLs no cambia. La App se instala desde `https://github.com/apps/<slug>/installations/new` (`App.InstallURL()`), que ya se usa en el setup.

Si ya se creó una App privada, hay que recrearla desde `/setup` o cambiar su visibilidad a pública en GitHub. En esta instancia el setup aún no está configurado, así que no hay migración de datos.

### 4.2 Descubrimiento de instalaciones

`App` gana un cliente a nivel de App (JWT), separado del cliente por instalación:

```go
func (a *App) AppClient() (*gh.Client, error) // ghinstallation.NewAppsTransport
```

`Syncer.ReconcileAll`:
1. Obtiene `AppClient` y lista `GET /app/installations` paginado (`per_page=100`).
2. Si la lista se obtiene completa: hace `UpsertInstallation` de cada una (login, tipo, `SuspendedAt`) y elimina de la BD las instalaciones que GitHub ya no reporta (cascada de repos/PRs).
3. Si la llamada falla: registra el error y concatena solo las instalaciones de la BD, **sin borrar nada**.
4. Sincroniza cada instalación no suspendida. Las suspendidas se conservan pero no se sincronizan. Además, `SyncInstallation` comprueba la instalación en la BD y devuelve nil sin llamar a GitHub si está suspendida, de modo que cualquier job ya encolado (incluido el que dispara `PATCH /api/repos` al habilitar un repo) es inofensivo.
5. El webhook `installation.unsuspend` limpia `SuspendedAt` y encola una sync inmediata; no espera al siguiente reconcile.
6. Un fallo por instalación se registra y no detiene las demás (comportamiento actual).

Esto cubre webhooks perdidos y modo polling. En modo webhook, el evento `installation.created` sigue dando de alta la instalación de inmediato; la reconciliación es la red de seguridad.

### 4.3 API

- `GET /api/installations` (admin) → `[{id, account_login, account_type, suspended_at}]`, ordenado por login.
- `GET /api/repos` añade `account_login` a cada repo (join con `installations`), para que la tabla muestre la cuenta. `installation_id` se mantiene.
- No se relaja ninguna otra ruta: habilitar repos y asignar equipos sigue siendo admin.

### 4.4 Web

- En `ReposView`, cabecera “Cuentas conectadas” **solo para admin**, con las cuentas y sus repos; botón **“Añadir cuenta”** también solo para admin, que abre `install_url` de `/api/setup/status` en una pestaña nueva.
- Columna “Cuenta” en la tabla de repos para todos los usuarios: se deriva de los repos que ya pueden ver, sin exponer instalaciones de otras cuentas. `GET /api/installations` sigue siendo solo admin.
- Al volver de instalar, `/setup/installed` encola la sync y redirige a `/`; la vista se refresca con el evento `repo.updated` o al cargar.
- Si `install_url` está vacío (instancia sin configurar), el guard de setup ya redirige a `/setup`; no se muestra el botón.

### 4.5 Seguridad y comportamiento

- Una App pública puede ser instalada por cualquier cuenta que la encuentre. La instalación **no** habilita repos: `SyncInstallation` los crea con `Enabled=false` y el admin decide cuáles activa.
- El secreto de webhook es único por App; todas las instalaciones comparten la misma App y el mismo secreto. La firma HMAC ya valida el cuerpo.
- El botón “Añadir cuenta” es solo admin; `/setup/installed` sigue siendo público como en el plan, pero solo encola una sync de una instalación a la que la App ya tiene acceso.
- La poda de instalaciones solo ocurre con una listación completa y exitosa de GitHub; nunca ante un error de red o paginación.

## 5. Alternativas consideradas

| Alternativa | Por qué no |
|---|---|
| **Una App pública, N instalaciones** (elegida) | Cambio pequeño, reutiliza el modelo de installations, una sola credencial. |
| Una App privada por cuenta | Aislamiento por cuenta, pero exige `app_configs` por App, migración, selección de App por repo, secretos de webhook por App y UI de configuración por cuenta. Desproporcionado para lo pedido. |
| Solo varios usuarios en el panel | No cambia la conexión de repos; los repos seguirían viniendo de una sola instalación. Ya está soportado. |

## 6. Plan de implementación (borrador)

1. Manifest `public=true` + test; documentar la recreación de la App si era privada.
2. `App.AppClient()` + descubrimiento/poda en `ReconcileAll` + tests con GitHub falso paginado.
3. `GET /api/installations` y `account_login` en `/api/repos` + tests de autorización/DTO.
4. `ReposView`: cuentas conectadas, botón “Añadir cuenta” y columna Cuenta + tests admin/no-admin.
5. `docs/install.md`: sección “Añadir otra cuenta u organización”; nota sobre App pública y repos deshabilitados por defecto.

## 7. Decisiones dejadas al constructor

- Formato exacto de la cabecera “Cuentas conectadas” y si se muestra el número de repos por cuenta.
- Si una instalación suspendida se muestra atenuada o se oculta; se conserva en BD en cualquier caso.
- Si `GET /api/installations` incluye `repository_count` (útil para la UI) o se calcula en el cliente.
- Estrategia de reintento de `GET /app/installations` dentro de `ReconcileAll` (el job periódico ya reintenta cada intervalo).

## 8. Tamaño

Cambio mediano: 5 tareas, sin migración de esquema. Requiere tests de syncer, API y web; no toca el motor de revisión.
