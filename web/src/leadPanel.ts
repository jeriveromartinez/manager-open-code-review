import type { PRView } from './api'
import { formatWait, isOverSla, qaBucket, waitMinutes } from './qaQueue'

export type Threshold = 'blocker' | 'major' | 'minor' | 'nit'

export const thresholds: Threshold[] = ['blocker', 'major', 'minor', 'nit']

const atOrAbove: Record<Threshold, (keyof PRView['open_findings'])[]> = {
  blocker: ['blocker'],
  major: ['blocker', 'major'],
  minor: ['blocker', 'major', 'minor'],
  nit: ['blocker', 'major', 'minor', 'nit'],
}

// Mirrors the gate in internal/publish: a PR is blocked when any open finding
// is at or above the team's threshold. It lets a lead see what a different
// threshold would do to today's queue before changing the policy.
export function blockedAt(prs: PRView[], threshold: Threshold): number {
  const severities = atOrAbove[threshold]
  return prs.filter((pr) => severities.some((severity) => pr.open_findings[severity] > 0)).length
}

export type DecisionRow = { pr: PRView; reason: string; wait: string; minutes: number }

function reasonFor(pr: PRView, slaSeconds: number, now: Date): string | null {
  // A draft is its author's work in progress, not something a lead unblocks.
  if (pr.qa_status === 'draft') return null
  if (pr.qa_status === 'pending' && pr.review_state === 'blocked') return pr.review_reason
  switch (qaBucket(pr)) {
    case 'defects':
      return 'Con defectos'
    case 'inconclusive':
      return 'Inconcluso'
    case 'other':
      return 'En pausa'
    case 'ready':
      return isOverSla(pr, slaSeconds, now) ? 'Fuera de plazo de QA' : null
    default:
      return null
  }
}

// What a lead has to unblock: PRs the review cannot move on its own.
export function decisionRows(prs: PRView[], slaSeconds: number, now: Date): DecisionRow[] {
  return prs
    .flatMap((pr) => {
      const reason = reasonFor(pr, slaSeconds, now)
      if (reason === null) return []
      const minutes = waitMinutes(pr, now)
      return [{ pr, reason, minutes, wait: formatWait(minutes) }]
    })
    .sort((a, b) => b.minutes - a.minutes)
}
