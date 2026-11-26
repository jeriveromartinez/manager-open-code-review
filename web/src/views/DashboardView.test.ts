import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, type LocationQueryRaw } from 'vue-router'
import DashboardView from './DashboardView.vue'
import { useLiveStore } from '../stores/live'
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
    openEventStream: vi.fn((onEvent: (event: EventPayload) => void, onState: (connected: boolean) => void) => {
      handlers.onEvent = onEvent
      handlers.onState = onState
      return () => {
        handlers.onEvent = null
        handlers.onState = null
      }
    }),
  }
})

vi.mock('../api', () => ({
  api: mocks.api,
  openEventStream: mocks.openEventStream,
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
  tokens: {
    window_days: 7,
    reviews: 0,
    input: 0,
    output: 0,
    cache_read: 0,
    cache_write: 0,
    total: 0,
    average: 0,
    max_context: 0,
    max_context_known: false,
  },
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

let pinia = createPinia()

function createTestRouter() {
  return createRouter({
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
}

function mountDashboard(): VueWrapper {
  pinia = createPinia()
  return mount(DashboardView, { global: { plugins: [pinia, createTestRouter()] } })
}

async function mountDashboardAt(query: LocationQueryRaw): Promise<VueWrapper> {
  const router = createTestRouter()
  await router.push({ path: '/', query })
  await router.isReady()
  pinia = createPinia()
  return mount(DashboardView, { global: { plugins: [pinia, router] } })
}

function tile(wrapper: VueWrapper, key: string) {
  return wrapper.get(`[data-tile="${key}"]`)
}

function cardTitles(wrapper: VueWrapper): string[] {
  return wrapper.findAll('.pr-card__link').map((link) => link.text())
}

async function chooseOption(scope: ParentNode, dropdownLabel: string, optionLabel: string): Promise<void> {
  const pill = Array.from(scope.querySelectorAll<HTMLButtonElement>('.filter-dropdown__pill')).find((button) =>
    button.textContent?.includes(dropdownLabel),
  )
  if (!pill) throw new Error(`dropdown ${dropdownLabel} not found`)
  const dropdown = pill.closest('.filter-dropdown') as HTMLElement

  pill.click()
  await nextTick()
  if (!dropdown.querySelector('[role="listbox"]')) {
    pill.click()
    await nextTick()
  }

  const option = Array.from(dropdown.querySelectorAll<HTMLElement>('[role="option"]')).find((item) =>
    item.textContent?.includes(optionLabel),
  )
  if (!option) throw new Error(`option ${optionLabel} not found`)
  option.click()
  await nextTick()
  await flushPromises()
}

function minutesAgo(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString()
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
    // The shell owns the stream; here it is started by hand.
    useLiveStore(pinia).start()

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

  it('puts the summary and the queue side by side so a wide screen shows both', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    const aside = wrapper.get('.split__aside')
    const main = wrapper.get('.split__main')
    expect(aside.find('[data-tile="ready"]').exists()).toBe(true)
    expect(aside.find('#queue-title').exists()).toBe(false)
    expect(main.find('#queue-title').exists()).toBe(true)
    wrapper.unmount()
  })

  it('names the teams in the queue heading instead of counting repos', async () => {
    mocks.api.me.mockResolvedValue({
      id: 2,
      login: 'mvega',
      avatar_url: '',
      is_admin: false,
      teams: [
        { id: 3, name: 'Pagos', role: 'qa' },
        { id: 4, name: 'Web', role: 'qa' },
      ],
    })
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.get('[data-queue-summary]').text()).toBe('Pagos y Web · 4 PRs abiertos · ordenados por tiempo de espera')
    wrapper.unmount()
  })

  it('does not narrow the heading to an admin own teams, since an admin sees every team', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.get('[data-queue-summary]').text()).toBe(
      'Todos los equipos · 4 PRs abiertos · ordenados por tiempo de espera',
    )
    wrapper.unmount()
  })

  it('keeps the dot figures on screen when there is nothing to count', async () => {
    mocks.api.qaSummary.mockResolvedValue({
      ...summary,
      ready_for_human: 0,
      untriaged: { blocker: 0, major: 0, minor: 0, nit: 0 },
      confirmed: 0,
      false_positive: 0,
      categories: [],
      runs_running: 0,
    })
    mocks.api.prs.mockResolvedValue([])
    const wrapper = mountDashboard()
    await flushPromises()

    const ring = tile(wrapper, 'precision').get('.dot-ring')
    const dots = ring.findAll('.dot')
    expect(dots.length).toBeGreaterThan(0)
    expect(dots.every((dot) => dot.classes().includes('dot--off'))).toBe(true)
    expect(tile(wrapper, 'triage').findAll('.dot-bar')).toHaveLength(4)
    expect(tile(wrapper, 'ready').find('.dot-bar').exists()).toBe(true)
    expect(tile(wrapper, 'categories').find('.dot-bar').exists()).toBe(true)
    wrapper.unmount()
  })

  it('only paints the inconclusive figure orange when there is something to look at', async () => {
    mocks.api.qaSummary.mockResolvedValue({ ...summary, inconclusive: 0 })
    const wrapper = mountDashboard()
    await flushPromises()

    expect(tile(wrapper, 'inconclusive').get('.kpi-card').classes()).toContain('kpi-card--default')
    wrapper.unmount()
  })

  it('draws the agent pulse only while a review runs', async () => {
    const busy = mountDashboard()
    await flushPromises()
    expect(busy.get('[data-agent-pulse]').find('.agent__pulse-live').exists()).toBe(true)
    busy.unmount()

    mocks.api.qaSummary.mockResolvedValue({ ...summary, runs_running: 0 })
    const idle = mountDashboard()
    await flushPromises()
    expect(idle.get('[data-agent-pulse]').find('.agent__pulse-live').exists()).toBe(false)
    idle.unmount()
  })

  it('filters the queue by search text', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    const search = wrapper.get('input[type="search"]')
    await search.setValue('carrito')

    expect(cardTitles(wrapper)).toEqual(['Corrige el carrito'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('1 DE 2')

    await search.setValue('frontend-app')
    expect(cardTitles(wrapper)).toEqual(['Corrige el carrito'])

    await search.setValue('octocat')
    expect(wrapper.findAll('.pr-card')).toHaveLength(2)
    wrapper.unmount()
  })

  it('filters by team and sorts by wait, newest or findings', async () => {
    mocks.api.me.mockResolvedValue({
      id: 1,
      login: 'mvega',
      avatar_url: '',
      is_admin: true,
      teams: [
        { id: 3, name: 'Pagos', role: 'qa' },
        { id: 4, name: 'Web', role: 'qa' },
      ],
    })
    mocks.api.prs.mockResolvedValue([
      pr({ id: 11, repo: 'api', title: 'Alpha', team_id: 3, status_since: minutesAgo(30) }),
      pr({
        id: 12,
        repo: 'web',
        title: 'Beta',
        team_id: 4,
        status_since: minutesAgo(120),
        open_findings: { blocker: 0, major: 0, minor: 1, nit: 0 },
      }),
      pr({
        id: 13,
        repo: 'infra',
        title: 'Gamma',
        team_id: 4,
        status_since: minutesAgo(10),
        open_findings: { blocker: 0, major: 5, minor: 0, nit: 0 },
      }),
      pr({
        id: 14,
        repo: 'etl',
        title: 'Delta',
        team_id: 3,
        qa_status: 'changes_requested',
        group: 'blocked',
        status_since: minutesAgo(60),
        open_findings: { blocker: 3, major: 0, minor: 0, nit: 0 },
      }),
    ])

    const wrapper = mountDashboard()
    await flushPromises()

    expect(cardTitles(wrapper)).toEqual(['Beta', 'Alpha', 'Gamma'])

    await chooseOption(wrapper.element, 'Equipo', 'Web')
    expect(cardTitles(wrapper)).toEqual(['Beta', 'Gamma'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('2 DE 3')

    await chooseOption(wrapper.element, 'Orden', 'Más recientes')
    expect(cardTitles(wrapper)).toEqual(['Gamma', 'Beta'])

    await chooseOption(wrapper.element, 'Orden', 'Más hallazgos')
    expect(cardTitles(wrapper)).toEqual(['Gamma', 'Beta'])

    await chooseOption(wrapper.element, 'Equipo', 'Todas')
    await wrapper.get('[data-tab="all"]').trigger('click')
    expect(wrapper.findAll('.pr-card')).toHaveLength(4)

    await chooseOption(wrapper.element, 'Orden', 'Más espera')
    expect(cardTitles(wrapper)).toEqual(['Beta', 'Delta', 'Alpha', 'Gamma'])

    await chooseOption(wrapper.element, 'Orden', 'Más hallazgos')
    expect(cardTitles(wrapper)).toEqual(['Gamma', 'Delta', 'Beta', 'Alpha'])

    await chooseOption(wrapper.element, 'Orden', 'Más recientes')
    expect(cardTitles(wrapper)).toEqual(['Gamma', 'Alpha', 'Delta', 'Beta'])
    wrapper.unmount()
  })

  it('does not treat a non-default sort as a filter', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    await chooseOption(wrapper.element, 'Orden', 'Más recientes')

    expect(cardTitles(wrapper)).toEqual(['Añade endpoint de pedidos', 'Corrige el carrito'])
    const count = wrapper.get('.filter-bar__count')
    expect(count.text()).toBe('2 DE 2')
    expect(count.classes()).not.toContain('filter-bar__count--active')
    expect(wrapper.get('.filter-bar__clear').attributes('disabled')).toBeDefined()
    expect(wrapper.get('.filter-bar__sheet').text()).toBe('FILTROS · 0')
    expect(wrapper.findAll('.filter-bar__chip')).toHaveLength(0)
    wrapper.unmount()
  })

  it('hydrates q, team and sort from the URL query', async () => {
    mocks.api.me.mockResolvedValue({
      id: 1,
      login: 'mvega',
      avatar_url: '',
      is_admin: true,
      teams: [
        { id: 3, name: 'Pagos', role: 'qa' },
        { id: 4, name: 'Web', role: 'qa' },
      ],
    })
    mocks.api.prs.mockResolvedValue([
      pr({ id: 21, repo: 'api', title: 'Corrige pedidos', team_id: 3, status_since: minutesAgo(20) }),
      pr({ id: 22, repo: 'web', title: 'Corrige carrito', team_id: 4, status_since: minutesAgo(100) }),
      pr({ id: 23, repo: 'web', title: 'Corrige tabla', team_id: 4, status_since: minutesAgo(5) }),
      pr({ id: 24, repo: 'web', title: 'Migra tabla', team_id: 4, status_since: minutesAgo(50) }),
    ])

    const wrapper = await mountDashboardAt({ q: 'corrige', team: '4', sort: 'newest' })
    await flushPromises()

    expect((wrapper.get('input[type="search"]').element as HTMLInputElement).value).toBe('corrige')
    expect(cardTitles(wrapper)).toEqual(['Corrige tabla', 'Corrige carrito'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('2 DE 4')
    expect(wrapper.get('.filter-bar__chip-label').text()).toBe('Web')
    expect(wrapper.get('.filter-bar__filters').text()).toContain('Más recientes')
    expect(wrapper.get('.filter-bar__clear').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('updates the status tab counts with the other filters', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    expect(wrapper.findAll('.filter-tab__count').map((count) => count.text())).toEqual(['2', '0', '1', '1', '0', '4'])

    await wrapper.get('input[type="search"]').setValue('carrito')

    expect(wrapper.findAll('.filter-tab__count').map((count) => count.text())).toEqual(['1', '0', '0', '0', '0', '1'])
    expect(wrapper.get('[data-tab="ready"]').classes()).toContain('filter-tab--active')

    await wrapper.get('[data-tab="all"]').trigger('click')
    expect(cardTitles(wrapper)).toEqual(['Corrige el carrito'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('1 DE 4')
    wrapper.unmount()
  })

  it('opens the mobile sheet and applies the same filters', async () => {
    mocks.api.me.mockResolvedValue({
      id: 1,
      login: 'mvega',
      avatar_url: '',
      is_admin: true,
      teams: [
        { id: 3, name: 'Pagos', role: 'qa' },
        { id: 4, name: 'Web', role: 'qa' },
      ],
    })
    mocks.api.prs.mockResolvedValue([
      pr({ id: 11, repo: 'api', title: 'Alpha', team_id: 3 }),
      pr({ id: 12, repo: 'web', title: 'Beta', team_id: 4, status_since: minutesAgo(90) }),
      pr({ id: 13, repo: 'infra', title: 'Gamma', team_id: 4, status_since: minutesAgo(5) }),
    ])

    const wrapper = mountDashboard()
    await flushPromises()

    await wrapper.get('.filter-bar__sheet').trigger('click')
    await flushPromises()

    const sheet = document.body.querySelector('.filter-sheet') as HTMLElement
    expect(sheet).not.toBeNull()
    expect(wrapper.get('.filter-bar__sheet').attributes('aria-expanded')).toBe('true')

    await chooseOption(sheet, 'Equipo', 'Web')
    expect(cardTitles(wrapper)).toEqual(['Beta', 'Gamma'])

    const apply = document.body.querySelector('.filter-sheet__apply') as HTMLElement
    expect(apply.textContent?.trim()).toBe('VER 2 PRs')
    apply.click()
    await flushPromises()

    expect(document.body.querySelector('.filter-sheet')).toBeNull()
    expect(wrapper.get('.filter-bar__sheet').attributes('aria-expanded')).toBe('false')
    expect(cardTitles(wrapper)).toEqual(['Beta', 'Gamma'])
    wrapper.unmount()
  })

  it('shows the filters empty state with LIMPIAR FILTROS', async () => {
    const wrapper = mountDashboard()
    await flushPromises()

    await wrapper.get('input[type="search"]').setValue('ninguno-de-estos')

    expect(wrapper.findAll('.pr-card')).toHaveLength(0)
    const empty = wrapper.get('.empty')
    expect(empty.get('.empty__title').text()).toBe('NADA COINCIDE CON ESTOS FILTROS')
    expect(empty.get('button').text()).toBe('LIMPIAR FILTROS')

    await empty.get('button').trigger('click')
    expect(wrapper.findAll('.pr-card')).toHaveLength(2)
    expect(wrapper.get('.filter-bar__count').text()).toBe('2 DE 2')
    wrapper.unmount()
  })
})
