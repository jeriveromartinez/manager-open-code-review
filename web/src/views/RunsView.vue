<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { RouterLink } from 'vue-router'
import EmptyState from '../components/EmptyState.vue'
import FilterBar from '../components/FilterBar.vue'
import FilterDropdown from '../components/FilterDropdown.vue'
import FilterSheet from '../components/FilterSheet.vue'
import { useListFilters } from '../composables/useListFilters'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { api, type ActiveRun, type RunHistoryPage, type RunHistoryRow } from '../api'
import { formatElapsed, phaseLabel } from '../runPhase'

type RunCard = ActiveRun &
  Partial<Pick<RunHistoryRow, 'error_code' | 'error_message' | 'finished_at' | 'duration_seconds'>>

const HISTORY_LIMIT = 50

const STATUS_LABELS: Record<string, string> = {
  queued: 'En cola',
  running: 'En curso',
  succeeded: 'Completada',
  inconclusive: 'Inconclusa',
  failed: 'Fallida',
  superseded: 'Reemplazada',
}

const STATUS_ORDER = ['queued', 'running', 'succeeded', 'inconclusive', 'failed', 'superseded']

const STATUS_TONES: Record<string, string> = {
  queued: 'muted',
  running: 'busy',
  succeeded: 'ok',
  failed: 'bad',
  inconclusive: 'muted',
  superseded: 'muted',
}

const activeRuns = ref<ActiveRun[]>([])
const historyRuns = ref<RunHistoryRow[]>([])
const cursor = ref<{ before: number; createdAt: string } | null>(null)
const hasMore = ref(false)
const loading = ref(true)
const loadingMore = ref(false)
const error = ref<string | null>(null)
const refreshError = ref<string | null>(null)
const now = ref(new Date())
let clock: ReturnType<typeof setInterval> | null = null

const { q, facets, isActive, activeCount: filterCount, setFacet, clear, apply, facetCounts } =
  useListFilters({ facets: ['status', 'repo', 'trigger'] })

const sheetOpen = ref(false)
const sheetId = `runs-filters-${useId()}`

function isLiveRun(run: RunCard): boolean {
  return run.status === 'running' || run.status === 'queued'
}

function statusLabel(status: string): string {
  return STATUS_LABELS[status] ?? status
}

function statusTone(status: string): string {
  return STATUS_TONES[status] ?? 'muted'
}

function statusRank(status: string): number {
  const index = STATUS_ORDER.indexOf(status)
  return index === -1 ? STATUS_ORDER.length : index
}

