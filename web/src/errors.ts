/**
 * Mira el código de estado en lugar de `instanceof ApiError` para que funcione
 * con cualquier error que lo lleve, también los de módulos sustituidos en test.
 */
export function isForbidden(err: unknown): boolean {
  return typeof err === 'object' && err !== null && (err as { status?: unknown }).status === 403
}
