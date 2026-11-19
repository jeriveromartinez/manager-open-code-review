import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import DashboardView from './DashboardView.vue'
import type { PRView, Stats } from '../api'

type EventPayload = { type: string; id: string }

const mocks = vi.hoisted(() => {
  const handlers: {
    onEvent: ((event: EventPayload) => void) | null
    onState: ((connected: boolean) => void) | null
  } = { onEvent: null, onState: null }

  return {
    handlers,
    api: {
      me: vi.fn(),
      stats: vi.fn(),
      prs: vi.fn(),
    },
    subscribeEvents: vi.fn(
      (onEvent: (event: EventPayload) => void, onState?: (connected: boolean) => void) => {
        handlers.onEvent = onEvent
        handlers.onState = onState ?? null
        return () => {
          handlers.onEvent = null
          handlers.onState = null
        }
      },
    ),
  }
})

vi.mock('../api', () => ({
  api: mocks.api,
  subscribeEvents: mocks.subscribeEvents,
}))

const stats: Stats = { open: 4, awaiting_human: 2, in_evaluation: 1, with_problems: 1 }

const prs: PRView[] = [
  {
    id: 1,
    repo: 'backend-api',
    number: 142,
    title: 'Añade endpoint de pedidos',
    author_login: 'octocat',
    html_url: 'https://github.com/acme/backend-api/pull/142',
    qa_status: 'ready_for_human',
    group: 'ready',
    validations: { passed: 9, total: 9 },
    updated_at: '2026-11-18T10:00:00Z',
  },
  {
    id: 2,
    repo: 'frontend-app',
    number: 77,
    title: 'Corrige el carrito',
    author_login: 'hubot',
    html_url: 'https://github.com/acme/frontend-app/pull/77',
    qa_status: 'pending',
    group: 'in_review',
    validations: { passed: 0, total: 0 },
    updated_at: '2026-11-18T09:00:00Z',
  },
]

function mountDashboard(): VueWrapper {
  return mount(DashboardView, { global: { plugins: [createPinia()] } })
}

function kpi(wrapper: VueWrapper, key: string): string {
  return wrapper.get(`[data-kpi="${key}"]`).text()
}

describe('DashboardView', () => {
  beforeEach(() => {
    mocks.api.me.mockResolvedValue({
      id: 1,
      login: 'julio',
      avatar_url: 'https://avatars.githubusercontent.com/u/1',
      is_admin: true,
      teams: [],
    })
    mocks.api.stats.mockResolvedValue(stats)
    mocks.api.prs.mockResolvedValue(prs)
    mocks.api.stats.mockClear()
    mocks.api.prs.mockClear()
    mocks.subscribeEvents.mockClear()
  })

  it('muestra los 4 KPIs con sus valores', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(kpi(wrapper, 'open')).toContain('PRs abiertos')
    expect(kpi(wrapper, 'open')).toContain('4')
    expect(kpi(wrapper, 'awaiting_human')).toContain('Esperando tu revisión')
    expect(kpi(wrapper, 'awaiting_human')).toContain('2')
    expect(kpi(wrapper, 'in_evaluation')).toContain('En evaluación')
    expect(kpi(wrapper, 'in_evaluation')).toContain('1')
    expect(kpi(wrapper, 'with_problems')).toContain('Con problemas')
    expect(kpi(wrapper, 'with_problems')).toContain('1')

    expect(wrapper.text()).toContain('Todos los repositorios')
    expect(wrapper.text()).toContain('2 repositorios')
    expect(wrapper.text()).toContain('backend-api #142')
    wrapper.unmount()
  })

  it('al pulsar "Listos" pide los PRs del grupo ready', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    const listos = wrapper.findAll('button.filter-tab').find((tab) => tab.text() === 'Listos')
    expect(listos).toBeTruthy()

    await listos!.trigger('click')
    await flushPromises()

    expect(mocks.api.prs).toHaveBeenCalledWith('ready')
    wrapper.unmount()
  })

  it('un evento SSE pr.updated provoca un refetch con debounce', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(mocks.subscribeEvents).toHaveBeenCalledTimes(1)
    expect(mocks.handlers.onEvent).toBeTypeOf('function')

    mocks.api.stats.mockClear()
    mocks.api.prs.mockClear()

    mocks.handlers.onEvent!({ type: 'pr.updated', id: '42' })

    await new Promise((resolve) => setTimeout(resolve, 350))
    await flushPromises()

    expect(mocks.api.stats).toHaveBeenCalledTimes(1)
    expect(mocks.api.prs).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
})
