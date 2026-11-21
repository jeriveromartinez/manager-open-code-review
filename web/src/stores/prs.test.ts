import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { watch } from 'vue'
import { usePrsStore } from './prs'

const mocks = vi.hoisted(() => ({ api: { qaSummary: vi.fn(), prs: vi.fn() } }))

vi.mock('../api', () => ({ api: mocks.api }))

describe('prs store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocks.api.qaSummary.mockResolvedValue(null)
    mocks.api.prs.mockResolvedValue([])
  })

  it('does not flag loading during a silent refresh', async () => {
    const store = usePrsStore()
    const seen: boolean[] = []
    watch(() => store.loading, (value) => seen.push(value), { flush: 'sync' })

    await store.refresh({ silent: true })
    expect(seen).toEqual([])

    await store.refresh()
    expect(seen).toEqual([true, false])
  })
})
