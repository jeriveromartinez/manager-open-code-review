<script setup lang="ts">
import { computed, onMounted, ref, useId } from 'vue'
import {
  api,
  ApiError,
  type ConsumerRef,
  type Installation,
  type QAPolicy,
  type QAPolicyInput,
  type Repository,
  type RepositoryPolicy,
  type RepositoryPolicyInput,
  type RepositoryPolicyOverrides,
  type RequiredCheck,
  type SetupStatus,
  type Team,
} from '../api'
import EmptyState from '../components/EmptyState.vue'
import FilterBar from '../components/FilterBar.vue'
import FilterDropdown from '../components/FilterDropdown.vue'
import FilterSheet from '../components/FilterSheet.vue'
import { useListFilters } from '../composables/useListFilters'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const repos = ref<Repository[]>([])
const teams = ref<Team[]>([])
const installations = ref<Installation[]>([])
const setupStatus = ref<SetupStatus | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const refreshing = ref(false)
const syncPending = ref<number[]>([])
const syncingAll = ref(false)
const syncMessage = ref<string | null>(null)
const syncError = ref<string | null>(null)

/** Solo los administradores pueden cambiar la configuración de los repositorios. */
const canEdit = computed(() => session.me?.is_admin === true)

/** Un lead del equipo del repositorio también puede editar sus overrides. */
function canManageRepo(repo: Repository): boolean {
  if (canEdit.value) return true
  if (repo.team_id === null) return false
  return session.me?.teams.some((team) => team.id === repo.team_id && team.role === 'lead') ?? false
}

function teamName(id: number | null): string {
  if (id === null) return 'Sin equipo'
  return teams.value.find((team) => team.id === id)?.name ?? 'Sin equipo'
}

const { q, facets, isActive, activeCount, setFacet, clear, apply, facetCounts } = useListFilters({
  facets: ['account', 'team', 'visibility', 'policy', 'enabled'],
})

const sheetOpen = ref(false)
const sheetId = `repos-filters-${useId()}`

function teamKey(id: number | null): string {
  return id === null ? '__none__' : String(id)
}

function teamKeyLabel(key: string): string {
  if (key === '__none__') return 'Sin equipo'
  const id = Number(key)
  return teams.value.some((team) => team.id === id) ? teamName(id) : key
}

function visibilityLabel(value: string): string {
  return value === 'private' ? 'Privado' : 'Público'
}

// `policy_observation` is a boolean flag in the API (api.ts), so it becomes a
// single «Observación» tag next to the policy and override tags.
const POLICY_OPTIONS = [
  { value: 'observation', label: 'Observación' },
  { value: 'policy', label: 'Con política' },
  { value: 'override', label: 'Con override' },
]

function policyLabel(value: string): string {
  return POLICY_OPTIONS.find((option) => option.value === value)?.label ?? value
}

function enabledLabel(value: string): string {
  return value === 'on' ? 'Activada' : 'Desactivada'
}

type RepositoryFacet = 'account' | 'team' | 'visibility' | 'policy' | 'enabled'

function facetValues(repo: Repository, facet: RepositoryFacet): string[] {
  switch (facet) {
    case 'account':
      return [repo.account_login]
    case 'team':
      return [teamKey(repo.team_id)]
    case 'visibility':
      return [repo.private ? 'private' : 'public']
    case 'policy': {
      const values: string[] = []
      if (repo.policy_observation) values.push('observation')
      if (repo.has_policy) values.push('policy')
      if (repo.has_policy_override) values.push('override')
      return values
    }
    case 'enabled':
      return [repo.enabled ? 'on' : 'off']
  }
}

function searchText(repo: Repository): string {
  return [repo.full_name, repo.account_login].join(' ')
}

const filteredRepos = apply(
  () => repos.value,
  searchText,
  (repo, active) =>
    Object.entries(active).every(([facet, selected]) => {
      if (selected.length === 0) return true
      return facetValues(repo, facet as RepositoryFacet).some((value) => selected.includes(value))
    }),
)

const counts = facetCounts(
  () => repos.value,
  (repo, facet) => facetValues(repo, facet as RepositoryFacet),
  searchText,
)

function optionCount(facet: string, value: string): number {
  return counts.value[facet]?.[value] ?? 0
}

const accountOptions = computed(() => {
  const values = new Set(repos.value.map((repo) => repo.account_login))
  for (const value of facets.value.account) values.add(value)
  return [...values]
    .sort((a, b) => a.localeCompare(b, 'es'))
    .map((value) => ({ value, label: value, count: optionCount('account', value) }))
})

const teamOptions = computed(() => {
  const values = new Set(repos.value.map((repo) => teamKey(repo.team_id)))
  for (const value of facets.value.team) values.add(value)
  return [...values]
    .sort((a, b) => teamKeyLabel(a).localeCompare(teamKeyLabel(b), 'es'))
    .map((value) => ({ value, label: teamKeyLabel(value), count: optionCount('team', value) }))
})

const visibilityOptions = computed(() =>
  ['public', 'private'].map((value) => ({
    value,
    label: visibilityLabel(value),
    count: optionCount('visibility', value),
  })),
)

const policyOptions = computed(() =>
  POLICY_OPTIONS.map((option) => ({
    ...option,
    count: optionCount('policy', option.value),
  })),
)

const enabledSegments = computed(() => {
  const on = optionCount('enabled', 'on')
  const off = optionCount('enabled', 'off')
  return [
    { value: '', label: 'Todas', count: on + off },
    { value: 'on', label: 'Activada', count: on },
    { value: 'off', label: 'Desactivada', count: off },
  ]
})

const enabledSelection = computed(() => facets.value.enabled[0] ?? '')

function setEnabledSegment(value: string): void {
  setFacet('enabled', value === '' ? [] : [value])
}

const CHIP_LABELS: Record<string, (value: string) => string> = {
  account: (value) => `Cuenta: ${value}`,
  team: (value) => `Equipo: ${teamKeyLabel(value)}`,
  visibility: (value) => `Visibilidad: ${visibilityLabel(value)}`,
  policy: (value) => `Política: ${policyLabel(value)}`,
  enabled: (value) => `Revisión: ${enabledLabel(value)}`,
}

const chips = computed(() =>
  Object.entries(facets.value).flatMap(([facet, values]) =>
    values.map((value) => ({
      key: `${facet}:${value}`,
      label: CHIP_LABELS[facet]?.(value) ?? value,
    })),
  ),
)

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

