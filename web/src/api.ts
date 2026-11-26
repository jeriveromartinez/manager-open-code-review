export type QAStatus =
  | 'draft'
  | 'awaiting_approval'
  | 'pending'
  | 'reviewing'
  | 'changes_requested'
  | 'waiting_ci'
  | 'ready_for_human'
  | 'inconclusive'
  | 'paused'
  | 'merged'
  | 'closed'

export type PRGroup = '' | 'in_review' | 'blocked' | 'ready'

export type PRView = {
  id: number
  repo: string
  number: number
  title: string
  author_login: string
  html_url: string
  qa_status: QAStatus
  group: PRGroup
  review_state: 'running' | 'scheduled' | 'blocked' | ''
  review_reason: string
  validations: { passed: number; total: number }
  open_findings: SeverityCounts
  /** Team that owns the repository; null when the repository has none. */
  team_id: number | null
  /** When the PR entered its current `qa_status` (RFC 3339). */
  status_since: string
  updated_at: string
  /** Latest human escalation request, when the engine could not conclude. */
  review_request?: ReviewRequest | null
}

export type ReviewRequestStatus = 'pending' | 'sending' | 'delivered' | 'rejected' | 'escalated'

/** Human review request of one PR revision, as GitHub reported it. */
export type ReviewRequest = {
  status: ReviewRequestStatus
  head_sha: string
  cause: string
  reviewer_login: string
  reviewer_team: string
  attempts: number
  last_error: string
  delivered_at: string | null
}

export type SeverityCounts = {
  blocker: number
  major: number
  minor: number
  nit: number
}

/** QA queue overview over the PRs the user can see. */
export type QASummary = {
  ready_for_human: number
  inconclusive: number
  runs_running: number
  runs_queued: number
  untriaged: SeverityCounts
  confirmed: number
  false_positive: number
  precision_window_days: number
  categories: { category: string; count: number }[]
  confirmed_by_repo: { repo: string; count: number }[]
  category_window_days: number
  /** Consumo de tokens de las ejecuciones terminadas en la ventana. */
  tokens: RunTokenStats
  validation_sla_seconds: number
}

/**
 * `reviews` cuenta solo las ejecuciones con datos: las anteriores al registro
 * de consumo (#73) no rebajan el promedio.
 */
export type RunTokenStats = {
  window_days: number
  reviews: number
  input: number
  output: number
  cache_read: number
  cache_write: number
  total: number
  average: number
  max_context: number
  /** False when no run in the window reported a max-context sample. */
  max_context_known: boolean
}

/**
 * Archivo dentro de la cobertura de una ejecución. `status` se deja como string
 * para que un estado nuevo del backend se muestre tal cual en vez de romper.
 */
export type CoverageFile = {
  path: string
  status: string
  /** Pasada que cubrió el archivo: ocr, chunked o text. */
  method?: string
  /** Trozos en los que se partió un archivo revisado por partes. */
  chunks?: number
  /** Motivo por el que un archivo no se revisó o quedó excluido. */
  reason?: string
  /** El archivo no tiene análisis automático comprobable y requiere criterio humano. */
  human?: boolean
}

/**
 * Cobertura de una ejecución. `files` falta en las ejecuciones anteriores a
 * #80, así que la lista por archivo es opcional.
 */
export type ReviewCoverage = Record<string, unknown> & {
  files?: CoverageFile[]
}

/** Ejecución de revisión. CloneTokenEnc nunca se expone en la API. */
export type ReviewRun = {
  id: number
  profile_id: number
  head_sha: string
  base_sha: string
  trigger: string
  status: string
  attempt: number
  summary: string
  coverage: ReviewCoverage
  usage: Record<string, unknown>
  error_code: string
  error_message: string
  /** null en ejecuciones anteriores al registro y en runs cortados antes de que OCR informe consumo (#73, #129). */
  input_tokens: number | null
  output_tokens: number | null
  /** Parte oculta de output_tokens; baja al elegir una variante de bajo esfuerzo (#65). */
  reasoning_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
  /** null si el run se cortó antes de que OCR informara su consumo (#129). */
  total_tokens: number | null
  /** Mayor prompt de la ejecución; vigila el techo de contexto (#72). */
  max_context_tokens: number | null
  /** Fase actual de un run en curso; vacía en cola, terminado o anterior a #123. */
  phase: string
  phase_started_at: string | null
  /** Revisión de política efectiva bajo la que se ejecutó (#153); vacía en runs antiguos. */
  policy_revision: string
  /** El run solo observó los overrides del repositorio; aplicó la política del equipo. */
  policy_observation: boolean
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}

