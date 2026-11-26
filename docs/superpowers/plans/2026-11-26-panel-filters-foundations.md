# Rediseño de filtros del panel — Fundaciones · Plan de implementación

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** dejar listo el sistema común de filtros (barra, desplegables, chips, hoja móvil, estado en URL) y el nuevo layout de shell (pantalla completa, solo la lista con scroll), sin cambiar todavía el contenido de ninguna vista.

**Architecture:** un composable `useListFilters` guarda `q`, facetas y orden y los sincroniza con la URL; componentes tontos (`FilterBar`, `FilterDropdown`, `FilterSheet`, `EmptyState`) los pintan. El shell pasa a cadena de alturas con una región de lista con scroll propio, y un único punto de estilo (`.list-scroll`) evita repetir reglas por vista.

**Tech Stack:** Vue 3 + TypeScript, vue-router, vitest + @vue/test-utils, CSS existente en `web/src/styles.css`.

**Spec:** `docs/superpowers/specs/2026-11-26-panel-filters-design.md`

## Global Constraints

- UI en español; código, tests y commits en inglés; Conventional Commits < 72 caracteres.
- Sin dependencias nuevas: componentes propios con `@vue/test-utils` y `vue-router` ya presentes.
- Los recuentos de facetas se calculan en cliente sobre la lista completa; no hay paginación.
- Toda la barra es accesible: roles ARIA de listbox, foco visible, teclado completo, `aria-label` por chip.
- El layout global no puede romper las vistas con scroll de página actuales (Agents, Settings, Setup, Setup, Login).
- Gate: `npm test -- --run` y `npx vue-tsc --noEmit` en `web/`. Nada de CI.

## Review Focus

- Facetas en la URL con valores desconocidos o repetidos: se ignoran, no rompen ni dejan la lista vacía.
- El recuento de cada opción refleja **el resto** de filtros (no su propia dimensión), así que una opción seleccionada nunca desaparece.
- Teclado en el desplegable: flechas con retorno circular, Enter selecciona, Esc cierra sin cambiar nada, clic fuera cierra.
- Hoja móvil: trampa y restauración de foco, Escape cierra, el fondo no hace scroll.
- Cambio de shell: las vistas que hoy dependen del scroll de página siguen funcionando dentro de la región de scroll.

---

### Task 1: Composable `useListFilters` con estado en URL

**Files:**
- Create: `web/src/composables/useListFilters.ts`
- Test: `web/src/composables/useListFilters.test.ts`

**Interfaces:**
- Consumes: `useRoute`, `useRouter` de `vue-router`.
- Produces:
  - `type FacetValues = Record<string, string[]>`
  - `useListFilters(options: { facets: readonly string[]; sorts?: readonly string[]; defaultSort?: string })`
  - Devuelve `{ q, facets, sort, isActive, activeCount, setFacet, toggleFacet, setSort, clear, apply, facetCounts }`, donde `apply<T>(items: T[], text: (item: T) => string, match: (item: T, facets: FacetValues) => boolean): ComputedRef<T[]>`.
  - `facetCounts<T>(items: T[], valuesOf: (item: T, facet: string) => string[]): ComputedRef<Record<string, Record<string, number>>>`: cuenta cada valor de cada faceta **ignorando su propia dimensión** (y aplicando texto y las demás), para que ninguna opción seleccionada desaparezca.

- [ ] **Step 1: Write the failing tests**

```ts
// URL con q y facetas válidas hidrata; valores desconocidos se ignoran; repetidos se deduplican.
it('hydrates valid filters from the URL and ignores unknown facets')
// toggleFacet añade y quita; clear vacía todo y borra las claves de la URL.
it('toggles facet values and clears the URL')
// apply filtra por texto y por facetas; isActive/activeCount cuentan valores seleccionados.
it('applies text and facets and reports activity')
// facetCounts cuenta cada dimensión con el resto de filtros aplicados, no con la suya.
it('counts facet options against the other filters only')
// sort inválido cae al defaultSort.
it('falls back to the default sort')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run useListFilters`
Expected: FAIL (módulo inexistente).