async function load({ silent = false } = {}): Promise<void> {
  if (!silent) loading.value = true
  error.value = null
  try {
    await session.load()
    const [repoList, teamList] = await Promise.all([api.repos(), api.teams()])
    repos.value = repoList
    teams.value = teamList

    if (canEdit.value) {
      const [installationsResult, setupResult] = await Promise.allSettled([
        api.installations(),
        api.setupStatus(),
      ])
      installations.value =
        installationsResult.status === 'fulfilled' ? installationsResult.value : []
      setupStatus.value = setupResult.status === 'fulfilled' ? setupResult.value : null
    } else {
      installations.value = []
      setupStatus.value = null
    }
  } catch {
    error.value = 'No se pudieron cargar los repositorios'
  } finally {
    loading.value = false
  }
}

function isSyncing(id: number): boolean {
  return syncPending.value.includes(id)
}

function clearSyncFeedback(): void {
  syncMessage.value = null
  syncError.value = null
}

async function refresh(): Promise<void> {
  refreshing.value = true
  clearSyncFeedback()
  try {
    await load()
  } finally {
    refreshing.value = false
  }
}

async function syncInstallation(id: number): Promise<void> {
  if (isSyncing(id)) return
  clearSyncFeedback()
  syncPending.value = [...syncPending.value, id]
  try {
    await api.syncInstallation(id)
    syncMessage.value = 'Sincronización encolada. Los repositorios aparecerán en unos segundos.'
  } catch {
    syncError.value = 'No se pudo encolar la sincronización.'
  } finally {
    syncPending.value = syncPending.value.filter((pending) => pending !== id)
  }
}

async function syncAll(): Promise<void> {
  if (syncingAll.value || installations.value.length === 0) return
  clearSyncFeedback()
  syncingAll.value = true
  try {
    const results = await Promise.allSettled(
      installations.value.map((installation) => api.syncInstallation(installation.id)),
    )
    if (results.some((result) => result.status === 'rejected')) {
      syncError.value = 'No se pudo encolar la sincronización.'
    } else {
      syncMessage.value = 'Sincronización encolada. Los repositorios aparecerán en unos segundos.'
    }
  } finally {
    syncingAll.value = false
  }
}

async function setEnabled(repo: Repository, event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const enabled = input.checked
  try {
    await api.patchRepo(repo.id, { enabled })
    repo.enabled = enabled
  } catch {
    input.checked = repo.enabled
    error.value = 'No se pudo actualizar la revisión automática'
  }
}

async function setTeam(repo: Repository, event: Event): Promise<void> {
  const select = event.target as HTMLSelectElement
  const raw = select.value
  const team_id = raw === '' ? null : Number(raw)
  try {
    await api.patchRepo(repo.id, { team_id })
  } catch {
    select.value = repo.team_id === null ? '' : String(repo.team_id)
    error.value = 'No se pudo asignar el equipo'
    return
  }
  // Only the server knows the policy of the newly assigned team, so the
  // inherited-policy warning and the review state must be refreshed.
  try {
    repos.value = await api.repos()
  } catch {
    error.value = `Se guardó el equipo de ${repo.full_name}. Vuelve a cargar para ver la configuración actual.`
  }
}

type OverrideKind = 'number' | 'select' | 'boolean' | 'textarea'

type OverrideField = {
  key: string
  label: string
  kind: OverrideKind
  options?: { value: string; label: string }[]
}

// Campos que se pueden sobrescribir por repositorio. El alcance y la
// profundidad viven aquí; el perfil LLM sigue siendo del equipo (#153).
const policyFields: OverrideField[] = [
  {
    key: 'blocking_threshold',
    label: 'Umbral bloqueante',
    kind: 'select',
    options: [
      { value: 'blocker', label: 'Bloqueante' },
      { value: 'major', label: 'Mayor' },
      { value: 'minor', label: 'Menor' },
      { value: 'nit', label: 'Nit' },
    ],
  },
  {
    key: 'review_effort',
    label: 'Profundidad (esfuerzo)',
    kind: 'select',
    options: [
      { value: 'low', label: 'Bajo · 1 ronda' },
      { value: 'medium', label: 'Medio · 2 rondas' },
      { value: 'high', label: 'Alto · 3 rondas' },
    ],
  },
  { key: 'max_files', label: 'Máx. archivos por pasada', kind: 'number' },
  { key: 'max_diff_lines', label: 'Máx. líneas de diff por pasada', kind: 'number' },
  { key: 'max_passes', label: 'Máx. pasadas', kind: 'number' },
  {
    key: 'extra_instructions',
    label: 'Instrucciones extra',
    kind: 'textarea',
  },
  { key: 'review_context_ceiling', label: 'Techo de contexto', kind: 'number' },
  { key: 'review_token_budget', label: 'Tope de gasto', kind: 'number' },
  { key: 'require_green_ci', label: 'Exigir CI en verde', kind: 'boolean' },
  {
    key: 'auto_triage_mode',
    label: 'Verificación automática',
    kind: 'select',
    options: [
      { value: 'off', label: 'Apagada' },
      { value: 'dry_run', label: 'Solo simular' },
      { value: 'apply', label: 'Aplicar' },
    ],
  },
  { key: 'auto_withdraw', label: 'Descartar automáticamente', kind: 'boolean' },
  { key: 'verify_max_findings', label: 'Hallazgos por verificar', kind: 'number' },
  { key: 'verify_max_files', label: 'Archivos por verificar', kind: 'number' },
  { key: 'verify_token_share', label: '% de tokens para verificar', kind: 'number' },
  { key: 'verify_time_share', label: '% de tiempo para verificar', kind: 'number' },
  { key: 'verify_token_max', label: 'Tope de tokens de verificación', kind: 'number' },
]

const policyOpen = ref(false)
const policyRepo = ref<Repository | null>(null)
const repoPolicy = ref<RepositoryPolicy | null>(null)
const policyLoading = ref(false)
const policySaving = ref(false)
const policyError = ref<string | null>(null)
const policyMessage = ref<string | null>(null)
const observationMode = ref(false)
const fieldEnabled = ref<Record<string, boolean>>({})
const fieldValues = ref<Record<string, string | number>>({})
const globsEnabled = ref(false)
const globsRaw = ref('')
const criticalGlobsEnabled = ref(false)
const criticalGlobsRaw = ref('')
const checksEnabled = ref(false)
const checksRaw = ref('')

type ConsumerRow = {
  key: number
  repository: string
  ref: string
  contract: string
  path: string
  installation_id?: number
  essential?: boolean
  visibility?: ConsumerRef['visibility']
  version?: string
}

let nextConsumerKey = 0

function newConsumerRow(consumer?: ConsumerRef): ConsumerRow {
  return {
    key: nextConsumerKey++,
    repository: consumer?.repository ?? '',
    ref: consumer?.ref ?? '',
    contract: consumer?.contract ?? '',
    path: consumer?.path ?? '',
    installation_id: consumer?.installation_id,
    essential: consumer?.essential,
    visibility: consumer?.visibility,
    version: consumer?.version,
  }
}

