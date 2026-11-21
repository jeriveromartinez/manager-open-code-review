import type { QAStatus } from './api'

export type StatusTone = 'ok' | 'busy' | 'bad' | 'muted'

export type StatusLabel = {
  tag: string
  action: string
  tone: StatusTone
}

const labels: Partial<Record<QAStatus, StatusLabel>> = {
  ready_for_human: { tag: 'Listo para ti', action: 'Pendiente de aprobación', tone: 'ok' },
  pending: { tag: 'Revisando IA', action: 'Análisis en curso', tone: 'busy' },
  reviewing: { tag: 'Revisando IA', action: 'Análisis en curso', tone: 'busy' },
  waiting_ci: { tag: 'Esperando CI', action: 'Análisis en curso', tone: 'busy' },
  changes_requested: { tag: 'Con defectos', action: 'Requiere corrección', tone: 'bad' },
  inconclusive: { tag: 'Inconcluso', action: 'Requiere atención', tone: 'bad' },
  awaiting_approval: { tag: 'Esperando aprobación', action: 'Requiere aprobación', tone: 'bad' },
  paused: { tag: 'Pausado', action: 'Pausado', tone: 'muted' },
  draft: { tag: 'Borrador', action: '—', tone: 'muted' },
}

export function statusLabel(status: QAStatus): StatusLabel {
  return labels[status] ?? { tag: status, action: '—', tone: 'muted' }
}

/**
 * El estado `pending` del PR no siempre significa que haya un análisis en
 * curso: también cubre PRs que nunca se revisarán (borrador, fork, sin equipo
 * o sin política). El backend clasifica ese caso en `review_state`.
 */
export function prStatusLabel(pr: {
  qa_status: QAStatus
  review_state: string
  review_reason: string
}): StatusLabel {
  if (pr.qa_status === 'pending' && pr.review_state === 'blocked') {
    return { tag: 'Sin revisión programada', action: pr.review_reason, tone: 'bad' }
  }
  if (pr.qa_status === 'pending' && pr.review_state === 'scheduled') {
    return { tag: 'Pendiente de revisión', action: 'Se encolará en la próxima reconciliación', tone: 'muted' }
  }
  return statusLabel(pr.qa_status)
}
