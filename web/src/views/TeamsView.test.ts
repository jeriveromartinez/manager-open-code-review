import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import TeamsView from './TeamsView.vue'
import type { Me, Team, User } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    teams: vi.fn(),
    users: vi.fn(),
    createTeam: vi.fn(),
    setMember: vi.fn(),
    removeMember: vi.fn(),
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

const teams: Team[] = [
  {
    id: 1,
    name: 'T1',
    slug: 't1',
    members: [
      { user_id: 11, login: 'lead1', avatar_url: '', role: 'lead' },
      { user_id: 12, login: 'dev1', avatar_url: '', role: 'developer' },
    ],
  },
  {
    id: 2,
    name: 'T2',
    slug: 't2',
    members: [{ user_id: 21, login: 'lead2', avatar_url: '', role: 'lead' }],
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
    mocks.api.createTeam.mockResolvedValue({ id: 3, name: 'Nuevo', slug: 'nuevo', members: [] })
    mocks.api.setMember.mockResolvedValue(undefined)
    mocks.api.removeMember.mockResolvedValue(undefined)
    mocks.session.load.mockResolvedValue(null)
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
})
