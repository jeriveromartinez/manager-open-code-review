# Rediseño de filtros del panel — Runs · Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** mostrar runs activos e histórico en la vista Runs, con estado, repo, trigger, error y duración, filtrables como las demás vistas y con paginación por cursor.

**Architecture:** una consulta de historial con scope de equipo (reutiliza `teamScopedPRs`), un endpoint `GET /api/review-runs/history` con cursor `before`/`limit`, y una vista que fusiona los activos (consulta actual) con las páginas de historial, deduplicando por id y filtrando en cliente sobre lo cargado. Sin migración: los índices existentes (`created_at`, `status`) cubren el barrido.

**Tech Stack:** Go 1.27 + GORM + PostgreSQL 17 (testcontainers en kiri), chi; Vue 3 + TypeScript + vitest.

**Spec:** `docs/superpowers/specs/2026-11-26-panel-filters-design.md`

## Global Constraints

- Scope de equipo igual que `/api/prs` y `/api/review-runs`: reutilizar `teamScopedPRs`; un admin ve todo y el resto solo sus equipos.
- Paginación por cursor como el log de runs: `before` (id) y `limit` (1–200, defecto 50), respuesta con `next_before` y `has_more`; parámetros inválidos → 400 con `writeAPIError`.
- Filtros y recuentos en cliente sobre las filas cargadas; la barra indica que es sobre lo cargado cuando haya más páginas.
- UI en español; código, tests y commits en inglés; Conventional Commits < 72 caracteres.
- Sin dependencias nuevas; sin migración (si aparece la necesidad, sería `00037_*`).
- Gate: suite Go completa en Linux sobre kiri y `npm test -- --run` + `npx vue-tsc --noEmit` en `web/`. Nada de CI.

## Review Focus

- Un run terminado sin `started_at` o sin `finished_at` no rompe la duración ni la ordenación (se muestra «—»).
- Un usuario no admin no ve runs de repositorios de otro equipo, ni por `repo` en la query aunque lo pida.
- El cursor no repite ni salta filas cuando cambian estados mientras se pagina (el historial solo incluye estados terminales).
- Un run activo que termina entre dos recargas se mueve de la sección activa al historial sin duplicarse.
- La vista sin resultados muestra «NADA COINCIDE CON ESTOS FILTROS» y no el vacío global.

---

### Task 1: Consulta de historial en el store

**Files:**
- Create: `internal/store/runhistory.go`
- Test: `internal/store/runhistory_test.go`

**Interfaces:**
- Consumes: `teamScopedPRs` (`internal/store/prs.go:503-517`), modelo `ReviewRun` (`models.go:500-540`), `ReviewStatus` y estados terminales existentes.
- Produces:
  - `type RunHistoryFilter struct { BeforeID int64; Limit int; Statuses []string; RepoID int64; Trigger string; Query string }`
  - `type RunHistoryRow struct { ID, PullRequestID uint; PRNumber int; PRTitle, Repo, Team string; Profile, Trigger, Status string; Attempt int; ErrorCode, ErrorMessage string; StartedAt, FinishedAt *time.Time; CreatedAt time.Time }`
  - `type RunHistoryPage struct { Runs []RunHistoryRow; NextBefore int64; HasMore bool }`
  - `func (s *Store) ReviewRunHistory(ctx context.Context, scope RunScope, filter RunHistoryFilter) (RunHistoryPage, error)` — `RunScope` es el tipo que ya usa la consulta de activos para el usuario/equipo; adáptalo al nombre real (`activeruns.go:33-56`).

- [ ] **Step 1: Write the failing tests**

```go
func TestReviewRunHistoryReturnsTerminalRunsNewestFirst(t *testing.T)
func TestReviewRunHistoryPaginatesWithCursor(t *testing.T)          // no repeats, no gaps between pages
func TestReviewRunHistoryFiltersByStatusRepoAndTrigger(t *testing.T)
func TestReviewRunHistorySearchesTitleAndRepo(t *testing.T)
func TestReviewRunHistoryRespectsTeamScope(t *testing.T)            // non-admin sees only own-team repos, even asking another repo
func TestReviewRunHistoryExcludesActiveRuns(t *testing.T)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store -run TestReviewRunHistory -count=1`
Expected: compile error (`undefined: ReviewRunHistory`).

- [ ] **Step 3: Implement the query**

- Condición base: `rr.status` en estados terminales (reutiliza la constante existente; no incluir queued/running), `pr.state = stateOpen` no obligatorio (el historial incluye PRs cerrados), scope `teamScopedPRs`.
- Filtros opcionales: `Statuses` (IN), `RepoID` (`pull_requests.repository_id`), `Trigger`, `Query` (`ILIKE` sobre `pr.title` y `repositories.full_name`).
- Orden `rr.created_at DESC, rr.id DESC`; `BeforeID` excluye `rr.id >= before`; `Limit` clampeado (1–200, defecto 50); `HasMore` = se obtuvo una fila extra; `NextBefore` = id de la última fila devuelta.
- `StartedAt`/`FinishedAt` nullable; no calcules duración en el store.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store -run TestReviewRunHistory -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/runhistory.go internal/store/runhistory_test.go
git commit -m "feat(store): query review run history with a cursor"
```

---

### Task 2: Endpoint de historial

**Files:**
- Modify: `internal/api/runs.go`, `internal/api/router.go:112-113`
- Test: `internal/api/runs_test.go` (créalo si no existe)

**Interfaces:**
- Consumes: `ReviewRunHistory`, `RunHistoryFilter`, `RunHistoryPage` (Task 1), `visiblePRs`/`teamScopedPRs` para el scope del usuario autenticado, `writeAPIError`.
- Produces: `GET /api/review-runs/history` → `{"runs":[<same shape as active runs plus error_code, error_message, finished_at, duration_seconds>],"next_before":<int64>,"has_more":<bool>}`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunHistoryReturnsTerminalRunsForTheUserTeams(t *testing.T)
func TestRunHistoryRejectsInvalidParams(t *testing.T)   // before<0, limit 0/201, unknown status → 400
func TestRunHistoryPaginatesByBefore(t *testing.T)      // page 1 + page 2 cover all rows once
func TestRunHistoryHidesOtherTeamsRuns(t *testing.T)    // non-admin asking repo=other → 200 with empty runs, no leak
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/api -run TestRunHistory -count=1`
Expected: 404 (ruta inexistente).

