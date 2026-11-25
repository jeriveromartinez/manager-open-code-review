# Gestión y borrado de equipos — Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** permitir renombrar y eliminar equipos desde el panel, bloqueando el borrado mientras el equipo tenga repositorios asignados y obligando a reasignarlos en el propio diálogo.

**Architecture:** dos rutas nuevas de administrador (`PATCH`/`DELETE /api/teams/{id}`) sobre métodos de store transaccionales, más la lista de equipos con `repository_count`; la reasignación reutiliza `PATCH /api/repos/{id}`. La vista de equipos gana un diálogo de renombrado y otro de borrado con reasignación previa y confirmación por nombre.

**Tech Stack:** Go 1.27, chi, GORM, PostgreSQL 17, Vue 3 + TypeScript + vitest.

**Spec:** `docs/superpowers/specs/2026-11-25-team-management-design.md`

## Global Constraints

- Solo administradores para `PATCH /api/teams/{id}` y `DELETE /api/teams/{id}` (grupo `RequireAdmin`, como `POST /teams`).
- Sin migración: el esquema ya tiene cascada en `team_members`/`qa_policies` y `SET NULL` en `repositories.team_id`.
- El `slug` se deriva del nombre con `slugify` y debe seguir siendo único.
- Auditoría: `team.updated` con `{"from": <nombre anterior>, "to": <nombre nuevo>}` y `team.deleted` con `{"name": <nombre>, "members": <n>}`.
- Errores de la API en texto plano con `writeAPIError`; el 409 de borrado devuelve JSON con `error` y `repositories: [{id, full_name}]`.
- UI en español; código, tests y commits en inglés.
- Gate local: suite Go completa en Linux sobre kiri y `npm test -- --run` + `npx vue-tsc --noEmit` en `web/`. Nada de CI.

## Review Focus

- **Carrera al borrar:** un repositorio se asigna entre la comprobación y el borrado; el borrado debe ser transaccional y devolver 409, no dejar el repo huérfano.
- **Renombrar al mismo nombre:** debe ser un 200 idempotente, no un 409 por chocar consigo mismo.
- **Slug duplicado con nombre distinto:** "QA Pilot" y "qa pilot" colisionan en slug; 409 claro.
- **Diálogo sin repos:** si no hay repositorios, el borrado debe habilitarse sin paso de reasignación.
- **Usuario sin permiso:** el lead no ve los botones y la API responde 403 aunque los invoque.

---

### Task 1: Store — recuento, renombrado y borrado guardado

**Files:**
- Modify: `internal/store/users.go`
- Test: `internal/store/users_test.go`

**Interfaces:**
- Consumes: `Team`, `TeamMember`, `Repository` de `models.go`, `slugify`, `ErrNotFound` (`internal/store/prs.go:17`).
- Produces:
  - `type TeamWithMembers struct { Team; Members []; RepositoryCount int }`
  - `var ErrTeamNameTaken error`
  - `var ErrTeamInUse error`
  - `func (s *Store) RenameTeam(ctx context.Context, teamID uint, name string) (Team, error)`
  - `func (s *Store) TeamRepositories(ctx context.Context, teamID uint) ([]Repository, error)`
  - `func (s *Store) DeleteTeam(ctx context.Context, teamID uint) (int64, error)` — devuelve miembros borrados

- [ ] **Step 1: Write the failing tests**

En `internal/store/users_test.go`, añadir:

```go
func TestRenameTeam(t *testing.T)      // éxito cambia name y slug; mismo nombre → 200 sin error; otro equipo con mismo slug → ErrTeamNameTaken; inexistente → ErrNotFound
func TestDeleteTeamGuardsRepositories(t *testing.T) // con repo asignado → ErrTeamInUse; sin repos → borra equipo, miembros y qa_policies, devuelve nº de miembros
func TestDeleteTeamRechecksInsideTheTransaction(t *testing.T) // TeamRepositories no ve repos, se asigna uno y DeleteTeam → ErrTeamInUse (carrera)
func TestListTeamsCountsRepositories(t *testing.T)  // 2 equipos, uno con 2 repos → RepositoryCount correcto
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store -run 'TestRenameTeam|TestDeleteTeam|TestListTeamsCounts' -count=1`
Expected: compile error (`undefined: RenameTeam`, `ErrTeamInUse`, `RepositoryCount`).

