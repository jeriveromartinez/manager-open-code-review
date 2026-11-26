# Rediseño de filtros del panel — Vistas · Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** aplicar la barra de filtros de las fundaciones a Cola, Repos, Lead y Hallazgos (en el detalle), con el layout de pantalla completa y el estado en la URL.

**Architecture:** un opt-in por ruta (`meta.flushContainer`) hace que AppShell use `.container--flush` y la vista declare `.list-view` + `.list-scroll`; cada vista compone `FilterBar` + `FilterDropdown` + `FilterSheet` con `useListFilters`, y calcula sus facetas con `facetCounts`. Los filtros son de cliente sobre las listas ya cargadas.

**Tech Stack:** Vue 3 + TypeScript, vue-router, vitest + @vue/test-utils.

**Spec:** `docs/superpowers/specs/2026-11-26-panel-filters-design.md`

## Global Constraints

- Reutilizar las primitivas de `web/src/components/` y `useListFilters`; no duplicar lógica de filtros ni crear dependencias.
- UI en español; código, tests y commits en inglés; Conventional Commits < 72 caracteres.
- `q` se refleja de forma síncrona en `FilterBar` (el debounce, si lo hubiera, va después); una sola regla de “hay filtros” (`q.trim() || chips`) para contador y LIMPIAR.
- Filtros y recuentos **en cliente**; `M` es el total de la pestaña/sección antes del filtro de la barra.
- En móvil, los desplegables de la hoja son marcado propio (`FilterSheet`), y el disparador es el de `FilterBar` (`open-sheet` + `sheetOpen`/`sheetId`).
- Gate: `npm test -- --run` y `npx vue-tsc --noEmit` en `web/`. Nada de CI.

## Review Focus

- Una opción seleccionada de una faceta nunca desaparece de su propio desplegable al filtrar (los recuentos excluyen su dimensión).
- La URL compartida reproduce el estado al abrir; atrás/adelante no rompe (limitación conocida: hidratación solo al montar).
- Una vista sin resultados por combinación muestra «NADA COINCIDE CON ESTOS FILTROS» con LIMPIAR, nunca una lista vacía muda.
- En móvil, abrir la hoja no deja dos scrolls ni pierde el foco al cerrar; Elegir en la hoja aplica el mismo estado que en escritorio.
- El cambio de layout no rompe las vistas que no adoptan el opt-in (Agents, Settings, Setup, Login, Dashboard antes de su tarea).

---

### Task 1: Opt-in de layout por ruta

**Files:**
- Modify: `web/src/router.ts` (meta en las rutas de cola/repos/lead/detalle)
- Modify: `web/src/components/AppShell.vue:90`
- Modify: `web/src/styles.css` (sticky de `.list-view .filter-bar` a ≥2200 px)
- Test: `web/src/components/AppShell.test.ts`, `web/src/router.test.ts`

**Interfaces:**
- Produces: ruta con `meta: { flushContainer: true }`; AppShell añade `container--flush list-view` a `main.container` cuando la ruta lo declara; clase `.list-view .filter-bar` sticky a ≥2200 px.

- [ ] **Step 1: Write the failing tests**

```ts
it('uses the flush container for routes that opt in')
it('keeps the default container for other routes')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run 'AppShell|router'`
Expected: FAIL (clase/meta inexistentes).

- [ ] **Step 3: Implement the opt-in**