- [ ] **Step 3: Implement the handler and route**

- Parseo estricto de `before`, `limit`, `status` (repetible o CSV), `repo`, `trigger`, `q`; `limit` 1–200 defecto 50; `before` ≥ 0 else 400; estado desconocido → 400 con `writeAPIError`.
- `duration_seconds` = `finished_at - started_at` cuando ambas existen; si no, `null`.
- Ruta `r.Get("/review-runs/history", s.runHistory)` en el grupo `RequireUser`, **antes** de `/review-runs/{id}/logs` si chi lo requiere por el patrón; el resto del scope lo resuelve el store.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api -run TestRunHistory -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/runs.go internal/api/router.go internal/api/runs_test.go
git commit -m "feat(api): expose the review run history endpoint"
```

---

### Task 3: Cliente web y vista Runs

**Files:**
- Modify: `web/src/api.ts`, `web/src/views/RunsView.vue`
- Test: `web/src/views/RunsView.test.ts`, `web/src/api.test.ts`

**Interfaces:**
- Consumes: `useListFilters`, `FilterBar`, `FilterDropdown`, `FilterSheet`, `EmptyState`, `formatElapsed` y las primitivas de las vistas anteriores.
- Produces en `api.ts`: `RunHistoryRow` (campos del DTO de Task 2) y `api.runHistory(params: { status?: string[]; repo?: string; trigger?: string; q?: string; before?: number; limit?: number }): Promise<{ runs: RunHistoryRow[]; next_before: number; has_more: boolean }>`.

- [ ] **Step 1: Write the failing tests**

```ts
it('lists active and finished runs together, newest first, without duplicates')
it('filters the loaded runs by status, repository, trigger and search')
it('shows the error of a failed run')
it('shows a dash for a finished run without start or finish time')
it('loads the next history page and keeps filters applied')
it('keeps the active list on a silent refresh failure')
it('shows the filters empty state when nothing matches')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run RunsView`
Expected: FAIL (sin historial ni barra).

- [ ] **Step 3: Implement the view**

- Al montar: `api.activeRuns()` + `api.runHistory({ limit: 50 })`; fusiona por id (los activos ganan), orden `created_at DESC`; `useLiveRefresh(['run.updated','pr.updated'])` recarga la primera página de historial sin parpadear.
- Facetas: `status` (queued/running + estados terminales), `repo`, `trigger`, más `q` sobre título y repo; recuentos con `facetCounts` sobre lo cargado.
- Botón «CARGAR MÁS» visible cuando `has_more`; añade la página con `before = next_before`; al cargar más, reaplica filtros (son locales) y no reordena los ya mostrados salvo por el orden global.
- Duración: `formatElapsed` sobre `finished_at - started_at`; «—» si falta cualquiera.
- Error de run: `error_code`/`error_message` visibles en la tarjeta (clase `.run-card__error`).
- Barra dentro de `.list-view`/`.list-scroll` de la ruta `/runs` (añade `meta.flushContainer` a la ruta y el opt-in de la Tarea 1 del plan 2 ya existe); `FilterSheet` con los mismos controles y `applyLabel="VER n runs"`. El estado de filtros va a la URL mediante `useListFilters` (claves `q`, `status`, `repo`, `trigger`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run RunsView && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/api.ts web/src/views/RunsView.vue web/src/views/RunsView.test.ts web/src/api.test.ts
git commit -m "feat(web): show and filter the review run history"
```

---

### Task 4: Gate y PR

- [ ] **Step 1: Full Go suite on Linux via kiri**

```bash
MSYS_NO_PATHCONV=1 docker --context kiri run --rm --network host \
  -v /var/lib/kiri-daemon/.kiri/run/docker.sock:/var/run/docker.sock \
  -v "<worktree>:/src" -v "D:/.go/pkg/mod:/go/pkg/mod:ro" -w /src \
  -e DOCKER_HOST=unix:///var/run/docker.sock -e GOMODCACHE=/go/pkg/mod \
  golang:1.27 go test -p 1 ./... -count=1
```
Expected: todos los paquetes `ok`.

- [ ] **Step 2: Web gate**

Run: `cd web && npm test -- --run && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 3: Push and open the PR**

```bash
git push -u origin feat/runs-history
gh pr create --base main --head feat/runs-history \
  --title "feat: review run history in the panel" \
  --body-file - <<'EOF'
Third of three plans for the panel filter redesign.

Adds a cursor-paginated history of finished review runs (status, repo, trigger, error, duration) behind the same team scope as the active list, the `/api/review-runs/history` endpoint, and a Runs view that merges active and finished runs with the shared filter bar and mobile sheet.

Spec: docs/superpowers/specs/2026-11-26-panel-filters-design.md
EOF
```
