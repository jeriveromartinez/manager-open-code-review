import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, LIVE_EVENT_TYPES, openEventStream } from './api'

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
