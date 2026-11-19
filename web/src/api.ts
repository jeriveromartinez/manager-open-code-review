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
