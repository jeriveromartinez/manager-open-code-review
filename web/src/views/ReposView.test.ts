import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
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
  { id: 1, name: 'T1', slug: 't1', members: [] },
  { id: 2, name: 'T2', slug: 't2', members: [] },
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
  return mount(ReposView)
}

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
})
