const phaseLabels: Record<string, string> = {
  cloning: 'Clonando',
  preparing: 'Preparando base y política',
  reviewing: 'Revisando con OCR',
  chunking: 'Revisando por trozos',
  finishing: 'Cerrando la revisión',
}

/**
 * Traduce el identificador de fase del run. Vacío significa que el run está en
 * cola, terminado o es anterior a #123: se pinta «—» en vez de inventar un paso.
 */
export function phaseLabel(phase: string): string {
  return phaseLabels[phase] ?? (phase || '—')
}

/** Tiempo transcurrido en formato corto: 42 s, 3 min, 2 h 05 min. */
export function formatElapsed(from: string | null, now: Date): string {
  if (!from) return '—'
  const start = new Date(from).getTime()
  if (Number.isNaN(start)) return '—'
  const seconds = Math.max(0, Math.floor((now.getTime() - start) / 1000))
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} h ${String(minutes % 60).padStart(2, '0')} min`
  const days = Math.floor(hours / 24)
  return `${days} d ${String(hours % 24).padStart(2, '0')} h`
}
