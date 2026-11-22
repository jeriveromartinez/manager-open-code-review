import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import RunsView from './RunsView.vue'
import type { ActiveRun } from '../api'

const mocks = vi.hoisted(() => ({ api: { activeRuns: vi.fn() } }))
vi.mock('../api', () => ({ api: mocks.api }))

type LiveRegistration = { types: string[]; reload: () => unknown }
const live = vi.hoisted(() => ({ registrations: [] as LiveRegistration[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: () => unknown) => {
    live.registrations.push({ types, reload })
  },
}))

function run(overrides: Partial<ActiveRun> = {}): ActiveRun {
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
    phase_started_at: new Date(Date.now() - 90_000).toISOString(),
    started_at: new Date(Date.now() - 120_000).toISOString(),
    created_at: new Date(Date.now() - 130_000).toISOString(),
    ...overrides,
  }
}

function mountRuns(): VueWrapper {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/runs', component: { template: '<div />' } },
      { path: '/prs/:id', component: { template: '<div />' } },
    ],
  })
  return mount(RunsView, { global: { plugins: [router] } })
}

describe('RunsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    live.registrations.length = 0
    mocks.api.activeRuns.mockResolvedValue({ runs: [] })
  })

  it('lists every active run with its metadata, phase and elapsed time', async () => {
    mocks.api.activeRuns.mockResolvedValue({
      runs: [
        run(),
        run({
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
        }),
      ],
    })

    const wrapper = mountRuns()
    await flushPromises()

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
    const wrapper = mountRuns()
    await flushPromises()

    expect(wrapper.get('[data-runs-empty]').text()).toContain('No hay revisiones en curso')
    expect(wrapper.get('[data-runs-count]').text()).toBe('sin revisiones activas')
    expect(wrapper.find('[data-active-run]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('refreshes on run and PR events without emptying the list on a silent failure', async () => {
    mocks.api.activeRuns.mockResolvedValue({ runs: [run()] })
    const wrapper = mountRuns()
    await flushPromises()

    const [registration] = live.registrations
    expect(registration.types).toEqual(['run.updated', 'pr.updated'])

    mocks.api.activeRuns.mockRejectedValueOnce(new Error('red caída'))
    await registration.reload()
    await flushPromises()

    expect(wrapper.findAll('[data-active-run]')).toHaveLength(1)
    expect(wrapper.get('[data-runs-refresh-error]').text()).toContain('No se pudo actualizar')

    wrapper.unmount()
  })

  it('reports a failed first load instead of an empty list', async () => {
    mocks.api.activeRuns.mockRejectedValue(new Error('sin conexión'))

    const wrapper = mountRuns()
    await flushPromises()

    expect(wrapper.get('.form-error').text()).toContain('No se pudieron cargar las revisiones en curso')
    expect(wrapper.find('[data-runs-empty]').exists()).toBe(false)

    wrapper.unmount()
  })
})
