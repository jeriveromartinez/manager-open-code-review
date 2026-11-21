import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import AppShell from './AppShell.vue'

const mocks = vi.hoisted(() => ({
  api: { me: vi.fn() },
}))

vi.mock('../api', () => ({ api: mocks.api }))

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
  })

  it('loads the session itself so the navigation shows on every page', async () => {
    const wrapper = await mountShell('/repos')

    expect(mocks.api.me).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-brand]').text()).toBe('QA Control Center')
    expect(navLabels(wrapper)).toEqual(['Mi cola', 'Repositorios', 'Equipos'])
    expect(wrapper.get('[data-nav="/repos"]').attributes('aria-current')).toBe('page')
    expect(wrapper.get('[data-nav="/"]').attributes('aria-current')).toBeUndefined()
    expect(wrapper.find('[data-page]').exists()).toBe(true)
  })

  it('adds the admin pages for an admin', async () => {
    mocks.api.me.mockResolvedValue(me({ is_admin: true }))
    const wrapper = await mountShell()

    expect(navLabels(wrapper)).toEqual(['Mi cola', 'Repositorios', 'Equipos', 'Agentes LLM', 'Ajustes'])
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

  it('widens the shell only for views that ask for it', async () => {
    const queue = await mountShell('/')
    expect(queue.get('.shell').classes()).toContain('shell--wide')
    queue.unmount()

    const repos = await mountShell('/repos')
    expect(repos.get('.shell').classes()).not.toContain('shell--wide')
  })

  it('names who is signed in on the logout button', async () => {
    const wrapper = await mountShell()

    expect(wrapper.get('[data-logout]').attributes('aria-label')).toBe('Salir (mvega)')
  })
})