- [ ] **Step 3: Implement the store methods**

- `RenameTeam`: `SELECT` del equipo (inexistente → `ErrNotFound`); si `name == team.Name` devolver el equipo sin escribir; calcular `slugify(name)`; si otro equipo ya tiene ese nombre o slug → `ErrTeamNameTaken`; `UPDATE name, slug, updated_at`.
- `TeamRepositories`: `SELECT id, full_name FROM repositories WHERE team_id = ? ORDER BY full_name`.
- `DeleteTeam`: en una transacción, `SELECT count(*) FROM repositories WHERE team_id = ?`; si > 0 → `ErrTeamInUse`; contar miembros; borrar el equipo (la cascada elimina miembros y política); devolver el número de miembros.
- `ListTeams`: consultar `SELECT team_id, count(*) FROM repositories WHERE team_id IS NOT NULL GROUP BY team_id` y rellenar `RepositoryCount`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store -run 'TestRenameTeam|TestDeleteTeam|TestListTeamsCounts' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/users.go internal/store/users_test.go
git commit -m "feat(store): rename and guarded team deletion"
```

---

### Task 2: API — PATCH y DELETE de equipos

**Files:**
- Modify: `internal/api/router.go`, `internal/api/handlers.go`
- Test: `internal/api/teams_test.go` (nuevo; usa el fixture existente de `agents_test.go` si aplica)

**Interfaces:**
- Consumes: `RenameTeam`, `TeamRepositories`, `DeleteTeam`, `ErrTeamNameTaken`, `ErrTeamInUse` (Task 1); `teamResponse`/`teamDTO` (`handlers.go:202`); `s.notify`; `s.Store.Audit`.
- Produces:
  - `teamResponse` gana `RepositoryCount int `json:"repository_count"``.
  - `func (s *server) patchTeam(w http.ResponseWriter, r *http.Request)`
  - `func (s *server) deleteTeam(w http.ResponseWriter, r *http.Request)`

- [ ] **Step 1: Write the failing tests**

```go
func TestPatchTeamRenamesAsAdmin(t *testing.T)        // 200, nombre/slug nuevos, auditoría team.updated
func TestPatchTeamRejectsNonAdmin(t *testing.T)       // 403
func TestPatchTeamRejectsEmptyNameAndMissingTeam(t *testing.T) // 400 / 404
func TestPatchTeamReportsNameCollision(t *testing.T)  // 409
func TestDeleteTeamBlockedByRepositories(t *testing.T) // 409 con repositories[].full_name
func TestDeleteTeamRemovesTeamAndAudits(t *testing.T)  // 204, equipo ausente, auditoría team.deleted
func TestDeleteTeamRejectsNonAdmin(t *testing.T)       // 403
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api -run 'TestPatchTeam|TestDeleteTeam' -count=1`
Expected: 404 (rutas inexistentes) y fallos de compilación si el DTO no compila.

- [ ] **Step 3: Implement handlers and routes**

- `patchTeam`: decodifica `{name}`, `strings.TrimSpace`, vacío → 400; `strconv.ParseUint` del `{id}` → 400; `RenameTeam`; mapea `ErrNotFound`→404, `ErrTeamNameTaken`→409; `notify team.updated`; `Audit` con `from`/`to`; responde 200 con `{id, name, slug, repository_count}`.
- `deleteTeam`: parsea id; `TeamRepositories`; si hay → 409 `{error, repositories: [{id, full_name}]}`; si no → `DeleteTeam`, `notify team.updated`, `Audit` `team.deleted` con nombre y miembros; 204.
- `router.go`: dentro del grupo `RequireAdmin`, `r.Patch("/teams/{id}", s.patchTeam)` y `r.Delete("/teams/{id}", s.deleteTeam)`.
- `teamDTO`: rellena `RepositoryCount`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api -run 'TestPatchTeam|TestDeleteTeam|TestTeams' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/router.go internal/api/handlers.go internal/api/teams_test.go
git commit -m "feat(api): rename and guarded delete team endpoints"
```

