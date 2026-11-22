<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, type Finding, type PRDetail } from '../api'
import { cacheShare, formatTokens } from '../tokens'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { isConflict, isForbidden } from '../errors'
import { prStatusLabel, statusLabel } from '../status'

const route = useRoute()
const prId = computed(() => Number(route.params.id))

const detail = ref<PRDetail | null>(null)
const loading = ref(true)
const retrying = ref(false)
const error = ref<string | null>(null)
const refreshError = ref<string | null>(null)
const acting = ref<string | null>(null)
const confirming = ref<{ finding: Finding; action: 'withdraw' | 'confirm' } | null>(null)
const reason = ref('')

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
    if (finding.status === 'withdrawn') continue
    const key = severityLabels[finding.severity] ? finding.severity : 'other'
    groups.find((group) => group.key === key)!.findings.push(finding)
  }
  return groups.filter((group) => group.findings.length > 0)
})

// Los descartados desde el panel o con /qa dismiss se apartan del recuento por
// severidad pero siguen visibles para poder confirmarlos de vuelta.
const withdrawnFindings = computed(() =>
  (detail.value?.findings ?? []).filter((finding) => finding.status === 'withdrawn'),
)

// ListReviewRuns devuelve las ejecuciones en orden ascendente de id: la última
// es la más reciente y la que aporta el SHA y la cobertura de la cabecera.
const latestRun = computed(() => {
  const runs = detail.value?.runs ?? []
  return runs.length > 0 ? runs[runs.length - 1] : null
})
const status = computed(() =>
  detail.value ? prStatusLabel(detail.value) : statusLabel('pending'),
)

// What QA checks before approving in GitHub. Resolved findings are left out:
// they are no longer anyone's decision.
const validation = computed(() => {
  const pr = detail.value
  const findings = (pr?.findings ?? []).filter((finding) => finding.status !== 'resolved')
  const open = findings.filter((finding) => finding.status === 'open')
  const validations = pr?.validations ?? { passed: 0, total: 0 }
  const run = latestRun.value
  return {
    triaged: findings.filter((finding) => finding.triage_verdict !== '').length,
    total: findings.length,
    untriaged: open.filter((finding) => finding.triage_verdict === '').length,
    openBlockers: open.filter((finding) => finding.severity === 'blocker').length,
    ciKnown: validations.total > 0,
    ciGreen: validations.total > 0 && validations.passed === validations.total,
    ciText: `${validations.passed}/${validations.total}`,
    aiDone: run?.status === 'succeeded',
    aiText: run
      ? `Última revisión IA: ${runStatusLabels[run.status] ?? run.status}`
      : 'La IA todavía no ha revisado este PR',
  }
})

const runStatusLabels: Record<string, string> = {
  queued: 'en cola',
  running: 'en curso',
  succeeded: 'completada',
  failed: 'fallida',
  inconclusive: 'inconclusa',
  superseded: 'sustituida',
}

// The note follows QA's verdict, not the status: a run can reopen a false
// positive and a /qa dismiss can withdraw a confirmed finding.
function triageNote(finding: Finding): string {
  if (finding.status === 'resolved') return 'Resuelto en el código'
  switch (finding.triage_verdict) {
    case 'confirmed':
      return `Confirmado por ${finding.triaged_by}`
    case 'false_positive':
      return `Falso positivo según ${finding.triaged_by}`
    default:
      return 'Sin triar'
  }
}

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

// Loads are numbered so an older answer cannot overwrite a newer one.
let latestLoad = 0

// A silent load keeps what is on screen, including an open triage dialog and
// any triage error; if it fails, the last detail stays with a note.
async function load({ silent = false } = {}): Promise<void> {
  const current = ++latestLoad
  if (!silent) {
    loading.value = true
    error.value = null
  }
  try {
    const next = await api.prDetail(prId.value)
    if (current !== latestLoad) return
    detail.value = next
    refreshError.value = null
  } catch {
    if (current !== latestLoad) return
    if (silent) refreshError.value = 'No se pudo actualizar; se muestran los últimos datos.'
    else error.value = 'No se pudo cargar el detalle del PR'
  } finally {
    if (current === latestLoad) loading.value = false
  }
}