/** Una revisión encolada o en curso en la vista global (#123). */
export type ActiveRun = {
  id: number
  pull_request_id: number
  pr_number: number
  pr_title: string
  repo: string
  team: string
  profile: string
  trigger: string
  status: string
  attempt: number
  phase: string
  phase_started_at: string | null
  started_at: string | null
  created_at: string
}

/** Una revisión terminada del historial; conserva la forma del run activo. */
export type RunHistoryRow = ActiveRun & {
  error_code: string
  error_message: string
  finished_at: string | null
  /** null cuando la ejecución no registró inicio o fin. */
  duration_seconds: number | null
}

export type RunHistoryParams = {
  status?: string[]
  repo?: string
  trigger?: string
  q?: string
  before?: number
  /** Timestamp RFC 3339 nano del cursor; viaja tal cual lo devolvió el servidor. */
  beforeCreatedAt?: string
  limit?: number
}

export type RunHistoryPage = {
  runs: RunHistoryRow[]
  next_before: number
  next_created_at: string | null
  has_more: boolean
}

/** Una línea del log de una ejecución; el panel la muestra en vivo (#124). */
export type ReviewRunLogEntry = {
  seq: number
  ts: string
  source: string
  invocation: string
  level: string
  phase: string
  line: string
}

/** Una página del log. `next_after` es el `seq` que se pide en el siguiente sondeo. */
export type ReviewRunLogPage = {
  run_id: number
  entries: ReviewRunLogEntry[]
  next_after: number
  has_more: boolean
}

/** Respuesta de la segunda pasada de OCR; `{}` cuando no verificó el hallazgo. */
export type FindingVerification = {
  verdict?: 'confirm' | 'refute' | 'uncertain'
  evidence?: string
  head_sha?: string
  model?: string
  session_id?: string
  verified_at?: string
  /** Why the verdict could not be applied; empty when it validated. */
  reason?: string
}

export type FindingCitation = {
  path: string
  start_line: number
  end_line: number
  /** RIGHT cita el head; LEFT cita el código eliminado de la base. */
  side?: 'RIGHT' | 'LEFT'
  existing_code?: string
}

export type Finding = {
  id: string
  fingerprint: string
  path: string
  start_line: number
  end_line: number
  severity: string
  category: string
  title: string
  body: string
  suggestion: string
  confidence: string
  status: string
  first_seen_run_id: number
  last_seen_run_id: number
  /** Alcance del hallazgo: un archivo, varios o todo el PR. */
  scope?: 'file' | 'files' | 'pr'
  /** Citas verificadas que anclan el hallazgo a la comparación vigente. */
  citations?: FindingCitation[]
  /** Evidencia del análisis arquitectónico: no bloquea el check ni se publica inline. */
  advisory?: boolean
  /** QA verdict from the panel; a review run or /qa moving the status leaves it unchanged. */
  triage_verdict: '' | 'confirmed' | 'false_positive'
  triaged_by: string
  triaged_at: string | null
  /** Quién produjo el veredicto: una persona desde el panel o el verificador OCR. */
  triage_source: '' | 'human' | 'verifier'
  verification: FindingVerification
  created_at: string
  updated_at: string
}

/**
 * El detalle reparte los campos del PR en la raíz de la respuesta (el DTO de
 * Go está incrustado), junto a `runs`, `findings` y el resultado humano.
 */
