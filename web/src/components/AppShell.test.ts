import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import AppShell from './AppShell.vue'

const mocks = vi.hoisted(() => ({
  api: { me: vi.fn() },
  stream: {
    onState: null as ((connected: boolean) => void) | null,
    close: vi.fn(),
  },
  openEventStream: vi.fn(),
}))

vi.mock('../api', () => ({ api: mocks.api, openEventStream: mocks.openEventStream }))

function me(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    login: 'mvega',
    avatar_url: '',
    is_admin: false,
    teams: [{ id: 3, name: 'Pagos', role: 'qa' }],
    ...overrides,
  }
}

async function mountShell(path = '/'): Promise<VueWrapper> {
  const page = { template: '<div />' }
  const router = createRouter({
    history: createMemoryHistory(),
    routes: ['/', '/lead', '/repos', '/teams', '/agents', '/settings'].map((p) => ({
      path: p,
      component: page,
      meta: { wide: p === '/' || p === '/lead' },
    })),
  })
  await router.push(path)
  const wrapper = mount(AppShell, {
    slots: { default: '<p data-page>contenido</p>' },
    global: { plugins: [createPinia(), router] },
  })
  await flushPromises()
  return wrapper
}

function navLabels(wrapper: VueWrapper): string[] {
  return wrapper.findAll('[data-nav]').map((link) => link.text())
}

describe('AppShell', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.me.mockResolvedValue(me())
    mocks.openEventStream.mockImplementation((_onEvent: unknown, onState: (connected: boolean) => void) => {
      mocks.stream.onState = onState
      return mocks.stream.close
    })
  })

  it('opens the live stream once signed in and shows whether it is connected', async () => {
    const wrapper = await mountShell()

    expect(mocks.openEventStream).toHaveBeenCalledTimes(1)
    const live = wrapper.get('[data-service-badge]')
    expect(live.text()).toBe('Reconectando…')
    mocks.stream.onState!(true)
    await flushPromises()
    expect(live.text()).toBe('En vivo')
    expect(live.classes()).toContain('live--on')

    wrapper.unmount()
    expect(mocks.stream.close).toHaveBeenCalledTimes(1)
  })

  it('does not open the stream without a session', async () => {
    mocks.api.me.mockResolvedValue(null)
    await mountShell()

    expect(mocks.openEventStream).not.toHaveBeenCalled()
  })

  it('loads the session itself so the navigation shows on every page', async () => {
    const wrapper = await mountShell('/repos')

    expect(mocks.api.me).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-brand]').text()).toBe('QA Control Center')
    expect(navLabels(wrapper)).toEqual(['Mi cola', 'Revisiones', 'Repositorios', 'Equipos'])
    expect(wrapper.get('[data-nav="/repos"]').attributes('aria-current')).toBe('page')
    expect(wrapper.get('[data-nav="/"]').attributes('aria-current')).toBeUndefined()
    expect(wrapper.find('[data-page]').exists()).toBe(true)
  })

  it('adds the admin pages for an admin', async () => {
    mocks.api.me.mockResolvedValue(me({ is_admin: true }))
    const wrapper = await mountShell()

    expect(navLabels(wrapper)).toEqual([
      'Mi cola',
      'Revisiones',
      'Repositorios',
      'Equipos',
      'Agentes LLM',
      'Ajustes',
    ])
  })

  it('offers the team lead view only to leads and admins', async () => {
    const qaOnly = await mountShell()
    expect(qaOnly.find('[data-role-lead]').exists()).toBe(false)
    qaOnly.unmount()

    mocks.api.me.mockResolvedValue(me({ teams: [{ id: 3, name: 'Pagos', role: 'lead' }] }))
    const lead = await mountShell()
    expect(lead.find('[data-role-lead]').exists()).toBe(true)
    lead.unmount()

    mocks.api.me.mockResolvedValue(me({ is_admin: true }))
    const admin = await mountShell()
    expect(admin.find('[data-role-lead]').exists()).toBe(true)
  })

  it('names who is signed in on the logout button', async () => {
    const wrapper = await mountShell()

    expect(wrapper.get('[data-logout]').attributes('aria-label')).toBe('Salir (mvega)')
  })
})