async function retry(): Promise<void> {
  retrying.value = true
  error.value = null
  try {
    await api.retryPR(prId.value)
    await load()
  } catch (err) {
    error.value = isForbidden(err)
      ? 'Solo QA o el lead del equipo pueden reintentar'
      : 'No se pudo reintentar la revisión'
  } finally {
    retrying.value = false
  }
}

function askWithdraw(finding: Finding): void {
  confirming.value = { finding, action: 'withdraw' }
  reason.value = ''
}

function askConfirm(finding: Finding): void {
  confirming.value = { finding, action: 'confirm' }
  reason.value = ''
}

function cancelAction(): void {
  confirming.value = null
  reason.value = ''
}

/**
 * Aplica el descarte o la confirmación con estado optimista: el hallazgo cambia
 * de estado en cuanto responde la API y, si falla, se restaura el detalle
 * anterior para que la lista no mienta.
 */
async function applyAction(): Promise<void> {
  const pending = confirming.value
  if (!pending) return
  const { finding, action } = pending
  const previous = detail.value
  acting.value = finding.id
  error.value = null

  if (detail.value) {
    detail.value = {
      ...detail.value,
      findings: detail.value.findings.map((item) =>
        item.id === finding.id
          ? { ...item, status: action === 'withdraw' ? 'withdrawn' : 'open' }
          : item,
      ),
    }
  }
  confirming.value = null

  try {
    if (action === 'withdraw') {
      await api.withdrawFinding(prId.value, finding.id, reason.value.trim() || undefined)
    } else {
      await api.confirmFinding(prId.value, finding.id)
    }
    await load()
  } catch (err) {
    detail.value = previous
    if (isForbidden(err)) {
      error.value = 'Solo QA o el lead del equipo pueden triar hallazgos'
    } else if (isConflict(err)) {
      error.value = 'El hallazgo ya está resuelto en el código'
    } else {
      error.value =
        action === 'withdraw' ? 'No se pudo descartar el hallazgo' : 'No se pudo confirmar el hallazgo'
    }
  } finally {
    acting.value = null
    reason.value = ''
  }
}

