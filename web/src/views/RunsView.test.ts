import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, type LocationQueryRaw, type Router } from 'vue-router'
import RunsView from './RunsView.vue'
import type { ActiveRun, RunHistoryRow } from '../api'

const mocks = vi.hoisted(() => ({
  api: { activeRuns: vi.fn(), runHistory: vi.fn() },
}))
vi.mock('../api', () => ({ api: mocks.api }))

type LiveRegistration = { types: string[]; reload: () => unknown }
const live = vi.hoisted(() => ({ registrations: [] as LiveRegistration[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: () => unknown) => {
    live.registrations.push({ types, reload })
  },
}))

const BASE = Date.now()

function iso(offsetMs: number): string {
  return new Date(BASE + offsetMs).toISOString()
}

function activeRun(overrides: Partial<ActiveRun> = {}): ActiveRun {
  return {
    id: 9,
    pull_request_id: 42,
    pr_number: 142,
    pr_title: 'Corrige el login',
    repo: 'backend-api',
    team: 'Pagos',
    profile: 'revisor',
    trigger: 'manual',
    status: 'running',
    attempt: 2,
    phase: 'reviewing',
    phase_started_at: iso(-90_000),
    started_at: iso(-120_000),
    created_at: iso(-130_000),
    ...overrides,
  }
}

function historyRun(overrides: Partial<RunHistoryRow> = {}): RunHistoryRow {
  return {
    id: 20,
    pull_request_id: 50,
    pr_number: 150,
    pr_title: 'Migra la tabla',
    repo: 'infra',
    team: 'Infra',
    profile: 'revisor',
    trigger: 'webhook',
    status: 'succeeded',
    attempt: 1,
    phase: '',
    phase_started_at: null,
    started_at: iso(-300_000),
    finished_at: iso(-210_000),
    created_at: iso(-320_000),
    error_code: '',
    error_message: '',
    duration_seconds: 90,
    ...overrides,
  }
}

function historyPage(
  runs: RunHistoryRow[],
  cursor: { next_before?: number; next_created_at?: string | null; has_more?: boolean } = {},
) {
  return {
    runs,
    next_before: cursor.next_before ?? 0,
    next_created_at: cursor.next_created_at ?? null,
    has_more: cursor.has_more ?? false,
  }
}

