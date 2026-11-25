import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import TeamsView from './TeamsView.vue'
import { ApiError, type Me, type Repository, type Team, type User } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    teams: vi.fn(),
    users: vi.fn(),
    createTeam: vi.fn(),
    setMember: vi.fn(),
    removeMember: vi.fn(),
    renameTeam: vi.fn(),
    deleteTeam: vi.fn(),
    repos: vi.fn(),
    patchRepo: vi.fn(),
  },
  session: {
    me: null as Me | null,
    load: vi.fn(),
  },
}))

vi.mock('../api', () => ({
  ApiError: class ApiError extends Error {
    status: number
    payload?: unknown

    constructor(status: number, message: string, payload?: unknown) {
      super(message)
      this.name = 'ApiError'
      this.status = status
      this.payload = payload
    }
  },
  api: mocks.api,
}))
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

const teams: Team[] = [
  {
    id: 1,
    name: 'T1',
    slug: 't1',
    repository_count: 0,
    members: [
      { user_id: 11, login: 'lead1', avatar_url: '', role: 'lead' },
      { user_id: 12, login: 'dev1', avatar_url: '', role: 'developer' },
    ],
  },
  {
    id: 2,
    name: 'T2',
    slug: 't2',
    repository_count: 1,
    members: [{ user_id: 21, login: 'lead2', avatar_url: '', role: 'lead' }],
  },
]

const repos: Repository[] = [
  {
    id: 30,
    installation_id: 1,
    full_name: 'acme/api',
    private: true,
    default_branch: 'main',
    enabled: true,
    team_id: 2,
    has_policy: false,
    has_policy_override: false,
    policy_observation: false,
    account_login: 'acme',
  },
  {
    id: 31,
    installation_id: 1,
    full_name: 'acme/web',
    private: true,
    default_branch: 'main',
    enabled: true,
    team_id: 1,
    has_policy: false,
    has_policy_override: false,
    policy_observation: false,
    account_login: 'acme',
  },
]

const users: User[] = [
  { id: 11, login: 'lead1', avatar_url: '', is_admin: false },
  { id: 12, login: 'dev1', avatar_url: '', is_admin: false },
  { id: 21, login: 'lead2', avatar_url: '', is_admin: false },
  { id: 30, login: 'newbie', avatar_url: '', is_admin: false },
]

const leadT1: Me = {
  id: 11,
  login: 'lead1',
  avatar_url: '',
  is_admin: false,
  teams: [{ id: 1, name: 'T1', role: 'lead' }],
}

const admin: Me = {
  id: 1,
  login: 'admin',
  avatar_url: '',
  is_admin: true,
  teams: [],
}

function mountTeams(): VueWrapper {
  return mount(TeamsView)
}

function card(wrapper: VueWrapper, id: number): ReturnType<VueWrapper['get']> {
  return wrapper.get(`[data-team-id="${id}"]`)
}

