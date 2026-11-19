<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Finding, type PRDetail } from '../api'
import { statusLabel } from '../status'

const route = useRoute()
const prId = computed(() => Number(route.params.id))

const detail = ref<PRDetail | null>(null)
const loading = ref(true)
const retrying = ref(false)
const error = ref<string | null>(null)

const severityLabels: Record<string, string> = {
  blocker: 'Bloqueantes',
  major: 'Mayores',
  minor: 'Menores',
  nit: 'Nits',
  other: 'Otros',
}

const groupedFindings = computed(() => {
  const groups = Object.keys(severityLabels).map((key) => ({
    key,
    label: severityLabels[key],
    findings: [] as Finding[],
  }))
  for (const finding of detail.value?.findings ?? []) {
    const key = severityLabels[finding.severity] ? finding.severity : 'other'
    groups.find((group) => group.key === key)!.findings.push(finding)
  }
  return groups.filter((group) => group.findings.length > 0)
})

// ListReviewRuns devuelve las ejecuciones en orden ascendente de id: la última
// es la más reciente y la que aporta el SHA y la cobertura de la cabecera.
const latestRun = computed(() => {
  const runs = detail.value?.runs ?? []
  return runs.length > 0 ? runs[runs.length - 1] : null
})
const status = computed(() => statusLabel(detail.value?.qa_status ?? 'pending'))

function coverageCount(key: string): number {
  const value = latestRun.value?.coverage?.[key]
  return typeof value === 'number' ? value : 0
}

function runTone(status: string): string {
  if (status === 'succeeded') return 'badge--ok'
  if (status === 'failed') return 'badge--bad'
  if (status === 'running' || status === 'queued') return 'badge--busy'
  return 'badge--muted'
}

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    detail.value = await api.prDetail(prId.value)
  } catch {
    error.value = 'No se pudo cargar el detalle del PR'
  } finally {
    loading.value = false
  }
}

async function retry(): Promise<void> {
  retrying.value = true
  error.value = null
  try {
    await api.retryPR(prId.value)
    await load()
  } catch {
    error.value = 'No se pudo reintentar la revisión'
  } finally {
    retrying.value = false
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <section v-if="loading" class="pr-detail">
    <p class="empty-state">Cargando el detalle del PR…</p>
  </section>
  <section v-else-if="!detail" class="pr-detail">
    <p class="empty-state">{{ error ?? 'No se encontró el pull request' }}</p>
  </section>
  <section v-else class="pr-detail">
    <header class="page-header">
      <div>
        <h1 class="page-title">{{ detail.title }}</h1>
        <p class="page-subtitle">
          {{ detail.repo }} #{{ detail.number }} · por {{ detail.author_login }}
        </p>
        <p class="pr-detail__sha">SHA <code>{{ latestRun?.head_sha ?? '—' }}</code></p>
      </div>
      <div class="pr-detail__actions">
        <span class="badge" :class="`badge--${status.tone}`" data-qa-status>{{ status.tag }}</span>
        <button
          class="button button--primary"
          type="button"
          data-retry
          :disabled="retrying"
          @click="retry"
        >
          {{ retrying ? 'Reintentando…' : 'Reintentar' }}
        </button>
      </div>
    </header>

    <p v-if="error" class="form-error">{{ error }}</p>

    <div class="pr-detail__grid">
      <section class="panel" aria-label="Ejecuciones">
        <h2 class="panel__title">Ejecuciones</h2>
        <ul v-if="detail.runs.length > 0" class="run-list">
          <li v-for="run in detail.runs" :key="run.id" class="run" data-run>
            <div class="run__head">
              <span class="badge" :class="runTone(run.status)">{{ run.status }}</span>
              <strong>{{ run.trigger }}</strong>
              <span class="run__attempt">intento {{ run.attempt }}</span>
            </div>
            <p class="run__times">
              Inicio: {{ run.started_at ?? '—' }} · Fin: {{ run.finished_at ?? '—' }}
            </p>
            <p v-if="run.summary" class="run__summary">{{ run.summary }}</p>
            <p v-if="run.error_message" class="run__error">
              {{ run.error_code }}: {{ run.error_message }}
            </p>
          </li>
        </ul>
        <p v-else class="empty-state">Todavía no hay ejecuciones</p>

        <div class="coverage">
          <span data-coverage-reviewed>{{ coverageCount('reviewed_files') }} archivos revisados</span>
          <span data-coverage-skipped>{{ coverageCount('skipped_files') }} archivos omitidos</span>
        </div>
      </section>

      <section class="panel" aria-label="Hallazgos">
        <h2 class="panel__title">Hallazgos</h2>
        <p v-if="detail.findings.length === 0" class="empty-state">No se encontraron hallazgos</p>
        <div v-for="group in groupedFindings" :key="group.key" class="severity" :data-severity="group.key">
          <h3 class="severity__title">{{ group.label }} ({{ group.findings.length }})</h3>
          <article v-for="finding in group.findings" :key="finding.id" class="finding" data-finding>
            <h4 class="finding__title">{{ finding.title }}</h4>
            <p class="finding__location">
              <code>{{ finding.path }}</code>:{{ finding.start_line }}–{{ finding.end_line }}
            </p>
            <p class="finding__body">{{ finding.body }}</p>
            <p v-if="finding.suggestion" class="finding__suggestion">
              Sugerencia: {{ finding.suggestion }}
            </p>
          </article>
        </div>
      </section>
    </div>
  </section>
</template>

<style scoped>
.pr-detail {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.pr-detail__sha {
  margin-top: var(--space-2);
  color: var(--text-faint);
  font-size: 13px;
}

.pr-detail__sha code,
.finding__location code {
  font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
}

.pr-detail__actions {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.pr-detail__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: var(--space-4);
  align-items: start;
}

.panel {
  padding: var(--space-4);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
}

.panel__title {
  margin: 0 0 var(--space-3);
  font-size: 16px;
  font-weight: 600;
}

.run-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  margin: 0 0 var(--space-4);
  padding: 0;
  list-style: none;
}

.run {
  padding: var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.run__head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
  font-size: 14px;
}

.run__attempt,
.run__times,
.run__summary {
  color: var(--text-muted);
  font-size: 12.5px;
}

.run__times {
  margin: var(--space-2) 0 0;
}

.run__summary {
  margin: var(--space-1) 0 0;
}

.run__error {
  margin: var(--space-2) 0 0;
  color: var(--bad);
  font-size: 12.5px;
}

.coverage {
  display: flex;
  gap: var(--space-4);
  flex-wrap: wrap;
  padding-top: var(--space-3);
  border-top: 1px dashed var(--border);
  color: var(--text-muted);
  font-size: 13px;
}

.severity {
  margin-bottom: var(--space-4);
}

.severity__title {
  margin: 0 0 var(--space-2);
  font-size: 14px;
}

.finding {
  padding: var(--space-3);
  margin-bottom: var(--space-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.finding__title {
  margin: 0;
  font-size: 14px;
}

.finding__location {
  margin: var(--space-1) 0;
  color: var(--text-muted);
  font-size: 12.5px;
}

.finding__body,
.finding__suggestion {
  margin: var(--space-1) 0 0;
  font-size: 13px;
}

.finding__suggestion {
  color: var(--text-muted);
}
</style>
