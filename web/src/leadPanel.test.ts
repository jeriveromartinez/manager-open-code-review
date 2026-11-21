import { describe, expect, it } from 'vitest'
import { blockedAt, decisionRows } from './leadPanel'
import type { PRView } from './api'

const now = new Date('2026-11-21T12:00:00Z')
const sla = 4 * 3600

function pr(overrides: Partial<PRView>): PRView {
  return {
    id: 1,
    repo: 'acme/app',
    number: 1,
    title: 'PR',
    author_login: 'octocat',
    html_url: 'https://github.com/acme/app/pull/1',
    qa_status: 'ready_for_human',
    group: 'ready',
    review_state: '',
    review_reason: '',
    validations: { passed: 0, total: 0 },
    open_findings: { blocker: 0, major: 0, minor: 0, nit: 0 },
    status_since: '2026-11-21T11:00:00Z',
    team_id: 3,
    updated_at: '2026-11-21T11:00:00Z',
    ...overrides,
  }
}

describe('blockedAt', () => {
  const prs = [
    pr({ id: 1, open_findings: { blocker: 1, major: 0, minor: 0, nit: 0 } }),
    pr({ id: 2, open_findings: { blocker: 0, major: 2, minor: 0, nit: 0 } }),
    pr({ id: 3, open_findings: { blocker: 0, major: 0, minor: 1, nit: 3 } }),
    pr({ id: 4 }),
  ]

  it.each([
    ['blocker', 1],
    ['major', 2],
    ['minor', 3],
    ['nit', 3],
  ] as const)('a %s threshold blocks %i PRs', (threshold, count) => {
    expect(blockedAt(prs, threshold)).toBe(count)
  })
})

describe('decisionRows', () => {
  it('lists what needs a lead, longest wait first, with the reason', () => {
    const rows = decisionRows(
      [
        pr({ id: 1, status_since: '2026-11-21T11:30:00Z' }),
        pr({ id: 2, status_since: '2026-11-21T06:00:00Z' }),
        pr({ id: 3, qa_status: 'changes_requested', status_since: '2026-11-21T10:00:00Z' }),
        pr({ id: 4, qa_status: 'inconclusive', status_since: '2026-11-21T09:00:00Z' }),
        pr({
          id: 5,
          qa_status: 'pending',
          review_state: 'blocked',
          review_reason: 'El equipo no tiene política QA',
          status_since: '2026-11-20T12:00:00Z',
        }),
        pr({ id: 6, qa_status: 'reviewing', status_since: '2026-11-20T00:00:00Z' }),
        pr({ id: 7, qa_status: 'paused', status_since: '2026-11-21T08:00:00Z' }),
      ],
      sla,
      now,
    )

    expect(rows.map((row) => row.pr.id)).toEqual([5, 2, 7, 4, 3])
    expect(rows.map((row) => row.reason)).toEqual([
      'El equipo no tiene política QA',
      'Fuera de plazo de QA',
      'En pausa',
      'Inconcluso',
      'Con defectos',
    ])
    expect(rows[0].wait).toBe('1d 00h')
  })
})
