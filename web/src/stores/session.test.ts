import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from './session'

const mocks = vi.hoisted(() => ({ api: { me: vi.fn() } }))

vi.mock('../api', () => ({ api: mocks.api }))

describe('session store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  it('shares one request between callers that load at the same time', async () => {
    let release: (value: unknown) => void = () => {}
    mocks.api.me.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = resolve
        }),
    )
    const session = useSessionStore()

    const first = session.load()
    const second = session.load()
    release({ id: 1, login: 'mvega', avatar_url: '', is_admin: false, teams: [] })

    expect((await first)?.login).toBe('mvega')
    expect((await second)?.login).toBe('mvega')
    expect(mocks.api.me).toHaveBeenCalledTimes(1)
  })

  it('asks again when forced', async () => {
    mocks.api.me.mockResolvedValue({ id: 1, login: 'mvega', avatar_url: '', is_admin: false, teams: [] })
    const session = useSessionStore()

    await session.load()
    await session.load(true)

    expect(mocks.api.me).toHaveBeenCalledTimes(2)
  })
})
