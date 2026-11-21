import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { useLiveRefresh } from './useLiveRefresh'
import { useLiveStore } from '../stores/live'
import type { ReviewEvent } from '../api'

const stream = vi.hoisted(() => ({ onEvent: null as ((event: ReviewEvent) => void) | null }))

vi.mock('../api', () => ({
  openEventStream: (onEvent: (event: ReviewEvent) => void) => {
    stream.onEvent = onEvent
    return () => {}
  },
}))

function mountWith(reload: () => void, filter?: (event: ReviewEvent) => boolean) {
  const Probe = defineComponent({
    setup() {
      useLiveRefresh(['pr.updated'], reload, filter)
      return () => h('div')
    },
  })
  return mount(Probe)
}

describe('useLiveRefresh', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
    useLiveStore().start()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('reloads once for a burst of events', () => {
    const reload = vi.fn()
    mountWith(reload)

    for (let i = 0; i < 20; i++) stream.onEvent!({ type: 'pr.updated', id: String(i) })
    vi.advanceTimersByTime(299)
    expect(reload).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)

    expect(reload).toHaveBeenCalledTimes(1)
    expect(reload.mock.calls[0][0]).toHaveLength(20)
  })

  it('skips events the filter rejects but always honours a resync', () => {
    const reload = vi.fn()
    mountWith(reload, (event) => event.id === '7')

    stream.onEvent!({ type: 'pr.updated', id: '8' })
    vi.advanceTimersByTime(300)
    expect(reload).not.toHaveBeenCalled()

    stream.onEvent!({ type: 'resync', id: '' })
    vi.advanceTimersByTime(300)
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('drops a pending reload when the view goes away', () => {
    const reload = vi.fn()
    const wrapper = mountWith(reload)

    stream.onEvent!({ type: 'pr.updated', id: '1' })
    wrapper.unmount()
    vi.advanceTimersByTime(300)
    stream.onEvent!({ type: 'pr.updated', id: '2' })
    vi.advanceTimersByTime(300)

    expect(reload).not.toHaveBeenCalled()
  })
})
