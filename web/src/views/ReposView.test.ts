import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import ReposView from './ReposView.vue'
import type { Me, QAPolicy, Repository, RepositoryPolicy, Team } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    repos: vi.fn(),
    teams: vi.fn(),
    patchRepo: vi.fn(),
    installations: vi.fn(),
    syncInstallation: vi.fn(),
    setupStatus: vi.fn(),
    getRepoPolicy: vi.fn(),
    saveRepoPolicy: vi.fn(),
    clearRepoPolicy: vi.fn(),
    getPolicy: vi.fn(),
    savePolicy: vi.fn(),
  },
  session: {
    me: null as Me | null,
    load: vi.fn(),
  },
}))

vi.mock('../api', () => ({ api: mocks.api }))
type LiveRegistration = { types: string[]; reload: () => unknown; filter?: (event: { type: string; id: string }) => boolean }
const live = vi.hoisted(() => ({ registrations: [] as LiveRegistration[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: () => unknown, filter?: LiveRegistration['filter']) => {
    live.registrations.push({ types, reload, filter })
  },
}))

vi.mock('../stores/session', () => ({
  useSessionStore: () => mocks.session,
}))

const repos = [
  {
    id: 10,
    installation_id: 1,
    full_name: 'acme/backend-api',
    private: true,
    default_branch: 'main',
    enabled: false,
    team_id: null,
    has_policy: false,
    has_policy_override: false,
    policy_observation: false,
    account_login: 'acme',
  },
  {
    id: 20,
    installation_id: 1,
    full_name: 'acme/frontend-app',
    private: false,
    default_branch: 'main',
    enabled: true,
    team_id: 1,
    has_policy: true,
    has_policy_override: true,
    policy_observation: false,
    account_login: 'personal',
  },
] as Repository[]

const teams: Team[] = [
  { id: 1, name: 'T1', slug: 't1', repository_count: 0, members: [] },
  { id: 2, name: 'T2', slug: 't2', repository_count: 0, members: [] },
]

function basePolicy(overrides: Partial<QAPolicy> = {}): QAPolicy {
  return {
    id: 3,
    team_id: 1,
    llm_profile_id: 7,
    blocking_threshold: 'major',
    extra_instructions: '',
    exclude_globs: [],
    max_files: 40,
    max_diff_lines: 3000,
    max_passes: 4,
    require_green_ci: false,
    required_checks: [],
    review_context_ceiling: null,
    global_context_ceiling: 270000,
    effective_context_ceiling: 270000,
    review_effort: 'low',
    review_token_budget: 0,
    auto_triage_mode: 'off',
    auto_withdraw: false,
    verify_max_findings: 50,
    verify_max_files: 20,
    verify_token_share: 20,
    verify_time_share: 15,
    verify_token_max: 100000,
    allow_external_reviews: false,
    external_trusted_associations: ['OWNER', 'MEMBER', 'COLLABORATOR'],
    max_external_diff_lines: 3000,
    max_external_runs_per_day: 10,
    external_daily_token_budget: 0,
    architecture_analysis_mode: 'observation',
    architecture_token_share: 25,
    architecture_time_share: 20,
    architecture_token_max: 100000,
    architecture_baseline_path: '.architecture/baseline.json',
    architecture_exception_policy: 'deny',
    consumer_allow_list: [],
    consumer_baseline_path: '',
    consumer_max_lookups: 8,
    consumer_max_bytes: 1048576,
    prompt_cache: false,
    revision: 'team-rev',
    updated_at: '2026-11-19T10:00:00Z',
    ...overrides,
  }
}

function repoPolicyResponse(overrides: Partial<RepositoryPolicy> = {}): RepositoryPolicy {
  const effective = basePolicy()
  return {
    repository_id: 20,
    team_id: 1,
    inherited: true,
    revision: 'team-rev',
    provenance: {},
    overrides: {
      observation_mode: false,
      blocking_threshold: null,
      extra_instructions: null,
      exclude_globs: null,
      max_files: null,
      max_diff_lines: null,
      max_passes: null,
      require_green_ci: null,
      required_checks: null,
      review_context_ceiling: null,
      review_effort: null,
      review_token_budget: null,
      auto_triage_mode: null,
      auto_withdraw: null,
      verify_max_findings: null,
      verify_max_files: null,
      verify_token_share: null,
      verify_time_share: null,
      verify_token_max: null,
    },
    effective,
    enforced: effective,
    updated_at: null,
    ...overrides,
  }
}

