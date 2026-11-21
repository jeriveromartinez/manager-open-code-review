import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me } from './api'
import router, { setConfigured } from './router'

vi.mock('./api', () => ({
  api: { setupStatus: vi.fn(), me: vi.fn() },
  setUnauthorizedHandler: vi.fn(),
}))

const admin: Me = { id: 1, login: 'admin', avatar_url: '', is_admin: true, teams: [] }
const member: Me = { id: 2, login: 'dev', avatar_url: '', is_admin: false, teams: [] }

describe('admin guard', () => {
  beforeEach(async () => {
    setActivePinia(createPinia())
    setConfigured(true)
    vi.mocked(api.me).mockReset()
    await router.replace('/login')
  })

  it('lets an admin reach the settings screen', async () => {
    vi.mocked(api.me).mockResolvedValue(admin)

    await router.push('/settings')

    expect(router.currentRoute.value.name).toBe('settings')
  })

  it('sends a non-admin back to the dashboard', async () => {
    vi.mocked(api.me).mockResolvedValue(member)

    await router.push('/settings')

    expect(router.currentRoute.value.name).toBe('dashboard')
  })
})
