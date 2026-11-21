import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import DashboardView from './DashboardView.vue'
import type { PRView, QASummary } from '../api'

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
      qaSummary: vi.fn(),
      prs: vi.fn(),
      readiness: vi.fn(),
      retryPR: vi.fn(),
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

const summary: QASummary = {
  ready_for_human: 2,
  inconclusive: 1,
  runs_running: 1,
  runs_queued: 3,
  untriaged: { blocker: 1, major: 2, minor: 4, nit: 0 },
  confirmed: 8,
  false_positive: 2,
  precision_window_days: 7,
  categories: [
    { category: 'tests', count: 5 },
    { category: 'security', count: 3 },
  ],
  confirmed_by_repo: [],
  category_window_days: 30,
  validation_sla_seconds: 4 * 3600,
}

function pr(overrides: Partial<PRView>): PRView {
  return {
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
    open_findings: { blocker: 0, major: 0, minor: 0, nit: 0 },
    status_since: new Date(Date.now() - 30 * 60_000).toISOString(),
    team_id: 3,
    updated_at: '2026-11-18T10:00:00Z',
    ...overrides,
  }
}

const prs: PRView[] = [
  pr({ id: 1 }),
  pr({
    id: 2,
    repo: 'frontend-app',
    number: 77,
    title: 'Corrige el carrito',
    status_since: new Date(Date.now() - 5 * 3600_000).toISOString(),
    team_id: 3,
  }),
  pr({
    id: 3,
    repo: 'frontend-app',
    number: 80,
    title: 'Migra la tabla',
    qa_status: 'reviewing',
    group: 'in_review',
    review_state: 'running',
  }),
  pr({ id: 4, repo: 'infra', number: 9, title: 'Sube el pool', qa_status: 'inconclusive', group: 'blocked' }),
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

function tile(wrapper: VueWrapper, key: string) {
  return wrapper.get(`[data-tile="${key}"]`)
}

describe('DashboardView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.me.mockResolvedValue({
      id: 1,
      login: 'mvega',
      avatar_url: '',
      is_admin: true,
      teams: [{ id: 3, name: 'Pagos', role: 'qa' }],
    })
    mocks.api.qaSummary.mockResolvedValue(summary)
    mocks.api.prs.mockResolvedValue(prs)
    mocks.api.readiness.mockResolvedValue({ ready: true, items: [] })
    mocks.api.retryPR.mockResolvedValue({ status: 'queued' })
  })

  it('saluda por rol y muestra el resumen de QA en las tarjetas', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.text()).toContain('Hola, mvega · QA en Pagos · plazo de validación 4 h')
    expect(tile(wrapper, 'ready').get('.kpi-card__value').text()).toBe('2')
    expect(tile(wrapper, 'triage').get('.kpi-card__value').text()).toBe('7')
    expect(wrapper.get('[data-precision]').text()).toBe('80%')
    expect(tile(wrapper, 'inconclusive').classes()).toContain('tile--alert')
    expect(wrapper.get('[data-runs-running]').text()).toBe('1')
    expect(wrapper.get('[data-runs-queued]').text()).toBe('3')
    expect(tile(wrapper, 'categories').text()).toContain('tests')
    wrapper.unmount()
  })

  it('ordena la espera de QA del más antiguo al más reciente y marca el plazo', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    const rows = tile(wrapper, 'ready').findAll('.waiting__line')
    expect(rows[0].text()).toContain('frontend-app #77')
    expect(rows[0].text()).toContain('fuera de plazo')
    expect(rows[1].text()).toContain('backend-api #142')
    expect(wrapper.get('[data-start-oldest]').attributes('href')).toBe('/prs/2')
    wrapper.unmount()
  })

  it('arranca en "Para validar" y filtra la cola en el cliente', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.findAll('.pr-card')).toHaveLength(2)
    expect(wrapper.get('[data-tab="ai"]').text()).toContain('1')

    await wrapper.get('[data-tab="ai"]').trigger('click')
    expect(wrapper.findAll('.pr-card')).toHaveLength(1)
    expect(wrapper.text()).toContain('frontend-app #80')

    await wrapper.get('[data-tab="all"]').trigger('click')
    expect(wrapper.findAll('.pr-card')).toHaveLength(4)
    expect(mocks.api.prs).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('reintenta un PR inconcluso y recarga la cola', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    await wrapper.get('[data-retry-pr="4"]').trigger('click')
    await flushPromises()

    expect(mocks.api.retryPR).toHaveBeenCalledWith(4)
    expect(mocks.api.qaSummary).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('explica por qué no puede reintentar quien no es QA ni lead', async () => {
    mocks.api.retryPR.mockRejectedValue(Object.assign(new Error('forbidden'), { status: 403 }))
    const wrapper = mountDashboard()
    await flushPromises()

    await wrapper.get('[data-retry-pr="4"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toBe('Solo QA o el lead del equipo pueden reintentar')
    wrapper.unmount()
  })

  it('no inventa una precisión sin hallazgos triados', async () => {
    mocks.api.qaSummary.mockResolvedValue({ ...summary, confirmed: 0, false_positive: 0, categories: [] })
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.find('[data-precision]').exists()).toBe(false)
    expect(tile(wrapper, 'precision').text()).toContain('Aún no hay hallazgos triados')
    expect(tile(wrapper, 'categories').text()).toContain('Sin defectos confirmados')
    wrapper.unmount()
  })

  it('un evento SSE pr.updated provoca un refetch con debounce', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(mocks.subscribeEvents).toHaveBeenCalledTimes(1)
    mocks.api.qaSummary.mockClear()
    mocks.api.prs.mockClear()

    mocks.handlers.onEvent!({ type: 'pr.updated', id: '42' })

    await new Promise((resolve) => setTimeout(resolve, 350))
    await flushPromises()

    expect(mocks.api.qaSummary).toHaveBeenCalledTimes(1)
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
    mocks.api.me.mockResolvedValue({ id: 2, login: 'dev', avatar_url: '', is_admin: false, teams: [] })
    mocks.api.readiness.mockResolvedValue({
      ready: false,
      items: [{ key: 'no_teams', label: 'No hay equipos', count: 1, link: '/teams' }],
    })

    const wrapper = mountDashboard()
    await flushPromises()

    expect(mocks.api.readiness).not.toHaveBeenCalled()
    expect(wrapper.find('[data-readiness-banner]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps PRs waiting for approval or paused reachable from their own tab', async () => {
    mocks.api.prs.mockResolvedValue([
      ...prs,
      pr({ id: 5, repo: 'payments', number: 12, qa_status: 'awaiting_approval', group: 'blocked' }),
    ])
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.get('[data-tab="other"]').text()).toContain('1')
    await wrapper.get('[data-tab="other"]').trigger('click')
    expect(wrapper.findAll('.pr-card')).toHaveLength(1)
    expect(wrapper.text()).toContain('payments #12')
    wrapper.unmount()
  })

  it('offers the team lead view only to leads and admins', async () => {
    const admin = mountDashboard()
    await flushPromises()
    expect(admin.find('[data-role-lead]').exists()).toBe(true)
    admin.unmount()

    mocks.api.me.mockResolvedValue({
      id: 2,
      login: 'mvega',
      avatar_url: '',
      is_admin: false,
      teams: [{ id: 3, name: 'Pagos', role: 'qa' }],
    })
    const qaOnly = mountDashboard()
    await flushPromises()
    expect(qaOnly.find('[data-role-lead]').exists()).toBe(false)
    qaOnly.unmount()
  })
})