---

### Task 3: Panel — renombrar y eliminar con reasignación

**Files:**
- Modify: `web/src/api.ts`, `web/src/views/TeamsView.vue`
- Test: `web/src/views/TeamsView.test.ts`

**Interfaces:**
- Consumes: `GET/PATCH/DELETE /api/teams/{id}`, `PATCH /api/repos/{id}` (`patchRepo` ya existe en `api.ts:781`), `ApiError` (`api.ts:682`).
- Produces:
  - `Team` gana `repository_count: number`.
  - `ApiError` gana `payload?: unknown` (el JSON del cuerpo de error cuando lo hay).
  - `renameTeam(id: number, name: string): Promise<Team>`
  - `deleteTeam(id: number): Promise<void>`

- [ ] **Step 1: Write the failing tests**

En `TeamsView.test.ts`:

```ts
it('renames a team from the dialog')            // PATCH con {name} y lista refrescada
it('blocks deletion while a repository is assigned') // diálogo lista el repo; botón deshabilitado
it('reassigns a repository before deleting')     // PATCH /api/repos/{id} con team_id y después DELETE
it('deletes a team without repositories')        // DELETE directo tras confirmar el nombre
it('shows the 409 repositories when the server refuses') // ApiError.payload
it('hides rename and delete from non-admins')    // sin botones
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run TeamsView`
Expected: FAIL (funciones y campos inexistentes).

- [ ] **Step 3: Implement the panel changes**

- `api.ts`: añade `repository_count`, `renameTeam`, `deleteTeam`; en `request`, cuando `!res.ok`, intenta `JSON.parse(detail)` y guárdalo en `ApiError.payload`.
- `TeamsView.vue`: en cada tarjeta, `N repos · M miembros`; botones `Renombrar` y `Eliminar` solo para admin. Diálogo de renombrado con el nombre actual. Diálogo de borrado: si `repository_count > 0`, carga los repos con `api.repos()` filtrados por `team_id`, muestra un `select` por repo con los demás equipos y "sin equipo", y aplica con `patchRepo(repo.id, { team_id })`; el botón `Eliminar` se habilita cuando no queda ninguno; exige escribir el nombre exacto; un 409 muestra `payload.repositories`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run TeamsView && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/api.ts web/src/views/TeamsView.vue web/src/views/TeamsView.test.ts
git commit -m "feat(web): rename and delete teams from the panel"
```

---

### Task 4: Gate completo y PR

**Files:**
- Modify: `docs/install.md` (sección de equipos) si existe una sección equivalente; si no, no tocar.

- [ ] **Step 1: Run the full Go suite on Linux via kiri**

```bash
MSYS_NO_PATHCONV=1 docker --context kiri run --rm --network host \
  -v /var/lib/kiri-daemon/.kiri/run/docker.sock:/var/run/docker.sock \
  -v "<worktree>:/src" -v "D:/.go/pkg/mod:/go/pkg/mod:ro" -w /src \
  -e DOCKER_HOST=unix:///var/run/docker.sock -e GOMODCACHE=/go/pkg/mod \
  golang:1.27 go test -p 1 ./... -count=1
```
Expected: todos los paquetes `ok`.

- [ ] **Step 2: Run the web gate**

Run: `cd web && npm test -- --run && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 3: Push and open the PR**

```bash
git push -u origin feat/team-management
gh pr create --base main --head feat/team-management \
  --title "feat: manage and delete teams from the panel" \
  --body-file - <<'EOF'
Closes #164 follow-up (team management).

Adds PATCH/DELETE /api/teams/{id} (admin), repository counts in the team
list, and the panel dialogs to rename a team or delete it after
reassigning its repositories. Deletion is blocked while any repository
is assigned; the dialog reassigns with the existing PATCH /api/repos/{id}.
EOF
```