const consumerRows = ref<ConsumerRow[]>([])
const consumerBaselinePath = ref('')
const consumerMaxLookups = ref<number | ''>('')
const consumerMaxBytes = ref<number | ''>('')

function effectiveValue(policy: QAPolicy, key: string): string {
  const values: Record<string, string | number | boolean | null> = {
    blocking_threshold: policy.blocking_threshold,
    review_effort: policy.review_effort,
    max_files: policy.max_files,
    max_diff_lines: policy.max_diff_lines,
    max_passes: policy.max_passes,
    extra_instructions: policy.extra_instructions,
    review_context_ceiling: policy.review_context_ceiling,
    review_token_budget: policy.review_token_budget,
    require_green_ci: policy.require_green_ci,
    auto_triage_mode: policy.auto_triage_mode,
    auto_withdraw: policy.auto_withdraw,
    verify_max_findings: policy.verify_max_findings,
    verify_max_files: policy.verify_max_files,
    verify_token_share: policy.verify_token_share,
    verify_time_share: policy.verify_time_share,
    verify_token_max: policy.verify_token_max,
  }
  const value = values[key]
  return value === null || value === undefined ? '' : String(value)
}

function formatChecks(checks: RequiredCheck[] | null | undefined): string {
  return (checks ?? [])
    .map((check) => (check.origin ? `${check.name} | ${check.origin}` : check.name))
    .join('\n')
}

function parseChecks(raw: string): RequiredCheck[] {
  const checks: RequiredCheck[] = []
  for (const line of raw.split('\n')) {
    const [name, origin] = line.split('|').map((part) => part.trim())
    if (!name) continue
    checks.push(origin ? { name, origin } : { name })
  }
  return checks
}

