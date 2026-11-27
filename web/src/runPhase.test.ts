import { describe, expect, it } from 'vitest'
import { phaseLabel } from './runPhase'

describe('phaseLabel', () => {
  it('labels the verifying phase', () => {
    expect(phaseLabel('verifying')).toBe('Verificando con OCR')
  })

  it('labels the recovery of an orphaned attempt', () => {
    expect(phaseLabel('recovering')).toBe('Recuperando la revisión')
  })

  it('keeps the empty phase as a dash instead of inventing a step', () => {
    expect(phaseLabel('')).toBe('—')
  })
})
