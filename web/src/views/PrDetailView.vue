<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { useRoute } from 'vue-router'
import { api, type CoverageFile, type Finding, type HumanVerdict, type HumanVerdictFile, type PRDetail, type ReviewRun } from '../api'
import EmptyState from '../components/EmptyState.vue'
import FilterBar from '../components/FilterBar.vue'
import FilterSheet from '../components/FilterSheet.vue'
import RunLogPanel from '../components/RunLogPanel.vue'
import { formatElapsed, phaseLabel } from '../runPhase'
import { cacheShare, formatTokens } from '../tokens'
import { useListFilters } from '../composables/useListFilters'
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
// Resultado humano de una revisión inconclusa: se registra aparte del run
// automático y queda atado a la comparación vigente.
const humanForm = ref({
  decision: 'approved',
  conclusion: '',
  reason: '',
  scope: '',
  pending: '',
  complete: false,
})
const recordingHuman = ref(false)
const humanError = ref<string | null>(null)
// Solo una ejecución muestra su log a la vez: el panel crece y se reinicia al
// abrir otra, así el detalle del PR no se llena de historiales.
const openLogRunId = ref<number | null>(null)
// Reloj local: el tiempo en la fase actual avanza aunque no llegue ningún
// evento, igual que el resto de vistas en vivo.
const now = ref(new Date())
let clock: ReturnType<typeof setInterval> | null = null

function toggleLog(id: number): void {
  openLogRunId.value = openLogRunId.value === id ? null : id
}

function runPhaseText(run: ReviewRun): string {
  if (!run.phase) return '—'
  return `${phaseLabel(run.phase)} · ${formatElapsed(run.phase_started_at, now.value)}`
}

const severityLabels: Record<string, string> = {
  blocker: 'Bloqueantes',
  major: 'Mayores',
  minor: 'Menores',
  nit: 'Nits',
  other: 'Otros',
}

const SEVERITY_CHIPS = ['blocker', 'major', 'minor', 'nit'] as const

const STATUS_TABS = [
  { value: '', label: 'Todos' },
  { value: 'untriaged', label: 'Sin triar' },
  { value: 'confirmed', label: 'Confirmados' },
  { value: 'dismissed', label: 'Descartados' },
  { value: 'resolved', label: 'Resueltos' },
]

const { q, facets, isActive, activeCount, setFacet, toggleFacet, clear, apply, facetCounts } =
  useListFilters({ facets: ['severity', 'status'] })

const sheetOpen = ref(false)
const sheetId = `findings-filters-${useId()}`

// The segmented control is single-select, but a shared URL can carry several
// status values. Keep the first so the control and the filter cannot disagree;
// the URL watcher then rewrites the query to that single value.
const [firstStatus] = facets.value.status
if (firstStatus !== undefined && facets.value.status.length > 1) {
  setFacet('status', [firstStatus])
}

// A finding can fall in more than one bucket (a withdrawn confirmed finding is
// both confirmed and dismissed); the segmented control selects one bucket.
function findingStatusKeys(finding: Finding): string[] {
  const keys: string[] = []
  if (finding.status === 'open' && !finding.triage_verdict) keys.push('untriaged')
  if (finding.triage_verdict === 'confirmed') keys.push('confirmed')
  if (finding.triage_verdict === 'false_positive' || finding.status === 'withdrawn') {
    keys.push('dismissed')
  }
  if (finding.status === 'resolved') keys.push('resolved')
  return keys
}

function findingSearchText(finding: Finding): string {
  return [finding.category, finding.path].join(' ')
}

const filteredFindings = apply(
  () => detail.value?.findings ?? [],
  findingSearchText,
  (finding, active) => {
    const severities = active.severity ?? []
    const statuses = active.status ?? []
    if (severities.length > 0 && !severities.includes(finding.severity)) return false
    if (statuses.length > 0 && !findingStatusKeys(finding).some((key) => statuses.includes(key))) {
      return false
    }
    return true
  },
)

const counts = facetCounts(
  () => detail.value?.findings ?? [],
  (finding, facet) => (facet === 'severity' ? [finding.severity] : findingStatusKeys(finding)),
  findingSearchText,
)

