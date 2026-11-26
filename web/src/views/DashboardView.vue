<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { RouterLink } from 'vue-router'
import DotBar from '../components/DotBar.vue'
import DotMeter from '../components/DotMeter.vue'
import DotRing from '../components/DotRing.vue'
import EmptyState from '../components/EmptyState.vue'
import FilterBar from '../components/FilterBar.vue'
import FilterDropdown from '../components/FilterDropdown.vue'
import FilterSheet from '../components/FilterSheet.vue'
import FilterTabs from '../components/FilterTabs.vue'
import KpiCard from '../components/KpiCard.vue'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { useListFilters } from '../composables/useListFilters'
import PrCard from '../components/PrCard.vue'
import { api, type PRView, type Readiness } from '../api'
import type { DotTone } from '../dots'
import { isForbidden } from '../errors'
import { formatWait, isOverSla, qaBucket, waitMinutes } from '../qaQueue'
import { usePrsStore, type BucketFilter } from '../stores/prs'
import { useSessionStore } from '../stores/session'

const prsStore = usePrsStore()
const session = useSessionStore()

const readiness = ref<Readiness | null>(null)
const now = ref(new Date())
const retrying = ref<number | null>(null)
const retryError = ref<string | null>(null)
let clock: ReturnType<typeof setInterval> | null = null

const SLA_DOTS = 8

const readinessLinkLabels: Record<string, string> = {
  '/repos': 'Ir a repositorios',
  '/teams': 'Ir a equipos',
  '/agents': 'Ir a perfiles LLM',
}

function readinessLinkLabel(link: string): string {
  return readinessLinkLabels[link] ?? 'Resolver'
}

async function loadReadiness(): Promise<void> {
  const me = await session.load()
  if (!me?.is_admin) {
    readiness.value = null
    return
  }
  try {
    readiness.value = await api.readiness()
  } catch {
    readiness.value = null
  }
}

const roleNames: Record<string, string> = { qa: 'QA', lead: 'lead', developer: 'dev' }

const greeting = computed(() => {
  const me = session.me
  if (!me) return ''
  const roles = me.teams.map((team) => `${roleNames[team.role] ?? team.role} en ${team.name}`)
  return [`Hola, ${me.login}`, ...roles].join(' · ')
})

const slaSeconds = computed(() => prsStore.summary?.validation_sla_seconds ?? 0)
const slaLabel = computed(() => {
  const hours = slaSeconds.value / 3600
  return Number.isInteger(hours) ? `${hours} h` : formatWait(Math.round(slaSeconds.value / 60))
})

const oldestReady = computed(() => prsStore.queue.filter((pr) => qaBucket(pr) === 'ready').slice(0, 3))

const waitingRows = computed(() =>
  oldestReady.value.map((pr) => {
    const minutes = waitMinutes(pr, now.value)
    const late = isOverSla(pr, slaSeconds.value, now.value)
    const lit = slaSeconds.value > 0 ? Math.min(SLA_DOTS, Math.round(((minutes * 60) / slaSeconds.value) * SLA_DOTS)) : 0
    return {
      pr,
      late,
      wait: formatWait(minutes),
      segments: [
        { count: lit, tone: (late ? 'bad' : 'on') as DotTone },
        { count: SLA_DOTS - lit, tone: 'off' as DotTone },
      ],
    }
  }),
)

const SORT_LABELS: Record<string, string> = {
  wait: 'Más espera',
  newest: 'Más recientes',
  findings: 'Más hallazgos',
}

const ORDER_SUMMARIES: Record<string, string> = {
  wait: 'ordenados por tiempo de espera',
  newest: 'los más recientes primero',
  findings: 'ordenados por hallazgos abiertos',
}

const { q, facets, sort, isActive, activeCount, setFacet, setSort, clear, apply, facetCounts } =
  useListFilters({ facets: ['team'], sorts: ['wait', 'newest', 'findings'], defaultSort: 'wait' })

const sheetOpen = ref(false)
const sheetId = `queue-filters-${useId()}`

function teamKey(pr: Pick<PRView, 'team_id'>): string {
  return pr.team_id === null ? '__none__' : String(pr.team_id)
}

function searchText(pr: PRView): string {
  return [pr.title, pr.repo, pr.author_login].join(' ')
}