export type PRDetail = PRView & {
  runs: ReviewRun[]
  findings: Finding[]
  /** Decisión humana de una revisión inconclusa; null si nadie la registró. */
  human_verdict: HumanVerdict | null
}

/** Un rango de archivo que una persona revisó. */
export type HumanVerdictFile = {
  path: string
  start_line: number
  end_line: number
}

/**
 * Alcance de la decisión humana: qué archivos/rangos y hallazgos pendientes se
 * revisaron, y si se cubrió todo lo que el motor no pudo cubrir.
 */
export type HumanVerdictScope = {
  files: HumanVerdictFile[]
  pending_findings: string[]
  complete: boolean
}

/**
 * Resultado humano de una revisión inconclusa. Se muestra aparte del run
 * automático; `current` es false cuando un push o retarget lo invalidó.
 */
export type HumanVerdict = {
  decision: 'approved' | 'changes_requested'
  conclusion: string
  reason: string
  actor_login: string
  scope: HumanVerdictScope
  head_sha: string
  base_ref: string
  merge_base_sha: string
  current: boolean
  invalidated_at: string | null
  invalidated_reason: string
  created_at: string
}

export type HumanVerdictInput = {
  decision: 'approved' | 'changes_requested'
  conclusion: string
  reason?: string
  scope: {
    files: HumanVerdictFile[]
    pending_findings?: string[]
    complete: boolean
  }
}

/** Un eslabón de configuración que falta para que se ejecute una revisión. */
export type ReadinessItem = {
  key: string
  label: string
  count: number
  link: string
}

export type Readiness = {
  ready: boolean
  items: ReadinessItem[]
}

export type TeamRole = 'lead' | 'qa' | 'developer'

export type Me = {
  id: number
  login: string
  avatar_url: string
  is_admin: boolean
  teams: { id: number; name: string; role: TeamRole }[]
}

export type Repository = {
  id: number
  installation_id: number
  full_name: string
  private: boolean
  default_branch: string
  enabled: boolean
  team_id: number | null
  has_policy: boolean
  /** El repositorio tiene overrides propios sobre la política del equipo. */
  has_policy_override: boolean
  /** Los overrides se resuelven y registran, pero no se aplican (#153). */
  policy_observation: boolean
  account_login: string
}

export type Installation = {
  id: number
  account_login: string
  account_type: string
  suspended_at: string | null
}

export type TeamMember = {
  user_id: number
  login: string
  avatar_url: string
  role: TeamRole
}

export type Team = {
  id: number
  name: string
  slug: string
  repository_count: number
  members: TeamMember[]
}

/** Response of PATCH /api/teams/{id}: no members. */
export interface TeamSummary {
  id: number
  name: string
  slug: string
  repository_count: number
}

export type User = {
  id: number
  login: string
  avatar_url: string
  is_admin: boolean
}

export type SetupStatus = {
  configured: boolean
  install_url: string
  token_required: boolean
}

export type SetupManifest = {
  action_url: string
  manifest: Record<string, unknown>
}

export type SetupManifestRequest = {
  name: string
  setup_token?: string
}

export type RepoPatch = {
  enabled?: boolean
  team_id?: number | null
}

export type SettingsKind = 'duration' | 'int' | 'bool' | 'url' | 'text' | 'path' | 'secret'

export interface SettingsField {
  key: string
  group: 'general' | 'review' | 'worker' | 'alerts' | 'backups'
  label: string
  help: string
  kind: SettingsKind
  unit: '' | 's' | 'm' | 'h'
  default: string
  restart: '' | 'app' | 'worker' | 'monitor'
  secret: boolean
}

/** `values` es la configuración efectiva, así que las duraciones llegan en formato de Go. */
export interface SettingsResponse {
  values: Record<string, string>
  fields: SettingsField[]
}

/**
 * Perfil LLM. `api_key_enc` nunca viaja al navegador: el backend expone solo
 * `api_key_set`, y `api_key` es un campo de escritura en los formularios.
 */