// Active wins over a stale history copy of the same run: the live row carries
// the phase the finished one lacks.
const mergedRuns = computed<RunCard[]>(() => {
  const byId = new Map<number, RunCard>()
  for (const run of historyRuns.value) byId.set(run.id, run)
  for (const run of activeRuns.value) byId.set(run.id, run)
  return [...byId.values()].sort(
    (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime() || b.id - a.id,
  )
})

const runningCount = computed(() => mergedRuns.value.filter((run) => run.status === 'running').length)
const queuedCount = computed(() => mergedRuns.value.filter((run) => run.status === 'queued').length)
const finishedCount = computed(() => mergedRuns.value.filter((run) => !isLiveRun(run)).length)

const countText = computed(() => {
  if (mergedRuns.value.length === 0) return 'sin revisiones'
  const parts: string[] = []
  if (runningCount.value > 0) parts.push(`${runningCount.value} en curso`)
  if (queuedCount.value > 0) parts.push(`${queuedCount.value} en cola`)
  if (finishedCount.value > 0) {
    parts.push(`${finishedCount.value} terminada${finishedCount.value === 1 ? '' : 's'}`)
  }
  return parts.join(' · ')
})

function searchText(run: RunCard): string {
  return `${run.pr_title} ${run.repo}`
}

function facetValues(run: RunCard, facet: string): string[] {
  if (facet === 'status') return [run.status]
  if (facet === 'repo') return [run.repo]
  return run.trigger ? [run.trigger] : []
}

const filteredRuns = apply(
  () => mergedRuns.value,
  searchText,
  (run, active) => {
    const statuses = active.status ?? []
    const repos = active.repo ?? []
    const triggers = active.trigger ?? []
    return (
      (statuses.length === 0 || statuses.includes(run.status)) &&
      (repos.length === 0 || repos.includes(run.repo)) &&
      (triggers.length === 0 || triggers.includes(run.trigger))
    )
  },
)

const counts = facetCounts(() => mergedRuns.value, facetValues, searchText)

const statusOptions = computed(() => {
  const values = new Set(mergedRuns.value.map((run) => run.status))
  for (const value of facets.value.status) values.add(value)
  return [...values]
    .sort((a, b) => statusRank(a) - statusRank(b) || a.localeCompare(b, 'es'))
    .map((value) => ({ value, label: statusLabel(value), count: counts.value.status?.[value] ?? 0 }))
})

const repoOptions = computed(() => {
  const values = new Set(mergedRuns.value.map((run) => run.repo))
  for (const value of facets.value.repo) values.add(value)
  return [...values]
    .sort((a, b) => a.localeCompare(b, 'es'))
    .map((repo) => ({ value: repo, label: repo, count: counts.value.repo?.[repo] ?? 0 }))
})

const triggerOptions = computed(() => {
  const values = new Set(mergedRuns.value.map((run) => run.trigger))
  for (const value of facets.value.trigger) values.add(value)
  return [...values]
    .sort((a, b) => a.localeCompare(b, 'es'))
    .map((trigger) => ({ value: trigger, label: trigger, count: counts.value.trigger?.[trigger] ?? 0 }))
})

const chips = computed(() => [
  ...facets.value.status.map((value) => ({ key: `status:${value}`, label: statusLabel(value) })),
  ...facets.value.repo.map((value) => ({ key: `repo:${value}`, label: `Repo: ${value}` })),
  ...facets.value.trigger.map((value) => ({ key: `trigger:${value}`, label: `Origen: ${value}` })),
])

function setQuery(value: string): void {
  q.value = value
}

function removeChip(key: string): void {
  const separator = key.indexOf(':')
  if (separator < 0) return
  const facet = key.slice(0, separator)
  const value = key.slice(separator + 1)
  setFacet(
    facet,
    (facets.value[facet] ?? []).filter((entry) => entry !== value),
  )
}

function phaseText(run: RunCard): string {
  if (!isLiveRun(run)) return '—'
  const label = phaseLabel(run.phase)
  if (!run.phase_started_at) return label
  return `${label} · ${formatElapsed(run.phase_started_at, now.value)}`
}

function timeLabel(run: RunCard): string {
  if (run.status === 'running') return 'En curso desde hace'
  if (run.status === 'queued') return 'En cola desde hace'
  return 'Duración'
}

// A live run counts up to the current clock; a finished one is a fixed span
// between its own timestamps, and «—» when either is missing.
function timeValue(run: RunCard): string {
  if (run.status === 'running') return formatElapsed(run.started_at ?? run.created_at, now.value)
  if (run.status === 'queued') return formatElapsed(run.created_at, now.value)
  if (!run.started_at || !run.finished_at) return '—'
  return formatElapsed(run.started_at, new Date(run.finished_at))
}

function mergeHistory(primary: RunHistoryRow[], existing: RunHistoryRow[]): RunHistoryRow[] {
  const byId = new Map<number, RunHistoryRow>()
  for (const run of existing) byId.set(run.id, run)
  for (const run of primary) byId.set(run.id, run)
  return [...byId.values()]
}

// The pair is the keyset cursor; with either half missing there is no next
// page, so the button never asks for a page the server cannot resolve.
function applyCursor(page: RunHistoryPage): void {
  if (page.has_more && page.next_before > 0 && page.next_created_at) {
    hasMore.value = true
    cursor.value = { before: page.next_before, createdAt: page.next_created_at }
    return
  }
  hasMore.value = false
  cursor.value = null
}

async function fetchFirstPage(): Promise<{ active: ActiveRun[]; page: RunHistoryPage }> {
  const [active, page] = await Promise.all([api.activeRuns(), api.runHistory({ limit: HISTORY_LIMIT })])
  return { active: active.runs, page }
}

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const { active, page } = await fetchFirstPage()
    activeRuns.value = active
    historyRuns.value = page.runs
    applyCursor(page)
    refreshError.value = null
  } catch {
    error.value = 'No se pudieron cargar las revisiones'
  } finally {
    loading.value = false
  }
}

