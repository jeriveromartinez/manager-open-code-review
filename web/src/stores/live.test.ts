import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useLiveStore } from './live'
import type { ReviewEvent } from '../api'

const stream = vi.hoisted(() => ({
  onEvent: null as ((event: ReviewEvent) => void) | null,
  onState: null as ((connected: boolean) => void) | null,
  close: vi.fn(),
  open: vi.fn(),
}))

vi.mock('../api', () => ({
  openEventStream: stream.open,
}))

describe('live store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    stream.open.mockImplementation((onEvent: (event: ReviewEvent) => void, onState: (connected: boolean) => void) => {
      stream.onEvent = onEvent
      stream.onState = onState
      return stream.close
    })
  })

  it('opens a single stream however many times it is started', () => {
    const live = useLiveStore()
    live.start()
    live.start()

    expect(stream.open).toHaveBeenCalledTimes(1)
  })

  it('hands each subscriber only the types it asked for', () => {
    const live = useLiveStore()
    live.start()
    const teams = vi.fn()
    const prs = vi.fn()
    live.subscribe(['team.updated'], teams)
    live.subscribe(['pr.updated'], prs)

    stream.onEvent!({ type: 'team.updated', id: '4' })

    expect(teams).toHaveBeenCalledWith({ type: 'team.updated', id: '4' })
    expect(prs).not.toHaveBeenCalled()
  })

  it('stops delivering after unsubscribing', () => {
    const live = useLiveStore()
    live.start()
    const handler = vi.fn()
    const off = live.subscribe(['pr.updated'], handler)

    off()
    stream.onEvent!({ type: 'pr.updated', id: '1' })

    expect(handler).not.toHaveBeenCalled()
  })

  it('asks every subscriber to resync when the connection comes back', () => {
    const live = useLiveStore()
    live.start()
    const handler = vi.fn()
    live.subscribe(['pr.updated'], handler)

    stream.onState!(true)
    expect(handler).not.toHaveBeenCalled()
    expect(live.connected).toBe(true)

    stream.onState!(false)
    expect(live.connected).toBe(false)
    stream.onState!(true)

    expect(handler).toHaveBeenCalledTimes(1)
    expect(handler).toHaveBeenCalledWith({ type: 'resync', id: '' })
  })

  it('closes the stream on stop and can start again', () => {
    const live = useLiveStore()
    live.start()
    stream.onState!(true)

    live.stop()

    expect(stream.close).toHaveBeenCalledTimes(1)
    expect(live.connected).toBe(false)
    live.start()
    expect(stream.open).toHaveBeenCalledTimes(2)
  })
})