export type LLMProfile = {
  id: number
  name: string
  provider: string
  model: string
  variant: string | null
  mode: string
  base_url: string | null
  concurrency: number
  enabled: boolean
  api_key_set: boolean
  health_status: string
  health_checked_at: string | null
  health_error: string
  health_age_seconds: number
  health_stale: boolean
  created_at: string
  updated_at: string
}

export type LLMProfileInput = {
  name?: string
  provider: string
  model: string
  variant?: string | null
  mode?: string
  base_url?: string | null
  concurrency?: number
  enabled?: boolean
  api_key: string
}

export type LLMProfilePatch = Partial<LLMProfileInput>

/**
 * Consumo de un perfil LLM en la ventana de tokens. Los campos de tokens son
 * `null` cuando el perfil no registró uso: «sin datos» no es un cero medido.
 */
export type LLMProfileTokenStats = {
  profile_id: number
  name: string
  provider: string
  model: string
  has_data: boolean
  runs: number
  checks: number
  input_tokens: number | null
  output_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
  total_tokens: number | null
  average_tokens: number | null
}

/** Consumo por perfil de ejecuciones y chequeos de salud (solo admins). */
export type LLMProfileUsage = {
  window_days: number
  has_data: boolean
  profiles: LLMProfileTokenStats[]
}

export type LLMModelOption = { id: string; name: string }

export type LLMModelsResponse = { models: LLMModelOption[] }

export type ReviewEffort = 'low' | 'medium' | 'high'

export type AutoTriageMode = 'off' | 'dry_run' | 'apply'

export type ArchitectureAnalysisMode = 'off' | 'observation'

export type ArchitectureExceptionPolicy = 'deny' | 'baseline'

/** One check the gate needs. origin is app:<id>, status, or omitted for any. */
export type RequiredCheck = {
  name: string
  origin?: string
}

/** Consumidor autorizado de un contrato publicado (#160). */
export type ConsumerRef = {
  repository: string
  contract: string
  /** Archivo del consumidor que fija el contrato; vacío usa `contract`. */
  path?: string
  /** Tag o SHA completo; una rama se resuelve y se registra el SHA observado. */
  ref: string
  installation_id?: number
  essential?: boolean
  /** `private` por defecto: solo se publica por nombre si el equipo lo autoriza. */
  visibility?: 'public' | 'private'
  version?: string
}

export type QAPolicy = {
  id: number
  team_id: number
  llm_profile_id: number
  blocking_threshold: string
  extra_instructions: string
  exclude_globs: string[]
  max_files: number
  max_diff_lines: number
  max_passes: number
  require_green_ci: boolean
  /** Empty requires every reported check to be green. */
  required_checks: RequiredCheck[]
  /** Null inherits the global review_context_ceiling. */
  review_context_ceiling: number | null
  /** Read-only: the stored global review_context_ceiling setting. */
  global_context_ceiling: number
  /** Read-only: the policy override, or the global setting when null. */
  effective_context_ceiling: number
  review_effort: ReviewEffort
  /** 0 disables the optional total-spend cap. */
  review_token_budget: number
  /** Second OCR pass: off, dry_run (records only) or apply. */
  auto_triage_mode: AutoTriageMode
  auto_withdraw: boolean
  verify_max_findings: number
  verify_max_files: number
  verify_token_share: number
  verify_time_share: number
  verify_token_max: number
  /** Review external (cross-repository) contributions when the author is trusted. */
  allow_external_reviews: boolean
  external_trusted_associations: string[]
  max_external_diff_lines: number
  max_external_runs_per_day: number
  /** Daily external token budget; 0 disables it. */
  external_daily_token_budget: number
  /** Architecture analysis shares its own budget with the review (#157). */
  architecture_analysis_mode: ArchitectureAnalysisMode
  architecture_token_share: number
  architecture_time_share: number
  architecture_token_max: number
  /** Trusted rule file resolved from the base commit, not the pull request. */
  architecture_baseline_path: string
  architecture_exception_policy: ArchitectureExceptionPolicy
  /** Autoriza la lectura de otros repositorios durante la revisión (#160). */
  consumer_allow_list: ConsumerRef[]
  /** Archivo del árbol base que declara consumidores adicionales. */
  consumer_baseline_path: string
  /** Consultas autorizadas por revisión; el backend limita el rango. */
  consumer_max_lookups: number
  /** Bytes máximos leídos de los consumidores; el backend limita el rango. */
  consumer_max_bytes: number
  /** Read-only: whether the profile accepts an explicit prompt cache key. */
  prompt_cache: boolean
  /** Identifica el contenido de la política; se registra en cada revisión (#153). */
  revision: string
  updated_at: string
}

