# Gestión y borrado de equipos — diseño

Fecha: 2026-11-25 · Issue de referencia: equipos sin gestión completa desde el panel.

## Intención y por qué

Hoy `TeamsView` crea equipos, añade y quita miembros, cambia roles y enlaza a la
política, pero no permite **renombrar** ni **eliminar**. Un equipo creado por
error (por ejemplo `QA Pilot 136 162`) solo se puede dejar ahí para siempre.

El borrado tiene una consecuencia real: `repositories.team_id` es
`ON DELETE SET NULL`, mientras que `team_members` y `qa_policies` se borran en
cascada. Borrar sin más dejaría repositorios sin equipo y sin aviso.

## Objetivos

- Renombrar un equipo desde el panel, con administrador.
- Eliminar un equipo, **bloqueado mientras tenga repositorios asignados**.
- En el diálogo de borrado, reasignar cada repositorio a otro equipo o dejarlo
  sin equipo, usando el `PATCH /api/repos/{id}` que ya existe.
- Confirmación escribiendo el nombre del equipo, auditoría (`team.updated`,
  `team.deleted`) y errores claros.

## No objetivos

- Borrado lógico o archivo de equipos.
- Mover miembros entre equipos en bloque ni transferir propiedad.
- Cambiar la política desde la vista de equipos (ya está en Agentes LLM).
- Tocar la identidad de los equipos existentes más allá del nombre.

## Restricciones

- Solo administradores: las rutas nuevas viven en el grupo `RequireAdmin`, como
  `POST /teams` y `PATCH /repos/{id}`.
- Sin migración: el esquema ya soporta borrado (cascada y `SET NULL`).
- El `slug` del equipo se deriva del nombre; el renombrado debe respetar la
  unicidad y devolver un error claro si el nombre ya existe.
- La asignación de repositorio ya existe en `PATCH /api/repos/{id}`
  (`AssignRepositoryTeam`); no se añade endpoint nuevo para eso.
- Idioma de la UI: español, como el resto del panel; commits y código en inglés.

## API

- `PATCH /api/teams/{id}` con `{ "name": "..." }`:
  - 200 con el equipo actualizado; 400 si el nombre está vacío o es inválido;
    404 si no existe; 409 si el slug ya existe.
  - Auditoría `team.updated` con el nombre anterior y el nuevo.
- `DELETE /api/teams/{id}`:
  - 409 con `{ "error": "...", "repositories": [{ "id", "full_name" }] }` si hay
    repositorios asignados; el cliente debe reasignarlos primero.
  - 204 si se borra; 404 si no existe.
  - Auditoría `team.deleted` con el nombre y el número de miembros borrados.
- `GET /api/teams` añade `repository_count` a cada equipo para mostrar el estado
  en la lista sin llamadas extra.

## Interfaz

- Cada tarjeta de equipo muestra `N repos · M miembros` y, solo para admin,
  `Renombrar` y `Eliminar`.
- Renombrar abre un diálogo con el nombre actual; guarda con `PATCH` y refresca
  la lista.
- Eliminar abre un diálogo que:
  1. Si hay repositorios, los lista con un selector por repositorio
     (`sin equipo` o cualquier otro equipo) y un botón para aplicar; mientras
     quede alguno asignado, el botón de borrar permanece deshabilitado.
  2. Pide escribir el nombre del equipo para confirmar.
  3. Al borrar, llama a `DELETE`, cierra y refresca; un 409 muestra los
     repositorios que siguen asignados.

```
┌─ Equipos ─────────────────────────────────┐
│ QA Pilot 136 162   1 repo · 0 miembros    │
│   [Renombrar]  [Política]  [Eliminar]     │
└───────────────────────────────────────────┘
        ↓ Eliminar
┌─ Eliminar equipo ─────────────────────────┐
│ Repositorios asignados:                   │
│   juliojmjc/qa-review-pilot   [j3rm ▾]    │
│ Escribe "QA Pilot 136 162": [__________]  │
│                     [Cancelar] [Eliminar] │
└───────────────────────────────────────────┘
```

## Pruebas

- Store: `RenameTeam` (éxito, vacío, duplicado), `DeleteTeam` (bloquea con
  repos, borra cascada de miembros y política), `repository_count`.
- API: `PATCH` y `DELETE` con permisos (admin sí, lead no), 400/404/409, forma
  del 409 con repositorios, auditoría escrita.
- Web: renombrar (payload y refresco), eliminar bloqueado con repos, reasignar
  y luego eliminar, confirmación por nombre, 409 mostrado, botones ocultos para
  no-admin.
- Gate: suite completa en Linux sobre kiri y `vitest`/`vue-tsc` en `web/`.

## Decisiones tomadas

- Borrado bloqueado hasta reasignar (opción 1 elegida por el usuario).
- Sin migración ni columna nueva; `repository_count` se calcula en la consulta.
- `PATCH` y `DELETE` en el grupo de administradores; el resto de la gestión de
  miembros ya tiene su propio control de permisos y no cambia.

## Lo que queda al implementador

- Forma exacta de `TeamWithMembers` y del DTO de la lista.
- Mensajes de error y textos de los diálogos, siguiendo el estilo del panel.
- Si `repository_count` se calcula con subconsulta o con un `LEFT JOIN`; lo
  decide la implementación según el estilo de `ListTeams`.