- `router.ts`: añade `meta: { flushContainer: true }` a `/` (cola), `/repos`, `/lead` y `/prs/:id`.
- `AppShell.vue`: `const flush = computed(() => route.meta.flushContainer === true)`; `:class="{ 'container--flush': flush, 'list-view': flush }"` en el `main`.
- `styles.css`: dentro de `@media (min-width: 2200px) { .list-view .filter-bar { position: sticky; top: 0; background: var(--bg); z-index: 5 } }`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run 'AppShell|router' && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/router.ts web/src/components/AppShell.vue web/src/styles.css web/src/components/AppShell.test.ts web/src/router.test.ts
git commit -m "feat(web): opt-in full-height layout per route"
```

---

### Task 2: Cola (Dashboard)

**Files:**
- Modify: `web/src/views/DashboardView.vue:369-386`
- Test: `web/src/views/DashboardView.test.ts`

**Interfaces:**
- Consumes: `useListFilters`, `FilterBar`, `FilterDropdown`, `FilterSheet`, `EmptyState`, `FilterTabs`, store `usePrsStore` (fields `prs`, `visible`, `counts`, `bucket`) y `session.me.teams`.
- Produces: facetas `team` y `sort` (`wait` | `newest` | `findings`); `q` sobre `title`, `repo` y `author_login`.

- [ ] **Step 1: Write the failing tests**

```ts
it('filters the queue by search text')
it('filters by team and sorts by wait, newest or findings')
it('keeps the status tabs and their counts while other filters apply')
it('opens the mobile sheet and applies the same filters')
it('shows the filters empty state with LIMPIAR')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run DashboardView`
Expected: FAIL (barra y facetas inexistentes).

- [ ] **Step 3: Implement the queue filters**

- `useListFilters({ facets: ['team'], sorts: ['wait', 'newest', 'findings'], defaultSort: 'wait' })`; `apply` parte de `store.visible` (ya filtrado por pestaña) y aplica `q` + `team`.
- Orden: `wait` = `waitMinutes(pr)` descendente; `newest` = `status_since` descendente; `findings` = suma de `open_findings` descendente. Reutiliza `waitMinutes`/`sortForQueue` de `qaQueue.ts`.
- `FilterBar` con `shown`/`total` (`total` = `store.visible.length` de la pestaña) y chips de team/sort; `FilterTabs` se mantiene encima. Un `select` de orden no: el orden es un `FilterDropdown` (no cuenta como filtro para `activeCount`).
- Lista dentro de `<div class="list-scroll">`; estados con `EmptyState` («NADA COINCIDE CON ESTOS FILTROS», acción LIMPIAR).
- Monta `FilterSheet` (abierto por `open-sheet`, con los desplegables duplicados dentro, `clearable`, `applyLabel="VER n PRs"`) y pasa `sheetOpen`/`sheetId` a `FilterBar`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run DashboardView`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/DashboardView.vue web/src/views/DashboardView.test.ts
git commit -m "feat(web): filter and sort the QA queue"
```

---

### Task 3: Repositorios

**Files:**
- Modify: `web/src/views/ReposView.vue:760-885`
- Test: `web/src/views/ReposView.test.ts`

**Interfaces:**
- Consumes: `Repository` (`full_name`, `enabled`, `team_id`, `private`, `has_policy`, `has_policy_override`, `policy_observation`, `account_login`), `teamName`, primitivas.
- Produces: facetas `account`, `team`, `visibility`, `policy`; segmentado `enabled` (`''` | `on` | `off`); columna «ID».

- [ ] **Step 1: Write the failing tests**

```ts
it('filters repositories by search, account, team, visibility and policy')
it('segments by automatic review with counts')
it('renders the repository id in its own right-aligned column')
it('shows the filters empty state when nothing matches')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run ReposView`
Expected: FAIL.

- [ ] **Step 3: Implement the repository filters**

- `team` incluye el valor `__none__` → «Sin equipo» (`team_id === null`); `visibility` `public`/`private`; `policy` con los literales reales de `policy_observation` (léelos en `api.ts` antes de construir las opciones) más «Con política» (`has_policy`) y «Con override» (`has_policy_override`).
- Segmentado de `enabled` con recuentos (Todas/Activada/Desactivada) usando el mismo patrón visual que los chips; participa en `facetCounts` como una faceta más.
- `FilterBar` fuera de `.table-wrap` (no dentro del overflow); lista en `.list-scroll`; `thead` sticky.
- Columna «ID» a la derecha: `th/td` estrechos, clase `.repo-table__id`, gris `#6B6B6B`, `font-variant-numeric: tabular-nums`, `border-left`.
- Monta `FilterSheet` con los mismos desplegables duplicados (`applyLabel="VER n repos"`) y pasa `sheetOpen`/`sheetId` a `FilterBar`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run ReposView && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/ReposView.vue web/src/views/ReposView.test.ts
git commit -m "feat(web): filter the repository registry"
```

---

### Task 4: Lead

**Files:**
- Modify: `web/src/views/LeadView.vue:433-462`
- Test: `web/src/views/LeadView.test.ts`

**Interfaces:**
- Consumes: `decisionRows(prs, slaSeconds, now)` y `DecisionRow` (`leadPanel.ts`), `statusLabel`, primitivas.
- Produces: chips de motivo (`reason`), faceta `repo`, `q` sobre `pr.title`/`pr.repo`/`pr.author_login`; el motivo sale de `decisionRows` (cliente).

- [ ] **Step 1: Write the failing tests**

```ts
it('filters decisions by reason chips with counts')
it('filters by repository and search text')
it('keeps the team tabs above the decisions table')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run LeadView`
Expected: FAIL.

- [ ] **Step 3: Implement the lead filters**

- Los motivos y sus recuentos salen de `decisionRows`; un chip por motivo presente, multiselección; `folio` de OR dentro del motivo.
- `repo` como `FilterDropdown` con opciones de los repos presentes; `q` filtra la tabla sin tocar el aside de umbrales.
- Barra fuera de `.decisions` (que tiene `overflow-x`), tabla en `.list-scroll`.
- Monta `FilterSheet` con motivo y repo duplicados (`applyLabel="VER n PRs"`) y pasa `sheetOpen`/`sheetId` a `FilterBar`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run LeadView`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/LeadView.vue web/src/views/LeadView.test.ts
git commit -m "feat(web): filter the lead decisions table"
```

---

### Task 5: Hallazgos en el detalle

**Files:**
- Modify: `web/src/views/PrDetailView.vue:52-78,763-887`
- Test: `web/src/views/PrDetailView.test.ts`

**Interfaces:**
- Consumes: `Finding` (`severity`, `status`, `triage_verdict`, `category`, `path`, `start_line`, `end_line`, `citations`), `findingLocation`, primitivas.
- Produces: chips de severidad (`blocker`/`major`/`minor`/`nit`) con recuento; segmentado de estado con el mapeo de la spec; categoría visible en cada tarjeta; `q` sobre `category`/`path`.

- [ ] **Step 1: Write the failing tests**

```ts
it('filters findings by severity chips and the triage segmented control')
it('searches findings by category and path')
it('renders the finding category on the card')
it('shows the empty state without hiding the QA aside')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run PrDetailView`
Expected: FAIL.

- [ ] **Step 3: Implement the findings filters**

- Mapeo del segmentado: Sin triar = `status === 'open' && !triage_verdict`; Confirmados = `triage_verdict === 'confirmed'`; Descartados = `triage_verdict === 'false_positive' || status === 'withdrawn'`; Resueltos = `status === 'resolved'`; Todos = sin condición.
- Aplicar el filtro **antes** de `groupedFindings`/`withdrawnFindings`, de modo que la agrupación por severidad y el bloque de retirados usen la lista filtrada.
- Barra y lista dentro de `.split__main`; el aside de QA no se filtra ni se oculta; LISTA en `.list-scroll`.
- La tarjeta muestra `category` junto a la ubicación (`findingLocation`).
- Monta `FilterSheet` con severidad y estado duplicados (`applyLabel="VER n hallazgos"`) y pasa `sheetOpen`/`sheetId` a `FilterBar`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run PrDetailView && npx vue-tsc --noEmit`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/PrDetailView.vue web/src/views/PrDetailView.test.ts
git commit -m "feat(web): filter findings in the pull request detail"
```

---

### Task 6: Gate y PR

- [ ] **Step 1: Run the web gate**

Run: `cd web && npm test -- --run && npx vue-tsc --noEmit`
Expected: toda la suite pasa.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin feat/panel-views
gh pr create --base main --head feat/panel-views \
  --title "feat(web): apply the panel filters to the list views" \
  --body-file - <<'EOF'
Second of three plans for the approved panel filter redesign.

Applies FilterBar/FilterDropdown/FilterSheet/useListFilters to the queue, the repository registry, the lead decisions table and the findings of the pull request detail, with the full-height layout behind a per-route opt-in.

Spec: docs/superpowers/specs/2026-11-26-panel-filters-design.md
EOF
```