useLiveRefresh(
  ['pr.updated'],
  () => load({ silent: true }),
  (event) => event.id === String(prId.value),
)

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
      <div class="pr-detail__identity">
        <h1 class="page-title">{{ detail.title }}</h1>
        <p class="page-subtitle">por <code>{{ detail.author_login }}</code></p>
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

    <dl class="title-block">
      <div class="title-block__cell">
        <dt class="title-block__label">Repositorio</dt>
        <dd class="title-block__value">
          <code>{{ detail.repo }}</code> <code>#{{ detail.number }}</code>
        </dd>
      </div>
      <div class="title-block__cell">
        <dt class="title-block__label">Commit</dt>
        <dd class="title-block__value"><code>{{ latestRun?.head_sha ?? '—' }}</code></dd>
      </div>
      <div class="title-block__cell">
        <dt class="title-block__label">Estado de revisión</dt>
        <dd class="title-block__value">{{ status.action }}</dd>
      </div>
    </dl>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <p v-if="refreshError" class="caption" role="status" data-refresh-error>{{ refreshError }}</p>

    <div class="split split--end">
      <div class="split__aside">
        <section class="tile qa-check" aria-labelledby="qa-check-title" data-qa-check>
          <div class="qa-check__head">
            <div>
              <h2 id="qa-check-title" class="tile__title">Validación QA</h2>
              <p class="qa-check__figure" data-triaged-count>
                {{ validation.triaged }}/{{ validation.total }} triados
              </p>
            </div>
            <a
              class="button button--light"
              :href="`${detail.html_url}/files`"
              target="_blank"
              rel="noreferrer"
            >
              Revisar y aprobar en GitHub ↗
            </a>
          </div>
          <ul class="qa-check__list">
            <li :class="validation.ciGreen ? 'is-ok' : validation.ciKnown ? 'is-bad' : 'is-unknown'" data-check="ci">
              {{ validation.ciKnown ? `CI: ${validation.ciText} checks en verde` : 'CI sin datos para este commit' }}
            </li>
            <li :class="validation.aiDone ? 'is-ok' : 'is-bad'" data-check="ai">{{ validation.aiText }}</li>
            <li :class="validation.openBlockers === 0 ? 'is-ok' : 'is-bad'" data-check="blockers">
              {{
                validation.openBlockers === 0
                  ? 'Sin bloqueantes abiertos'
                  : `${validation.openBlockers} bloqueante${validation.openBlockers === 1 ? '' : 's'} abierto${validation.openBlockers === 1 ? '' : 's'}`
              }}
            </li>
            <li :class="validation.untriaged === 0 ? 'is-ok' : 'is-unknown'" data-check="untriaged">
              {{
                validation.untriaged === 0
                  ? 'Todos los hallazgos abiertos están triados'
                  : `${validation.untriaged} hallazgo${validation.untriaged === 1 ? '' : 's'} sin triar`
              }}
            </li>
          </ul>
        </section>

        <section class="pr-section" aria-label="Ejecuciones">
          <div class="pr-section__head">
            <h2 class="pr-section__title">Ejecuciones</h2>
            <span class="pr-section__count">{{ detail.runs.length }} en total</span>
          </div>
          <ol v-if="detail.runs.length > 0" class="run-list">
            <li v-for="run in detail.runs" :key="run.id" class="run" data-run>
              <div class="run__head">
                <span class="badge" :class="runTone(run.status)">{{ run.status }}</span>
                <span class="run__trigger"><code>{{ run.trigger }}</code></span>
                <span class="run__attempt">intento <code>{{ run.attempt }}</code></span>
              </div>
              <dl class="run__times">
                <div class="run__time">
                  <dt>Inicio</dt>
                  <dd><code>{{ run.started_at ?? '—' }}</code></dd>
                </div>
                <div class="run__time">
                  <dt>Fin</dt>
                  <dd><code>{{ run.finished_at ?? '—' }}</code></dd>
                </div>
              </dl>
              <p v-if="run.summary" class="run__summary">{{ run.summary }}</p>
              <p v-if="run.error_message" class="run__error">
                <code>{{ run.error_code }}</code>: {{ run.error_message }}
              </p>
              <dl v-if="run.total_tokens !== null" class="run__tokens" data-run-tokens>
                <div class="run__token">
                  <dt>Tokens</dt>
                  <dd><code>{{ formatTokens(run.total_tokens) }}</code></dd>
                </div>
                <div class="run__token">
                  <dt>Entrada / salida</dt>
                  <dd>
                    <code>{{ formatTokens(run.input_tokens ?? 0) }} / {{ formatTokens(run.output_tokens ?? 0) }}</code>
                  </dd>
                </div>
                <div v-if="run.reasoning_tokens !== null" class="run__token" data-run-reasoning>
                  <dt>Razonamiento</dt>
                  <dd><code>{{ formatTokens(run.reasoning_tokens) }}</code></dd>
                </div>
                <div class="run__token">
                  <dt>Caché</dt>
                  <dd><code>{{ cacheShare(run) }}</code></dd>
                </div>
                <div class="run__token">
                  <dt>Mayor prompt</dt>
                  <dd><code>{{ formatTokens(run.max_context_tokens ?? 0) }}</code></dd>
                </div>
              </dl>
              <p v-else class="run__no-tokens" data-run-no-tokens>
                Sin datos de tokens: ejecución anterior al registro de consumo
              </p>
            </li>
          </ol>
          <p v-else class="empty-state">Todavía no hay ejecuciones</p>

          <dl class="coverage">
            <div class="coverage__item" data-coverage-reviewed>
              <dt>Archivos revisados</dt>
              <dd><code>{{ coverageCount('reviewed_files') }}</code></dd>
            </div>
            <div class="coverage__item" data-coverage-skipped>
              <dt>Archivos omitidos</dt>
              <dd><code>{{ coverageCount('skipped_files') }}</code></dd>
            </div>
            <div class="coverage__item" data-coverage-excluded>
              <dt>Archivos excluidos por la política</dt>
              <dd><code>{{ coverageCount('excluded_files') }}</code></dd>
            </div>
          </dl>
        </section>
      </div>

      <div class="split__main">
        <section class="pr-section" aria-label="Hallazgos">
          <div class="pr-section__head">
            <h2 class="pr-section__title">Hallazgos</h2>
            <span class="pr-section__count">{{ detail.findings.length }} en total</span>
          </div>
          <p v-if="detail.findings.length === 0" class="empty-state">No se encontraron hallazgos</p>

          <div class="severity-list">
            <div
              v-for="group in groupedFindings"
              :key="group.key"
              class="severity"
              :data-severity="group.key"
            >
              <div class="severity__head">
                <h3 class="severity__title">{{ group.label }}</h3>
                <span class="severity__count">{{ group.findings.length }}</span>
              </div>
              <article v-for="finding in group.findings" :key="finding.id" class="finding" data-finding>
                <div class="finding__meta">
                  <h4 class="finding__title">{{ finding.title }}</h4>
                  <p class="finding__location">
                    <code>{{ finding.path }}:{{ finding.start_line }}–{{ finding.end_line }}</code>
                  </p>
                </div>
                <p class="finding__body">{{ finding.body }}</p>
                <p v-if="finding.suggestion" class="finding__suggestion">
                  <span class="finding__label">Sugerencia</span>
                  <span>{{ finding.suggestion }}</span>
                </p>
                <div class="finding__actions">
                  <span
                    class="finding__triage"
                    :class="{ 'finding__triage--done': finding.triage_verdict === 'confirmed' }"
                    data-triage-note
                  >
                    {{ triageNote(finding) }}
                  </span>
                  <!-- A resolved finding is no longer QA's call: confirming it would
                       reopen it and block the PR again. -->
                  <button
                    v-if="finding.status === 'open'"
                    class="button button--ghost finding__action"
                    type="button"
                    data-withdraw
                    :data-finding-id="finding.id"
                    :disabled="acting === finding.id"
                    @click="askWithdraw(finding)"
                  >
                    Falso positivo
                  </button>
                  <button
                    v-if="finding.status === 'open' && finding.triage_verdict !== 'confirmed'"
                    class="button button--light finding__action"
                    type="button"
                    data-confirm
                    :data-finding-id="finding.id"
                    :disabled="acting === finding.id"
                    @click="askConfirm(finding)"
                  >
                    Confirmar
                  </button>
                </div>
              </article>
            </div>
          </div>

          <div v-if="withdrawnFindings.length > 0" class="severity severity--withdrawn" data-withdrawn>
            <div class="severity__head">
              <h3 class="severity__title">Descartados</h3>
              <span class="severity__count">{{ withdrawnFindings.length }}</span>
            </div>
            <article
              v-for="finding in withdrawnFindings"
              :key="finding.id"
              class="finding finding--muted"
              data-finding
            >
              <div class="finding__meta">
                <h4 class="finding__title">{{ finding.title }}</h4>
                <p class="finding__location">
                  <code>{{ finding.path }}:{{ finding.start_line }}–{{ finding.end_line }}</code>
                </p>
              </div>
              <div class="finding__actions">
                <span class="finding__triage" data-triage-note>{{ triageNote(finding) }}</span>
                <button
                  class="button button--ghost finding__action"
                  type="button"
                  data-confirm
                  :data-finding-id="finding.id"
                  :disabled="acting === finding.id"
                  @click="askConfirm(finding)"
                >
                  Restaurar
                </button>
              </div>
            </article>
          </div>
        </section>
      </div>
    </div>

    <div v-if="confirming" class="dialog-backdrop" data-confirm-dialog>
      <div class="dialog" role="dialog" aria-modal="true" aria-labelledby="finding-action-title">
        <h2 id="finding-action-title" class="dialog__title">
          {{ confirming.action === 'withdraw' ? 'Marcar como falso positivo' : 'Confirmar hallazgo' }}
        </h2>
        <p class="dialog__text">
          <strong>{{ confirming.finding.title }}</strong>
          <br />
          <code>{{ confirming.finding.path }}:{{ confirming.finding.start_line }}–{{ confirming.finding.end_line }}</code>
        </p>
        <label v-if="confirming.action === 'withdraw'" class="field dialog__field">
          <span class="field__label">Motivo (queda en la auditoría)</span>
          <input
            v-model="reason"
            class="field__input"
            type="text"
            data-finding-reason
            placeholder="Falso positivo"
          />
        </label>
        <div class="dialog__actions">
          <button class="button button--ghost" type="button" data-cancel @click="cancelAction">
            Cancelar
          </button>
          <button
            class="button button--primary"
            type="button"
            data-apply
            :disabled="acting !== null"
            @click="applyAction"
          >
            {{ confirming.action === 'withdraw' ? 'Falso positivo' : 'Confirmar' }}
          </button>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.pr-detail {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.severity-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

@media (min-width: 2200px) {
  .severity-list {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    align-items: start;
  }
}

.pr-detail code {
  font-family: var(--font-mono);
  overflow-wrap: anywhere;
}

.pr-detail__identity {
  min-width: 0;
}

.pr-detail__identity .page-subtitle code {
  font-size: 13.5px;
}

.pr-detail__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-3);
}