// A silent refresh keeps the pages already loaded and the list on screen; a
// failure leaves the last good rows in place and warns with a note.
async function refresh(): Promise<void> {
  try {
    const { active, page } = await fetchFirstPage()
    activeRuns.value = active
    historyRuns.value = mergeHistory(page.runs, historyRuns.value)
    if (!hasMore.value) applyCursor(page)
    refreshError.value = null
  } catch {
    refreshError.value = 'No se pudo actualizar; se muestran las últimas revisiones.'
  }
}

async function loadMore(): Promise<void> {
  if (loadingMore.value || !cursor.value) return
  loadingMore.value = true
  try {
    const page = await api.runHistory({
      before: cursor.value.before,
      beforeCreatedAt: cursor.value.createdAt,
      limit: HISTORY_LIMIT,
    })
    historyRuns.value = mergeHistory(page.runs, historyRuns.value)
    applyCursor(page)
    refreshError.value = null
  } catch {
    refreshError.value = 'No se pudo cargar más historial.'
  } finally {
    loadingMore.value = false
  }
}

useLiveRefresh(['run.updated', 'pr.updated'], () => refresh())

onMounted(() => {
  clock = setInterval(() => (now.value = new Date()), 1000)
  void load()
})

onBeforeUnmount(() => {
  if (clock !== null) clearInterval(clock)
})
</script>

