export type DotTone = 'on' | 'dim' | 'bad' | 'ok' | 'off' | 'empty'

export type DotSegment = { value: number; tone: DotTone }

export type RingDot = { x: number; y: number; tone: DotTone }

/**
 * Lays `count` dots on a circle of `radius` centred in a `size` box, starting
 * at twelve o'clock and coloured by the share of each segment. With nothing to
 * show every dot stays unlit, so an empty ring still reads as a ring.
 */
export function ringDots(segments: DotSegment[], count: number, radius: number, size: number, dot: number): RingDot[] {
  const total = segments.reduce((sum, segment) => sum + Math.max(0, segment.value), 0)
  const centre = size / 2
  return Array.from({ length: count }, (_, i) => {
    const angle = ((-90 + (i * 360) / count) * Math.PI) / 180
    return {
      x: centre + radius * Math.cos(angle) - dot / 2,
      y: centre + radius * Math.sin(angle) - dot / 2,
      tone: toneAt(segments, total, i / count),
    }
  })
}

function toneAt(segments: DotSegment[], total: number, share: number): DotTone {
  if (total === 0) return 'off'
  let reached = 0
  for (const segment of segments) {
    reached += Math.max(0, segment.value) / total
    if (share < reached) return segment.tone
  }
  return 'off'
}