function findingsCount(pr: PRView): number {
  const { blocker, major, minor, nit } = pr.open_findings
  return blocker + major + minor + nit
}

const teamNames = computed(() => new Map((session.me?.teams ?? []).map((team) => [String(team.id), team.name])))

function teamLabel(key: string): string {
  if (key === '__none__') return 'Sin equipo'
  return teamNames.value.get(key) ?? `Equipo ${key}`
}

function matchesQueueFilters(pr: PRView, active: { team?: string[] }): boolean {
  const teams = active.team ?? []
  return teams.length === 0 || teams.includes(teamKey(pr))
}

const filteredQueue = apply(() => prsStore.visible, searchText, matchesQueueFilters)

// The tabs count the whole queue with the bar's filters applied, not only the
// current tab, so their figures follow the search and team selection.
const filteredTab = apply(() => prsStore.prs, searchText, matchesQueueFilters)

const tabCounts = computed<Record<BucketFilter, number>>(() => {
  const counts: Record<BucketFilter, number> = {
    '': 0,
    ready: 0,
    defects: 0,
    inconclusive: 0,
    ai: 0,
    other: 0,
  }
  for (const pr of filteredTab.value) {
    counts['']++
    counts[qaBucket(pr)]++
  }
  return counts
})

const teamCounts = facetCounts(
  () => prsStore.visible,
  (pr) => [teamKey(pr)],
  searchText,
)

const teamOptions = computed(() => {
  const values = new Set<string>()
  for (const pr of prsStore.prs) values.add(teamKey(pr))
  for (const value of facets.value.team) values.add(value)
  return [...values]
    .sort((a, b) => teamLabel(a).localeCompare(teamLabel(b), 'es'))
    .map((value) => ({ value, label: teamLabel(value), count: teamCounts.value.team?.[value] ?? 0 }))
})

const sortOptions = computed(() => Object.entries(SORT_LABELS).map(([value, label]) => ({ value, label })))

const sortSelection = computed(() => [sort.value ?? 'wait'])

// The order is a view of the same rows, not a filter: it never adds to activeCount.
const queue = computed(() => {
  const items = [...filteredQueue.value]
  if (sort.value === 'newest') {
    items.sort((a, b) => new Date(b.status_since).getTime() - new Date(a.status_since).getTime())
  } else if (sort.value === 'findings') {
    items.sort((a, b) => findingsCount(b) - findingsCount(a))
  } else {
    items.sort((a, b) => waitMinutes(b, now.value) - waitMinutes(a, now.value))
  }
  return items
})

const chips = computed(() => {
  const list = facets.value.team.map((value) => ({ key: `team:${value}`, label: teamLabel(value) }))
  const currentSort = sort.value ?? 'wait'
  if (currentSort !== 'wait') {
    list.push({ key: `sort:${currentSort}`, label: `Orden: ${SORT_LABELS[currentSort] ?? currentSort}` })
  }
  return list
})

function setQuery(value: string): void {
  q.value = value
}

function onSort(values: string[]): void {
  setSort(values[0])
}

function removeChip(key: string): void {
  if (key.startsWith('team:')) {
    setFacet('team', facets.value.team.filter((value) => `team:${value}` !== key))
    return
  }
  if (key.startsWith('sort:')) setSort('wait')
}

function bar(label: string, count: number, tone: DotTone) {
  return { label, count, tone }
}

const queueSummary = computed(() => {
  const names = (session.me?.teams ?? []).map((team) => team.name)
  const own = names.length > 1 ? `${names.slice(0, -1).join(', ')} y ${names[names.length - 1]}` : names[0]
  const teams = session.me?.is_admin ? 'Todos los equipos' : own
  const count = prsStore.prs.length
  const open = `${count} ${count === 1 ? 'PR abierto' : 'PRs abiertos'}`
  return [teams, open, ORDER_SUMMARIES[sort.value ?? 'wait']].filter(Boolean).join(' · ')
})

const untriaged = computed(() => {
  const counts = prsStore.summary?.untriaged
  if (!counts) return { total: 0, rows: [] }
  return {
    total: counts.blocker + counts.major + counts.minor + counts.nit,
    rows: [
      bar('Bloqueantes', counts.blocker, 'bad'),
      bar('Mayores', counts.major, 'on'),
      bar('Menores', counts.minor, 'dim'),
      bar('Nits', counts.nit, 'dim'),
    ],
  }
})

