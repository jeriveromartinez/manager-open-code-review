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
