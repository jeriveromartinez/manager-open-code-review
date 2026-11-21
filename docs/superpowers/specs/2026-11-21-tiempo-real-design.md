# QA Control Center — Panel en tiempo real

Fecha: 2026-11-21 · Estado: aprobado

## 1. Intención

Nadie debería tener que recargar la web para ver un cambio. Si otro QA confirma un hallazgo, una revisión arranca o termina, un admin cambia una política o un chequeo de salud de un perfil LLM devuelve su resultado, cada pantalla abierta lo muestra sola en uno o dos segundos.

## 2. Estado actual y huecos

Ya existe un canal en vivo: `events.Publisher` emite `pg_notify` en `qa_events`, `events.Broker` lo reparte y `GET /api/events` lo sirve por SSE. Los eventos solo llevan `{type, id}` y el cliente vuelve a pedir los datos por la API normal, filtrada por permisos. Solo la cola de QA (`stores/prs.ts`) escucha.

Huecos:
- **Escrituras que no avisan:**
  - encolar una revisión (nueva o reintento, `review/trigger.go`);
  - empezar una revisión (`ClaimReviewRun` en `review/run.go`);
  - confirmar o descartar un hallazgo (`api/findings.go`);
  - equipos y miembros, políticas, perfiles LLM y su salud (`review/health.go`), ajustes, usuarios nuevos.
- **Vistas que no escuchan:** detalle del PR, lead, Agentes, Repos, Equipos, Ajustes.
- **Reconexión:** los eventos que llegan durante un corte se pierden y nadie vuelve a pedir los datos al reconectar.

## 3. Objetivos, no objetivos, anti-objetivos

- **Objetivo:** todas las vistas autenticadas se actualizan solas ante cambios de cualquier usuario o proceso.
- **No objetivo:** enviar los datos dentro del evento, o reemplazar SSE.
- **Anti-objetivos:**
  - Perder lo que alguien está escribiendo porque llegó un evento.
  - Filtrar datos de equipos ajenos por el canal de eventos.

## 4. Enfoques considerados

| Enfoque | A favor | En contra |
|---|---|---|
| **Publish desde el código Go** (elegido) | Sin migración. Cada aviso está junto al código que escribe. | Hay que acordarse en cada escritura nueva. Se mitiga con un test por punto. |
| Triggers de Postgres con `pg_notify` | Cubre cualquier escritor, presente o futuro. | Migración nueva, y la lógica de eventos queda fuera del código Go. |
| Refrescar cada N segundos | Trivial. | Carga constante y retraso de hasta N segundos. |

El usuario eligió publicar desde el código Go.

## 5. Diseño

### Backend: eventos nuevos

Un fallo al publicar nunca revierte la escritura, que ya está confirmada. Se registra con `slog.Warn`, igual que hoy en webhook y syncer.

| Tipo | `id` | Dónde |
|---|---|---|
| `pr.updated` | id del PR | Revisión encolada (`Trigger`). Revisión reclamada (`Runner`, tras `ClaimReviewRun`). Hallazgo confirmado o descartado (`findingTransition`, siempre que la acción tenga éxito: el veredicto puede cambiar aunque el estado no). |
| `team.updated` | id del equipo | `createTeam`, `setTeamMember`, `removeTeamMember`, `putPolicy` |
| `llm_profile.updated` | id del perfil | `createLLMProfile`, `patchLLMProfile`, `deleteLLMProfile`, `enqueueLLMHealth`, `ProfileChecker.Check` tras `SetLLMHealth` |
| `settings.updated` | `""` | `putSettings` |
| `user.updated` | id del usuario | En cada login (`auth.Auth`). Avisar solo en el alta obligaría a cambiar la API de `UpsertUserFromGitHub`, y los logins son poco frecuentes |

`repo.updated` (`patchRepo`, syncer) y los `pr.updated` y `run.updated` que ya existen no cambian. `ProfileChecker` recibe un campo `Events events.Publisher`, que se conecta en `cmd/qa/main.go`.

Visibilidad: un evento solo revela que una entidad con ese id cambió. Los datos llegan por los endpoints de siempre, con sus permisos. Un no-admin que recibe `settings.updated` no pide nada.

### Frontend: una conexión para toda la app

- **`stores/live.ts`:**
  - Abre un único `EventSource` mientras hay sesión y lo cierra al cerrar sesión.
  - Expone `connected`.
  - Expone `subscribe(types, handler)`, que devuelve la función para darse de baja.
  - Al reconectar después de un corte, emite un evento sintético `resync` a todos los suscriptores.
- **Composable `useLiveRefresh(types, reload, filter?)`:**
  - Agrupa las ráfagas en 300 ms.
  - Llama a `reload` también ante `resync`.
  - Se da de baja al desmontar la vista.
- **`subscribeEvents` y el manejo de SSE en `stores/prs.ts`** se sustituyen por lo anterior.
- **La insignia «En vivo / Reconectando…»** pasa de la cola de QA a la cabecera de la app.

| Vista | Escucha |
|---|---|
| Cola de QA | `pr.updated`, `repo.updated` |
| Detalle del PR | `pr.updated` con su id |
| Lead | `pr.updated`, `repo.updated`, `team.updated` |
| Agentes | `llm_profile.updated`, `team.updated` (políticas) |
| Repos | `repo.updated`, `team.updated` |
| Equipos | `team.updated`, `user.updated` |
| Ajustes | `settings.updated` |

### Formularios

Las listas y los estados se actualizan solos. Lo que el usuario está editando no se pisa:
- **Ajustes:** un grupo con cambios sin guardar conserva sus valores. Si llega `settings.updated`, ese grupo muestra «Otra persona cambió estos ajustes · Recargar». Los grupos sin cambios se refrescan.
- **Agentes:**
  - Si el formulario de perfil LLM o el editor de política están abiertos o tienen cambios y llega un evento de ese perfil o equipo, aparece el mismo aviso.
  - La lista de perfiles y su salud se actualizan siempre.
- **Equipos:** los borradores de «añadir miembro» se conservan. La lista se refresca.
- **Detalle del PR:** el diálogo de confirmar o descartar sigue abierto con su motivo. Si el hallazgo ya fue triado por otra persona, la acción responde como hoy (idempotente o 409).

## 6. Pruebas

- **Go:** un test por punto nuevo de publicación, con el `recordingPublisher` que ya existe en cada paquete. Comprueba el tipo y el id.
- **Vitest:**
  - `live.ts`: una sola conexión, reparto por tipo, `resync` al reconectar y cierre al desmontar.
  - `useLiveRefresh`: agrupa las ráfagas.
  - Cada vista: un evento relevante recarga y uno ajeno no. En las vistas con formulario, un cambio sin guardar sobrevive a un evento.
- `go vet`, `go test ./...` en el contenedor Linux, `vitest`, `npm run build`.

## 7. Queda a criterio del implementador

- Nombres internos y la forma exacta del aviso «Recargar», reutilizando los estilos existentes.
- Si `enqueueLLMHealth` marca el perfil como pendiente antes de publicar, si hoy no lo hace.