const precision = computed(() => {
  const confirmed = prsStore.summary?.confirmed ?? 0
  const falsePositive = prsStore.summary?.false_positive ?? 0
  const decided = confirmed + falsePositive
  return {
    confirmed,
    falsePositive,
    decided,
    percent: decided === 0 ? 0 : Math.round((confirmed / decided) * 100),
  }
})

const categories = computed(() =>
  (prsStore.summary?.categories ?? []).map((item, index) =>
    bar(item.category, item.count, index === 0 ? 'bad' : 'on'),
  ),
)

const inconclusive = computed(() =>
  prsStore.queue.filter((pr) => qaBucket(pr) === 'inconclusive').slice(0, 3),
)

const running = computed(() => prsStore.summary?.runs_running ?? 0)
const queued = computed(() => prsStore.summary?.runs_queued ?? 0)

async function retry(id: number): Promise<void> {
  retrying.value = id
  retryError.value = null
  try {
    await api.retryPR(id)
    await prsStore.refresh()
  } catch (err) {
    retryError.value = isForbidden(err)
      ? 'Solo QA o el lead del equipo pueden reintentar'
      : 'No se pudo reintentar la revisión'
  } finally {
    retrying.value = null
  }
}

function changeBucket(next: BucketFilter): void {
  prsStore.setBucket(next)
}

useLiveRefresh(['pr.updated', 'repo.updated'], () => prsStore.refresh({ silent: true }))

onMounted(() => {
  void loadReadiness()
  void prsStore.refresh()
  clock = setInterval(() => {
    now.value = new Date()
  }, 60_000)
})

onBeforeUnmount(() => {
  if (clock !== null) clearInterval(clock)
})
</script>

