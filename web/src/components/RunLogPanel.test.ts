import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import RunLogPanel from './RunLogPanel.vue'

const mocks = vi.hoisted(() => ({ reviewRunLogs: vi.fn() }))
vi.mock('../api', () => ({ api: { reviewRunLogs: mocks.reviewRunLogs } }))

type LiveRegistration = {
  types: string[]
  reload: () => unknown
  filter?: (event: { type: string; id: string }) => boolean
}
const live = vi.hoisted(() => ({ registrations: [] as LiveRegistration[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: () => unknown, filter?: LiveRegistration['filter']) => {
    live.registrations.push({ types, reload, filter })
  },
}))

function entry(seq: number, line: string) {
  return {
    seq,
    ts: '2026-11-22T10:00:00Z',
    source: 'ocr',
    invocation: 'main',
    level: 'info',
    phase: 'ocr',
    line,
  }
}

describe('RunLogPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    live.registrations.length = 0
    mocks.reviewRunLogs.mockImplementation(
      async (_id: number, after: number) => {
        if (after === 0) {
          return { run_id: 9, entries: [entry(1, 'primera'), entry(2, 'segunda')], next_after: 2, has_more: true }
        }
        if (after === 2) {
          return { run_id: 9, entries: [entry(3, 'tercera')], next_after: 3, has_more: false }
        }
        return { run_id: 9, entries: [], next_after: after, has_more: false }
      },
    )
  })

  it('loads every page and appends the new lines on a live event', async () => {
    const wrapper = mount(RunLogPanel, { props: { runId: 9, live: false } })
    await flushPromises()

    expect(mocks.reviewRunLogs).toHaveBeenNthCalledWith(1, 9, 0, 500)
    expect(mocks.reviewRunLogs).toHaveBeenNthCalledWith(2, 9, 2, 500)
    expect(wrapper.findAll('[data-run-log-line]')).toHaveLength(3)
    expect(wrapper.get('[data-run-log-count]').text()).toContain('3 líneas')

    const registration = live.registrations.find((item) => item.types.includes('run.log'))
    expect(registration).toBeDefined()
    expect(registration!.filter!({ type: 'run.log', id: '9' })).toBe(true)
    expect(registration!.filter!({ type: 'run.log', id: '10' })).toBe(false)

    mocks.reviewRunLogs.mockResolvedValueOnce({
      run_id: 9,
      entries: [entry(4, 'cuarta')],
      next_after: 4,
      has_more: false,
    })
    await registration!.reload()
    await flushPromises()
    expect(mocks.reviewRunLogs).toHaveBeenLastCalledWith(9, 3, 500)
    expect(wrapper.findAll('[data-run-log-line]')).toHaveLength(4)
    wrapper.unmount()
  })

  it('reports a load failure without losing what is on screen', async () => {
    mocks.reviewRunLogs.mockRejectedValueOnce(new Error('boom'))
    const wrapper = mount(RunLogPanel, { props: { runId: 9, live: false } })
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudo cargar el log')
    expect(wrapper.findAll('[data-run-log-line]')).toHaveLength(0)
    wrapper.unmount()
  })
})
