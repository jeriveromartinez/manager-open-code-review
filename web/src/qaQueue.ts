import type { PRView } from './api'

export type QaBucket = 'ready' | 'defects' | 'inconclusive' | 'ai' | 'other'

const bucketOrder: QaBucket[] = ['ready', 'defects', 'inconclusive', 'ai', 'other']

export function qaBucket(pr: Pick<PRView, 'qa_status'>): QaBucket {
  switch (pr.qa_status) {
    case 'ready_for_human':
      return 'ready'
    case 'changes_requested':
      return 'defects'
    case 'inconclusive':
      return 'inconclusive'
    case 'pending':
    case 'reviewing':
    case 'waiting_ci':
      return 'ai'
    default:
      return 'other'
  }
}

export function waitMinutes(pr: Pick<PRView, 'status_since'>, now: Date): number {
  const elapsed = now.getTime() - new Date(pr.status_since).getTime()
  return Math.max(0, Math.floor(elapsed / 60_000))
}

export function formatWait(minutes: number): string {
  if (minutes < 60) return `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ${String(minutes % 60).padStart(2, '0')}m`
  return `${Math.floor(hours / 24)}d ${String(hours % 24).padStart(2, '0')}h`
}

// Only PRs waiting on QA run against the SLA, and an SLA of 0 turns it off.
export function isOverSla(pr: Pick<PRView, 'qa_status' | 'status_since'>, slaSeconds: number, now: Date): boolean {
  if (slaSeconds <= 0 || qaBucket(pr) !== 'ready') return false
  return waitMinutes(pr, now) * 60 > slaSeconds
}

// Work waiting on QA first; within each bucket, the longest wait first.
export function sortForQueue(prs: PRView[]): PRView[] {
  return [...prs].sort((a, b) => {
    const byBucket = bucketOrder.indexOf(qaBucket(a)) - bucketOrder.indexOf(qaBucket(b))
    if (byBucket !== 0) return byBucket
    return new Date(a.status_since).getTime() - new Date(b.status_since).getTime()
  })
}