describe('TeamsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.teams.mockResolvedValue(
      teams.map((team) => ({ ...team, members: team.members.map((member) => ({ ...member })) })),
    )
    mocks.api.users.mockResolvedValue(users.map((user) => ({ ...user })))
    mocks.api.createTeam.mockResolvedValue({
      id: 3,
      name: 'Nuevo',
      slug: 'nuevo',
      repository_count: 0,
      members: [],
    })
    mocks.api.setMember.mockResolvedValue(undefined)
    mocks.api.removeMember.mockResolvedValue(undefined)
    mocks.api.renameTeam.mockResolvedValue({
      id: 1,
      name: 'T1',
      slug: 't1',
      repository_count: 0,
    })
    mocks.api.deleteTeam.mockResolvedValue(undefined)
    mocks.api.repos.mockResolvedValue(repos.map((repo) => ({ ...repo })))
    mocks.api.patchRepo.mockResolvedValue(undefined)
    mocks.session.load.mockResolvedValue(null)
    live.registrations.length = 0
  })

  it('refreshes teams and people on events and keeps a half-filled new member', async () => {
    mocks.session.me = leadT1
    const wrapper = mountTeams()
    await flushPromises()

    const [registration] = live.registrations
    expect(registration.types).toEqual(['team.updated', 'repo.updated', 'user.updated'])
    await card(wrapper, 1).get('select[data-add-member-role]').setValue('developer')
    mocks.api.teams.mockClear()
    mocks.api.users.mockClear()
    await registration.reload()
    await flushPromises()

    expect(mocks.api.teams).toHaveBeenCalledTimes(1)
    expect(mocks.api.users).toHaveBeenCalledTimes(1)
    expect((card(wrapper, 1).get('select[data-add-member-role]').element as HTMLSelectElement).value).toBe('developer')
    wrapper.unmount()
  })

  it('lays the teams out as cards in a grid', async () => {
    mocks.session.me = leadT1
    const wrapper = mountTeams()
    await flushPromises()

    const grid = wrapper.get('.card-grid')
    expect(grid.findAll('[data-team-id]')).toHaveLength(2)
    wrapper.unmount()
  })

  it('como lead de T1 ve controles en T1 y no en T2', async () => {
    mocks.session.me = leadT1

    const wrapper = mountTeams()
    await flushPromises()

    const t1 = card(wrapper, 1)
    const t2 = card(wrapper, 2)

    expect(t1.text()).toContain('T1')
    expect(t1.text()).toContain('Lead')
    expect(t1.text()).toContain('Desarrollador')
    expect(t1.find('[data-add-member]').exists()).toBe(true)
    expect(t1.findAll('[data-change-role]').length).toBe(2)
    expect(t1.findAll('[data-remove-member]').length).toBe(2)

    expect(t2.text()).toContain('T2')
    expect(t2.find('[data-add-member]').exists()).toBe(false)
    expect(t2.findAll('[data-change-role]').length).toBe(0)
    expect(t2.findAll('[data-remove-member]').length).toBe(0)

    wrapper.unmount()
  })

  it('si falla api.users, el lead sigue viendo equipos y controles', async () => {
    mocks.session.me = leadT1
    mocks.api.users.mockRejectedValue(new Error('403'))

    const wrapper = mountTeams()
    await flushPromises()

    expect(mocks.api.users).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-team-id="1"]').exists()).toBe(true)
    expect(wrapper.find('[data-team-id="2"]').exists()).toBe(true)
    expect(card(wrapper, 1).find('[data-add-member]').exists()).toBe(true)
    expect(card(wrapper, 1).findAll('[data-change-role]').length).toBe(2)
    expect(card(wrapper, 1).findAll('[data-remove-member]').length).toBe(2)
    expect(wrapper.text()).toContain('No se pudo cargar la lista de usuarios')
    expect(wrapper.text()).not.toContain('No se pudieron cargar los equipos')

    wrapper.unmount()
  })

  it('un desarrollador sin equipo liderado no consulta api.users', async () => {
    mocks.session.me = { ...leadT1, id: 12, login: 'dev1', teams: [{ id: 1, name: 'T1', role: 'developer' }] }

    const wrapper = mountTeams()
    await flushPromises()

    expect(wrapper.find('[data-team-id="1"]').exists()).toBe(true)
    expect(mocks.api.users).not.toHaveBeenCalled()

    wrapper.unmount()
  })

  it('el control de crear equipo aparece solo para admin', async () => {
    mocks.session.me = admin
    const adminWrapper = mountTeams()
    await flushPromises()
    expect(adminWrapper.find('[data-create-team]').exists()).toBe(true)
    adminWrapper.unmount()

    mocks.session.me = leadT1
    const leadWrapper = mountTeams()
    await flushPromises()
    expect(leadWrapper.find('[data-create-team]').exists()).toBe(false)
    leadWrapper.unmount()
  })

  it('como admin, crear un equipo llama a createTeam con el nombre escrito', async () => {
    mocks.session.me = admin

    const wrapper = mountTeams()
    await flushPromises()

    await wrapper.get('input[data-create-team-name]').setValue('Equipo Nuevo')
    await wrapper.get('[data-create-team]').trigger('click')
    await flushPromises()

    expect(mocks.api.createTeam).toHaveBeenCalledWith('Equipo Nuevo')
    wrapper.unmount()
  })

  it('el lead de T1 puede cambiar el rol y quitar miembros solo de su equipo', async () => {
    mocks.session.me = leadT1

    const wrapper = mountTeams()
    await flushPromises()

    const roles = card(wrapper, 1).findAll('select[data-change-role]')
    await roles[1].setValue('qa')
    await flushPromises()
    expect(mocks.api.setMember).toHaveBeenCalledWith(1, 12, 'qa')

    await card(wrapper, 1).findAll('button[data-remove-member]')[1].trigger('click')
    await flushPromises()
    expect(mocks.api.removeMember).toHaveBeenCalledWith(1, 12)

    wrapper.unmount()
  })

  it('muestra un estado vacío en español cuando no hay equipos', async () => {
    mocks.session.me = admin
    mocks.api.teams.mockResolvedValue([])

    const wrapper = mountTeams()
    await flushPromises()

    expect(wrapper.text()).toContain('No hay equipos todavía')
    wrapper.unmount()
  })

  it('muestra un error en español cuando falla la carga', async () => {
    mocks.session.me = admin
    mocks.api.teams.mockRejectedValue(new Error('falló la red'))

    const wrapper = mountTeams()
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudieron cargar los equipos')
    wrapper.unmount()
  })

  it('como admin, cada equipo enlaza a su política con el equipo preseleccionado', async () => {
    mocks.session.me = admin

    const wrapper = mountTeams()
    await flushPromises()

    const link = card(wrapper, 2).get('[data-team-policy]')
    expect(link.attributes('href')).toBe('/agents?team=2#policy')
    expect(link.text()).toContain('Política')
    wrapper.unmount()
  })

  it('un lead no ve el enlace a la política', async () => {
    mocks.session.me = leadT1

    const wrapper = mountTeams()
    await flushPromises()

    expect(card(wrapper, 1).find('[data-team-policy]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('renames a team from the dialog', async () => {
    mocks.session.me = admin
    mocks.api.renameTeam.mockResolvedValue({
      id: 1,
      name: 'Renombrado',
      slug: 'renombrado',
      repository_count: 0,
    })

    const wrapper = mountTeams()
    await flushPromises()
    mocks.api.teams.mockClear()

    await card(wrapper, 1).get('[data-rename-team]').trigger('click')
    const input = wrapper.get('[data-rename-dialog] input[data-rename-name]')
    expect((input.element as HTMLInputElement).value).toBe('T1')
    await input.setValue('Renombrado')
    await wrapper.get('[data-rename-dialog] form').trigger('submit')
    await flushPromises()

    expect(mocks.api.renameTeam).toHaveBeenCalledWith(1, 'Renombrado')
    expect(mocks.api.teams).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-rename-dialog]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('blocks deletion while a repository is assigned', async () => {
    mocks.session.me = admin

    const wrapper = mountTeams()
    await flushPromises()

    expect(card(wrapper, 2).text()).toContain('1 repo · 1 miembro')

    await card(wrapper, 2).get('[data-delete-team]').trigger('click')
    await flushPromises()

    expect(mocks.api.repos).toHaveBeenCalledTimes(1)
    const dialog = wrapper.get('[data-delete-dialog]')
    expect(dialog.text()).toContain('acme/api')
    expect(dialog.text()).not.toContain('acme/web')

    await dialog.get('input[data-delete-confirm]').setValue('T2')
    expect(dialog.get('[data-delete-submit]').attributes('disabled')).toBeDefined()
    await dialog.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.api.deleteTeam).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('reassigns a repository before deleting', async () => {
    mocks.session.me = admin

    const wrapper = mountTeams()
    await flushPromises()

    await card(wrapper, 2).get('[data-delete-team]').trigger('click')
    await flushPromises()

    await wrapper.get('[data-delete-repo="30"]').setValue('1')
    await flushPromises()
    expect(mocks.api.patchRepo).toHaveBeenCalledWith(30, { team_id: 1 })
    expect(wrapper.find('[data-delete-repo="30"]').exists()).toBe(false)

    await wrapper.get('[data-delete-dialog] input[data-delete-confirm]').setValue('T2')
    await wrapper.get('[data-delete-dialog] form').trigger('submit')
    await flushPromises()

    expect(mocks.api.deleteTeam).toHaveBeenCalledWith(2)
    wrapper.unmount()
  })

  it('deletes a team without repositories', async () => {
    mocks.session.me = admin

    const wrapper = mountTeams()
    await flushPromises()
    mocks.api.teams.mockClear()

    await card(wrapper, 1).get('[data-delete-team]').trigger('click')
    await flushPromises()
    expect(mocks.api.repos).not.toHaveBeenCalled()

    const dialog = wrapper.get('[data-delete-dialog]')
    await dialog.get('input[data-delete-confirm]').setValue('T')
    expect(dialog.get('[data-delete-submit]').attributes('disabled')).toBeDefined()

    await dialog.get('input[data-delete-confirm]').setValue('T1')
    await dialog.get('form').trigger('submit')
    await flushPromises()

    expect(mocks.api.deleteTeam).toHaveBeenCalledWith(1)
    expect(mocks.api.teams).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-delete-dialog]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows the 409 repositories when the server refuses', async () => {
    mocks.session.me = admin
    mocks.api.deleteTeam.mockRejectedValue(
      new ApiError(409, 'conflict', {
        error: 'team has repositories',
        repositories: [{ id: 40, full_name: 'acme/late-repo' }],
      }),
    )

    const wrapper = mountTeams()
    await flushPromises()

    await card(wrapper, 1).get('[data-delete-team]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-delete-dialog] input[data-delete-confirm]').setValue('T1')
    await wrapper.get('[data-delete-dialog] form').trigger('submit')
    await flushPromises()

    expect(mocks.api.deleteTeam).toHaveBeenCalledWith(1)
    const dialog = wrapper.get('[data-delete-dialog]')
    expect(dialog.text()).toContain('acme/late-repo')
    expect(dialog.text()).toContain('acme/web')
    expect(dialog.get('[data-delete-submit]').attributes('disabled')).toBeDefined()

    await wrapper.get('[data-delete-repo="31"]').setValue('2')
    await flushPromises()
    expect(mocks.api.patchRepo).toHaveBeenCalledWith(31, { team_id: 2 })
    expect(dialog.get('[data-delete-submit]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('restores the repository select when a reassignment fails', async () => {
    mocks.session.me = admin
    mocks.api.patchRepo.mockRejectedValue(new Error('boom'))

    const wrapper = mountTeams()
    await flushPromises()

    await card(wrapper, 2).get('[data-delete-team]').trigger('click')
    await flushPromises()

    const select = wrapper.get('[data-delete-repo="30"]')
    expect((select.element as HTMLSelectElement).value).toBe('2')

    await select.setValue('1')
    await flushPromises()

    expect((select.element as HTMLSelectElement).value).toBe('2')
    expect(wrapper.get('[data-delete-dialog]').text()).toContain(
      'No se pudo reasignar el repositorio',
    )
    wrapper.unmount()
  })

  it('focuses the dialog, traps Tab and restores the opener focus', async () => {
    mocks.session.me = admin
    const host = document.createElement('div')
    document.body.appendChild(host)
    const wrapper = mount(TeamsView, { attachTo: host })
    await flushPromises()

    const opener = card(wrapper, 1).get('[data-rename-team]')
    ;(opener.element as HTMLElement).focus()
    await opener.trigger('click')
    await flushPromises()

    const input = wrapper.get('[data-rename-name]').element as HTMLElement
    expect(document.activeElement).toBe(input)

    const submit = wrapper.get('[data-rename-submit]')
    ;(submit.element as HTMLElement).focus()
    await wrapper.get('[data-rename-dialog]').trigger('keydown', { key: 'Tab' })
    expect(document.activeElement).toBe(input)

    await wrapper
      .get('[data-rename-dialog] .modal__actions button[type="button"]')
      .trigger('click')
    await flushPromises()
    expect(document.activeElement).toBe(opener.element)

    wrapper.unmount()
    host.remove()
  })

  it('hides rename and delete from non-admins', async () => {
    mocks.session.me = leadT1

    const wrapper = mountTeams()
    await flushPromises()

    expect(card(wrapper, 1).find('[data-rename-team]').exists()).toBe(false)
    expect(card(wrapper, 1).find('[data-delete-team]').exists()).toBe(false)
    wrapper.unmount()
  })
})
