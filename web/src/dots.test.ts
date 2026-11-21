import { describe, expect, it } from 'vitest'
import { ringDots } from './dots'

describe('ringDots', () => {
  it('splits the ring by the share of each segment, starting at the top', () => {
    const dots = ringDots(
      [
        { value: 3, tone: 'on' },
        { value: 1, tone: 'bad' },
      ],
      8,
      50,
      100,
      4,
    )

    expect(dots.map((dot) => dot.tone)).toEqual(['on', 'on', 'on', 'on', 'on', 'on', 'bad', 'bad'])
    expect(dots[0].x).toBeCloseTo(48)
    expect(dots[0].y).toBeCloseTo(-2)
  })

  it('leaves every dot unlit when there is nothing to show', () => {
    const dots = ringDots([{ value: 0, tone: 'on' }], 6, 40, 100, 4)
    expect(dots.every((dot) => dot.tone === 'off')).toBe(true)
  })
})
