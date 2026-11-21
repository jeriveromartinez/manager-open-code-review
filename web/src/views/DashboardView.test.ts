import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
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
      readiness: vi.fn(),
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
    review_state: '',
    review_reason: '',
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
    review_state: 'running',
    review_reason: '',
    validations: { passed: 0, total: 0 },
    updated_at: '2026-11-18T09:00:00Z',
  },
]

function mountDashboard(): VueWrapper {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/prs/:id', component: { template: '<div />' } },
      { path: '/repos', component: { template: '<div />' } },
      { path: '/teams', component: { template: '<div />' } },
      { path: '/agents', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } },
    ],
  })
  return mount(DashboardView, { global: { plugins: [createPinia(), router] } })
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
    mocks.api.readiness.mockResolvedValue({ ready: true, items: [] })
    mocks.api.stats.mockClear()
    mocks.api.prs.mockClear()
    mocks.api.readiness.mockClear()
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

  it('muestra el aviso de configuración pendiente con cada eslabón y su enlace', async () => {
    mocks.api.readiness.mockResolvedValue({
      ready: false,
      items: [
        {
          key: 'repos_without_team',
          label: '2 repositorios habilitados sin equipo asignado',
          count: 2,
          link: '/repos',
        },
        {
          key: 'teams_without_policy',
          label: '1 equipo sin política de revisión',
          count: 1,
          link: '/teams',
        },
        {
          key: 'unhealthy_profiles',
          label: '1 perfil LLM habilitado sin estado saludable',
          count: 1,
          link: '/agents',
        },
      ],
    })

    const wrapper = mountDashboard()
    await flushPromises()

    const banner = wrapper.get('[data-readiness-banner]')
    expect(banner.text()).toContain('2 repositorios habilitados sin equipo asignado')
    expect(banner.text()).toContain('1 equipo sin política de revisión')
    expect(banner.text()).toContain('1 perfil LLM habilitado sin estado saludable')

    expect(banner.get('[data-readiness-item="repos_without_team"] a').attributes('href')).toBe('/repos')
    expect(banner.get('[data-readiness-item="teams_without_policy"] a').attributes('href')).toBe('/teams')
    expect(banner.get('[data-readiness-item="unhealthy_profiles"] a').attributes('href')).toBe('/agents')
    wrapper.unmount()
  })

  it('oculta el aviso cuando todos los eslabones están completos', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(mocks.api.readiness).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-readiness-banner]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('no consulta ni muestra el aviso a quien no es admin', async () => {
    mocks.api.me.mockResolvedValue({
      id: 2,
      login: 'dev',
      avatar_url: '',
      is_admin: false,
      teams: [],
    })
    mocks.api.readiness.mockResolvedValue({
      ready: false,
      items: [
        { key: 'no_teams', label: 'No hay equipos', count: 1, link: '/teams' },
      ],
    })

    const wrapper = mountDashboard()
    await flushPromises()

    expect(mocks.api.readiness).not.toHaveBeenCalled()
    expect(wrapper.find('[data-readiness-banner]').exists()).toBe(false)
    wrapper.unmount()
  })
})