<template>
  <section class="runs">
    <header class="page-header">
      <div>
        <h1 class="page-title">Revisiones</h1>
        <p class="page-subtitle" data-runs-count>{{ countText }}</p>
      </div>
      <button class="button button--ghost" type="button" data-runs-refresh :disabled="loading" @click="load()">
        Actualizar
      </button>
    </header>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <p v-if="refreshError" class="caption" role="status" data-runs-refresh-error>{{ refreshError }}</p>

    <p v-if="loading && mergedRuns.length === 0" class="empty-state">Cargando revisiones…</p>

    <template v-else-if="!error">
      <p v-if="mergedRuns.length === 0" class="empty-state" data-runs-empty>
        No hay revisiones en curso ni terminadas. Cuando un PR necesite revisión aparecerá aquí.
      </p>

      <template v-else>
        <FilterBar
          :q="q"
          :shown="filteredRuns.length"
          :total="mergedRuns.length"
          :chips="chips"
          :active-count="filterCount"
          :sheet-open="sheetOpen"
          :sheet-id="sheetId"
          @update:q="setQuery"
          @clear="clear"
          @remove="removeChip"
          @open-sheet="sheetOpen = true"
        >
          <FilterDropdown
            label="Estado"
            :options="statusOptions"
            :model-value="facets.status"
            multiple
            @update:model-value="setFacet('status', $event)"
          />
          <FilterDropdown
            label="Repo"
            :options="repoOptions"
            :model-value="facets.repo"
            multiple
            @update:model-value="setFacet('repo', $event)"
          />
          <FilterDropdown
            label="Origen"
            :options="triggerOptions"
            :model-value="facets.trigger"
            multiple
            @update:model-value="setFacet('trigger', $event)"
          />
        </FilterBar>

        <div class="list-scroll runs__scroll">
          <EmptyState
            v-if="filteredRuns.length === 0 && isActive"
            title="NADA COINCIDE CON ESTOS FILTROS"
            action-label="LIMPIAR FILTROS"
            @action="clear"
          />
          <p v-else-if="filteredRuns.length === 0" class="empty-state">
            No hay revisiones en curso ni terminadas.
          </p>

          <ol v-else class="run-cards">
            <li
              v-for="run in filteredRuns"
              :key="run.id"
              class="run-card"
              data-run-card
              :data-run-id="run.id"
              :data-active-run="isLiveRun(run) ? '' : undefined"
            >
              <div class="run-card__head">
                <span class="badge" :class="`badge--${statusTone(run.status)}`" data-run-status>
                  {{ statusLabel(run.status) }}
                </span>
                <RouterLink class="run-card__pr" :to="`/prs/${run.pull_request_id}`">
                  <code>{{ run.repo }}#{{ run.pr_number }}</code>
                  <span class="run-card__title">{{ run.pr_title }}</span>
                </RouterLink>
              </div>

              <dl class="run-card__meta">
                <div class="run-card__cell">
                  <dt>Equipo</dt>
                  <dd>{{ run.team || '—' }}</dd>
                </div>
                <div class="run-card__cell">
                  <dt>Perfil</dt>
                  <dd>{{ run.profile || '—' }}</dd>
                </div>
                <div class="run-card__cell">
                  <dt>Origen</dt>
                  <dd><code>{{ run.trigger }}</code> · intento {{ run.attempt }}</dd>
                </div>
                <div class="run-card__cell">
                  <dt>Fase</dt>
                  <dd data-run-phase>{{ phaseText(run) }}</dd>
                </div>
                <div class="run-card__cell">
                  <dt>{{ timeLabel(run) }}</dt>
                  <dd data-run-elapsed>{{ timeValue(run) }}</dd>
                </div>
              </dl>

              <p v-if="run.error_code || run.error_message" class="run-card__error">
                <code v-if="run.error_code">{{ run.error_code }}</code>
                <span v-if="run.error_message">{{ run.error_message }}</span>
              </p>

              <RouterLink class="button button--light run-card__open" :to="`/prs/${run.pull_request_id}`">
                Abrir detalle y log
              </RouterLink>
            </li>
          </ol>

          <div v-if="hasMore" class="runs__more">
            <button
              class="button button--light"
              type="button"
              data-runs-more
              :disabled="loadingMore"
              @click="loadMore"
            >
              {{ loadingMore ? 'CARGANDO…' : 'CARGAR MÁS' }}
            </button>
          </div>
        </div>
      </template>
    </template>

    <FilterSheet
      :id="sheetId"
      :open="sheetOpen"
      :active-count="filterCount"
      :shown="filteredRuns.length"
      clearable
      :apply-label="`VER ${filteredRuns.length} runs`"
      @close="sheetOpen = false"
      @clear="clear"
    >
      <FilterDropdown
        label="Estado"
        :options="statusOptions"
        :model-value="facets.status"
        multiple
        @update:model-value="setFacet('status', $event)"
      />
      <FilterDropdown
        label="Repo"
        :options="repoOptions"
        :model-value="facets.repo"
        multiple
        @update:model-value="setFacet('repo', $event)"
      />
      <FilterDropdown
        label="Origen"
        :options="triggerOptions"
        :model-value="facets.trigger"
        multiple
        @update:model-value="setFacet('trigger', $event)"
      />
    </FilterSheet>
  </section>
</template>

<style scoped>
.runs {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: var(--space-4);
  min-height: 0;
}

.run-cards {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  margin: 0;
  padding: 0;
  list-style: none;
}

.runs__scroll {
  padding-right: var(--space-1);
}

.runs__more {
  display: flex;
  justify-content: center;
  padding: var(--space-4) 0;
}

.run-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--surface);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-lg);
}

.run-card__head {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-3);
}

.run-card__pr {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: var(--space-2);
  min-width: 0;
  color: var(--text);
  text-decoration: none;
}

.run-card__pr:hover .run-card__title {
  text-decoration: underline;
}

.run-card__title {
  color: var(--text-muted);
  overflow-wrap: anywhere;
}

.run-card__meta {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: var(--space-2) var(--space-5);
  margin: 0;
}

.run-card__cell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.run-card__cell dt {
  color: var(--text-faint);
  font-size: 12.5px;
}

.run-card__cell dd {
  margin: 0;
  color: var(--text-muted);
  overflow-wrap: anywhere;
}

.run-card__error {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--space-2);
  margin: 0;
  padding: var(--space-2) var(--space-3);
  border-left: 2px solid var(--bad);
  background: var(--bad-soft);
  color: var(--bad);
  font-size: 13px;
  overflow-wrap: anywhere;
}

.run-card__open {
  align-self: flex-start;
  font-size: 13px;
  text-decoration: none;
}
</style>
