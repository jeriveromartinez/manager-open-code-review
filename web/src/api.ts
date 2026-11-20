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
  validations: { passed: number; total: number }
  updated_at: string
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
  coverage: Record<string, unknown>
  usage: Record<string, unknown>
  error_code: string
  error_message: string
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

export type Stats = {
  open: number
  awaiting_human: number
  in_evaluation: number
  with_problems: number
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
}

export type SetupManifest = {
  action_url: string
  manifest: Record<string, unknown>
}

export type SetupManifestRequest = {
  setup_token: string
  org: string
}

export type RepoPatch = {
  enabled?: boolean
  team_id?: number | null
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
  stats: () => request<Stats>('/api/stats'),
  prs: (group?: PRGroup) =>
    request<PRView[]>(group ? `/api/prs?group=${encodeURIComponent(group)}` : '/api/prs'),
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
  createLLMProfile: (body: LLMProfileInput) =>
    request<LLMProfile>('/api/llm-profiles', { method: 'POST', body }),
  updateLLMProfile: (id: number, patch: LLMProfilePatch) =>
    request<LLMProfile>(`/api/llm-profiles/${id}`, { method: 'PATCH', body: patch }),
  checkLLMHealth: (id: number) =>
    request<{ status: string }>(`/api/llm-profiles/${id}/health`, { method: 'POST' }),
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
  setupStatus: () => request<SetupStatus>('/api/setup/status'),
  setupManifest: (body: SetupManifestRequest) =>
    request<SetupManifest>('/api/setup/manifest', { method: 'POST', body }),
}

/**
 * Abre la conexión SSE con el backend. La reconexión la gestiona EventSource de
 * forma nativa; aquí solo se propaga el estado para la insignia del Dashboard.
 */
export function subscribeEvents(
  onEvent: (event: ReviewEvent) => void,
  onState?: (connected: boolean) => void,
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

  source.addEventListener('pr.updated', handle as EventListener)
  source.addEventListener('repo.updated', handle as EventListener)
  source.onopen = () => onState?.(true)
  source.onerror = () => onState?.(false)

  return () => {
    source.removeEventListener('pr.updated', handle as EventListener)
    source.removeEventListener('repo.updated', handle as EventListener)
    source.close()
  }
}