export type QAPolicyInput = Pick<
  QAPolicy,
  | 'llm_profile_id'
  | 'blocking_threshold'
  | 'extra_instructions'
  | 'exclude_globs'
  | 'max_files'
  | 'max_diff_lines'
  | 'max_passes'
  | 'require_green_ci'
  | 'required_checks'
  | 'review_context_ceiling'
  | 'review_effort'
  | 'review_token_budget'
  | 'auto_triage_mode'
  | 'auto_withdraw'
  | 'verify_max_findings'
  | 'verify_max_files'
  | 'verify_token_share'
  | 'verify_time_share'
  | 'verify_token_max'
  | 'allow_external_reviews'
  | 'external_trusted_associations'
  | 'max_external_diff_lines'
  | 'max_external_runs_per_day'
  | 'external_daily_token_budget'
  | 'architecture_analysis_mode'
  | 'architecture_token_share'
  | 'architecture_time_share'
  | 'architecture_token_max'
  | 'architecture_baseline_path'
  | 'architecture_exception_policy'
> & {
  // Consumer settings are omitted unless the panel edited them, so the API
  // keeps the stored values on an unrelated policy save.
  consumer_allow_list?: ConsumerRef[]
  consumer_baseline_path?: string
  consumer_max_lookups?: number
  consumer_max_bytes?: number
}

/**
 * Overrides de revisión de un repositorio (#153). Solo se envían los campos
 * que se quieren sobrescribir; `null` o ausente hereda el valor del equipo.
 */
export type RepositoryPolicyInput = {
  observation_mode?: boolean
  blocking_threshold?: string | null
  extra_instructions?: string | null
  exclude_globs?: string[] | null
  max_files?: number | null
  max_diff_lines?: number | null
  max_passes?: number | null
  require_green_ci?: boolean | null
  required_checks?: RequiredCheck[] | null
  review_context_ceiling?: number | null
  review_effort?: ReviewEffort | null
  review_token_budget?: number | null
  auto_triage_mode?: AutoTriageMode | null
  auto_withdraw?: boolean | null
  verify_max_findings?: number | null
  verify_max_files?: number | null
  verify_token_share?: number | null
  verify_time_share?: number | null
  verify_token_max?: number | null
}

/** Overrides almacenados: `null` significa que el campo se hereda. */
export type RepositoryPolicyOverrides = {
  observation_mode: boolean
  blocking_threshold: string | null
  extra_instructions: string | null
  exclude_globs: string[] | null
  max_files: number | null
  max_diff_lines: number | null
  max_passes: number | null
  require_green_ci: boolean | null
  required_checks: RequiredCheck[] | null
  review_context_ceiling: number | null
  review_effort: ReviewEffort | null
  review_token_budget: number | null
  auto_triage_mode: AutoTriageMode | null
  auto_withdraw: boolean | null
  verify_max_findings: number | null
  verify_max_files: number | null
  verify_token_share: number | null
  verify_time_share: number | null
  verify_token_max: number | null
}

export type PolicySource = 'team' | 'repo' | 'global'

/** Política efectiva de un repositorio con la procedencia de cada campo. */
export type RepositoryPolicy = {
  repository_id: number
  team_id: number | null
  inherited: boolean
  revision: string
  provenance: Record<string, PolicySource>
  overrides: RepositoryPolicyOverrides
  effective: QAPolicy
  enforced: QAPolicy
  updated_at: string | null
}

