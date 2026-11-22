import { describe, expect, it } from 'vitest'
import { cacheShare, formatTokens } from './tokens'

describe('formatTokens', () => {
  it('keeps small counts exact and shortens thousands and millions', () => {
    expect(formatTokens(0)).toBe('0')
    expect(formatTokens(950)).toBe('950')
    expect(formatTokens(12_300)).toBe('12.3k')
    expect(formatTokens(1_500_000)).toBe('1.5M')
  })
})

describe('cacheShare', () => {
  it('adds cache reads and writes before dividing by the total', () => {
    expect(
      cacheShare({ total_tokens: 100_000, cache_read_tokens: 60_000, cache_write_tokens: 10_000 }),
    ).toBe('70 %')
  })

  it('reports no percentage for a run without tokens', () => {
    expect(cacheShare({ total_tokens: null, cache_read_tokens: null, cache_write_tokens: null })).toBe('—')
    expect(cacheShare({ total_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0 })).toBe('—')
  })

  it('treats a missing cache column as zero', () => {
    expect(cacheShare({ total_tokens: 1000, cache_read_tokens: 500, cache_write_tokens: null })).toBe('50 %')
  })
})
