import { describe, expect, it } from 'vitest'
import { formatWait, isOverSla, qaBucket, sortForQueue, waitMinutes } from './qaQueue'
import type { PRView } from './api'

const now = new Date('2026-11-21T12:00:00Z')

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

describe('qaBucket', () => {
  it.each([
    ['ready_for_human', 'ready'],
    ['changes_requested', 'defects'],
    ['inconclusive', 'inconclusive'],
    ['pending', 'ai'],
    ['reviewing', 'ai'],
    ['waiting_ci', 'ai'],
    ['awaiting_approval', 'other'],
    ['paused', 'other'],
  ] as const)('%s goes to %s', (status, bucket) => {
    expect(qaBucket(pr({ qa_status: status }))).toBe(bucket)
  })
})

describe('waitMinutes and formatWait', () => {
  it('counts whole minutes since the status changed', () => {
    expect(waitMinutes(pr({ status_since: '2026-11-21T08:49:30Z' }), now)).toBe(190)
  })

  it('never reports a negative wait for a clock skewed into the future', () => {
    expect(waitMinutes(pr({ status_since: '2026-11-21T12:05:00Z' }), now)).toBe(0)
  })

  it.each([
    [0, '0 min'],
    [12, '12 min'],
    [190, '3h 10m'],
    [1562, '1d 02h'],
  ])('%i minutes read as %s', (minutes, text) => {
    expect(formatWait(minutes)).toBe(text)
  })
})

describe('isOverSla', () => {
  const sla = 4 * 3600

  it('flags a PR ready for QA that waited longer than the SLA', () => {
    expect(isOverSla(pr({ status_since: '2026-11-21T07:00:00Z' }), sla, now)).toBe(true)
    expect(isOverSla(pr({ status_since: '2026-11-21T09:00:00Z' }), sla, now)).toBe(false)
  })

  it('ignores PRs that are not waiting on QA', () => {
    const old = '2026-11-20T00:00:00Z'
    expect(isOverSla(pr({ qa_status: 'reviewing', status_since: old }), sla, now)).toBe(false)
  })

  it('is off when no SLA is configured', () => {
    expect(isOverSla(pr({ status_since: '2026-11-01T00:00:00Z' }), 0, now)).toBe(false)
  })
})

describe('sortForQueue', () => {
  it('puts the work waiting on QA first, oldest first, then the rest', () => {
    const sorted = sortForQueue([
      pr({ id: 1, qa_status: 'reviewing', status_since: '2026-11-21T01:00:00Z' }),
      pr({ id: 2, status_since: '2026-11-21T10:00:00Z' }),
      pr({ id: 3, qa_status: 'changes_requested', status_since: '2026-11-21T09:00:00Z' }),
      pr({ id: 4, status_since: '2026-11-21T06:00:00Z' }),
    ])
    expect(sorted.map((item) => item.id)).toEqual([4, 2, 3, 1])
  })
})