function parseLines(raw: string): string[] {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

function addConsumer(): void {
  consumerRows.value = [...consumerRows.value, newConsumerRow()]
}

function removeConsumer(index: number): void {
  consumerRows.value = consumerRows.value.filter((_, current) => current !== index)
}

function consumerPayload(row: ConsumerRow): ConsumerRef {
  const consumer: ConsumerRef = {
    repository: row.repository.trim(),
    ref: row.ref.trim(),
    contract: row.contract.trim(),
  }
  const path = row.path.trim()
  if (path) consumer.path = path
  if (row.installation_id !== undefined) consumer.installation_id = row.installation_id
  if (row.essential) consumer.essential = true
  if (row.visibility) consumer.visibility = row.visibility
  if (row.version) consumer.version = row.version
  return consumer
}

// The allow-list is only sent when it changed. Comparing the visible fields
// keeps hidden metadata (installation, visibility, version) out of the diff.
function consumerListChanged(original: ConsumerRef[]): boolean {
  const rows = consumerRows.value
  if (rows.length !== original.length) return true
  return rows.some((row, index) => {
    const consumer = consumerPayload(row)
    const stored = original[index]
    return (
      consumer.repository !== stored.repository ||
      consumer.ref !== stored.ref ||
      consumer.contract !== stored.contract ||
      (consumer.path ?? consumer.contract) !== (stored.path || stored.contract)
    )
  })
}

// Only the fields the user touched travel in the PUT; the rest keep their
// stored value because consumer settings are replaced as a whole by the API.
function consumerChanges(): Pick<
  QAPolicyInput,
  'consumer_allow_list' | 'consumer_baseline_path' | 'consumer_max_lookups' | 'consumer_max_bytes'
> {
  const effective = repoPolicy.value?.effective
  const changes: ReturnType<typeof consumerChanges> = {}
  if (!effective) return changes
  if (consumerListChanged(effective.consumer_allow_list ?? [])) {
    changes.consumer_allow_list = consumerRows.value.map(consumerPayload)
  }
  const baseline = consumerBaselinePath.value.trim()
  if (baseline !== (effective.consumer_baseline_path ?? '').trim()) {
    changes.consumer_baseline_path = baseline
  }
  if (
    consumerMaxLookups.value !== '' &&
    Number(consumerMaxLookups.value) !== effective.consumer_max_lookups
  ) {
    changes.consumer_max_lookups = Number(consumerMaxLookups.value)
  }
  if (
    consumerMaxBytes.value !== '' &&
    Number(consumerMaxBytes.value) !== effective.consumer_max_bytes
  ) {
    changes.consumer_max_bytes = Number(consumerMaxBytes.value)
  }
  return changes
}

function consumerName(consumer: ConsumerRow): string {
  const repository = consumer.repository.trim()
  const ref = consumer.ref.trim()
  if (repository && ref) return `${repository} ${ref}`
  return repository || ref || 'sin identificar'
}

function validateConsumers(): string | null {
  for (const [index, consumer] of consumerRows.value.entries()) {
    const position = index + 1
    const repository = consumer.repository.trim()
    if (!repository) {
      return `El consumidor ${position} necesita un repositorio (owner/repo)`
    }
    if (!consumer.ref.trim()) {
      return `El consumidor ${position} (${repository}) necesita una ref fija`
    }
    if (!consumer.contract.trim()) {
      return `El consumidor ${position} (${repository}) necesita un contrato`
    }
  }

  if (consumerMaxLookups.value === '') {
    return 'El máximo de consultas no puede quedar vacío; usa un valor entre 1 y 50'
  }
  const lookups = Number(consumerMaxLookups.value)
  if (!Number.isInteger(lookups) || lookups < 1 || lookups > 50) {
    return 'El máximo de consultas debe ser un entero entre 1 y 50'
  }

  if (consumerMaxBytes.value === '') {
    return 'El máximo de bytes no puede quedar vacío; usa un valor entre 4096 y 16777216'
  }
  const bytes = Number(consumerMaxBytes.value)
  if (!Number.isInteger(bytes) || bytes < 4096 || bytes > 16 * 1024 * 1024) {
    return 'El máximo de bytes debe ser un entero entre 4096 y 16777216'
  }
  return null
}

function teamPolicyBody(policy: QAPolicy): QAPolicyInput {
  return {
    llm_profile_id: policy.llm_profile_id,
    blocking_threshold: policy.blocking_threshold,
    extra_instructions: policy.extra_instructions,
    exclude_globs: policy.exclude_globs,
    critical_globs: policy.critical_globs,
    max_files: policy.max_files,
    max_diff_lines: policy.max_diff_lines,
    max_passes: policy.max_passes,
    require_green_ci: policy.require_green_ci,
    required_checks: policy.required_checks,
    review_context_ceiling: policy.review_context_ceiling,
    review_effort: policy.review_effort,
    review_token_budget: policy.review_token_budget,
    auto_triage_mode: policy.auto_triage_mode,
    auto_withdraw: policy.auto_withdraw,
    verify_max_findings: policy.verify_max_findings,
    verify_max_files: policy.verify_max_files,
    verify_token_share: policy.verify_token_share,
    verify_time_share: policy.verify_time_share,
    verify_token_max: policy.verify_token_max,
    allow_external_reviews: policy.allow_external_reviews,
    external_trusted_associations: policy.external_trusted_associations,
    max_external_diff_lines: policy.max_external_diff_lines,
    max_external_runs_per_day: policy.max_external_runs_per_day,
    external_daily_token_budget: policy.external_daily_token_budget,
    // The team PUT rebuilds omitted fields from zero, so the architecture
    // settings must travel with the body or a consumer save resets them.
    architecture_analysis_mode: policy.architecture_analysis_mode,
    architecture_token_share: policy.architecture_token_share,
    architecture_time_share: policy.architecture_time_share,
    architecture_token_max: policy.architecture_token_max,
    architecture_baseline_path: policy.architecture_baseline_path,
    architecture_exception_policy: policy.architecture_exception_policy,
  }
}

function backendError(err: unknown): string {
  if (typeof err !== 'object' || err === null) return ''
  const message = (err as { message?: unknown }).message
  if (typeof message !== 'string') return ''
  try {
    const parsed = JSON.parse(message) as { error?: unknown }
    if (typeof parsed.error === 'string') return parsed.error
  } catch {
    // The body was not JSON; fall through to the generic message.
  }
  return ''
}

const consumerErrors: Record<string, string> = {
  invalid_consumer_allow_list: 'La lista de consumidores no es válida',
  invalid_consumer_baseline_path: 'La ruta de línea base de consumidores no es válida',
  invalid_consumer_max_lookups: 'El máximo de consultas de consumidores no es válido',
  invalid_consumer_max_bytes: 'El máximo de bytes de consumidores no es válido',
}

function consumerErrorMessage(err: unknown): string {
  return consumerErrors[backendError(err)] ?? 'No se pudo guardar la política del equipo'
}

function saveErrorMessage(): string {
  return 'No se pudo guardar la política del repositorio'
}

function fillFromPolicy(policy: RepositoryPolicy): void {
  repoPolicy.value = policy
  observationMode.value = policy.overrides.observation_mode
  const enabled: Record<string, boolean> = {}
  const values: Record<string, string | number> = {}
  for (const field of policyFields) {
    const override = policy.overrides[field.key as keyof RepositoryPolicyOverrides]
    enabled[field.key] = override !== null && override !== undefined
    values[field.key] = enabled[field.key] ? String(override) : effectiveValue(policy.effective, field.key)
  }
  fieldEnabled.value = enabled
  fieldValues.value = values
  globsEnabled.value = policy.overrides.exclude_globs !== null
  globsRaw.value = (policy.overrides.exclude_globs ?? policy.effective.exclude_globs ?? []).join('\n')
  criticalGlobsEnabled.value = policy.overrides.critical_globs !== null
  criticalGlobsRaw.value = (policy.overrides.critical_globs ?? policy.effective.critical_globs ?? []).join('\n')
  checksEnabled.value = policy.overrides.required_checks !== null
  checksRaw.value = formatChecks(policy.overrides.required_checks ?? policy.effective.required_checks)
  consumerRows.value = (policy.effective.consumer_allow_list ?? []).map(newConsumerRow)
  consumerBaselinePath.value = policy.effective.consumer_baseline_path ?? ''
  consumerMaxLookups.value = policy.effective.consumer_max_lookups ?? ''
  consumerMaxBytes.value = policy.effective.consumer_max_bytes ?? ''
}

function applyRepoState(repo: Repository, policy: RepositoryPolicy): void {
  repo.has_policy_override = !policy.inherited
  repo.policy_observation = policy.overrides.observation_mode
}

async function openPolicy(repo: Repository): Promise<void> {
  policyRepo.value = repo
  policyOpen.value = true
  policyLoading.value = true
  policyError.value = null
  policyMessage.value = null
  try {
    fillFromPolicy(await api.getRepoPolicy(repo.id))
  } catch (err) {
    policyError.value =
      err instanceof ApiError && err.status === 409
        ? 'El equipo no tiene política: créala en Agentes antes de configurar overrides'
        : 'No se pudo cargar la política del repositorio'
  } finally {
    policyLoading.value = false
  }
}

function closePolicy(): void {
  policyOpen.value = false
  policyRepo.value = null
  repoPolicy.value = null
  policyError.value = null
  policyMessage.value = null
}

function toggleOverride(key: string, event: Event): void {
  fieldEnabled.value[key] = (event.target as HTMLInputElement).checked
  if (fieldEnabled.value[key] && !fieldValues.value[key]) {
    const policy = repoPolicy.value
    fieldValues.value[key] = policy ? effectiveValue(policy.effective, key) : ''
  }
}

function fieldOptions(field: OverrideField): { value: string; label: string }[] {
  if (field.kind === 'boolean') {
    return [
      { value: 'true', label: 'Sí' },
      { value: 'false', label: 'No' },
    ]
  }
  return field.options ?? []
}

function provenanceLabel(key: string): string {
  const source = repoPolicy.value?.provenance[key]
  if (source === 'repo') return 'Propio'
  if (source === 'global') return 'Global'
  return 'Equipo'
}

async function saveRepoPolicy(): Promise<void> {
  const repo = policyRepo.value
  if (!repo) return
  policyError.value = null
  policyMessage.value = null

  const body: RepositoryPolicyInput = {}
  if (observationMode.value) body.observation_mode = true
  for (const field of policyFields) {
    if (!fieldEnabled.value[field.key]) continue
    const value = fieldValues.value[field.key]
    const raw = value === null || value === undefined ? '' : String(value).trim()
    if (field.kind === 'number') {
      if (raw === '' || !Number.isFinite(Number(raw))) {
        policyError.value = `«${field.label}» necesita un número`
        return
      }
      ;(body as Record<string, unknown>)[field.key] = Number(raw)
    } else if (field.kind === 'boolean') {
      ;(body as Record<string, unknown>)[field.key] = raw === 'true'
    } else {
      ;(body as Record<string, unknown>)[field.key] = raw
    }
  }
  if (globsEnabled.value) body.exclude_globs = parseLines(globsRaw.value)
  if (criticalGlobsEnabled.value) body.critical_globs = parseLines(criticalGlobsRaw.value)
  if (checksEnabled.value) body.required_checks = parseChecks(checksRaw.value)

  const consumerValidation = validateConsumers()
  if (consumerValidation) {
    policyError.value = consumerValidation
    return
  }

  // Deliberate scope (#160): consumers are stored on the team policy, so this
  // dialog edits them for the whole team. The repository PUT ignores them.
  // Only the fields the user touched are sent; the API preserves the rest.
  const consumers = consumerChanges()
  const hasConsumers = Object.keys(consumers).length > 0
  const teamId = repo.team_id ?? repoPolicy.value?.team_id ?? null
  if (hasConsumers && teamId === null) {
    policyError.value = 'El repositorio no tiene equipo: no se pueden guardar consumidores'
    return
  }

  policySaving.value = true
  try {
    // Persist the repository overrides first so a failed team PUT can state
    // exactly what did and did not apply.
    const saved =
      Object.keys(body).length === 0
        ? await api.clearRepoPolicy(repo.id)
        : await api.saveRepoPolicy(repo.id, body)
    if (hasConsumers && teamId !== null) {
      try {
        const current = await api.getPolicy(teamId)
        await api.savePolicy(teamId, { ...teamPolicyBody(current), ...consumers })
      } catch (err) {
        applyRepoState(repo, saved)
        policyError.value = `La política del repositorio se guardó, pero los consumidores no: ${consumerErrorMessage(err)}`
        return
      }
      // savePolicy returns the team policy, so re-read the effective one.
      let refreshed: RepositoryPolicy
      try {
        refreshed = await api.getRepoPolicy(repo.id)
      } catch {
        applyRepoState(repo, saved)
        policyMessage.value = 'Cambios guardados; vuelve a abrir para ver la política efectiva'
        return
      }
      fillFromPolicy(refreshed)
      applyRepoState(repo, refreshed)
      policyMessage.value = 'Cambios guardados'
      return
    }
    fillFromPolicy(saved)
    applyRepoState(repo, saved)
    policyMessage.value = saved.inherited
      ? 'Política heredada del equipo'
      : 'Overrides guardados'
  } catch {
    policyError.value = saveErrorMessage()
  } finally {
    policySaving.value = false
  }
}

async function clearOverride(): Promise<void> {
  const repo = policyRepo.value
  if (!repo) return
  policyError.value = null
  policyMessage.value = null
  policySaving.value = true
  try {
    const saved = await api.clearRepoPolicy(repo.id)
    fillFromPolicy(saved)
    applyRepoState(repo, saved)
    policyMessage.value = 'Política heredada del equipo'
  } catch {
    policyError.value = 'No se pudo volver a la herencia'
  } finally {
    policySaving.value = false
  }
}

useLiveRefresh(['repo.updated', 'team.updated'], () => load({ silent: true }))

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="repos">
    <header class="page-header">
      <div>
        <h1 class="page-title">Repositorios</h1>
        <p class="page-subtitle">Revisión automática y equipo asignado por repositorio</p>
      </div>
    </header>

    <div class="split split--narrow">
      <div v-if="canEdit" class="split__aside">
        <section class="accounts register" aria-label="Cuentas conectadas">
          <div class="register__head">
            <h2 class="register__title">Cuentas conectadas</h2>
            <div class="register__actions">
              <button
                type="button"
                class="button button--ghost"
                data-refresh-repos
                :disabled="refreshing"
                @click="refresh"
              >
                {{ refreshing ? 'Actualizando…' : 'Actualizar' }}
              </button>
              <button
                type="button"
                class="button button--primary"
                data-sync-all
                :disabled="syncingAll || syncPending.length > 0 || installations.length === 0"
                @click="syncAll"
              >
                {{ syncingAll ? 'Sincronizando…' : 'Sincronizar todo' }}
              </button>
              <a
                v-if="setupStatus?.install_url"
                class="button button--ghost accounts__add"
                :href="setupStatus.install_url"
                target="_blank"
                rel="noreferrer"
              >
                Añadir cuenta
              </a>
            </div>
          </div>
          <p class="register__hint">
            Sincronizar vuelve a leer las instalaciones y los repositorios desde GitHub. Sincronizar
            todo encola una operación por cada cuenta. El proceso se ejecuta en segundo plano y tarda
            unos segundos.
          </p>
          <div v-if="installations.length > 0" class="table-wrap">
            <table class="register-table accounts__table">
              <thead>
                <tr>
                  <th scope="col">Cuenta</th>
                  <th scope="col">Tipo de cuenta</th>
                  <th scope="col" class="cell--action">Sincronización</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="installation in installations" :key="installation.id">
                  <td class="mono">{{ installation.account_login }}</td>
                  <td>{{ installation.account_type === 'Organization' ? 'Organización' : 'Usuario' }}</td>
                  <td class="cell--action">
                    <button
                      type="button"
                      class="button button--ghost"
                      :data-sync-installation="installation.id"
                      :disabled="isSyncing(installation.id) || syncingAll"
                      @click="syncInstallation(installation.id)"
                    >
                      {{ isSyncing(installation.id) ? 'Sincronizando…' : 'Sincronizar' }}
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="empty-state accounts__empty">Todavía no hay cuentas conectadas</p>
          <p v-if="syncError" class="form-error" role="alert">{{ syncError }}</p>
          <p v-else-if="syncMessage" class="register__message" role="status">{{ syncMessage }}</p>
        </section>
      </div>

      <div class="split__main">
        <section class="register register--repo" aria-label="Repositorios">
          <div class="register__head">
            <div>
              <h2 class="register__title">Registro de repositorios</h2>
              <span v-if="!loading" class="register__meta">Total: {{ repos.length }}</span>
            </div>
            <div class="repo-segment" role="group" aria-label="Revisión automática">
              <button
                v-for="segment in enabledSegments"
                :key="segment.value || 'all'"
                type="button"
                class="repo-segment__button"
                :class="{ 'repo-segment__button--active': enabledSelection === segment.value }"
                :aria-pressed="enabledSelection === segment.value"
                :data-enabled-segment="segment.value || 'all'"
                @click="setEnabledSegment(segment.value)"
              >
                {{ segment.label }}
                <span class="repo-segment__count">{{ segment.count }}</span>
              </button>
            </div>
          </div>
          <p v-if="error" class="empty-state" role="alert">{{ error }}</p>
          <p v-else-if="!loading && repos.length === 0" class="empty-state">
            No hay repositorios disponibles
          </p>
          <template v-else>
            <FilterBar
              :q="q"
              :shown="filteredRepos.length"
              :total="repos.length"
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
                label="Cuenta"
                :options="accountOptions"
                :model-value="facets.account"
                multiple
                @update:model-value="setFacet('account', $event)"
              />
              <FilterDropdown
                label="Equipo"
                :options="teamOptions"
                :model-value="facets.team"
                multiple
                @update:model-value="setFacet('team', $event)"
              />
              <FilterDropdown
                label="Visibilidad"
                :options="visibilityOptions"
                :model-value="facets.visibility"
                multiple
                @update:model-value="setFacet('visibility', $event)"
              />
              <FilterDropdown
                label="Política"
                :options="policyOptions"
                :model-value="facets.policy"
                multiple
                @update:model-value="setFacet('policy', $event)"
              />
            </FilterBar>

            <div class="list-scroll repo-list">
              <EmptyState
                v-if="!loading && filteredRepos.length === 0 && isActive"
                title="NADA COINCIDE CON ESTOS FILTROS"
                action-label="LIMPIAR FILTROS"
                @action="clear"
              />
              <p v-else-if="!loading && filteredRepos.length === 0" class="empty-state">
                No hay repositorios disponibles
              </p>
              <div v-else class="table-wrap">
                <table class="repo-table register-table">
                <thead>
                  <tr>
                    <th scope="col">Repositorio</th>
                    <th scope="col">Cuenta</th>
                    <th scope="col">Visibilidad</th>
                    <th scope="col">Revisión automática</th>
                    <th scope="col">Equipo</th>
                    <th scope="col">Política</th>
                    <th scope="col" class="repo-table__id">ID</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="repo in filteredRepos" :key="repo.id" :data-repo-row="repo.id">
                    <td class="repo-table__name">
                      <span class="repo-table__fullname">{{ repo.full_name }}</span>
                    </td>
                  <td class="mono">{{ repo.account_login }}</td>
                  <td>
                    <span class="badge" :class="repo.private ? 'badge--muted' : 'badge--ok'">
                      {{ repo.private ? 'Privado' : 'Público' }}
                    </span>
                  </td>
                  <td>
                    <label class="switch">
                      <input
                        type="checkbox"
                        role="switch"
                        :checked="repo.enabled"
                        :disabled="!canEdit"
                        :aria-label="`Revisión automática para ${repo.full_name}`"
                        :data-repo-enabled="repo.id"
                        @change="setEnabled(repo, $event)"
                      />
                      <span>{{ repo.enabled ? 'Activada' : 'Desactivada' }}</span>
                    </label>
                    <p
                      v-if="repo.enabled && repo.team_id === null"
                      class="repo-table__warning"
                      :data-repo-warning="repo.id"
                    >
                      Habilitado sin equipo: no se revisará
                    </p>
                    <p
                      v-else-if="repo.enabled && repo.team_id !== null && !repo.has_policy"
                      class="repo-table__warning"
                      :data-repo-policy-warning="repo.id"
                    >
                      El equipo no tiene política: no se revisará
                    </p>
                    <p
                      v-else-if="canEdit && repo.team_id === null"
                      class="repo-table__hint"
                      :data-repo-hint="repo.id"
                    >
                      Sin equipo: activar no iniciará revisiones
                    </p>
                    <p
                      v-else-if="canEdit && repo.team_id !== null && !repo.has_policy"
                      class="repo-table__hint"
                      :data-repo-policy-hint="repo.id"
                    >
                      El equipo no tiene política: activar no iniciará revisiones
                    </p>
                  </td>
                  <td>
                    <select
                      v-if="canEdit"
                      class="field__input repo-table__select"
                      :value="repo.team_id ?? ''"
                      :aria-label="`Equipo de ${repo.full_name}`"
                      :data-repo-team="repo.id"
                      @change="setTeam(repo, $event)"
                    >
                      <option value="">Sin equipo</option>
                      <option v-for="team in teams" :key="team.id" :value="team.id">
                        {{ team.name }}
                      </option>
                    </select>
                    <span v-else :data-repo-team="repo.id">{{ teamName(repo.team_id) }}</span>
                  </td>
                  <td>
                    <div v-if="repo.team_id === null" class="repo-table__policy repo-table__policy--none">
                      —
                    </div>
                    <div v-else class="repo-table__policy">
                      <span
                        class="badge"
                        :class="repo.has_policy_override ? 'badge--ok' : 'badge--muted'"
                        :data-repo-policy-state="repo.id"
                      >
                        {{ repo.has_policy_override ? 'Propia' : 'Heredada' }}
                      </span>
                      <span
                        v-if="repo.policy_observation"
                        class="badge badge--busy"
                        :data-repo-policy-observation="repo.id"
                      >
                        Observación
                      </span>
                      <button
                        v-if="canManageRepo(repo) && repo.has_policy"
                        type="button"
                        class="button button--ghost repo-table__policy-edit"
                        :data-repo-policy="repo.id"
                        @click="openPolicy(repo)"
                      >
                        Editar
                      </button>
                    </div>
                  </td>
                    <td class="repo-table__id">{{ repo.id }}</td>
                  </tr>
                </tbody>
                </table>
              </div>
            </div>
          </template>
        </section>

        <FilterSheet
          :id="sheetId"
          :open="sheetOpen"
          :active-count="activeCount"
          :shown="filteredRepos.length"
          clearable
          :apply-label="`VER ${filteredRepos.length} repos`"
          @close="sheetOpen = false"
          @clear="clear"
        >
          <div class="repos__sheet-filters">
            <FilterDropdown
              label="Cuenta"
              :options="accountOptions"
              :model-value="facets.account"
              multiple
              @update:model-value="setFacet('account', $event)"
            />
            <FilterDropdown
              label="Equipo"
              :options="teamOptions"
              :model-value="facets.team"
              multiple
              @update:model-value="setFacet('team', $event)"
            />
            <FilterDropdown
              label="Visibilidad"
              :options="visibilityOptions"
              :model-value="facets.visibility"
              multiple
              @update:model-value="setFacet('visibility', $event)"
            />
            <FilterDropdown
              label="Política"
              :options="policyOptions"
              :model-value="facets.policy"
              multiple
              @update:model-value="setFacet('policy', $event)"
            />
          </div>
        </FilterSheet>
      </div>
    </div>

    <div v-if="policyOpen" class="policy-backdrop" @keydown.esc="closePolicy">
      <section
        class="policy-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="repo-policy-title"
      >
        <header class="policy-dialog__head">
          <div>
            <h2 id="repo-policy-title" class="policy-dialog__title">
              Política de {{ policyRepo?.full_name }}
            </h2>
            <p class="policy-dialog__hint">
              Los campos desmarcados heredan la política del equipo. El perfil LLM se configura por
              equipo.
            </p>
          </div>
          <button type="button" class="button button--ghost" data-close-policy @click="closePolicy">
            Cerrar
          </button>
        </header>

        <p v-if="policyLoading" class="empty-state">Cargando política…</p>
        <p v-else-if="!repoPolicy && policyError" class="form-error" role="alert">
          {{ policyError }}
        </p>
        <template v-else-if="repoPolicy">
          <p class="policy-dialog__meta">
            Revisión
            <span class="mono" data-repo-policy-revision>{{ repoPolicy.revision }}</span>
            · {{ repoPolicy.inherited ? 'Sin overrides' : 'Con overrides' }}
          </p>

          <label class="policy-toggle">
            <input v-model="observationMode" type="checkbox" data-repo-policy-observation-mode />
            <span>
              <span class="field__label">Modo observación</span>
              <span class="field__hint">
                Calcula, expone y registra los overrides sin cambiar el comportamiento de la
                revisión; se aplica la política del equipo.
              </span>
            </span>
          </label>

          <div class="policy-dialog__fields">
            <label v-for="field in policyFields" :key="field.key" class="field policy-field">
              <span class="policy-field__head">
                <input
                  type="checkbox"
                  :checked="fieldEnabled[field.key]"
                  :data-policy-override="field.key"
                  @change="toggleOverride(field.key, $event)"
                />
                <span class="field__label">{{ field.label }}</span>
                <span class="badge badge--muted" :data-policy-source="field.key">
                  {{ provenanceLabel(field.key) }}
                </span>
              </span>
              <select
                v-if="field.kind === 'select' || field.kind === 'boolean'"
                v-model="fieldValues[field.key]"
                class="field__input"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
              >
                <option
                  v-for="option in fieldOptions(field)"
                  :key="option.value"
                  :value="option.value"
                >
                  {{ option.label }}
                </option>
              </select>
              <textarea
                v-else-if="field.kind === 'textarea'"
                v-model="fieldValues[field.key]"
                class="field__input"
                rows="3"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
                placeholder="Ej.: no comentes estilo; sigue las convenciones del repo"
              ></textarea>
              <input
                v-else
                v-model="fieldValues[field.key]"
                class="field__input"
                type="number"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
              />
            </label>
          </div>

          <label class="field policy-field">
            <span class="policy-field__head">
              <input v-model="globsEnabled" type="checkbox" data-policy-override="exclude_globs" />
              <span class="field__label">Archivos excluidos</span>
              <span class="badge badge--muted" :data-policy-source="'exclude_globs'">
                {{ provenanceLabel('exclude_globs') }}
              </span>
            </span>
            <textarea
              v-model="globsRaw"
              class="field__input"
              rows="3"
              :disabled="!globsEnabled"
              data-policy-field="exclude_globs"
              placeholder="vendor/**&#10;**/*.pb.go"
            ></textarea>
            <span class="field__hint">Un patrón por línea.</span>
          </label>

          <label class="field policy-field">
            <span class="policy-field__head">
              <input
                v-model="criticalGlobsEnabled"
                type="checkbox"
                data-policy-override="critical_globs"
              />
              <span class="field__label">Zonas críticas</span>
              <span class="badge badge--muted" :data-policy-source="'critical_globs'">
                {{ provenanceLabel('critical_globs') }}
              </span>
            </span>
            <textarea
              v-model="criticalGlobsRaw"
              class="field__input"
              rows="3"
              :disabled="!criticalGlobsEnabled"
              data-policy-field="critical_globs"
              placeholder="internal/payments/**&#10;migrations/**"
            ></textarea>
            <span class="field__hint">
              El plan revisa estos cambios primero; si quedan sin verificar, la revisión no aprueba.
            </span>
          </label>

          <label class="field policy-field">
            <span class="policy-field__head">
              <input v-model="checksEnabled" type="checkbox" data-policy-override="required_checks" />
              <span class="field__label">Checks requeridos</span>
              <span class="badge badge--muted" :data-policy-source="'required_checks'">
                {{ provenanceLabel('required_checks') }}
              </span>
            </span>
            <textarea
              v-model="checksRaw"
              class="field__input"
              rows="3"
              :disabled="!checksEnabled"
              data-policy-field="required_checks"
              placeholder="build | app:12345&#10;ci/test | status"
            ></textarea>
            <span class="field__hint">
              Uno por línea: <code>nombre</code> o <code>nombre | origen</code>. Vacío exige todos
              los checks reportados.
            </span>
          </label>

          <section class="policy-consumers" data-policy-consumers>
            <header class="policy-consumers__head">
              <h3 class="policy-consumers__title">Consumidores autorizados</h3>
              <p class="field__hint">
                Se guardan en la política del equipo y se aplican a todos sus repositorios. Solo se
                leen las refs fijadas aquí o en la línea base.
              </p>
            </header>

            <div
              v-for="(consumer, index) in consumerRows"
              :key="consumer.key"
              class="consumer-row"
              :data-consumer-row="index"
            >
              <label class="field consumer-row__field">
                <span class="field__label">Repositorio</span>
                <input
                  v-model="consumer.repository"
                  class="field__input"
                  type="text"
                  placeholder="owner/repo"
                  :data-consumer-repository="index"
                />
              </label>
              <label class="field consumer-row__field">
                <span class="field__label">Ref</span>
                <input
                  v-model="consumer.ref"
                  class="field__input"
                  type="text"
                  placeholder="v1.2.0 o SHA"
                  :data-consumer-ref="index"
                />
              </label>
              <label class="field consumer-row__field">
                <span class="field__label">Contrato</span>
                <input
                  v-model="consumer.contract"
                  class="field__input"
                  type="text"
                  placeholder="api/openapi.yaml"
                  :data-consumer-contract="index"
                />
              </label>
              <label class="field consumer-row__field">
                <span class="field__label">Ruta</span>
                <input
                  v-model="consumer.path"
                  class="field__input"
                  type="text"
                  placeholder="Igual que el contrato"
                  :data-consumer-path="index"
                />
              </label>
              <button
                type="button"
                class="button button--ghost consumer-row__remove"
                :data-consumer-remove="index"
                :aria-label="`Quitar consumidor ${consumerName(consumer)}`"
                @click="removeConsumer(index)"
              >
                Quitar
              </button>
            </div>

            <button type="button" class="button button--ghost" data-consumer-add @click="addConsumer">
              Añadir consumidor
            </button>

            <div class="policy-consumers__limits">
              <label class="field">
                <span class="field__label">Línea base</span>
                <input
                  v-model="consumerBaselinePath"
                  class="field__input"
                  type="text"
                  placeholder=".qa/consumers.json"
                  data-consumer-baseline
                />
                <span class="field__hint">
                  Archivo del árbol base con consumidores adicionales; un PR no puede cambiarlo.
                </span>
              </label>
              <label class="field">
                <span class="field__label">Máx. consultas</span>
                <input
                  v-model.number="consumerMaxLookups"
                  class="field__input"
                  type="number"
                  min="1"
                  max="50"
                  data-consumer-max-lookups
                />
                <span class="field__hint">Entre 1 y 50 lecturas por revisión.</span>
              </label>
              <label class="field">
                <span class="field__label">Máx. bytes</span>
                <input
                  v-model.number="consumerMaxBytes"
                  class="field__input"
                  type="number"
                  min="4096"
                  max="16777216"
                  data-consumer-max-bytes
                />
                <span class="field__hint">Entre 4 KiB y 16 MiB de contratos leídos.</span>
              </label>
            </div>
          </section>

          <p v-if="policyError" class="form-error" role="alert">{{ policyError }}</p>
          <p v-else-if="policyMessage" class="policy-message" role="status">{{ policyMessage }}</p>

          <footer class="policy-dialog__actions">
            <span class="field__hint policy-dialog__scope">
              Los consumidores se guardan para todo el equipo.
            </span>
            <button
              type="button"
              class="button button--ghost"
              data-repo-policy-clear
              :disabled="policySaving"
              @click="clearOverride"
            >
              Volver a herencia
            </button>
            <button
              type="button"
              class="button button--primary"
              data-repo-policy-save
              :disabled="policySaving"
              @click="saveRepoPolicy"
            >
              Guardar cambios
            </button>
          </footer>
        </template>
      </section>
    </div>
  </section>
</template>

<style scoped>
.repos {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
  gap: var(--space-5);
}

.register {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.register--repo {
  flex: 1;
  min-height: 0;
}

.register__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--space-3);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.register__title {
  font-size: 17px;
}

.register__meta {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}

.register__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.register .button {
  min-height: 40px;
}

.accounts__add {
  text-decoration: none;
}

.register__hint {
  max-width: 72ch;
  color: var(--text-muted);
  font-size: 13.5px;
}

.register__message {
  color: var(--ok);
  font-size: 13.5px;
}

.accounts__empty {
  color: var(--text-muted);
}

.register-table {
  font-size: 14.5px;
}

.register-table th {
  padding: var(--space-3);
  color: var(--text-muted);
  font-size: 13px;
  font-weight: 500;
}

.register-table td {
  padding: var(--space-3);
}

.register-table th + th,
.register-table td + td {
  border-left: var(--rule-hair) solid var(--border);
}

.cell--action {
  text-align: right;
  white-space: nowrap;
}

.mono,
.repo-table__name {
  font-family: var(--font-mono);
}

.register-table .repo-table__id {
  width: 1%;
  padding-right: var(--space-2);
  padding-left: var(--space-2);
  border-left: var(--rule-hair) solid var(--border);
  color: #6b6b6b;
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  text-align: right;
  white-space: nowrap;
}

.repos__sheet-filters {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.repo-segment {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--space-1);
}

.repo-segment__button {
  appearance: none;
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  min-height: 32px;
  padding: 0 var(--space-3);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
  background: transparent;
  color: var(--text-muted);
  font: inherit;
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
  transition:
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease;
}

.repo-segment__button:hover {
  border-color: var(--border-strong);
  color: var(--text);
}

.repo-segment__button--active {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent);
}

