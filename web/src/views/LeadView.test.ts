import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import LeadView from './LeadView.vue'
import type { PRView, QAPolicy, QASummary, Repository } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    me: vi.fn(),
    teams: vi.fn(),
    prs: vi.fn(),
    qaSummary: vi.fn(),
    getPolicy: vi.fn(),
    repos: vi.fn(),
  },
}))

vi.mock('../api', () => ({ api: mocks.api }))

const hoursAgo = (hours: number) => new Date(Date.now() - hours * 3600_000).toISOString()

function pr(overrides: Partial<PRView>): PRView {
  return {
    id: 1,
    repo: 'acme/payments-api',
    number: 1,
    title: 'PR',
    author_login: 'octocat',
    html_url: 'https://github.com/acme/payments-api/pull/1',
    qa_status: 'ready_for_human',
    group: 'ready',
    review_state: '',
    review_reason: '',
    validations: { passed: 0, total: 0 },
    open_findings: { blocker: 0, major: 0, minor: 0, nit: 0 },
    status_since: hoursAgo(1),
    team_id: 3,
    updated_at: hoursAgo(1),
    ...overrides,
  }
}

const prs: PRView[] = [
  pr({ id: 1, number: 482, title: 'Idempotencia de cobros', status_since: hoursAgo(6) }),
  pr({ id: 2, number: 485, title: 'Límite de reembolsos' }),
  pr({
    id: 3,
    number: 479,
    title: 'Rotar claves',
    qa_status: 'changes_requested',
    group: 'blocked',
    open_findings: { blocker: 1, major: 2, minor: 0, nit: 0 },
    status_since: hoursAgo(2),
  }),
  pr({
    id: 4,
    number: 490,
    title: 'Exportar CSV',
    qa_status: 'reviewing',
    group: 'in_review',
    open_findings: { blocker: 0, major: 1, minor: 3, nit: 0 },
  }),
  pr({ id: 9, repo: 'acme/web-portal', number: 1207, title: 'De otro equipo', team_id: 4 }),
]

const summary: QASummary = {
  ready_for_human: 2,
  inconclusive: 0,
  runs_running: 1,
  runs_queued: 0,
  untriaged: { blocker: 0, major: 0, minor: 0, nit: 0 },
  confirmed: 12,
  false_positive: 1,
  precision_window_days: 7,
  categories: [],
  confirmed_by_repo: [
    { repo: 'acme/payments-api', count: 7 },
    { repo: 'acme/payments-sdk', count: 2 },
  ],
  category_window_days: 30,
  validation_sla_seconds: 4 * 3600,
}

const policy: QAPolicy = {
  id: 1,
  team_id: 3,
  llm_profile_id: 1,
  blocking_threshold: 'major',
  extra_instructions: '',
  exclude_globs: [],
  max_files: 40,
  max_diff_lines: 3000,
  max_passes: 3,
  require_green_ci: true,
  updated_at: hoursAgo(48),
}

function repo(overrides: Partial<Repository>): Repository {
  return {
    id: 1,
    installation_id: 1,
    full_name: 'acme/payments-api',
    private: true,
    default_branch: 'main',
    enabled: true,
    team_id: 3,
    has_policy: true,
    account_login: 'acme',
    ...overrides,
  }
}

const repos: Repository[] = [
  repo({ id: 1 }),
  repo({ id: 2, full_name: 'acme/payments-sdk', enabled: false }),
  repo({ id: 3, full_name: 'acme/web-portal', team_id: 4 }),
]

function mountLead(): VueWrapper {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/lead', component: LeadView },
      { path: '/prs/:id', component: { template: '<div />' } },
      { path: '/agents', component: { template: '<div />' } },
    ],
  })
  void router.push('/lead')
  return mount(LeadView, { global: { plugins: [createPinia(), router] } })
}

