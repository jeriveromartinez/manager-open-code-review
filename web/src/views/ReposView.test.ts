import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ReposView from './ReposView.vue'
import type { Me, Repository, Team } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    repos: vi.fn(),
    teams: vi.fn(),
    patchRepo: vi.fn(),
    installations: vi.fn(),
    setupStatus: vi.fn(),
  },
  session: {
    me: null as Me | null,
    load: vi.fn(),
  },
}))

vi.mock('../api', () => ({ api: mocks.api }))
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
    account_login: 'personal',
  },
] as Repository[]

const teams: Team[] = [
  { id: 1, name: 'T1', slug: 't1', members: [] },
  { id: 2, name: 'T2', slug: 't2', members: [] },
]

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
    mocks.api.setupStatus.mockResolvedValue({ configured: true, install_url: '' })
    mocks.session.load.mockResolvedValue(null)
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
})