<template>
  <section class="dashboard">
    <header class="page-header">
      <div class="dashboard__title">
        <h1 class="page-title">Mi cola de QA</h1>
        <p class="page-subtitle">
          <span>{{ greeting }}<template v-if="slaSeconds > 0"> · plazo de validación {{ slaLabel }}</template></span>
        </p>
      </div>
    </header>

    <section
      v-if="session.me?.is_admin && readiness && !readiness.ready"
      class="tile tile--alert readiness"
      data-readiness-banner
      aria-labelledby="readiness-title"
    >
      <div>
        <h2 id="readiness-title" class="tile__title readiness__title">! Configuración pendiente</h2>
        <p class="readiness__hint">Completa estos pasos para que la revisión automática se ejecute.</p>
      </div>
      <ul class="readiness__list">
        <li
          v-for="item in readiness.items"
          :key="item.key"
          class="readiness__item"
          :data-readiness-item="item.key"
        >
          <span>{{ item.label }}</span>
          <RouterLink class="button button--light" :to="item.link">
            {{ readinessLinkLabel(item.link) }}
          </RouterLink>
        </li>
      </ul>
    </section>

    <p v-if="retryError" class="form-error" role="alert">{{ retryError }}</p>

    <div class="split">
      <div class="split__aside">
        <section class="tile-grid" aria-label="Resumen de QA">
          <article class="tile" data-tile="ready">
            <h2 class="tile__title"><span class="tile__index">01</span>Para validar</h2>
            <KpiCard label="PRs listos para ti" :value="prsStore.summary?.ready_for_human ?? 0" />
            <ul v-if="waitingRows.length > 0" class="waiting">
              <li v-for="row in waitingRows" :key="row.pr.id" class="waiting__row">
                <div class="waiting__line">
                  <span>{{ row.pr.repo }} #{{ row.pr.number }}</span>
                  <span :class="{ 'is-late': row.late }">{{ row.wait }}{{ row.late ? ' · fuera de plazo' : '' }}</span>
                </div>
                <DotMeter
                  v-if="slaSeconds > 0"
                  :segments="row.segments"
                  :label="`Espera ${row.wait} de un plazo de ${slaLabel}`"
                />
              </li>
            </ul>
            <div v-else class="empty-bar">
              <p class="tile__empty">Nada esperando validación.</p>
              <DotBar :count="0" tone="on" />
            </div>
            <RouterLink
              v-if="oldestReady.length > 0"
              class="button button--light tile__foot"
              :to="`/prs/${oldestReady[0].id}`"
              data-start-oldest
            >
              Empezar por el más antiguo →
            </RouterLink>
          </article>

          <article class="tile" data-tile="triage">
            <h2 class="tile__title"><span class="tile__index">02</span>Triage de hallazgos</h2>
            <KpiCard label="Sin triar" :value="untriaged.total" />
            <p class="tile__hint">Confirma lo que es real y marca los falsos positivos antes de validar.</p>
            <ul class="bars tile__foot">
              <li v-for="row in untriaged.rows" :key="row.label" class="bars__row">
                <span class="caption">{{ row.label }}</span>
                <DotBar :count="row.count" :tone="row.tone" :label="`${row.count} ${row.label.toLowerCase()} sin triar`" />
                <span class="bars__count" :class="{ 'is-late': row.tone === 'bad' && row.count > 0 }">{{ row.count }}</span>
              </li>
            </ul>
          </article>

          <article class="tile" data-tile="precision">
            <h2 class="tile__title">
              <span class="tile__index">03</span>Precisión de la IA ·
              {{ prsStore.summary?.precision_window_days ?? 7 }} días
            </h2>
            <div class="ring-row">
              <DotRing
                :segments="[
                  { value: precision.confirmed, tone: 'on' },
                  { value: precision.falsePositive, tone: 'bad' },
                ]"
                :label="
                  precision.decided > 0
                    ? `${precision.percent}% de hallazgos confirmados: ${precision.confirmed} confirmados, ${precision.falsePositive} falsos positivos`
                    : 'Acierto sin datos'
                "
              >
                <span v-if="precision.decided > 0" class="ring-row__figure" data-precision>{{ precision.percent }}%</span>
                <span v-else class="ring-row__figure" aria-hidden="true">—</span>
                <span class="caption">Acierto</span>
              </DotRing>
              <dl class="legend">
                <div>
                  <dt class="caption"><span class="dot dot--on" />Confirmados</dt>
                  <dd>{{ precision.confirmed }}</dd>
                </div>
                <div>
                  <dt class="caption"><span class="dot dot--bad" />Falsos positivos</dt>
                  <dd :class="{ 'is-late': precision.falsePositive > 0 }">{{ precision.falsePositive }}</dd>
                </div>
              </dl>
            </div>
            <p v-if="precision.decided === 0" class="tile__empty">Aún no hay hallazgos triados en este periodo.</p>
          </article>

          <article
            class="tile"
            :class="{ 'tile--alert': (prsStore.summary?.inconclusive ?? 0) > 0 }"
            data-tile="inconclusive"
          >
            <h2 class="tile__title"><span class="tile__index">04</span>Inconclusos</h2>
            <KpiCard
              label="La IA no terminó"
              :tone="(prsStore.summary?.inconclusive ?? 0) > 0 ? 'bad' : 'default'"
              :value="prsStore.summary?.inconclusive ?? 0"
            />
            <ul v-if="inconclusive.length > 0" class="rows tile__foot">
              <li v-for="pr in inconclusive" :key="pr.id" class="rows__item">
                <RouterLink :to="`/prs/${pr.id}`" class="rows__link">{{ pr.repo }} #{{ pr.number }}</RouterLink>
                <button
                  class="button"
                  type="button"
                  :data-retry-pr="pr.id"
                  :disabled="retrying === pr.id"
                  @click="retry(pr.id)"
                >
                  {{ retrying === pr.id ? 'Reintentando…' : 'Reintentar' }}
                </button>
              </li>
            </ul>
            <p v-else class="tile__empty">Todas las revisiones terminaron.</p>
          </article>

          <article class="tile" data-tile="agent">
            <h2 class="tile__title"><span class="tile__index">05</span>Agente IA</h2>
            <p class="agent__state">{{ running > 0 ? 'Revisando…' : 'En espera' }}</p>
            <svg class="agent__face" width="84" height="84" viewBox="0 0 96 96" aria-hidden="true">
              <circle cx="48" cy="48" r="40" fill="none" stroke="currentColor" stroke-width="3.5" stroke-linecap="round" stroke-dasharray="0 7.33" />
              <line x1="34" y1="46" x2="40" y2="46" class="agent__eye" stroke-width="4" stroke-linecap="round" />
              <line x1="56" y1="46" x2="62" y2="46" class="agent__eye" stroke-width="4" stroke-linecap="round" />
            </svg>
            <svg class="agent__pulse" width="100%" height="36" viewBox="0 0 280 40" aria-hidden="true" data-agent-pulse>
              <template v-if="running > 0">
                <polyline
                  points="0,20 64,20 70,15 76,25 82,8 88,32 94,4 100,36 106,10 112,29 118,17 124,22 176,20 182,13 188,27 194,20 280,20"
                  class="agent__pulse-track"
                />
                <polyline points="76,25 82,8 88,32 94,4 100,36 106,10 112,29" class="agent__pulse-live" />
              </template>
              <polyline v-else points="0,20 280,20" class="agent__pulse-track" />
            </svg>
            <dl class="stats tile__foot">
              <div>
                <dt class="caption">En curso</dt>
                <dd data-runs-running>{{ running }}</dd>
              </div>
              <div>
                <dt class="caption">En cola</dt>
                <dd data-runs-queued>{{ queued }}</dd>
              </div>
            </dl>
          </article>

          <article class="tile" data-tile="categories">
            <h2 class="tile__title"><span class="tile__index">06</span>Dónde probar a mano</h2>
            <p class="tile__hint">
              Defectos confirmados por categoría, últimos {{ prsStore.summary?.category_window_days ?? 30 }} días.
            </p>
            <ul v-if="categories.length > 0" class="bars tile__foot">
              <li v-for="row in categories" :key="row.label" class="bars__row">
                <span class="caption">{{ row.label }}</span>
                <DotBar :count="row.count" :tone="row.tone" :label="`${row.count} defectos de ${row.label}`" />
                <span class="bars__count">{{ row.count }}</span>
              </li>
            </ul>
            <div v-else class="empty-bar tile__foot">
              <p class="tile__empty">Sin defectos confirmados todavía.</p>
              <DotBar :count="0" tone="on" />
            </div>
          </article>
        </section>
      </div>

      <div class="split__main">
        <section class="pr-section" aria-labelledby="queue-title">
          <div class="pr-section__head queue__head">
            <div>
              <h2 id="queue-title" class="pr-section__title">PRs de mis equipos</h2>
              <p class="page-subtitle" data-queue-summary>{{ queueSummary }}</p>
            </div>
            <FilterTabs :model-value="prsStore.bucket" :counts="tabCounts" @update:model-value="changeBucket" />
          </div>

          <FilterBar
            :q="q"
            :shown="queue.length"
            :total="prsStore.visible.length"
            :chips="chips"
            :active-count="activeCount"
            :sheet-open="sheetOpen"
            :sheet-id="sheetId"
            @update:q="setQuery"
            @clear="clear"
            @remove="removeChip"
            @open-sheet="sheetOpen = true"
          >
            <FilterDropdown
              label="Equipo"
              :options="teamOptions"
              :model-value="facets.team"
              multiple
              @update:model-value="setFacet('team', $event)"
            />
            <FilterDropdown
              label="Orden"
              :options="sortOptions"
              :model-value="sortSelection"
              :active="(sort ?? 'wait') !== 'wait'"
              @update:model-value="onSort"
            />
          </FilterBar>

          <div class="list-scroll queue__scroll">
            <p v-if="prsStore.error" class="empty-state">{{ prsStore.error }}</p>
            <EmptyState
              v-else-if="!prsStore.loading && queue.length === 0 && isActive"
              title="NADA COINCIDE CON ESTOS FILTROS"
              action-label="LIMPIAR FILTROS"
              @action="clear"
            />
            <p v-else-if="!prsStore.loading && queue.length === 0" class="empty-state">
              Nada pendiente en este filtro.
            </p>
            <div v-else class="pr-grid">
              <PrCard v-for="pr in queue" :key="pr.id" :pr="pr" :sla-seconds="slaSeconds" :now="now" />
            </div>
          </div>
        </section>

        <FilterSheet
          :id="sheetId"
          :open="sheetOpen"
          :active-count="activeCount"
          :shown="queue.length"
          clearable
          :apply-label="`VER ${queue.length} PRs`"
          @close="sheetOpen = false"
          @clear="clear"
        >
          <div class="queue__sheet-filters">
            <FilterDropdown
              label="Equipo"
              :options="teamOptions"
              :model-value="facets.team"
              multiple
              @update:model-value="setFacet('team', $event)"
            />
            <FilterDropdown
              label="Orden"
              :options="sortOptions"
              :model-value="sortSelection"
              :active="(sort ?? 'wait') !== 'wait'"
              @update:model-value="onSort"
            />
          </div>
        </FilterSheet>
      </div>
    </div>
  </section>