- [ ] **Step 3: Implement the composable**

- Hidratación en creación desde `route.query`: `q` string, facetas `string | string[]` (deduplicadas, descartando nombres fuera de `options.facets`), `sort` si está en `options.sorts`.
- Escritura con `router.replace({ query })` conservando claves ajenas de la ruta; omitir claves vacías.
- `apply` compone filtro de texto (case-insensitive, `trim`) y el predicado del consumidor sobre `facets`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run useListFilters`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/composables/useListFilters.ts web/src/composables/useListFilters.test.ts
git commit -m "feat(web): shared list filters with URL state"
```

---

### Task 2: `FilterDropdown`, listbox propio con recuentos

**Files:**
- Create: `web/src/components/FilterDropdown.vue`
- Test: `web/src/components/FilterDropdown.test.ts`

**Interfaces:**
- Props: `{ label: string; options: { value: string; label: string; count?: number }[]; modelValue: string[]; multiple?: boolean }`.
- Emits: `update:modelValue: [string[]]`.
- Contrato de teclado/ARIA: `role="listbox"` en el popover, `role="option"` con `aria-selected`, `aria-expanded` en la píldora, ✓ en seleccionadas, recuento a la derecha.

- [ ] **Step 1: Write the failing tests**

```ts
it('opens with Enter and selects with the keyboard')
it('does not change the selection when Escape closes')
it('shows a check and the count for each option')
it('single mode replaces the selection; multiple mode toggles')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run FilterDropdown`
Expected: FAIL (componente inexistente).

- [ ] **Step 3: Implement the component**

- Estado `open`, `activeIndex`; apertura con Enter/Espacio/flecha abajo; navegación circular; Enter selecciona; Escape cierra sin emitir; clic fuera y pérdida de foco cierran.
- La píldora muestra la etiqueta y, si hay selección en modo simple, el valor; borde blanco cuando hay selección.
- En modo múltiple, una opción "Todas" (valor `''`) limpia la selección.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run FilterDropdown`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/FilterDropdown.vue web/src/components/FilterDropdown.test.ts
git commit -m "feat(web): accessible filter dropdown"
```

---

### Task 3: `FilterBar` con búsqueda, contador, LIMPIAR y chips

**Files:**
- Create: `web/src/components/FilterBar.vue`
- Test: `web/src/components/FilterBar.test.ts`

**Interfaces:**
- Props: `{ q: string; shown: number; total: number; chips: { key: string; label: string }[] }`.
- Emits: `update:q: [string]`, `clear: []`, `remove: [key: string]`.
- Slot `default` para los desplegables; slot `sheet` con el botón móvil.

- [ ] **Step 1: Write the failing tests**

```ts
it('emits the search text')
it('renders N DE M with the active style when filters are on')
it('emits remove for a chip and clear for LIMPIAR')
it('keeps the bar in normal flow at narrow widths')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run FilterBar`
Expected: FAIL (componente inexistente).

- [ ] **Step 3: Implement the component**

- Píldora de búsqueda con placeholder "Buscar" y `maxlength=120`, sin submit.
- Contador `{{ shown }} DE {{ total }}` en clase `.filter-bar__count` (Doto, naranja con `--active`).
- `LIMPIAR` deshabilitado sin filtros; chips con `aria-label="Quitar filtro <label>"` y `×`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run FilterBar`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/FilterBar.vue web/src/components/FilterBar.test.ts
git commit -m "feat(web): shared filter bar"
```

---

### Task 4: `FilterSheet`, hoja inferior móvil

**Files:**
- Create: `web/src/components/FilterSheet.vue`
- Test: `web/src/components/FilterSheet.test.ts`

**Interfaces:**
- Props: `{ open: boolean; activeCount: number; shown: number }`.
- Emits: `close: []`, `clear: []`.
- Slot `default` con los desplegables apilados; el disparador lo pinta `FilterBar` (botón `FILTROS · n`).

- [ ] **Step 1: Write the failing tests**