.pr-detail .badge {
  font-size: 13px;
}

.pr-detail .button {
  min-height: 40px;
}

@media (pointer: coarse) {
  .pr-detail .button {
    min-height: 44px;
  }

  .dialog__field .field__input {
    min-height: 44px;
  }
}

/* Title block: a drawing-style register of the PR references. */
.title-block {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  margin: 0;
  border: var(--rule-hair) solid var(--border-strong);
}

.title-block__cell {
  min-width: 0;
  padding: var(--space-3) var(--space-4);
}

.title-block__cell + .title-block__cell {
  border-left: var(--rule-hair) solid var(--border);
}

.title-block__label {
  color: var(--text-muted);
  font-size: 13px;
}

.title-block__value {
  margin: var(--space-1) 0 0;
  min-width: 0;
  font-size: 15px;
  overflow-wrap: anywhere;
}

.title-block__value code {
  font-size: 13.5px;
}

.run-list {
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0;
  list-style: none;
}

.run {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-4) 0;
  border-bottom: var(--rule-hair) solid var(--border);
}

.run__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-3);
}

.run__trigger,
.run__attempt {
  color: var(--text-muted);
  font-size: 13px;
}

.run__trigger code,
.run__attempt code {
  color: var(--text);
}

.run__times,
.run__tokens {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: var(--space-1) var(--space-5);
  margin: 0;
}