</template>

<style scoped>
.dashboard {
  gap: 36px;
}

.dashboard__title {
  min-width: 0;
}

.readiness {
  flex-direction: row;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
}

.readiness__title {
  color: var(--accent);
  font-weight: 700;
}

.readiness__hint,
.tile__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 13px;
}

.readiness__list {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
  margin: 0;
  padding: 0;
  list-style: none;
}

.readiness__item {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-1) var(--space-1) var(--space-1) var(--space-4);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
  font-size: 13px;
}

.tile__empty {
  color: var(--text-faint);
  font-size: 13px;
}

.is-late {
  color: var(--accent);
  font-weight: 700;
}

.waiting,
.bars,
.rows {
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0;
  list-style: none;
}

.waiting,
.bars {
  gap: var(--space-3);
}

.waiting__row {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.waiting__line {
  display: flex;
  justify-content: space-between;
  gap: var(--space-3);
  font-size: 12px;
}

.bars__row {
  display: grid;
  grid-template-columns: 112px minmax(0, 1fr) 32px;
  align-items: center;
  gap: 10px;
}

.bars__row .caption {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.bars__count {
  text-align: right;
  font-size: 12px;
  font-weight: 700;
}

.rows__item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-3) 0;
  border-bottom: var(--rule-hair) dashed var(--border);
  font-size: 13px;
}

