import { describe, expect, it } from 'vitest'
import { statusLabel } from './status'
import type { QAStatus } from './api'

type Expected = {
  status: QAStatus
  tag: string
  action: string
  tone: 'ok' | 'busy' | 'bad' | 'muted'
}

const cases: Expected[] = [
  { status: 'ready_for_human', tag: 'Listo para ti', action: 'Pendiente de aprobación', tone: 'ok' },
  { status: 'pending', tag: 'Revisando IA', action: 'Análisis en curso', tone: 'busy' },
  { status: 'reviewing', tag: 'Revisando IA', action: 'Análisis en curso', tone: 'busy' },
  { status: 'waiting_ci', tag: 'Esperando CI', action: 'Análisis en curso', tone: 'busy' },
  { status: 'changes_requested', tag: 'Con defectos', action: 'Requiere corrección', tone: 'bad' },
  { status: 'inconclusive', tag: 'Inconcluso', action: 'Requiere atención', tone: 'bad' },
  { status: 'awaiting_approval', tag: 'Esperando aprobación', action: 'Requiere aprobación', tone: 'bad' },
  { status: 'paused', tag: 'Pausado', action: 'Pausado', tone: 'muted' },
  { status: 'draft', tag: 'Borrador', action: '—', tone: 'muted' },
]

describe('statusLabel', () => {
  it.each(cases)('mapea $status a "$tag" / "$action" / $tone', ({ status, tag, action, tone }) => {
    expect(statusLabel(status)).toEqual({ tag, action, tone })
  })
})