```ts
it('traps focus and restores it on close')
it('closes with Escape and with the backdrop')
it('shows LIMPIAR / VER n PRs and emits clear')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run FilterSheet`
Expected: FAIL (componente inexistente).

- [ ] **Step 3: Implement the component**

- `Teleport` a `body`, fondo semitransparente, hoja inferior con `role="dialog"` y `aria-modal`.
- Bloquea el scroll del `body` mientras está abierto y lo restaura al cerrar.
- Foco inicial en el primer control; Tab/Shift+Tab atrapados; restaurar foco al disparador.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run FilterSheet`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/FilterSheet.vue web/src/components/FilterSheet.test.ts
git commit -m "feat(web): mobile filter sheet"
```

---

### Task 5: Shell a pantalla completa y `EmptyState`

**Files:**
- Modify: `web/src/styles.css:83,171-185,283-292` (shell, container, breakpoints)
- Modify: `web/src/components/AppShell.vue` (envolver el contenido en `.shell__body`)
- Create: `web/src/components/EmptyState.vue`
- Test: `web/src/components/AppShell.test.ts`, `web/src/components/EmptyState.test.ts`

**Interfaces:**
- `EmptyState` props `{ title: string; actionLabel?: string }`, emits `action: []`.
- Clases nuevas: `.shell__body` (flex: 1; min-height: 0), `.list-scroll` (flex: 1; min-height: 0; overflow-y: auto; scrollbar fina `#3A3A3A`), `.container--full` (sin tope).

- [ ] **Step 1: Write the failing tests**

```ts
// AppShell.test.ts
it('renders the scroll body between the bar and the routed view')
// EmptyState.test.ts
it('renders the title and emits action')
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npm test -- --run 'AppShell|EmptyState'`
Expected: FAIL (clase/componente inexistentes).

- [ ] **Step 3: Implement the layout and the component**

- `.shell { height: 100vh; overflow: hidden; display: flex; flex-direction: column }`; `.shell__body { flex: 1; min-height: 0; display: flex; flex-direction: column }`; `.shell__bar` deja de crecer.
- `.container` mantiene 1360 px por defecto y a partir de **2108 px** pasa a ancho completo con padding 40 px (sustituye la media de 2200 para el ancho; las reglas de `split` a 2200 se conservan).
- `.filter-bar { position: sticky; top: 0; background: var(--bg); z-index: 5 }` solo a ≥2200 px; `.list-scroll` con scrollbar fina y `thead` sticky.
- `AppShell.vue` envuelve el `<router-view>` (y el contenido scrolleable de cada vista) en `.shell__body`; las vistas existentes siguen funcionando porque mantienen su propio contenedor dentro.
- `EmptyState` pinta título y botón opcional, con la copia aprobada del diseño.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npm test -- --run 'AppShell|EmptyState' && npx vue-tsc --noEmit`
Expected: PASS y sin errores de tipos.

- [ ] **Step 5: Commit**

```bash
git add web/src/styles.css web/src/components/AppShell.vue web/src/components/EmptyState.vue web/src/components/AppShell.test.ts web/src/components/EmptyState.test.ts
git commit -m "feat(web): full-height shell and shared empty state"
```

---

### Task 6: Gate y PR de las fundaciones

- [ ] **Step 1: Run the web gate**

Run: `cd web && npm test -- --run && npx vue-tsc --noEmit`
Expected: toda la suite pasa.

- [ ] **Step 2: Push and open the PR**

```bash
git push -u origin feat/panel-redesign
gh pr create --base main --head feat/panel-redesign \
  --title "feat(web): filter foundations and full-height panel shell" \
  --body-file - <<'EOF'
First of three plans for the approved panel filter redesign (canvas v19).

Adds the shared filter primitives (URL-synced state, custom dropdown with counts, filter bar with N DE M / LIMPIAR / chips, mobile sheet, empty state) and the full-height shell where only the list scrolls. No view content changes yet; views adopt the primitives in the next plan.

Spec: docs/superpowers/specs/2026-11-26-panel-filters-design.md
EOF
```
