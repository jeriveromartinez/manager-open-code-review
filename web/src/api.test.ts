import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, LIVE_EVENT_TYPES, openEventStream } from './api'

describe('api.llmModels', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('posts the provider and key to the models endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ models: [{ id: 'm', name: 'M' }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.llmModels('anthropic', 'sk-ant')).resolves.toEqual({
      models: [{ id: 'm', name: 'M' }],
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/llm-profiles/models',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ provider: 'anthropic', api_key: 'sk-ant' }),
      }),
    )
  })
})

describe('api team mutations', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renames a team with PATCH and returns its summary', async () => {
    const summary = { id: 7, name: 'Nuevo', slug: 'nuevo', repository_count: 2 }
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(summary), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.renameTeam(7, 'Nuevo')).resolves.toEqual(summary)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/teams/7',
      expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ name: 'Nuevo' }) }),
    )
  })

  it('deletes a team with DELETE', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.deleteTeam(7)).resolves.toBeUndefined()
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/teams/7',
      expect.objectContaining({ method: 'DELETE' }),
    )
  })

  it('exposes the parsed JSON body on ApiError', async () => {
    const body = { error: 'conflict', repositories: [{ id: 30, full_name: 'acme/api' }] }
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(JSON.stringify(body), { status: 409 })),
    )

    const error = await api.deleteTeam(7).catch((err: unknown) => err)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(409)
    expect((error as ApiError).payload).toEqual(body)
  })
})

describe('api.runHistory', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  const page = {
    runs: [],
    next_before: 42,
    next_created_at: '2026-11-26T10:00:00.123456789Z',
    has_more: true,
  }

  function stubPage(body: unknown = page) {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }

  it('sends the filters and the full keyset cursor', async () => {
    const fetchMock = stubPage()

    await expect(
      api.runHistory({
        status: ['failed', 'inconclusive'],
        repo: 'acme/api',
        trigger: 'manual',
        q: 'login fix',
        before: 42,
        beforeCreatedAt: '2026-11-26T10:00:00.123456789Z',
        limit: 25,
      }),
    ).resolves.toEqual(page)

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('GET')
    const params = new URL(url, 'http://localhost').searchParams
    expect(params.getAll('status')).toEqual(['failed', 'inconclusive'])
    expect(params.get('repo')).toBe('acme/api')
    expect(params.get('trigger')).toBe('manual')
    expect(params.get('q')).toBe('login fix')
    expect(params.get('before')).toBe('42')
    // RFC 3339 nano precision must survive untouched.
    expect(params.get('before_created_at')).toBe('2026-11-26T10:00:00.123456789Z')
    expect(params.get('limit')).toBe('25')
  })

  it('omits the parameters that were not set', async () => {
    const fetchMock = stubPage({ runs: [], next_before: 0, next_created_at: null, has_more: false })

    await api.runHistory()

    expect(fetchMock.mock.calls[0][0]).toBe('/api/review-runs/history')
  })
})

describe('openEventStream', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  class FakeEventSource {
    static last: FakeEventSource | null = null
    listeners = new Map<string, EventListener>()
    onopen: (() => void) | null = null
    onerror: (() => void) | null = null
    closed = false
    constructor(public url: string) {
      FakeEventSource.last = this
    }
    addEventListener(type: string, listener: EventListener) {
      this.listeners.set(type, listener)
    }
    removeEventListener(type: string) {
      this.listeners.delete(type)
    }
    close() {
      this.closed = true
    }
    emit(type: string, data: unknown) {
      this.listeners.get(type)?.(new MessageEvent(type, { data: JSON.stringify(data) }))
    }
  }

  it('listens to every live event type and reports the connection', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const onEvent = vi.fn()
    const onState = vi.fn()

    const close = openEventStream(onEvent, onState)
    const source = FakeEventSource.last!

    expect(source.url).toBe('/api/events')
    expect([...source.listeners.keys()].sort()).toEqual([...LIVE_EVENT_TYPES].sort())
    source.emit('team.updated', { type: 'team.updated', id: '4' })
    expect(onEvent).toHaveBeenCalledWith({ type: 'team.updated', id: '4' })
    source.onopen?.()
    expect(onState).toHaveBeenLastCalledWith(true)
    source.onerror?.()
    expect(onState).toHaveBeenLastCalledWith(false)

    close()
    expect(source.closed).toBe(true)
    expect(source.listeners.size).toBe(0)
  })

  it('ignores a malformed payload', () => {
    vi.stubGlobal('EventSource', FakeEventSource)
    const onEvent = vi.fn()

    openEventStream(onEvent, vi.fn())
    FakeEventSource.last!.listeners.get('pr.updated')?.(new MessageEvent('pr.updated', { data: 'not json' }))

    expect(onEvent).not.toHaveBeenCalled()
  })
})
