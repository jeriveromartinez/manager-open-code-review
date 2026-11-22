/** Compact token count for the panel: 950, 12.3k, 1.5M. */
export function formatTokens(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}k`
  return String(value)
}

type CacheUsage = {
  total_tokens: number | null
  cache_read_tokens: number | null
  cache_write_tokens: number | null
}

/**
 * Share of the run served from the provider cache. An empty run reports "—"
 * instead of 0 %, which would read as a fully uncached run.
 */
export function cacheShare(usage: CacheUsage): string {
  const total = usage.total_tokens ?? 0
  if (total === 0) return '—'
  const cached = (usage.cache_read_tokens ?? 0) + (usage.cache_write_tokens ?? 0)
  return `${Math.round((cached / total) * 100)} %`
}