.rows__item:last-child {
  border-bottom: 0;
}

.rows__link {
  min-width: 0;
  overflow-wrap: anywhere;
  text-decoration: none;
}

.ring-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
  flex-grow: 1;
}

.ring-row__figure {
  font-family: var(--font-display);
  font-size: 40px;
  font-weight: 900;
  line-height: 1;
}

.legend {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.legend .dot {
  display: inline-block;
  margin-right: var(--space-2);
}

.legend dd,
.stats dd {
  margin-top: var(--space-1);
  font-family: var(--font-display);
  font-size: 26px;
  font-weight: 800;
  font-variant-numeric: tabular-nums;
}

.agent__state {
  font-family: var(--font-display);
  font-size: 34px;
  font-weight: 900;
  line-height: 1;
  text-transform: uppercase;
}

.agent__face {
  align-self: center;
  color: var(--dot-on);
}

.agent__eye {
  stroke: var(--accent);
}

.agent__pulse polyline {
  fill: none;
  stroke-width: 3;
  stroke-linecap: round;
  stroke-dasharray: 0 6;
}

.agent__pulse-track {
  stroke: var(--dot-dim);
}

.agent__pulse-live {
  stroke: var(--accent);
}

.stats {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3);
}

.stats > div + div {
  padding-left: var(--space-3);
  border-left: var(--rule-hair) solid var(--border);
}

.queue__head {
  flex-wrap: wrap;
  align-items: flex-end;
}

/* The shell gives this view the leftover height. Below the split breakpoint
   the whole dashboard scrolls; at wide widths only the queue does. */
.dashboard {
  flex: 1;
  min-height: 0;
}

.queue__scroll {
  padding-right: var(--space-1);
}

@media (max-width: 2199.98px) {
  .dashboard {
    overflow-y: auto;
  }

  .queue__scroll {
    overflow-y: visible;
  }
}

@media (min-width: 2200px) {
  .split,
  .split__main,
  .pr-section {
    min-height: 0;
  }

  .split {
    flex: 1;
    grid-template-rows: minmax(0, 1fr);
    align-items: stretch;
  }

  .split__aside {
    overflow-y: auto;
  }

  .pr-section {
    flex: 1;
  }
}

@media (max-width: 480px) {
  .readiness__item {
    width: 100%;
    justify-content: space-between;
  }
}
</style>