describe('LeadView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.me.mockResolvedValue({
      id: 7,
      login: 'lmartin',
      avatar_url: '',
      is_admin: false,
      teams: [
        { id: 3, name: 'Pagos', role: 'lead' },
        { id: 5, name: 'Plataforma', role: 'qa' },
      ],
    })
    mocks.api.prs.mockResolvedValue(prs)
    mocks.api.qaSummary.mockResolvedValue(summary)
    mocks.api.getPolicy.mockResolvedValue(policy)
    mocks.api.repos.mockResolvedValue(repos)
  })

  it('shows only the led team and its figures', async () => {
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.get('.page-title').text()).toBe('Equipo Pagos')
    expect(wrapper.find('[data-team]').exists()).toBe(false)
    expect(mocks.api.qaSummary).toHaveBeenCalledWith(3)
    expect(mocks.api.getPolicy).toHaveBeenCalledWith(3)
    expect(wrapper.text()).not.toContain('De otro equipo')
    expect(wrapper.get('[data-blocked-count]').text()).toBe('1')
    expect(wrapper.get('[data-late-count]').text()).toBe('1')
    expect(wrapper.get('[data-precision]').text()).toBe('92%')
    // The repo tile counts its own 30-day window, not the 7-day precision figure.
    expect(wrapper.get('[data-tile="repos-defects"] .kpi-card__value').text()).toBe('9')
    expect(wrapper.get('[data-tile="repos-defects"]').text()).toContain('acme/payments-sdk')
    expect(wrapper.get('[data-repo="acme/payments-sdk"]').text()).toContain('Deshabilitado')
    wrapper.unmount()
  })

  it('simulates what each blocking threshold would do to the open PRs', async () => {
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.get('[data-threshold="blocker"]').text()).toBe('Bloqueante1')
    expect(wrapper.get('[data-threshold="major"]').text()).toBe('Mayor (actual)2')
    expect(wrapper.get('[data-threshold="minor"]').text()).toBe('Menor2')
    expect(wrapper.get('[data-threshold="nit"]').text()).toBe('Nit2')
    expect(wrapper.find('[data-tile="policy"] a').exists()).toBe(false)
    wrapper.unmount()
  })

  it('lists the PRs that need a decision, longest wait first', async () => {
    const wrapper = mountLead()
    await flushPromises()

    const rows = wrapper.findAll('[data-decision]')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('Idempotencia de cobros')
    expect(rows[0].text()).toContain('Fuera de plazo de QA')
    expect(rows[1].text()).toContain('Con defectos')
    wrapper.unmount()
  })

  it('switches between the teams a person leads', async () => {
    mocks.api.me.mockResolvedValue({
      id: 7,
      login: 'lmartin',
      avatar_url: '',
      is_admin: false,
      teams: [
        { id: 3, name: 'Pagos', role: 'lead' },
        { id: 4, name: 'Web', role: 'lead' },
      ],
    })
    const wrapper = mountLead()
    await flushPromises()

    await wrapper.get('[data-team="4"]').trigger('click')
    await flushPromises()

    expect(mocks.api.qaSummary).toHaveBeenLastCalledWith(4)
    expect(wrapper.get('.page-title').text()).toBe('Equipo Web')
    expect(wrapper.get('.page-subtitle').text()).toContain('1 PRs abiertos · 1 repositorio')
    expect(wrapper.find('[data-repo="acme/web-portal"]').exists()).toBe(true)
    expect(wrapper.find('[data-repo="acme/payments-api"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('says so when the team has no policy', async () => {
    mocks.api.getPolicy.mockResolvedValue({ ...policy, id: 0 })
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.find('[data-no-policy]').exists()).toBe(true)
    expect(wrapper.get('[data-tile="policy"]').classes()).toContain('tile--alert')
    wrapper.unmount()
  })

  it('lets an admin pick any team and reach the policy editor', async () => {
    mocks.api.me.mockResolvedValue({ id: 1, login: 'admin', avatar_url: '', is_admin: true, teams: [] })
    mocks.api.teams.mockResolvedValue([
      { id: 3, name: 'Pagos', slug: 'pagos', members: [] },
      { id: 4, name: 'Web', slug: 'web', members: [] },
    ])
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.findAll('[data-team]')).toHaveLength(2)
    expect(wrapper.get('[data-tile="policy"] a').attributes('href')).toBe('/agents')
    wrapper.unmount()
  })

  it('explains the page to someone who leads no team', async () => {
    mocks.api.me.mockResolvedValue({
      id: 8,
      login: 'mvega',
      avatar_url: '',
      is_admin: false,
      teams: [{ id: 3, name: 'Pagos', role: 'qa' }],
    })
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.find('[data-no-teams]').exists()).toBe(true)
    expect(mocks.api.qaSummary).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('shows that it is loading instead of a blank page', async () => {
    let release: (value: QAPolicy) => void = () => {}
    mocks.api.getPolicy.mockImplementation(
      () =>
        new Promise<QAPolicy>((resolve) => {
          release = resolve
        }),
    )
    const wrapper = mountLead()
    await flushPromises()

    expect(wrapper.get('[role="status"]').text()).toContain('Cargando')
    expect(wrapper.find('[data-tile="state"]').exists()).toBe(false)

    release(policy)
    await flushPromises()
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(wrapper.find('[data-tile="state"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('ignores a slow answer for a team that is no longer selected', async () => {
    mocks.api.me.mockResolvedValue({
      id: 7,
      login: 'lmartin',
      avatar_url: '',
      is_admin: false,
      teams: [
        { id: 3, name: 'Pagos', role: 'lead' },
        { id: 4, name: 'Web', role: 'lead' },
      ],
    })
    const wrapper = mountLead()
    await flushPromises()

    let releasePagos: (value: QASummary) => void = () => {}
    mocks.api.qaSummary.mockImplementation((id: number) =>
      id === 3
        ? new Promise<QASummary>((resolve) => {
            releasePagos = resolve
          })
        : Promise.resolve({ ...summary, confirmed: 1, false_positive: 0 }),
    )

    await wrapper.get('[data-team="3"]').trigger('click')
    await wrapper.get('[data-team="4"]').trigger('click')
    await flushPromises()
    releasePagos({ ...summary, confirmed: 50, false_positive: 50 })
    await flushPromises()

    expect(wrapper.get('.page-title').text()).toBe('Equipo Web')
    expect(wrapper.get('[data-precision]').text()).toBe('100%')
    wrapper.unmount()
  })

  it('drops the previous team data when a load fails', async () => {
    mocks.api.me.mockResolvedValue({
      id: 7,
      login: 'lmartin',
      avatar_url: '',
      is_admin: false,
      teams: [
        { id: 3, name: 'Pagos', role: 'lead' },
        { id: 4, name: 'Web', role: 'lead' },
      ],
    })
    const wrapper = mountLead()
    await flushPromises()

    mocks.api.getPolicy.mockRejectedValue(new Error('boom'))
    await wrapper.get('[data-team="4"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toBe('No se pudo cargar el equipo')
    expect(wrapper.get('.page-subtitle').text()).toContain('0 PRs abiertos · 0 repositorios')
    wrapper.unmount()
  })
})
