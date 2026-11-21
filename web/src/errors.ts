// These read the status code instead of `instanceof ApiError` so they work with
// any error that carries one, including errors from modules replaced in tests.
function hasStatus(err: unknown, status: number): boolean {
  return typeof err === 'object' && err !== null && (err as { status?: unknown }).status === status
}

export function isForbidden(err: unknown): boolean {
  return hasStatus(err, 403)
}

export function isConflict(err: unknown): boolean {
  return hasStatus(err, 409)
}