const admin: Me = {
  id: 1,
  login: 'admin',
  avatar_url: '',
  is_admin: true,
  teams: [],
}

const developer: Me = {
  id: 2,
  login: 'dev',
  avatar_url: '',
  is_admin: false,
  teams: [],
}

function mountRepos(): VueWrapper {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } },
    ],
  })
  return mount(ReposView, { global: { plugins: [router] } })
}

async function chooseOption(
  scope: ParentNode,
  dropdownLabel: string,
  optionLabel: string,
): Promise<void> {
  const pill = Array.from(scope.querySelectorAll<HTMLButtonElement>('.filter-dropdown__pill')).find(
    (button) => button.textContent?.includes(dropdownLabel),
  )
  if (!pill) throw new Error(`dropdown ${dropdownLabel} not found`)
  const dropdown = pill.closest('.filter-dropdown') as HTMLElement
  if (!dropdown.querySelector('.filter-dropdown__list')) {
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

function rowIds(wrapper: VueWrapper): string[] {
  return wrapper.findAll('[data-repo-row]').map((row) => String(row.attributes('data-repo-row')))
}

const filterRepos: Repository[] = [
  {
    ...repos[0],
    id: 10,
    full_name: 'acme/backend-api',
    private: true,
    enabled: false,
    team_id: null,
    has_policy: false,
    has_policy_override: false,
    policy_observation: false,
    account_login: 'acme',
  },
  {
    ...repos[1],
    id: 20,
    full_name: 'acme/frontend-app',
    private: false,
    enabled: true,
    team_id: 1,
    has_policy: true,
    has_policy_override: true,
    policy_observation: false,
    account_login: 'personal',
  },
  {
    ...repos[1],
    id: 30,
    full_name: 'acme/infra-api',
    private: true,
    enabled: true,
    team_id: 2,
    has_policy: true,
    has_policy_override: false,
    policy_observation: true,
    account_login: 'acme',
  },
]

function toggle(wrapper: VueWrapper, id: number): HTMLInputElement {
  return wrapper.get(`input[data-repo-enabled="${id}"]`).element as HTMLInputElement
}

describe('ReposView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.repos.mockResolvedValue(repos.map((repo) => ({ ...repo })))
    mocks.api.teams.mockResolvedValue(teams.map((team) => ({ ...team })))
    mocks.api.patchRepo.mockResolvedValue(undefined)
    mocks.api.installations.mockResolvedValue([])
    mocks.api.syncInstallation.mockResolvedValue({ status: 'queued' })
    mocks.api.setupStatus.mockResolvedValue({ configured: true, install_url: '' })
    mocks.api.getRepoPolicy.mockResolvedValue(repoPolicyResponse())
    mocks.api.saveRepoPolicy.mockResolvedValue(repoPolicyResponse())
    mocks.api.clearRepoPolicy.mockResolvedValue(repoPolicyResponse())
    mocks.api.getPolicy.mockResolvedValue(basePolicy())
    mocks.api.savePolicy.mockResolvedValue(basePolicy())
    mocks.session.load.mockResolvedValue(null)
    live.registrations.length = 0
  })

  it('refreshes the registry quietly when a repo or a team changes', async () => {
    mocks.session.me = admin
    const wrapper = mountRepos()
    await flushPromises()

    const [registration] = live.registrations
    expect(registration.types).toEqual(['repo.updated', 'team.updated'])
    mocks.api.repos.mockResolvedValue([{ ...repos[0], full_name: 'acme/renamed' }])
    await registration.reload()
    await flushPromises()

    expect(wrapper.text()).toContain('acme/renamed')
    wrapper.unmount()
  })

  it('como admin, activar el interruptor llama a patchRepo(id, {enabled:true}) y refleja el cambio', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.text()).toContain('acme/backend-api')
    expect(wrapper.text()).toContain('Privado')
    expect(wrapper.text()).toContain('Público')

    const control = wrapper.get('input[data-repo-enabled="10"]')
    expect(toggle(wrapper, 10).disabled).toBe(false)

    await control.setValue(true)
    await flushPromises()

    expect(mocks.api.patchRepo).toHaveBeenCalledWith(10, { enabled: true })
    expect(toggle(wrapper, 10).checked).toBe(true)
    expect(wrapper.get('[data-repo-row="10"]').text()).toContain('Activada')
    wrapper.unmount()
  })

  it('puts connected accounts in a side column for admins and the register alone otherwise', async () => {
    mocks.session.me = admin
    const asAdmin = mountRepos()
    await flushPromises()
    expect(asAdmin.get('.split__aside').find('[aria-label="Cuentas conectadas"]').exists()).toBe(true)
    expect(asAdmin.get('.split__main').find('.repo-table').exists()).toBe(true)
    asAdmin.unmount()

    mocks.session.me = developer
    const asDeveloper = mountRepos()
    await flushPromises()
    expect(asDeveloper.find('.split__aside').exists()).toBe(false)
    expect(asDeveloper.get('.split__main').find('.repo-table').exists()).toBe(true)
    asDeveloper.unmount()
  })

  it('como developer, el interruptor está deshabilitado (solo lectura)', async () => {
    mocks.session.me = developer

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.text()).toContain('acme/backend-api')
    expect(toggle(wrapper, 10).disabled).toBe(true)
    expect(toggle(wrapper, 20).disabled).toBe(true)
    expect(wrapper.find('select[data-repo-team="10"]').exists()).toBe(false)
    expect(wrapper.get('[data-repo-row="10"]').text()).toContain('Sin equipo')
    wrapper.unmount()
  })

  it('como admin, cambiar el equipo llama a patchRepo(id, {team_id})', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    const select = wrapper.get('select[data-repo-team="10"]')
    await select.setValue('2')
    await flushPromises()

    expect(mocks.api.patchRepo).toHaveBeenCalledWith(10, { team_id: 2 })
    wrapper.unmount()
  })

  it('muestra un estado vacío en español cuando no hay repositorios', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.text()).toContain('No hay repositorios disponibles')
    wrapper.unmount()
  })

  it('muestra un error en español cuando falla la carga', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockRejectedValue(new Error('falló la red'))

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudieron cargar los repositorios')
    wrapper.unmount()
  })

  it('admin ve cuentas conectadas y el botón Añadir cuenta', async () => {
    mocks.session.me = admin
    mocks.api.installations.mockResolvedValue([
      { id: 1, account_login: 'acme', account_type: 'Organization', suspended_at: null },
      { id: 2, account_login: 'personal', account_type: 'User', suspended_at: null },
    ])
    mocks.api.setupStatus.mockResolvedValue({
      configured: true,
      install_url: 'https://github.com/apps/x/installations/new',
    })

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.text()).toContain('Cuentas conectadas')
    expect(wrapper.text()).toContain('acme')
    expect(wrapper.text()).toContain('personal')
    const link = wrapper.get('a[href="https://github.com/apps/x/installations/new"]')
    expect(link.text()).toContain('Añadir cuenta')
    expect(link.attributes('target')).toBe('_blank')
    wrapper.unmount()
  })

  it('admin sin cuentas conectadas ve un mensaje en la tarjeta de cuentas', async () => {
    mocks.session.me = admin
    mocks.api.installations.mockResolvedValue([])
    mocks.api.setupStatus.mockResolvedValue({
      configured: true,
      install_url: 'https://github.com/apps/x/installations/new',
    })

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.find('.accounts').text()).toContain('Todavía no hay cuentas conectadas')
    expect(wrapper.text()).toContain('Añadir cuenta')
    wrapper.unmount()
  })

  it('developer no ve la sección de cuentas ni Añadir cuenta, pero ve la columna Cuenta', async () => {
    mocks.session.me = developer

    const wrapper = mountRepos()
    await flushPromises()

    expect(mocks.api.installations).not.toHaveBeenCalled()
    expect(wrapper.text()).not.toContain('Cuentas conectadas')
    expect(wrapper.text()).not.toContain('Añadir cuenta')
    expect(wrapper.text()).toContain('Cuenta')
    expect(wrapper.text()).toContain('acme')
    expect(wrapper.text()).toContain('personal')
    wrapper.unmount()
  })

  it('avisa en la fila cuando un repositorio habilitado no tiene equipo', async () => {
    mocks.session.me = developer
    mocks.api.repos.mockResolvedValue([{ ...repos[0], enabled: true, team_id: null }])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.get('[data-repo-warning="10"]').text()).toBe(
      'Habilitado sin equipo: no se revisará',
    )
    wrapper.unmount()
  })

  it('al activar un repositorio sin equipo muestra el aviso de que no se revisará', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.get('[data-repo-hint="10"]').text()).toContain('Sin equipo')

    const control = wrapper.get('input[data-repo-enabled="10"]')
    await control.setValue(true)
    await flushPromises()

    expect(mocks.api.patchRepo).toHaveBeenCalledWith(10, { enabled: true })
    expect(wrapper.get('[data-repo-row="10"]').text()).toContain(
      'Habilitado sin equipo: no se revisará',
    )
    wrapper.unmount()
  })

  it('avisa en la fila cuando el equipo del repositorio no tiene política', async () => {
    mocks.session.me = developer
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy: false }])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.get('[data-repo-policy-warning="20"]').text()).toBe(
      'El equipo no tiene política: no se revisará',
    )
    wrapper.unmount()
  })

  it('avisa antes de activar un repositorio cuyo equipo no tiene política', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], enabled: false, has_policy: false }])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.get('[data-repo-policy-hint="20"]').text()).toBe(
      'El equipo no tiene política: activar no iniciará revisiones',
    )
    wrapper.unmount()
  })

  it('Sincronizar una cuenta llama a syncInstallation con su id y muestra el aviso', async () => {
    mocks.session.me = admin
    mocks.api.installations.mockResolvedValue([
      { id: 1, account_login: 'acme', account_type: 'Organization', suspended_at: null },
      { id: 2, account_login: 'personal', account_type: 'User', suspended_at: null },
    ])

    let resolveSync!: (value: { status: string }) => void
    mocks.api.syncInstallation.mockReturnValue(
      new Promise<{ status: string }>((resolve) => {
        resolveSync = resolve
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()

    const button = wrapper.get('[data-sync-installation="2"]')
    await button.trigger('click')

    expect(mocks.api.syncInstallation).toHaveBeenCalledWith(2)
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toContain('Sincronizando')

    resolveSync({ status: 'queued' })
    await flushPromises()

    expect(wrapper.text()).toContain(
      'Sincronización encolada. Los repositorios aparecerán en unos segundos.',
    )
    expect(button.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('Sincronizar todo llama a syncInstallation una vez por instalación', async () => {
    mocks.session.me = admin
    mocks.api.installations.mockResolvedValue([
      { id: 1, account_login: 'acme', account_type: 'Organization', suspended_at: null },
      { id: 2, account_login: 'personal', account_type: 'User', suspended_at: null },
    ])

    const wrapper = mountRepos()
    await flushPromises()

    await wrapper.get('[data-sync-all]').trigger('click')
    await flushPromises()

    expect(mocks.api.syncInstallation).toHaveBeenCalledTimes(2)
    expect(mocks.api.syncInstallation).toHaveBeenNthCalledWith(1, 1)
    expect(mocks.api.syncInstallation).toHaveBeenNthCalledWith(2, 2)
    expect(wrapper.text()).toContain(
      'Sincronización encolada. Los repositorios aparecerán en unos segundos.',
    )
    wrapper.unmount()
  })

  it('Actualizar recarga los repositorios sin refrescar la página', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    expect(mocks.api.repos).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-refresh-repos]').trigger('click')
    await flushPromises()

    expect(mocks.api.repos).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('si falla la sincronización muestra el error y vuelve a habilitar el botón', async () => {
    mocks.session.me = admin
    mocks.api.installations.mockResolvedValue([
      { id: 1, account_login: 'acme', account_type: 'Organization', suspended_at: null },
    ])
    mocks.api.syncInstallation.mockRejectedValue(new Error('falló la red'))

    const wrapper = mountRepos()
    await flushPromises()

    const button = wrapper.get('[data-sync-installation="1"]')
    await button.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudo encolar la sincronización.')
    expect(button.attributes('disabled')).toBeUndefined()
    expect(button.text()).toContain('Sincronizar')
    wrapper.unmount()
  })

  it('muestra la política heredada y abre el editor con la revisión efectiva', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.get('[data-repo-policy-state="20"]').text()).toBe('Heredada')
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    expect(mocks.api.getRepoPolicy).toHaveBeenCalledWith(20)
    expect(wrapper.get('[data-repo-policy-revision]').text()).toBe('team-rev')
    expect(
      (wrapper.get('[data-policy-override="blocking_threshold"]').element as HTMLInputElement).checked,
    ).toBe(false)
    wrapper.unmount()
  })

  it('guarda solo los campos sobrescritos y refleja la política propia', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    mocks.api.saveRepoPolicy.mockResolvedValue(
      repoPolicyResponse({
        inherited: false,
        revision: 'repo-rev',
        overrides: {
          ...repoPolicyResponse().overrides,
          blocking_threshold: 'blocker',
          max_files: 10,
        },
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-policy-override="blocking_threshold"]').setValue(true)
    await wrapper.get('[data-policy-field="blocking_threshold"]').setValue('blocker')
    await wrapper.get('[data-policy-override="max_files"]').setValue(true)
    await wrapper.get('[data-policy-field="max_files"]').setValue('10')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.saveRepoPolicy).toHaveBeenCalledWith(20, {
      blocking_threshold: 'blocker',
      max_files: 10,
    })
    expect(wrapper.get('[data-repo-policy-state="20"]').text()).toBe('Propia')
    wrapper.unmount()
  })

  it('activa el modo observación al guardar y lo muestra en la fila', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    mocks.api.saveRepoPolicy.mockResolvedValue(
      repoPolicyResponse({
        inherited: false,
        overrides: {
          ...repoPolicyResponse().overrides,
          observation_mode: true,
          blocking_threshold: 'blocker',
        },
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-repo-policy-observation-mode]').setValue(true)
    await wrapper.get('[data-policy-override="blocking_threshold"]').setValue(true)
    await wrapper.get('[data-policy-field="blocking_threshold"]').setValue('blocker')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.saveRepoPolicy).toHaveBeenCalledWith(20, {
      observation_mode: true,
      blocking_threshold: 'blocker',
    })
    expect(wrapper.get('[data-repo-policy-observation="20"]').text()).toBe('Observación')
    wrapper.unmount()
  })

  it('volver a herencia borra el override y actualiza la fila', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: true }])
    mocks.api.clearRepoPolicy.mockResolvedValue(repoPolicyResponse())

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-repo-policy-clear]').trigger('click')
    await flushPromises()

    expect(mocks.api.clearRepoPolicy).toHaveBeenCalledWith(20)
    expect(wrapper.get('[data-repo-policy-state="20"]').text()).toBe('Heredada')
    expect(wrapper.text()).toContain('Política heredada del equipo')
    wrapper.unmount()
  })

  it('un developer no puede editar la política del repositorio', async () => {
    mocks.session.me = developer

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.find('[data-repo-policy="20"]').exists()).toBe(false)
    expect(wrapper.get('[data-repo-policy-state="20"]').text()).toBe('Propia')
    wrapper.unmount()
  })

  it('conserva extra_instructions al editar otro campo del override', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: true }])
    mocks.api.getRepoPolicy.mockResolvedValue(
      repoPolicyResponse({
        inherited: false,
        overrides: {
          ...repoPolicyResponse().overrides,
          extra_instructions: 'reglas guardadas',
          blocking_threshold: 'major',
        },
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    expect(
      (wrapper.get('[data-policy-override="extra_instructions"]').element as HTMLInputElement)
        .checked,
    ).toBe(true)
    await wrapper.get('[data-policy-field="blocking_threshold"]').setValue('blocker')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.saveRepoPolicy).toHaveBeenCalledWith(20, {
      extra_instructions: 'reglas guardadas',
      blocking_threshold: 'blocker',
    })
    wrapper.unmount()
  })

  it('guarda exclude_globs y required_checks y tolera checks nulos heredados', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    const inherited = repoPolicyResponse()
    inherited.effective.required_checks = null as never
    inherited.effective.exclude_globs = null as never
    mocks.api.getRepoPolicy.mockResolvedValue(inherited)

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    // A null effective list is an inherited empty policy, not a load error.
    expect(wrapper.find('[data-close-policy]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('No se pudo cargar la política del repositorio')

    await wrapper.get('[data-policy-override="exclude_globs"]').setValue(true)
    await wrapper.get('[data-policy-field="exclude_globs"]').setValue('vendor/**\n**/*.pb.go')
    await wrapper.get('[data-policy-override="required_checks"]').setValue(true)
    await wrapper.get('[data-policy-field="required_checks"]').setValue('build | app:7\nci/test | status')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.saveRepoPolicy).toHaveBeenCalledWith(20, {
      exclude_globs: ['vendor/**', '**/*.pb.go'],
      required_checks: [
        { name: 'build', origin: 'app:7' },
        { name: 'ci/test', origin: 'status' },
      ],
    })
    wrapper.unmount()
  })

  it('no ofrece editar overrides si el equipo todavía no tiene política', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy: false }])

    const wrapper = mountRepos()
    await flushPromises()

    expect(wrapper.find('[data-repo-policy="20"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('muestra los consumidores efectivos y sus límites en el editor', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    mocks.api.getRepoPolicy.mockResolvedValue(
      repoPolicyResponse({
        effective: basePolicy({
          consumer_allow_list: [
            { repository: 'acme/consumer', ref: 'v1.2.0', contract: 'api/openapi.yaml' },
          ],
          consumer_baseline_path: '.qa/consumers.json',
          consumer_max_lookups: 3,
          consumer_max_bytes: 65536,
        }),
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-policy-consumers]').text()).toContain('Consumidores autorizados')
    expect(
      (wrapper.get('[data-consumer-repository="0"]').element as HTMLInputElement).value,
    ).toBe('acme/consumer')
    expect((wrapper.get('[data-consumer-ref="0"]').element as HTMLInputElement).value).toBe(
      'v1.2.0',
    )
    expect((wrapper.get('[data-consumer-contract="0"]').element as HTMLInputElement).value).toBe(
      'api/openapi.yaml',
    )
    expect((wrapper.get('[data-consumer-baseline]').element as HTMLInputElement).value).toBe(
      '.qa/consumers.json',
    )
    expect((wrapper.get('[data-consumer-max-lookups]').element as HTMLInputElement).value).toBe(
      '3',
    )
    expect((wrapper.get('[data-consumer-max-bytes]').element as HTMLInputElement).value).toBe(
      '65536',
    )
    expect(wrapper.get('[data-consumer-remove="0"]').attributes('aria-label')).toBe(
      'Quitar consumidor acme/consumer v1.2.0',
    )
    wrapper.unmount()
  })

  it('añade y quita filas de consumidores', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    expect(wrapper.findAll('[data-consumer-row]')).toHaveLength(0)
    await wrapper.get('[data-consumer-add]').trigger('click')
    expect(wrapper.findAll('[data-consumer-row]')).toHaveLength(1)
    await wrapper.get('[data-consumer-remove="0"]').trigger('click')
    expect(wrapper.findAll('[data-consumer-row]')).toHaveLength(0)
    wrapper.unmount()
  })

  it('guarda los consumidores en la política del equipo y conserva los campos intactos', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    const team = basePolicy({
      architecture_analysis_mode: 'off',
      architecture_token_share: 30,
      architecture_time_share: 10,
      architecture_token_max: 50000,
      architecture_baseline_path: 'docs/arch-baseline.json',
      architecture_exception_policy: 'baseline',
      allow_external_reviews: true,
      external_trusted_associations: ['OWNER'],
      max_external_diff_lines: 1234,
      max_external_runs_per_day: 5,
      external_daily_token_budget: 9000,
      verify_max_findings: 33,
      verify_max_files: 7,
      verify_token_share: 12,
      verify_time_share: 9,
      verify_token_max: 120000,
    })
    mocks.api.getPolicy.mockResolvedValue(team)
    const saved = repoPolicyResponse({
      effective: basePolicy({
        consumer_allow_list: [
          { repository: 'acme/consumer', ref: 'v1.2.0', contract: 'api/openapi.yaml' },
        ],
        consumer_max_lookups: 4,
      }),
    })
    mocks.api.getRepoPolicy
      .mockResolvedValueOnce(repoPolicyResponse())
      .mockResolvedValueOnce(saved)

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-consumer-add]').trigger('click')
    await wrapper.get('[data-consumer-repository="0"]').setValue('acme/consumer')
    await wrapper.get('[data-consumer-ref="0"]').setValue('v1.2.0')
    await wrapper.get('[data-consumer-contract="0"]').setValue('api/openapi.yaml')
    await wrapper.get('[data-consumer-max-lookups]').setValue('4')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.getPolicy).toHaveBeenCalledWith(1)
    const [teamId, body] = mocks.api.savePolicy.mock.calls[0]
    expect(teamId).toBe(1)
    expect(body.consumer_allow_list).toEqual([
      { repository: 'acme/consumer', ref: 'v1.2.0', contract: 'api/openapi.yaml' },
    ])
    expect(body.consumer_max_lookups).toBe(4)
    expect(body).not.toHaveProperty('consumer_baseline_path')
    expect(body).not.toHaveProperty('consumer_max_bytes')
    expect(body).toMatchObject({
      architecture_analysis_mode: 'off',
      architecture_token_share: 30,
      architecture_time_share: 10,
      architecture_token_max: 50000,
      architecture_baseline_path: 'docs/arch-baseline.json',
      architecture_exception_policy: 'baseline',
      allow_external_reviews: true,
      external_trusted_associations: ['OWNER'],
      max_external_diff_lines: 1234,
      max_external_runs_per_day: 5,
      external_daily_token_budget: 9000,
      verify_max_findings: 33,
      verify_max_files: 7,
      verify_token_share: 12,
      verify_time_share: 9,
      verify_token_max: 120000,
    })
    expect(mocks.api.getRepoPolicy).toHaveBeenCalledTimes(2)
    expect(
      (wrapper.get('[data-consumer-repository="0"]').element as HTMLInputElement).value,
    ).toBe('acme/consumer')
    expect(wrapper.text()).toContain('Cambios guardados')
    wrapper.unmount()
  })

  it('no guarda la política del equipo cuando solo cambian los overrides del repositorio', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    mocks.api.saveRepoPolicy.mockResolvedValue(
      repoPolicyResponse({
        inherited: false,
        overrides: {
          ...repoPolicyResponse().overrides,
          blocking_threshold: 'blocker',
        },
      }),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-policy-override="blocking_threshold"]').setValue(true)
    await wrapper.get('[data-policy-field="blocking_threshold"]').setValue('blocker')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.saveRepoPolicy).toHaveBeenCalledWith(20, {
      blocking_threshold: 'blocker',
    })
    expect(mocks.api.getPolicy).not.toHaveBeenCalled()
    expect(mocks.api.savePolicy).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('rechaza consumidores incompletos y nombra la fila antes de guardar', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-consumer-add]').trigger('click')
    await wrapper.get('[data-consumer-repository="0"]').setValue('acme/consumer')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain(
      'El consumidor 1 (acme/consumer) necesita una ref fija',
    )
    expect(mocks.api.saveRepoPolicy).not.toHaveBeenCalled()
    expect(mocks.api.clearRepoPolicy).not.toHaveBeenCalled()
    expect(mocks.api.savePolicy).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('rechaza un límite de consumidores vacío en vez de omitirlo', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-consumer-max-lookups]').setValue('')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('El máximo de consultas no puede quedar vacío')
    expect(mocks.api.savePolicy).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('muestra el error de validación del backend al guardar consumidores', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue([{ ...repos[1], has_policy_override: false }])
    mocks.api.savePolicy.mockRejectedValue(
      new Error('{"error":"invalid_consumer_allow_list"}'),
    )

    const wrapper = mountRepos()
    await flushPromises()
    await wrapper.get('[data-repo-policy="20"]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-consumer-add]').trigger('click')
    await wrapper.get('[data-consumer-repository="0"]').setValue('acme/consumer')
    await wrapper.get('[data-consumer-ref="0"]').setValue('v1.2.0')
    await wrapper.get('[data-consumer-contract="0"]').setValue('api/openapi.yaml')
    await wrapper.get('[data-repo-policy-save]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain(
      'La política del repositorio se guardó, pero los consumidores no',
    )
    expect(wrapper.text()).toContain('La lista de consumidores no es válida')
    wrapper.unmount()
  })

  it('filters repositories by search, account, team, visibility and policy', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue(filterRepos.map((repo) => ({ ...repo })))

    const wrapper = mountRepos()
    await flushPromises()

    expect(rowIds(wrapper)).toEqual(['10', '20', '30'])

    const search = wrapper.get('input[type="search"]')
    await search.setValue('frontend')
    expect(rowIds(wrapper)).toEqual(['20'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('1 DE 3')
    await search.setValue('')

    await chooseOption(wrapper.element, 'Cuenta', 'personal')
    expect(rowIds(wrapper)).toEqual(['20'])
    await wrapper.get('.filter-bar__clear').trigger('click')

    await chooseOption(wrapper.element, 'Visibilidad', 'Privado')
    expect(rowIds(wrapper)).toEqual(['10', '30'])
    await chooseOption(wrapper.element, 'Política', 'Observación')
    expect(rowIds(wrapper)).toEqual(['30'])
    await wrapper.get('.filter-bar__clear').trigger('click')

    await chooseOption(wrapper.element, 'Equipo', 'Sin equipo')
    expect(rowIds(wrapper)).toEqual(['10'])
    await chooseOption(wrapper.element, 'Equipo', 'T2')
    expect(rowIds(wrapper)).toEqual(['10', '30'])
    await wrapper.get('.filter-bar__clear').trigger('click')

    await chooseOption(wrapper.element, 'Política', 'Con override')
    expect(rowIds(wrapper)).toEqual(['20'])
    wrapper.unmount()
  })

  it('segments by automatic review with counts', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue(filterRepos.map((repo) => ({ ...repo })))

    const wrapper = mountRepos()
    await flushPromises()

    const count = (value: string) =>
      wrapper.get(`[data-enabled-segment="${value}"] .repo-segment__count`).text()
    expect(count('all')).toBe('3')
    expect(count('on')).toBe('2')
    expect(count('off')).toBe('1')

    await wrapper.get('[data-enabled-segment="on"]').trigger('click')
    expect(rowIds(wrapper)).toEqual(['20', '30'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('2 DE 3')

    await wrapper.get('input[type="search"]').setValue('frontend')
    expect(count('all')).toBe('1')
    expect(count('on')).toBe('1')
    expect(count('off')).toBe('0')

    await wrapper.get('input[type="search"]').setValue('')
    await wrapper.get('[data-enabled-segment="off"]').trigger('click')
    expect(rowIds(wrapper)).toEqual(['10'])

    await wrapper.get('[data-enabled-segment="all"]').trigger('click')
    expect(rowIds(wrapper)).toEqual(['10', '20', '30'])
    wrapper.unmount()
  })

  it('renders the repository id in its own right-aligned column', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    const headers = wrapper.findAll('thead th')
    expect(headers[headers.length - 1].text()).toBe('ID')
    expect(headers[headers.length - 1].classes()).toContain('repo-table__id')

    const rows = wrapper.findAll('[data-repo-row]')
    const idCells = rows.map((row) => row.findAll('td')[row.findAll('td').length - 1])
    expect(idCells.map((cell) => cell.text())).toEqual(['10', '20'])
    expect(idCells.every((cell) => cell.classes().includes('repo-table__id'))).toBe(true)

    const name = wrapper.get('[data-repo-row="10"] .repo-table__name')
    expect(name.text()).toBe('acme/backend-api')
    wrapper.unmount()
  })

  it('shows the filters empty state when nothing matches', async () => {
    mocks.session.me = admin

    const wrapper = mountRepos()
    await flushPromises()

    await wrapper.get('input[type="search"]').setValue('ninguno-de-estos')

    expect(rowIds(wrapper)).toEqual([])
    const empty = wrapper.get('.empty')
    expect(empty.get('.empty__title').text()).toBe('NADA COINCIDE CON ESTOS FILTROS')

    await empty.get('button').trigger('click')
    expect(rowIds(wrapper)).toEqual(['10', '20'])
    expect(wrapper.get('.filter-bar__count').text()).toBe('2 DE 2')
    wrapper.unmount()
  })

  it('opens the mobile sheet with the same repository dropdowns', async () => {
    mocks.session.me = admin
    mocks.api.repos.mockResolvedValue(filterRepos.map((repo) => ({ ...repo })))

    const wrapper = mountRepos()
    await flushPromises()

    await wrapper.get('.filter-bar__sheet').trigger('click')
    await flushPromises()

    const sheet = document.body.querySelector('.filter-sheet') as HTMLElement
    expect(sheet).not.toBeNull()
    expect(wrapper.get('.filter-bar__sheet').attributes('aria-expanded')).toBe('true')

    await chooseOption(sheet, 'Cuenta', 'personal')
    expect(rowIds(wrapper)).toEqual(['20'])

    const apply = document.body.querySelector('.filter-sheet__apply') as HTMLElement
    expect(apply.textContent?.trim()).toBe('VER 1 repos')
    apply.click()
    await flushPromises()

    expect(document.body.querySelector('.filter-sheet')).toBeNull()
    expect(wrapper.get('.filter-bar__sheet').attributes('aria-expanded')).toBe('false')
    wrapper.unmount()
  })
})