async function mountRuns(
  query: LocationQueryRaw = {},
): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/runs', component: { template: '<div />' } },
      { path: '/prs/:id', component: { template: '<div />' } },
    ],
  })
  await router.push({ path: '/runs', query })
  await router.isReady()
  const wrapper = mount(RunsView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

describe('RunsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    live.registrations.length = 0
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
    mocks.api.runHistory.mockResolvedValue(historyPage([]))
  })

  it('lists active and finished runs together, newest first, without duplicates', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [activeRun()] })
    mocks.api.runHistory.mockResolvedValue(
      historyPage([
        historyRun({ id: 20, created_at: iso(-320_000) }),
        // A stale history copy of an active run: the active row must win.
        historyRun({ id: 9, status: 'succeeded', created_at: iso(-900_000) }),
      ]),
    )

    const { wrapper } = await mountRuns()

    const cards = wrapper.findAll('[data-run-card]')
    expect(cards).toHaveLength(2)
    expect(cards.map((card) => card.attributes('data-run-id'))).toEqual(['9', '20'])
    expect(cards[0].get('[data-run-status]').text()).toBe('En curso')
    expect(cards[1].get('[data-run-status]').text()).toBe('Completada')
    expect(cards[1].text()).toContain('infra#150')

    wrapper.unmount()
  })

  it('filters the loaded runs by status, repository, trigger and search', async () => {
    mocks.api.runHistory.mockResolvedValue(
      historyPage([
        historyRun({
          id: 21,
          status: 'failed',
          repo: 'infra',
          trigger: 'manual',
          pr_title: 'Rompe el login',
          created_at: iso(-200_000),
        }),
        historyRun({
          id: 22,
          status: 'succeeded',
          repo: 'backend-api',
          trigger: 'webhook',
          pr_title: 'Migra la tabla',
          created_at: iso(-300_000),
        }),
      ]),
    )

    const byStatus = await mountRuns({ status: 'failed' })
    expect(byStatus.wrapper.findAll('[data-run-card]').map((card) => card.attributes('data-run-id'))).toEqual(['21'])
    byStatus.wrapper.unmount()

    const byRepo = await mountRuns({ repo: 'backend-api' })
    expect(byRepo.wrapper.findAll('[data-run-card]').map((card) => card.attributes('data-run-id'))).toEqual(['22'])
    byRepo.wrapper.unmount()

    const byTrigger = await mountRuns({ trigger: 'manual' })
    expect(byTrigger.wrapper.findAll('[data-run-card]').map((card) => card.attributes('data-run-id'))).toEqual(['21'])
    byTrigger.wrapper.unmount()

    const byTitle = await mountRuns({ q: 'login' })
    expect(byTitle.wrapper.findAll('[data-run-card]').map((card) => card.attributes('data-run-id'))).toEqual(['21'])
    byTitle.wrapper.unmount()

    const bySearchRepo = await mountRuns({ q: 'backend-api' })
    expect(bySearchRepo.wrapper.findAll('[data-run-card]').map((card) => card.attributes('data-run-id'))).toEqual(['22'])
    bySearchRepo.wrapper.unmount()
  })

  it('shows the error of a failed run', async () => {
    mocks.api.runHistory.mockResolvedValue(
      historyPage([
        historyRun({
          id: 30,
          status: 'failed',
          repo: 'backend-api',
          error_code: 'clone_failed',
          error_message: 'No se pudo clonar el repositorio',
        }),
      ]),
    )

    const { wrapper } = await mountRuns()

    const error = wrapper.get('.run-card__error')
    expect(error.text()).toContain('clone_failed')
    expect(error.text()).toContain('No se pudo clonar el repositorio')

    wrapper.unmount()
  })

  it('shows a dash for a finished run without start or finish time', async () => {
    mocks.api.runHistory.mockResolvedValue(
      historyPage([
        historyRun({ id: 31, status: 'superseded', started_at: null, finished_at: null, duration_seconds: null }),
        historyRun({ id: 32, status: 'failed', started_at: iso(-200_000), finished_at: null, duration_seconds: null }),
      ]),
    )

    const { wrapper } = await mountRuns()

    const dashes = wrapper.findAll('[data-run-elapsed]')
    expect(dashes).toHaveLength(2)
    expect(dashes[0].text()).toBe('—')
    expect(dashes[1].text()).toBe('—')

    wrapper.unmount()
  })

  it('loads the next history page and keeps filters applied', async () => {
    const cursor = '2026-11-26T10:00:00.123456789Z'
    mocks.api.runHistory.mockResolvedValueOnce(
      historyPage(
        [
          historyRun({
            id: 20,
            status: 'failed',
            repo: 'infra',
            trigger: 'manual',
            created_at: iso(-200_000),
          }),
        ],
        { has_more: true, next_before: 20, next_created_at: cursor },
      ),
    )
    mocks.api.runHistory.mockResolvedValueOnce(
      historyPage([
        historyRun({
          id: 10,
          status: 'failed',
          repo: 'infra',
          trigger: 'manual',
          created_at: iso(-500_000),
        }),
        historyRun({ id: 11, status: 'succeeded', repo: 'infra', created_at: iso(-400_000) }),
      ]),
    )

    const { wrapper } = await mountRuns({ status: 'failed' })
    expect(wrapper.findAll('[data-run-card]')).toHaveLength(1)

    await wrapper.get('[data-runs-more]').trigger('click')
    await flushPromises()

    expect(mocks.api.runHistory).toHaveBeenLastCalledWith({
      before: 20,
      beforeCreatedAt: cursor,
      limit: 50,
    })
    const cards = wrapper.findAll('[data-run-card]')
    expect(cards.map((card) => card.attributes('data-run-id'))).toEqual(['20', '10'])
    expect(wrapper.find('[data-runs-more]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('keeps the active list on a silent refresh failure', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [activeRun()] })
    const { wrapper } = await mountRuns()

    const [registration] = live.registrations
    expect(registration.types).toEqual(['run.updated', 'pr.updated'])

    mocks.api.activeRuns.mockRejectedValueOnce(new Error('red caída'))
    await registration.reload()
    await flushPromises()

    expect(wrapper.findAll('[data-run-card]')).toHaveLength(1)
    expect(wrapper.get('[data-runs-refresh-error]').text()).toContain('No se pudo actualizar')

    wrapper.unmount()
  })

  it('shows the filters empty state when nothing matches', async () => {
    mocks.api.runHistory.mockResolvedValue(historyPage([historyRun()]))

    const { wrapper } = await mountRuns({ q: 'no-existe' })

    expect(wrapper.text()).toContain('NADA COINCIDE CON ESTOS FILTROS')
    expect(wrapper.get('.filter-bar__count').text()).toBe('0 DE 1')
    expect(wrapper.findAll('[data-run-card]')).toHaveLength(0)

    wrapper.unmount()
  })

  it('lists every active run with its metadata, phase and elapsed time', async () => {
    mocks.api.activeRuns.mockResolvedValue({
      runs: [
        activeRun(),
        activeRun({
          id: 10,
          pull_request_id: 43,
          pr_number: 143,
          repo: 'infra',
          team: '',
          profile: 'rápido',
          status: 'queued',
          phase: '',
          phase_started_at: null,
          started_at: null,
          created_at: iso(-200_000),
        }),
      ],
    })

    const { wrapper } = await mountRuns()

    const cards = wrapper.findAll('[data-active-run]')
    expect(cards).toHaveLength(2)
    expect(cards[0].text()).toContain('backend-api#142')
    expect(cards[0].text()).toContain('Corrige el login')
    expect(cards[0].text()).toContain('Pagos')
    expect(cards[0].text()).toContain('revisor')
    expect(cards[0].get('[data-run-status]').text()).toBe('En curso')
    expect(cards[0].get('[data-run-phase]').text()).toContain('Revisando con OCR')
    expect(cards[0].get('[data-run-elapsed]').text()).toContain('min')

    expect(cards[1].get('[data-run-status]').text()).toBe('En cola')
    expect(cards[1].get('[data-run-phase]').text()).toBe('—')
    expect(cards[1].text()).toContain('infra#143')
    expect(wrapper.get('[data-runs-count]').text()).toBe('1 en curso · 1 en cola')

    wrapper.unmount()
  })

  it('shows an empty state when nothing is running or queued', async () => {
    const { wrapper } = await mountRuns()

    expect(wrapper.get('[data-runs-empty]').text()).toContain('No hay revisiones en curso ni terminadas')
    expect(wrapper.get('[data-runs-count]').text()).toBe('sin revisiones')
    expect(wrapper.find('[data-run-card]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('reports a failed first load instead of an empty list', async () => {
    mocks.api.activeRuns.mockRejectedValue(new Error('sin conexión'))
    mocks.api.runHistory.mockRejectedValue(new Error('sin conexión'))

    const { wrapper } = await mountRuns()

    expect(wrapper.get('.form-error').text()).toContain('No se pudieron cargar las revisiones')
    expect(wrapper.find('[data-runs-empty]').exists()).toBe(false)

    wrapper.unmount()
  })
})
