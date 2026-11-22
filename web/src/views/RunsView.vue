<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { api, type ActiveRun } from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { formatElapsed, phaseLabel } from '../runPhase'

const runs = ref<ActiveRun[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
const refreshError = ref<string | null>(null)
const now = ref(new Date())
let clock: ReturnType<typeof setInterval> | null = null

const running = computed(() => runs.value.filter((run) => run.status === 'running').length)
const queued = computed(() => runs.value.length - running.value)

const countText = computed(() => {
  if (runs.value.length === 0) return 'sin revisiones activas'
  return `${running.value} en curso · ${queued.value} en cola`
})

// Los runs llegan con el run primero y luego los encolados por antigüedad; el
// servidor ya aplica el alcance de visibilidad del dashboard.
function elapsedOf(run: ActiveRun): string {
  return formatElapsed(run.status === 'running' ? (run.started_at ?? run.created_at) : run.created_at, now.value)
}

function phaseText(run: ActiveRun): string {
  const label = phaseLabel(run.phase)
  if (!run.phase_started_at) return label
  return `${label} · ${formatElapsed(run.phase_started_at, now.value)}`
}

// Un fallo silencioso conserva la lista anterior y avisa con una nota, en vez
// de vaciar la pantalla por un corte momentáneo del SSE.
async function load({ silent = false } = {}): Promise<void> {
  if (!silent) {
    loading.value = true
    error.value = null
  }
  try {
    const response = await api.activeRuns()
    runs.value = response.runs
    refreshError.value = null
  } catch {
    if (silent) refreshError.value = 'No se pudo actualizar; se muestran las últimas revisiones.'
    else error.value = 'No se pudieron cargar las revisiones en curso'
  } finally {
    if (!silent) loading.value = false
  }
}

// Cada cambio de fase publica run.updated; el debounce de useLiveRefresh junta
// la ráfaga y la vista se refresca una vez.
useLiveRefresh(['run.updated', 'pr.updated'], () => load({ silent: true }))

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
        <h1 class="page-title">Revisiones en curso</h1>
        <p class="page-subtitle" data-runs-count>{{ countText }}</p>
      </div>
      <button class="button button--ghost" type="button" data-runs-refresh :disabled="loading" @click="load()">
        Actualizar
      </button>
    </header>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <p v-if="refreshError" class="caption" role="status" data-runs-refresh-error>{{ refreshError }}</p>

    <p v-if="loading && runs.length === 0" class="empty-state">Cargando revisiones…</p>
    <p v-else-if="!error && runs.length === 0" class="empty-state" data-runs-empty>
      No hay revisiones en curso ni en cola. Cuando un PR necesite revisión aparecerá aquí.
    </p>

    <ol v-else class="run-cards">
      <li v-for="run in runs" :key="run.id" class="run-card" data-active-run>
        <div class="run-card__head">
          <span
            class="badge"
            :class="run.status === 'running' ? 'badge--busy' : 'badge--muted'"
            data-run-status
          >
            {{ run.status === 'running' ? 'En curso' : 'En cola' }}
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
            <dt>{{ run.status === 'running' ? 'En curso' : 'En cola' }} desde hace</dt>
            <dd data-run-elapsed>{{ elapsedOf(run) }}</dd>
          </div>
        </dl>

        <RouterLink class="button button--light run-card__open" :to="`/prs/${run.pull_request_id}`">
          Abrir detalle y log
        </RouterLink>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.runs {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.run-cards {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  margin: 0;
  padding: 0;
  list-style: none;
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

.run-card__open {
  align-self: flex-start;
  font-size: 13px;
  text-decoration: none;
}
</style>
