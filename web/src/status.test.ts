import { describe, expect, it } from 'vitest'
import { prStatusLabel, statusLabel } from './status'
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

describe('prStatusLabel', () => {
  it('explica por qué un PR pendiente no tiene revisión programada', () => {
    expect(
      prStatusLabel({
        qa_status: 'pending',
        review_state: 'blocked',
        review_reason: 'Repositorio sin equipo asignado',
      }),
    ).toEqual({
      tag: 'Sin revisión programada',
      action: 'Repositorio sin equipo asignado',
      tone: 'bad',
    })
  })

  it('muestra que un PR pendiente se encolará en la próxima reconciliación', () => {
    expect(
      prStatusLabel({ qa_status: 'pending', review_state: 'scheduled', review_reason: '' }),
    ).toEqual({
      tag: 'Pendiente de revisión',
      action: 'Se encolará en la próxima reconciliación',
      tone: 'muted',
    })
  })

  it('deja intacto un PR pendiente con una ejecución en curso', () => {
    expect(
      prStatusLabel({ qa_status: 'pending', review_state: 'running', review_reason: '' }),
    ).toEqual({ tag: 'Revisando IA', action: 'Análisis en curso', tone: 'busy' })
  })

  it('distingue un PR en cola de una ejecución en curso', () => {
    expect(
      prStatusLabel({ qa_status: 'pending', review_state: 'queued', review_reason: '' }),
    ).toEqual({ tag: 'En cola para revisión', action: 'Esperando turno del worker', tone: 'muted' })
  })

  it('solo reinterpreta el estado pending', () => {
    expect(
      prStatusLabel({
        qa_status: 'changes_requested',
        review_state: 'blocked',
        review_reason: 'Repositorio sin equipo asignado',
      }),
    ).toEqual({ tag: 'Con defectos', action: 'Requiere corrección', tone: 'bad' })
  })
})