.repo-segment__count {
  font-family: var(--font-display);
  font-size: 14px;
  font-weight: 900;
  font-variant-numeric: tabular-nums;
}

.repo-table__fullname {
  white-space: nowrap;
}

.repo-table__select {
  min-height: 40px;
  max-width: 100%;
  padding: var(--space-1) var(--space-2);
  font-size: 14px;
}

.repo-table__select:hover {
  border-color: var(--text);
  background: var(--surface-3);
}

.repo-table__select:active {
  background: var(--surface-3);
}

.repo-table__select:focus-visible,
.switch input:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--space-1);
}

.switch {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 40px;
  cursor: pointer;
}

.switch input {
  width: 18px;
  height: 18px;
  accent-color: var(--accent);
  cursor: pointer;
}

.switch:hover span {
  text-decoration: underline;
  text-underline-offset: 3px;
}

.switch:active span {
  color: var(--text-muted);
}

.switch input:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}

.repo-table__warning {
  margin-top: var(--space-1);
  color: var(--bad);
  font-size: 13px;
}

.repo-table__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 13px;
}

@media (pointer: coarse) {
  .register .button,
  .switch,
  .repo-table__select {
    min-height: 44px;
  }
}

.repo-table__policy {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-1) var(--space-2);
}

.repo-table__policy--none {
  color: var(--text-faint);
}

