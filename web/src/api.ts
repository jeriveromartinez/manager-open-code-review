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
}

/**
 * Archivo dentro de la cobertura de una ejecución. `status` se deja como string
 * para que un estado nuevo del backend se muestre tal cual en vez de romper.
 */
export type CoverageFile = {
  path: string
  status: string
  /** Trozos en los que se partió un archivo revisado por partes. */
  chunks?: number
  /** Motivo por el que un archivo no se revisó o quedó excluido. */
  reason?: string
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
  /** null en ejecuciones anteriores al registro de consumo (#73). */
  input_tokens: number | null
  output_tokens: number | null
  /** Parte oculta de output_tokens; baja al elegir una variante de bajo esfuerzo (#65). */
  reasoning_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
  total_tokens: number | null
  /** Mayor prompt de la ejecución; vigila el techo de contexto (#72). */
  max_context_tokens: number | null
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
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
  /** QA verdict from the panel; a review run or /qa moving the status leaves it unchanged. */
  triage_verdict: '' | 'confirmed' | 'false_positive'
  triaged_by: string
  triaged_at: string | null
  created_at: string
  updated_at: string
}

/**
 * El detalle reparte los campos del PR en la raíz de la respuesta (el DTO de
 * Go está incrustado), junto a `runs` y `findings`.
 */
export type PRDetail = PRView & {
  runs: ReviewRun[]
  findings: Finding[]
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
  members: TeamMember[]
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
  /** Null inherits the global review_context_ceiling. */
  review_context_ceiling: number | null
  /** Read-only: the stored global review_context_ceiling setting. */
  global_context_ceiling: number
  /** Read-only: the policy override, or the global setting when null. */
  effective_context_ceiling: number
  review_effort: ReviewEffort
  /** 0 disables the optional total-spend cap. */
  review_token_budget: number
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
  | 'review_context_ceiling'
  | 'review_effort'
  | 'review_token_budget'
>

export type ReviewEvent = {
  type: string
  id: string
}

export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
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
    throw new ApiError(res.status, detail || res.statusText || `HTTP ${res.status}`)
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
  retryPR: (id: number) =>
    request<{ status: string }>(`/api/prs/${id}/retry`, { method: 'POST' }),
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
  repos: () => request<Repository[]>('/api/repos'),
  patchRepo: (id: number, patch: RepoPatch) =>
    request<void>(`/api/repos/${id}`, { method: 'PATCH', body: patch }),
  teams: () => request<Team[]>('/api/teams'),
  createTeam: (name: string) => request<Team>('/api/teams', { method: 'POST', body: { name } }),
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