export type ReviewEvent = {
  type: string
  id: string
}

export class ApiError extends Error {
  status: number
  /** JSON body of the error when the server sent one. */
  payload?: unknown

  constructor(status: number, message: string, payload?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

let onUnauthorized: (() => void) | null = null

/** Registra el manejador global de 401 (el router redirige a /login). */
export function setUnauthorizedHandler(handler: () => void): void {
  onUnauthorized = handler
}

type RequestOptions = {
  method?: string
  body?: unknown
}

async function request<T>(path: string, init: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (init.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }

  const res = await fetch(path, {
    method: init.method ?? 'GET',
    credentials: 'same-origin',
    headers,
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  })

  if (res.status === 401) {
    onUnauthorized?.()
    throw new ApiError(401, 'unauthorized')
  }

  if (!res.ok) {
    const detail = await res.text().catch(() => '')
    let payload: unknown
    if (detail) {
      try {
        payload = JSON.parse(detail)
      } catch {
        payload = undefined
      }
    }
    throw new ApiError(res.status, detail || res.statusText || `HTTP ${res.status}`, payload)
  }

  if (res.status === 204) {
    return undefined as T
  }

  return (await res.json()) as T
}

export const api = {
  me: () => request<Me>('/api/me'),
  qaSummary: (teamId?: number) =>
    request<QASummary>(teamId === undefined ? '/api/qa/summary' : `/api/qa/summary?team_id=${teamId}`),
  readiness: () => request<Readiness>('/api/readiness'),
  prs: () => request<PRView[]>('/api/prs'),
  prDetail: (id: number) => request<PRDetail>(`/api/prs/${id}`),
  activeRuns: () => request<{ runs: ActiveRun[] }>('/api/review-runs'),
  runHistory: (params: RunHistoryParams = {}) => {
    const query = new URLSearchParams()
    for (const status of params.status ?? []) query.append('status', status)
    if (params.repo) query.set('repo', params.repo)
    if (params.trigger) query.set('trigger', params.trigger)
    if (params.q) query.set('q', params.q)
    if (params.before !== undefined) query.set('before', String(params.before))
    if (params.beforeCreatedAt) query.set('before_created_at', params.beforeCreatedAt)
    if (params.limit !== undefined) query.set('limit', String(params.limit))
    const search = query.toString()
    return request<RunHistoryPage>(
      search === '' ? '/api/review-runs/history' : `/api/review-runs/history?${search}`,
    )
  },
  reviewRunLogs: (id: number, after = 0, limit = 500) =>
    request<ReviewRunLogPage>(`/api/review-runs/${id}/logs?after=${after}&limit=${limit}`),
  retryPR: (id: number) =>
    request<{ status: string }>(`/api/prs/${id}/retry`, { method: 'POST' }),
  recordHumanVerdict: (id: number, body: HumanVerdictInput) =>
    request<HumanVerdict>(`/api/prs/${id}/human-verdict`, { method: 'POST', body }),
  withdrawFinding: (prId: number, findingId: string, reason?: string) =>
    request<Finding>(`/api/prs/${prId}/findings/${encodeURIComponent(findingId)}/withdraw`, {
      method: 'POST',
      body: reason ? { reason } : undefined,
    }),
  confirmFinding: (prId: number, findingId: string) =>
    request<Finding>(`/api/prs/${prId}/findings/${encodeURIComponent(findingId)}/confirm`, {
      method: 'POST',
    }),
  llmProfiles: () => request<LLMProfile[]>('/api/llm-profiles'),
  llmProfileUsage: () => request<LLMProfileUsage>('/api/llm-profiles/usage'),
  createLLMProfile: (body: LLMProfileInput) =>
    request<LLMProfile>('/api/llm-profiles', { method: 'POST', body }),
  updateLLMProfile: (id: number, patch: LLMProfilePatch) =>
    request<LLMProfile>(`/api/llm-profiles/${id}`, { method: 'PATCH', body: patch }),
  checkLLMHealth: (id: number) =>
    request<{ status: string }>(`/api/llm-profiles/${id}/health`, { method: 'POST' }),
  llmModels: (provider: string, api_key: string) =>
    request<LLMModelsResponse>('/api/llm-profiles/models', {
      method: 'POST',
      body: { provider, api_key },
    }),
  getPolicy: (teamId: number) => request<QAPolicy>(`/api/teams/${teamId}/policy`),
  savePolicy: (teamId: number, body: QAPolicyInput) =>
    request<QAPolicy>(`/api/teams/${teamId}/policy`, { method: 'PUT', body }),
  getRepoPolicy: (repoId: number) =>
    request<RepositoryPolicy>(`/api/repos/${repoId}/policy`),
  saveRepoPolicy: (repoId: number, body: RepositoryPolicyInput) =>
    request<RepositoryPolicy>(`/api/repos/${repoId}/policy`, { method: 'PUT', body }),
  clearRepoPolicy: (repoId: number) =>
    request<RepositoryPolicy>(`/api/repos/${repoId}/policy`, { method: 'DELETE' }),
  repos: () => request<Repository[]>('/api/repos'),
  patchRepo: (id: number, patch: RepoPatch) =>
    request<void>(`/api/repos/${id}`, { method: 'PATCH', body: patch }),
  teams: () => request<Team[]>('/api/teams'),
  createTeam: (name: string) => request<Team>('/api/teams', { method: 'POST', body: { name } }),
  renameTeam: (id: number, name: string) =>
    request<TeamSummary>(`/api/teams/${id}`, { method: 'PATCH', body: { name } }),
  deleteTeam: (id: number) => request<void>(`/api/teams/${id}`, { method: 'DELETE' }),
  setMember: (teamId: number, userId: number, role: TeamRole) =>
    request<void>(`/api/teams/${teamId}/members/${userId}`, { method: 'PUT', body: { role } }),
  removeMember: (teamId: number, userId: number) =>
    request<void>(`/api/teams/${teamId}/members/${userId}`, { method: 'DELETE' }),
  users: () => request<User[]>('/api/users'),
  installations: () => request<Installation[]>('/api/installations'),
  syncInstallation: (id: number) =>
    request<{ status: string }>(`/api/installations/${id}/sync`, { method: 'POST' }),
  setupStatus: () => request<SetupStatus>('/api/setup/status'),
  setupManifest: (body: SetupManifestRequest) =>
    request<SetupManifest>('/api/setup/manifest', { method: 'POST', body }),
  settings: () => request<SettingsResponse>('/api/settings'),
  saveSettings: (values: Record<string, string>) =>
    request<SettingsResponse>('/api/settings', { method: 'PUT', body: { values } }),
}

// SSE delivers named events only to listeners registered for that name, so
// every type the backend emits has to be listed here.
export const LIVE_EVENT_TYPES = [
  'pr.updated',
  'repo.updated',
  'run.updated',
  'run.log',
  'team.updated',
  'llm_profile.updated',
  'settings.updated',
  'user.updated',
] as const

/**
 * Abre la conexión SSE con el backend. La reconexión la gestiona EventSource de
 * forma nativa; aquí solo se propaga el estado de la conexión.
 */
export function openEventStream(
  onEvent: (event: ReviewEvent) => void,
  onState: (connected: boolean) => void,
): () => void {
  const source = new EventSource('/api/events')

  const handle = (message: MessageEvent<string>) => {
    try {
      const data = JSON.parse(message.data) as ReviewEvent
      if (data && typeof data.type === 'string') {
        onEvent(data)
      }
    } catch {
      // Un evento mal formado se ignora: el siguiente refetch corregirá el estado.
    }
  }

  for (const type of LIVE_EVENT_TYPES) {
    source.addEventListener(type, handle as EventListener)
  }
  source.onopen = () => onState(true)
  source.onerror = () => onState(false)

  return () => {
    for (const type of LIVE_EVENT_TYPES) {
      source.removeEventListener(type, handle as EventListener)
    }
    source.close()
  }
}