.repo-table__policy-edit {
  min-height: 32px;
  padding: 0 var(--space-2);
  font-size: 13px;
}

.policy-backdrop {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: var(--space-5);
  overflow: auto;
  background: rgb(0 0 0 / 55%);
}

.policy-dialog {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  width: min(780px, 100%);
  max-height: calc(100vh - 2 * var(--space-5));
  padding: var(--space-4);
  overflow: auto;
  background: var(--surface);
  border: var(--rule-heavy) solid var(--text);
}

.policy-dialog__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.policy-dialog__title {
  font-size: 17px;
}

.policy-dialog__hint,
.policy-dialog__meta {
  color: var(--text-muted);
  font-size: 13.5px;
}

.policy-dialog__fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: var(--space-3);
}

.policy-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.policy-field__head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.policy-toggle {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
}

.policy-toggle .field__hint {
  display: block;
}

.policy-message {
  color: var(--ok);
  font-size: 13.5px;
}

.policy-consumers {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding-top: var(--space-3);
  border-top: var(--rule-hair) solid var(--border);
}

.policy-consumers__head {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.policy-consumers__title {
  font-size: 15px;
}

.consumer-row {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr)) auto;
  gap: var(--space-2);
  align-items: end;
}

.consumer-row__remove {
  min-height: 40px;
}

.policy-consumers__limits {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: var(--space-3);
}

@media (max-width: 900px) {
  .consumer-row {
    grid-template-columns: 1fr;
  }
}

.policy-dialog__actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.policy-dialog__scope {
  margin-right: auto;
  align-self: center;
}

@media (max-width: 700px) {
  .policy-dialog__fields {
    grid-template-columns: 1fr;
  }
}

/* The shell gives this view the leftover height. Below the split breakpoint
   the whole registry scrolls; at wide widths only the list does. */
@media (max-width: 2199.98px) {
  .repos {
    overflow-y: auto;
  }
}

@media (min-width: 2200px) {
  .split,
  .split__main,
  .register--repo {
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

  .split__main,
  .register--repo {
    flex: 1;
  }
}
</style>
