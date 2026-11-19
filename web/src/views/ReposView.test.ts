import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ReposView from './ReposView.vue'
import type { Me, Repository, Team } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    repos: vi.fn(),
    teams: vi.fn(),
    patchRepo: vi.fn(),
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

const repos: Repository[] = [
  {
    id: 10,
    installation_id: 1,
    full_name: 'acme/backend-api',
    private: true,
    default_branch: 'main',
    enabled: false,
    team_id: null,
  },
  {
    id: 20,
    installation_id: 1,
    full_name: 'acme/frontend-app',
    private: false,
    default_branch: 'main',
    enabled: true,
    team_id: 1,
  },
]

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
})