const severityChips = computed(() =>
  SEVERITY_CHIPS.map((value) => ({
    value,
    label: severityLabels[value],
    count: counts.value.severity?.[value] ?? 0,
  })),
)

const statusSelection = computed(() => facets.value.status[0] ?? '')

function statusLabelFor(value: string): string {
  return STATUS_TABS.find((tab) => tab.value === value)?.label ?? value
}

function setStatus(value: string): void {
  setFacet('status', value === '' ? [] : [value])
}

const chips = computed(() => [
  ...facets.value.severity.map((value) => ({
    key: `severity:${value}`,
    label: `Severidad: ${severityLabels[value] ?? value}`,
  })),
  ...facets.value.status.map((value) => ({
    key: `status:${value}`,
    label: `Estado: ${statusLabelFor(value)}`,
  })),
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

const groupedFindings = computed(() => {
  const groups = Object.keys(severityLabels).map((key) => ({
    key,
    label: severityLabels[key],
    findings: [] as Finding[],
  }))
  for (const finding of filteredFindings.value) {
    if (finding.status === 'withdrawn') continue
    const key = severityLabels[finding.severity] ? finding.severity : 'other'
    groups.find((group) => group.key === key)!.findings.push(finding)
  }
  return groups.filter((group) => group.findings.length > 0)
})

// Los descartados desde el panel o con /qa dismiss se apartan del recuento por
// severidad pero siguen visibles para poder confirmarlos de vuelta.
const withdrawnFindings = computed(() =>
  filteredFindings.value.filter((finding) => finding.status === 'withdrawn'),
)

// ListReviewRuns devuelve las ejecuciones en orden ascendente de id: la última
// es la más reciente y la que aporta el SHA y la cobertura de la cabecera.
const latestRun = computed(() => {
  const runs = detail.value?.runs ?? []
  return runs.length > 0 ? runs[runs.length - 1] : null
})
// El resultado humano se pinta como una tarjeta aparte del run automático.
const humanVerdict = computed(() => detail.value?.human_verdict ?? null)
const canRecordHumanVerdict = computed(
  () => latestRun.value?.status === 'inconclusive' || latestRun.value?.status === 'failed',
)
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
  if (finding.triage_source === 'verifier') {
    const verdict = verificationMatchesRun(finding) ? finding.verification.verdict : undefined
    if (verdict === 'confirm') return 'Confirmado por la verificación automática'
    if (verdict === 'refute') return 'Descartado por la verificación automática'
    return 'Sin triar'
  }
  switch (finding.triage_verdict) {
    case 'confirmed':
      return `Confirmado por ${finding.triaged_by}`
    case 'false_positive':
      return `Falso positivo según ${finding.triaged_by}`
    default:
      return 'Sin triar'
  }
}

const verificationLabels: Record<string, string> = {
  confirm: 'Confirmado',
  refute: 'Descartado',
  uncertain: 'Sin confirmar',
}

// La segunda pasada deja su veredicto aunque la política no lo aplique (una
// simulación), así que el bloque sigue al veredicto guardado y no al triage.
// Una verificación de otro commit no es evidencia de este.
function verificationMatchesRun(finding: Finding): boolean {
  const head = latestRun.value?.head_sha
  const verified = finding.verification.head_sha
  if (!head || !verified) return false
  return head.toLowerCase() === verified.toLowerCase()
}

function verificationLabel(finding: Finding): string {
  if (!finding.verification.verdict) return ''
  if (!verificationMatchesRun(finding)) return verificationLabels['uncertain'] ?? 'Sin confirmar'
  return verificationLabels[finding.verification.verdict] ?? 'Sin confirmar'
}

function verificationEvidence(finding: Finding): string {
  if (!verificationMatchesRun(finding)) return ''
  return (finding.verification.evidence ?? '').trim()
}

function verificationReason(finding: Finding): string {
  if (finding.verification.verdict && !verificationMatchesRun(finding)) {
    return 'La verificación corresponde a otro commit'
  }
  return (finding.verification.reason ?? '').trim()
}

// Un hallazgo de diseño muestra todas sus citas; sin citas conserva la
// ubicación histórica path:start–end.
function findingLocation(finding: Finding): string {
  const citations = finding.citations ?? []
  if (citations.length > 0) {
    return citations
      .map((citation) => {
        const range = `${citation.path}:${citation.start_line}–${citation.end_line}`
        return citation.side === 'LEFT' ? `${range} (código anterior)` : range
      })
      .join(' · ')
  }
  if (!finding.path) {
    return finding.scope === 'pr' ? 'todo el PR' : 'sin ruta'
  }
  if (finding.start_line <= 0) {
    return finding.path
  }
  return `${finding.path}:${finding.start_line}–${finding.end_line}`
}

function coverageCount(key: string): number {
  const value = latestRun.value?.coverage?.[key]
  return typeof value === 'number' ? value : 0
}

// `files` llega solo con las ejecuciones nuevas; sin lista no se pinta nada.
const coverageFiles = computed<CoverageFile[]>(() => {
  const files = latestRun.value?.coverage?.files
  return Array.isArray(files) && files.length > 0 ? files : []
})

function coverageMethodSuffix(file: CoverageFile): string {
  switch (file.method) {
    case 'ocr':
      return ' (OCR)'
    case 'text':
      return ' (ruta de texto)'
    default:
      return ''
  }
}

function coverageHumanSuffix(file: CoverageFile): string {
  return file.human ? ' — revisión humana' : ''
}

function coverageFileLabel(file: CoverageFile): string {
  switch (file.status) {
    case 'reviewed':
      return `Revisado${coverageMethodSuffix(file)}`
    case 'reviewed_by_parts':
      return typeof file.chunks === 'number'
        ? `Revisado por partes (${file.chunks} ${file.chunks === 1 ? 'trozo' : 'trozos'})${coverageMethodSuffix(file)}`
        : `Revisado por partes${coverageMethodSuffix(file)}`
    case 'not_reviewed':
      return file.reason ? `No revisado: ${file.reason}` : 'No revisado'
    case 'pending':
      return `${file.reason ? `Pendiente: ${file.reason}` : 'Pendiente'}${coverageHumanSuffix(file)}`
    case 'excluded':
      return file.reason ? `Excluido: ${file.reason}` : 'Excluido'
    case 'deleted':
      return 'Eliminado'
    case 'binary':
      return file.human ? 'Binario — revisión humana' : 'Binario'
    case 'submodule':
      return file.human ? 'Submódulo — revisión humana' : 'Submódulo'
    default:
      return file.status
  }
}

function coverageFileTone(status: string): string {
  if (status === 'reviewed' || status === 'reviewed_by_parts') return 'badge--ok'
  if (status === 'not_reviewed' || status === 'pending') return 'badge--bad'
  return 'badge--muted'
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

function humanDecisionLabel(verdict: HumanVerdict): string {
  return verdict.decision === 'changes_requested' ? 'Cambios requeridos' : 'Aprobado'
}

function humanInvalidationLabel(verdict: HumanVerdict): string {
  if (verdict.invalidated_reason === 'head_moved') return 'un push cambió el commit revisado'
  if (verdict.invalidated_reason === 'retarget') return 'la rama base o el merge base cambiaron'
  if (verdict.invalidated_reason === 'superseded') return 'se registró otra decisión'
  return 'la comparación revisada ya no está vigente'
}

function humanScopeFileLabel(file: HumanVerdictFile): string {
  if (file.start_line <= 0) return file.path
  if (file.end_line > 0 && file.end_line !== file.start_line) {
    return `${file.path}:${file.start_line}-${file.end_line}`
  }
  return `${file.path}:${file.start_line}`
}

function parseHumanScope(text: string): { files: HumanVerdictFile[]; error: string | null } {
  const files: HumanVerdictFile[] = []
  for (const raw of text.split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const match = /^(.+?):(\d+)(?:-(\d+))?$/.exec(line)
    if (match) {
      const start = Number(match[2])
      const end = match[3] ? Number(match[3]) : start
      if (end < start) return { files, error: `Rango inválido en ${line}` }
      files.push({ path: match[1], start_line: start, end_line: end })
      continue
    }
    files.push({ path: line, start_line: 0, end_line: 0 })
  }
  return { files, error: null }
}

/**
 * Registra la decisión humana de una revisión inconclusa. El alcance se escribe
 * como una línea por archivo (`ruta` o `ruta:inicio-fin`) y los pendientes por
 * su id; el backend lo ata a la comparación vigente.
 */
async function recordHumanVerdict(): Promise<void> {
  const conclusion = humanForm.value.conclusion.trim()
  if (!conclusion) {
    humanError.value = 'Escribe la conclusión de la revisión'
    return
  }
  const parsed = parseHumanScope(humanForm.value.scope)
  if (parsed.error) {
    humanError.value = parsed.error
    return
  }
  const pending = humanForm.value.pending
    .split(',')
    .map((value) => value.trim())
    .filter((value) => value !== '')
  if (!humanForm.value.complete && parsed.files.length === 0 && pending.length === 0) {
    humanError.value = 'Indica qué archivos o hallazgos revisaste, o marca el alcance como completo'
    return
  }

  recordingHuman.value = true
  humanError.value = null
  try {
    await api.recordHumanVerdict(prId.value, {
      decision: humanForm.value.decision === 'changes_requested' ? 'changes_requested' : 'approved',
      conclusion,
      reason: humanForm.value.reason.trim() || undefined,
      scope: { files: parsed.files, pending_findings: pending, complete: humanForm.value.complete },
    })
    humanForm.value = { decision: 'approved', conclusion: '', reason: '', scope: '', pending: '', complete: false }
    await load()
  } catch (err) {
    humanError.value = isForbidden(err)
      ? 'Solo un admin, el lead o QA del equipo pueden registrar el resultado humano'
      : isConflict(err)
        ? 'La revisión automática ya no está inconclusa'
        : 'No se pudo registrar el resultado humano'
  } finally {
    recordingHuman.value = false
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
  ['pr.updated', 'run.updated'],
  () => load({ silent: true }),
  (event) =>
    event.type === 'pr.updated'
      ? event.id === String(prId.value)
      : (detail.value?.runs ?? []).some((run) => String(run.id) === event.id),
)

onMounted(() => {
  clock = setInterval(() => (now.value = new Date()), 1000)
  void load()
})

onBeforeUnmount(() => {
  if (clock !== null) clearInterval(clock)
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

        <section v-if="humanVerdict" class="tile human-result" aria-labelledby="human-result-title" data-human-verdict>
          <div class="human-result__head">
            <div>
              <h2 id="human-result-title" class="tile__title">Resultado humano</h2>
              <p class="caption">Decisión de una persona, aparte de la revisión automática</p>
            </div>
            <span class="badge" :class="humanVerdict.current ? 'badge--ok' : 'badge--muted'" data-human-verdict-state>
              {{ humanVerdict.current ? humanDecisionLabel(humanVerdict) : 'Obsoleta' }}
            </span>
          </div>
          <p v-if="!humanVerdict.current" class="human-result__stale" data-human-verdict-stale>
            Invalidada porque {{ humanInvalidationLabel(humanVerdict) }}. La revisión automática vuelve a contar.
          </p>
          <p class="human-result__meta">
            <code>{{ humanVerdict.actor_login }}</code> ·
            {{ humanVerdict.scope.complete ? 'alcance completo' : 'alcance parcial' }} ·
            <code>{{ humanVerdict.head_sha }}</code>
          </p>
          <ul v-if="humanVerdict.scope.files.length > 0" class="human-result__scope" data-human-verdict-scope>
            <li v-for="file in humanVerdict.scope.files" :key="humanScopeFileLabel(file)">
              <code>{{ humanScopeFileLabel(file) }}</code>
            </li>
          </ul>
          <ul v-if="humanVerdict.scope.pending_findings.length > 0" class="human-result__scope">
            <li v-for="id in humanVerdict.scope.pending_findings" :key="id">
              hallazgo <code>{{ id }}</code>
            </li>
          </ul>
          <p class="human-result__conclusion"><strong>Conclusión:</strong> {{ humanVerdict.conclusion }}</p>
          <p v-if="humanVerdict.reason" class="caption">Motivo: {{ humanVerdict.reason }}</p>
        </section>

        <section v-if="canRecordHumanVerdict" class="tile human-form" aria-labelledby="human-form-title" data-human-verdict-form>
          <div class="human-result__head">
            <div>
              <h2 id="human-form-title" class="tile__title">Registrar resultado humano</h2>
              <p class="caption">La automatización no concluyó. Registra qué revisaste y tu decisión.</p>
            </div>
          </div>
          <p v-if="humanError" class="form-error" role="alert" data-human-verdict-error>{{ humanError }}</p>
          <label class="field human-form__field">
            <span class="field__label">Decisión</span>
            <select v-model="humanForm.decision" class="field__input" data-human-verdict-decision>
              <option value="approved">Aprobado</option>
              <option value="changes_requested">Cambios requeridos</option>
            </select>
          </label>
          <label class="field human-form__field">
            <span class="field__label">Conclusión (obligatoria)</span>
            <textarea v-model="humanForm.conclusion" class="field__input" rows="2" data-human-verdict-conclusion />
          </label>
          <label class="field human-form__field">
            <span class="field__label">Archivos o rangos revisados (uno por línea: ruta o ruta:inicio-fin)</span>
            <textarea v-model="humanForm.scope" class="field__input" rows="3" data-human-verdict-scope-input />
          </label>
          <label class="field human-form__field">
            <span class="field__label">Hallazgos pendientes revisados (ids separados por coma)</span>
            <input v-model="humanForm.pending" class="field__input" type="text" data-human-verdict-pending />
          </label>
          <label class="human-form__check">
            <input v-model="humanForm.complete" type="checkbox" data-human-verdict-complete />
            <span>Cubrí todo el alcance que quedó pendiente</span>
          </label>
          <label class="field human-form__field">
            <span class="field__label">Motivo (opcional)</span>
            <input v-model="humanForm.reason" class="field__input" type="text" data-human-verdict-reason />
          </label>
          <button
            class="button button--primary"
            type="button"
            data-human-verdict-submit
            :disabled="recordingHuman"
            @click="recordHumanVerdict"
          >
            {{ recordingHuman ? 'Registrando…' : 'Registrar resultado' }}
          </button>
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
                <div
                  v-if="run.status === 'running' || run.status === 'queued'"
                  class="run__time"
                  data-run-phase
                >
                  <dt>Fase</dt>
                  <dd data-run-phase-value>{{ runPhaseText(run) }}</dd>
                </div>
              </dl>
              <p v-if="run.summary" class="run__summary">{{ run.summary }}</p>
              <p v-if="run.error_message" class="run__error">
                <code>{{ run.error_code }}</code>: {{ run.error_message }}
              </p>
              <button
                class="button button--ghost run__log-toggle"
                type="button"
                data-run-log-toggle
                @click="toggleLog(run.id)"
              >
                {{
                  openLogRunId === run.id
                    ? 'Ocultar log'
                    : run.status === 'running' || run.status === 'queued'
                      ? 'Ver log en vivo'
                      : 'Ver log'
                }}
              </button>
              <RunLogPanel
                v-if="openLogRunId === run.id"
                :key="run.id"
                :run-id="run.id"
                :live="run.status === 'running' || run.status === 'queued'"
              />
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
                <div class="run__token" data-run-max-context>
                  <dt>Mayor prompt</dt>
                  <dd><code>{{ run.max_context_tokens === null ? '—' : formatTokens(run.max_context_tokens) }}</code></dd>
                </div>
              </dl>
              <p v-else class="run__no-tokens" data-run-no-tokens>
                {{ run.finished_at === null
                  ? 'Sin datos de tokens: la revisión aún no ha informado su consumo'
                  : run.error_code
                    ? 'Sin datos de tokens: la revisión se cortó antes de que OCR informara el consumo'
                    : 'Sin datos de tokens: ejecución anterior al registro de consumo' }}
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
            <div class="coverage__item" data-coverage-not-required>
              <dt>Archivos no requeridos</dt>
              <dd><code>{{ coverageCount('not_required_files') }}</code></dd>
            </div>
          </dl>

          <ul
            v-if="coverageFiles.length > 0"
            class="coverage-files"
            aria-label="Cobertura por archivo"
            data-coverage-files
          >
            <li v-for="file in coverageFiles" :key="file.path" class="coverage-files__item" data-coverage-file>
              <code class="coverage-files__path">{{ file.path }}</code>
              <span class="badge" :class="coverageFileTone(file.status)" data-coverage-file-status>
                {{ coverageFileLabel(file) }}
              </span>
            </li>
          </ul>
        </section>
      </div>

      <div class="split__main">
        <section class="pr-section" aria-label="Hallazgos">
          <div class="pr-section__head">
            <h2 class="pr-section__title">Hallazgos</h2>
            <span class="pr-section__count">{{ detail.findings.length }} en total</span>
          </div>

          <FilterBar
            :q="q"
            :shown="filteredFindings.length"
            :total="detail.findings.length"
            :chips="chips"
            :active-count="activeCount"
            :sheet-open="sheetOpen"
            :sheet-id="sheetId"
            @update:q="setQuery"
            @clear="clear"
            @remove="removeChip"
            @open-sheet="sheetOpen = true"
          >
            <div class="findings-bar">
              <div class="finding-chips" role="group" aria-label="Severidad">
                <button
                  v-for="chip in severityChips"
                  :key="chip.value"
                  type="button"
                  class="finding-chip"
                  :class="{ 'finding-chip--active': facets.severity.includes(chip.value) }"
                  :aria-pressed="facets.severity.includes(chip.value)"
                  :data-severity-chip="chip.value"
                  @click="toggleFacet('severity', chip.value)"
                >
                  {{ chip.label }}<span class="finding-chip__count" data-severity-count>{{ chip.count }}</span>
                </button>
              </div>
              <div class="finding-status" role="group" aria-label="Estado">
                <button
                  v-for="tab in STATUS_TABS"
                  :key="tab.value || 'all'"
                  type="button"
                  class="finding-tab"
                  :class="{ 'finding-tab--active': statusSelection === tab.value }"
                  :aria-pressed="statusSelection === tab.value"
                  :data-status-tab="tab.value || 'all'"
                  @click="setStatus(tab.value)"
                >
                  {{ tab.label }}
                </button>
              </div>
            </div>
          </FilterBar>

          <div class="list-scroll findings__scroll">
            <EmptyState
              v-if="filteredFindings.length === 0 && isActive"
              title="NADA COINCIDE CON ESTOS FILTROS"
              action-label="LIMPIAR FILTROS"
              @action="clear"
            />
            <p v-else-if="filteredFindings.length === 0" class="empty-state">
              No se encontraron hallazgos
            </p>
            <template v-else>
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
                        <code>{{ findingLocation(finding) }}</code>
                        <span v-if="finding.category" class="finding__category">{{ finding.category }}</span>
                        <span v-if="finding.advisory" class="finding__label">observación</span>
                      </p>
                    </div>
                    <p class="finding__body">{{ finding.body }}</p>
                    <p v-if="finding.suggestion" class="finding__suggestion">
                      <span class="finding__label">Sugerencia</span>
                      <span>{{ finding.suggestion }}</span>
                    </p>
                    <p v-if="verificationLabel(finding)" class="finding__verification" data-verification>
                      <span class="finding__label">Verificación automática</span>
                      <span data-verification-verdict>{{ verificationLabel(finding) }}</span>
                      <template v-if="verificationEvidence(finding)">
                        <span class="finding__label">Evidencia</span>
                        <code data-verification-evidence>{{ verificationEvidence(finding) }}</code>
                      </template>
                      <template v-if="verificationReason(finding)">
                        <span class="finding__label">Motivo</span>
                        <span data-verification-reason>{{ verificationReason(finding) }}</span>
                      </template>
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
                      <code>{{ findingLocation(finding) }}</code>
                      <span v-if="finding.category" class="finding__category">{{ finding.category }}</span>
                    </p>
                  </div>
                  <p v-if="verificationLabel(finding)" class="finding__verification" data-verification>
                    <span class="finding__label">Verificación automática</span>
                    <span data-verification-verdict>{{ verificationLabel(finding) }}</span>
                    <template v-if="verificationEvidence(finding)">
                      <span class="finding__label">Evidencia</span>
                      <code data-verification-evidence>{{ verificationEvidence(finding) }}</code>
                    </template>
                    <template v-if="verificationReason(finding)">
                      <span class="finding__label">Motivo</span>
                      <span data-verification-reason>{{ verificationReason(finding) }}</span>
                    </template>
                  </p>
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
            </template>
          </div>
        </section>

        <FilterSheet
          :id="sheetId"
          :open="sheetOpen"
          :active-count="activeCount"
          :shown="filteredFindings.length"
          clearable
          :apply-label="`VER ${filteredFindings.length} hallazgos`"
          @close="sheetOpen = false"
          @clear="clear"
        >
          <div class="findings__sheet-filters">
            <div class="finding-chips" role="group" aria-label="Severidad">
              <button
                v-for="chip in severityChips"
                :key="chip.value"
                type="button"
                class="finding-chip"
                :class="{ 'finding-chip--active': facets.severity.includes(chip.value) }"
                :aria-pressed="facets.severity.includes(chip.value)"
                :data-severity-chip="chip.value"
                @click="toggleFacet('severity', chip.value)"
              >
                {{ chip.label }}<span class="finding-chip__count" data-severity-count>{{ chip.count }}</span>
              </button>
            </div>
            <div class="finding-status" role="group" aria-label="Estado">
              <button
                v-for="tab in STATUS_TABS"
                :key="tab.value || 'all'"
                type="button"
                class="finding-tab"
                :class="{ 'finding-tab--active': statusSelection === tab.value }"
                :aria-pressed="statusSelection === tab.value"
                :data-status-tab="tab.value || 'all'"
                @click="setStatus(tab.value)"
              >
                {{ tab.label }}
              </button>
            </div>
          </div>
        </FilterSheet>
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
          <code>{{ findingLocation(confirming.finding) }}</code>
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
  flex: 1;
  flex-direction: column;
  gap: var(--space-5);
  min-height: 0;
}

.findings-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2) var(--space-3);
}

.finding-chips,
.finding-status {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.finding-chip,
.finding-tab {
  appearance: none;
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 44px;
  padding: 0 var(--space-4);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
  background: transparent;
  color: var(--text-muted);
  font: inherit;
  font-size: 12px;
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  transition:
    background-color var(--dur-micro) ease,
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease;
}

.finding-chip:hover:not(.finding-chip--active),
.finding-tab:hover:not(.finding-tab--active) {
  border-color: var(--border-strong);
  color: var(--text);
}

.finding-chip--active {
  border-color: var(--accent);
  color: var(--accent);
}

.finding-tab--active {
  border-color: var(--text);
  background: var(--text);
  color: var(--bg);
}

.finding-chip__count {
  color: var(--text-faint);
  font-family: var(--font-display);
  font-size: 15px;
  font-weight: 900;
  font-variant-numeric: tabular-nums;
}

.finding-chip--active .finding-chip__count {
  color: var(--accent);
}

.findings__scroll {
  padding-right: var(--space-1);
}

.findings__sheet-filters {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

/* The shell gives this view the leftover height. Below the split breakpoint
   the whole detail scrolls; at wide widths only the findings list does. */
@media (max-width: 2199.98px) {
  .pr-detail {
    overflow-y: auto;
  }

  .findings__scroll {
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

.run__log-toggle {
  align-self: flex-start;
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

.coverage-files {
  display: flex;
  flex-direction: column;
  margin: var(--space-3) 0 0;
  padding: 0;
  border: var(--rule-hair) solid var(--border);
  list-style: none;
}

.coverage-files__item {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
  min-width: 0;
  padding: var(--space-2) var(--space-4);
}

.coverage-files__item + .coverage-files__item {
  border-top: var(--rule-hair) solid var(--border);
}

.coverage-files__path {
  min-width: 0;
  overflow-wrap: anywhere;
}

.coverage-files__item .badge {
  flex: none;
  font-size: 11px;
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

.finding__category {
  margin-left: var(--space-2);
  color: var(--text-faint);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-transform: uppercase;
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

.finding__verification {
  max-width: 72ch;
  color: var(--text-muted);
  font-size: 15px;
}

.finding__verification code {
  display: block;
  overflow-wrap: anywhere;
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

.human-result__head {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}

.human-result__stale {
  color: var(--bad);
  font-size: 15px;
}

.human-result__meta {
  color: var(--text-muted);
  font-size: 13px;
}

.human-result__scope {
  margin: var(--space-2) 0;
  padding-left: var(--space-5);
  font-size: 14px;
}

.human-result__conclusion {
  max-width: 72ch;
  overflow-wrap: anywhere;
}

.human-form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.human-form__field {
  margin: 0;
}

.human-form__field .field__input {
  width: 100%;
}

.human-form__check {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 14px;
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