.run__time,
.run__token {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}

.run__time dt,
.run__token dt {
  flex: none;
  color: var(--text-faint);
  font-size: 13px;
}

.run__time dd,
.run__token dd {
  margin: 0;
  min-width: 0;
  color: var(--text-muted);
}

.run__time code,
.run__token code {
  font-size: 13px;
}

.run__no-tokens {
  color: var(--text-faint);
  font-size: 13px;
}

.run__summary {
  font-size: 16px;
  overflow-wrap: anywhere;
}

.run__error {
  color: var(--bad);
  font-size: 16px;
  overflow-wrap: anywhere;
}

.run__error code {
  color: var(--bad);
}

.coverage {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  margin: var(--space-4) 0 0;
  border: var(--rule-hair) solid var(--border-strong);
}

.coverage__item {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
  min-width: 0;
  padding: var(--space-3) var(--space-4);
}

.coverage__item + .coverage__item {
  border-left: var(--rule-hair) solid var(--border);
}

.coverage dt {
  color: var(--text-muted);
  font-size: 13px;
}

.coverage dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
}

.coverage dd code {
  font-size: 16px;
}

.severity {
  display: flex;
  flex-direction: column;
}

.severity__head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-2) 0;
  border-bottom: var(--rule-hair) solid var(--border);
}

.severity__title {
  font-size: 15px;
  font-weight: 600;
}

.severity__count {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}

.severity[data-severity='blocker'] .severity__title,
.severity[data-severity='blocker'] .severity__count,
.severity[data-severity='major'] .severity__title,
.severity[data-severity='major'] .severity__count {
  color: var(--bad);
}

.severity[data-severity='minor'] .severity__title,
.severity[data-severity='minor'] .severity__count {
  color: var(--busy);
}

.severity[data-severity='blocker'] .severity__head,
.severity[data-severity='major'] .severity__head {
  border-bottom-color: var(--bad);
}

.severity[data-severity='minor'] .severity__head {
  border-bottom-color: var(--busy);
}

.finding {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: 22px;
  background: var(--surface);
  border: var(--rule-hair) solid var(--tile-border);
  border-radius: var(--radius-lg);
}

[data-severity='blocker'] .finding {
  border-color: var(--accent);
}

.severity[data-severity='blocker'] .finding,
.severity[data-severity='major'] .finding {
  border-left-color: var(--bad);
}

.severity[data-severity='minor'] .finding {
  border-left-color: var(--busy);
}

.finding__meta {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  min-width: 0;
}

.finding__title {
  font-size: 16px;
  line-height: 1.35;
}

.finding__location {
  color: var(--text-muted);
  font-size: 13px;
}

.finding__location code {
  font-size: 13px;
}

.finding__body {
  max-width: 72ch;
  font-size: 16px;
  overflow-wrap: anywhere;
}

.finding__suggestion {
  max-width: 72ch;
  color: var(--text-muted);
  font-size: 16px;
}

.finding__label {
  display: block;
  color: var(--text-faint);
  font-size: 13px;
}

.finding__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-2);
  padding-top: var(--space-3);
  border-top: var(--rule-hair) dashed var(--border);
}

.finding__triage {
  margin-right: auto;
  color: var(--text-faint);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}

.finding__triage--done {
  color: var(--ok);
}

.qa-check__head {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-4);
}

.qa-check__figure {
  margin-top: var(--space-2);
  font-size: 30px;
  font-weight: 800;
  line-height: 1;
}

.qa-check__list {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: var(--space-2) var(--space-5);
  margin: 0;
  padding: 0;
  list-style: none;
  font-size: 13px;
}

.qa-check__list li {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
}

.qa-check__list li::before {
  content: '';
  flex: none;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--dot-dim);
}

.qa-check__list .is-ok::before {
  background: var(--ok);
}

.qa-check__list .is-bad::before {
  background: var(--accent);
}

.severity--withdrawn .severity__title {
  color: var(--text-muted);
}

.severity--withdrawn .finding {
  border-left-color: var(--border-strong);
}

.finding--muted .finding__title {
  color: var(--text-muted);
  text-decoration: line-through;
  text-decoration-thickness: var(--rule-hair);
}

.finding--muted .finding__location {
  color: var(--text-faint);
}

/* El diálogo usa <div> en lugar de <dialog> para no depender del soporte de
   showModal en el entorno de test, que solo monta el componente. */
.dialog-backdrop {
  position: fixed;
  inset: 0;
  z-index: 20;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--space-4);
  background: var(--scrim);
}

.dialog {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  width: min(460px, 100%);
  max-height: 100%;
  overflow-y: auto;
  padding: var(--space-5);
  background: var(--surface);
  border: var(--rule-heavy) solid var(--text);
}

.dialog__title {
  font-size: 18px;
}

.dialog__text {
  color: var(--text-muted);
  font-size: 16px;
  overflow-wrap: anywhere;
}

.dialog__text code {
  font-size: 13px;
}

.dialog__field {
  margin: 0;
}

.dialog__field .field__input {
  width: 100%;
}

.dialog__field .field__input:hover {
  border-color: var(--text-muted);
}

.dialog__field .field__input:active {
  border-color: var(--text);
}

.dialog__actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: var(--space-2);
}

@media (max-width: 640px) {
  .pr-detail__actions {
    width: 100%;
    justify-content: flex-start;
  }

  .title-block {
    grid-template-columns: minmax(0, 1fr);
  }

  .title-block__cell + .title-block__cell {
    border-left: 0;
    border-top: var(--rule-hair) solid var(--border);
  }

  .coverage {
    grid-template-columns: minmax(0, 1fr);
  }

  .coverage__item + .coverage__item {
    border-left: 0;
    border-top: var(--rule-hair) solid var(--border);
  }

  .finding {
    padding: var(--space-3);
  }

  .dialog {
    padding: var(--space-4);
  }
}
</style>
