import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h, nextTick, ref } from 'vue'
import { createMemoryHistory, createRouter, type LocationQueryRaw } from 'vue-router'
import { describe, expect, it } from 'vitest'
import { useListFilters, type FacetValues } from './useListFilters'

interface Pr {
  title: string
  status: string
  severity: string[]
}

const options = {
  facets: ['status', 'severity'],
  sorts: ['waiting', 'recent'],
  defaultSort: 'waiting',
}

async function mountFilters(query: LocationQueryRaw = {}, opts = options) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }],
  })
  await router.push({ path: '/', query })
  await router.isReady()

  let filters!: ReturnType<typeof useListFilters>
  mount(
    defineComponent({
      setup() {
        filters = useListFilters(opts)
        return () => h('div')
      },
    }),
    { global: { plugins: [router] } },
  )

  async function settle(): Promise<void> {
    await nextTick()
    await flushPromises()
  }

  return { router, filters, settle }
}

describe('useListFilters', () => {
  it('hydrates valid filters from the URL and ignores unknown facets', async () => {
    const { filters } = await mountFilters({
      q: ['login', 'ignored'],
      status: ['open', 'open', null, 'draft'],
      severity: 'high',
      unknown: 'value',
    })

    expect(filters.q.value).toBe('login')
    expect(filters.facets.value).toEqual({ status: ['open', 'draft'], severity: ['high'] })
    expect(filters.isActive.value).toBe(true)
    expect(filters.activeCount.value).toBe(3)
  })

  it('toggles facet values and clears the URL', async () => {
    const { filters, router, settle } = await mountFilters({ tab: 'queue' })

    filters.toggleFacet('status', 'open')
    await settle()
    expect(filters.facets.value.status).toEqual(['open'])
    expect(router.currentRoute.value.query).toMatchObject({ tab: 'queue', status: ['open'] })

    filters.toggleFacet('status', 'draft')
    await settle()
    expect(router.currentRoute.value.query.status).toEqual(['open', 'draft'])

    filters.toggleFacet('status', 'open')
    await settle()
    expect(filters.facets.value.status).toEqual(['draft'])

    filters.toggleFacet('status', 'draft')
    await settle()
    expect(router.currentRoute.value.query).not.toHaveProperty('status')

    filters.q.value = 'auth'
    filters.toggleFacet('severity', 'high')
    filters.setSort('recent')
    await settle()
    expect(router.currentRoute.value.query).toMatchObject({ q: 'auth', severity: ['high'], sort: 'recent' })

    filters.clear()
    await settle()
    expect(filters.q.value).toBe('')
    expect(filters.facets.value).toEqual({ status: [], severity: [] })
    expect(filters.sort.value).toBe('waiting')
    expect(filters.isActive.value).toBe(false)
    expect(filters.activeCount.value).toBe(0)
    expect(router.currentRoute.value.query).toEqual({ tab: 'queue' })
  })

  it('applies text and facets and reports activity', async () => {
    const items = ref<Pr[]>([
      { title: 'Fix login', status: 'open', severity: ['high'] },
      { title: 'Rework login form', status: 'draft', severity: ['low'] },
      { title: 'Write docs', status: 'open', severity: ['low'] },
    ])
    const { filters } = await mountFilters({ q: ' LOGIN ', status: ['open'] })

    const visible = filters.apply(
      items,
      (item) => item.title,
      (item, facets: FacetValues) => !facets.status?.length || facets.status.includes(item.status),
    )

    expect(visible.value.map((item) => item.title)).toEqual(['Fix login'])
    expect(filters.isActive.value).toBe(true)
    expect(filters.activeCount.value).toBe(1)

    items.value = [...items.value, { title: 'Login redesign', status: 'open', severity: ['high'] }]
    expect(visible.value.map((item) => item.title)).toEqual(['Fix login', 'Login redesign'])

    filters.setSort('recent')
    expect(filters.isActive.value).toBe(true)
    expect(filters.activeCount.value).toBe(1)
  })

  it('counts facet options against the other filters only', async () => {
    const items: Pr[] = [
      { title: 'Fix auth', status: 'open', severity: ['high'] },
      { title: 'Fix login', status: 'open', severity: ['low'] },
      { title: 'Docs', status: 'closed', severity: ['high'] },
    ]
    const { filters } = await mountFilters({ q: 'fix', status: ['open'], severity: ['high'] })

    const counts = filters.facetCounts(
      items,
      (item, facet) => (facet === 'severity' ? item.severity : [item.status]),
      (item) => item.title,
    )

    expect(counts.value.severity).toEqual({ high: 1, low: 1 })
    expect(counts.value.status).toEqual({ open: 1 })
    expect(counts.value.status.closed ?? 0).toBe(0)
  })

  it('falls back to the default sort', async () => {
    const invalid = await mountFilters({ sort: 'sideways' })
    expect(invalid.filters.sort.value).toBe('waiting')

    const valid = await mountFilters({ sort: 'recent' })
    expect(valid.filters.sort.value).toBe('recent')

    const { filters, router, settle } = await mountFilters()
    expect(filters.sort.value).toBe('waiting')

    filters.setSort('recent')
    await settle()
    expect(filters.sort.value).toBe('recent')
    expect(router.currentRoute.value.query.sort).toBe('recent')

    filters.setSort('sideways')
    await settle()
    expect(filters.sort.value).toBe('waiting')
    expect(router.currentRoute.value.query).not.toHaveProperty('sort')
  })
})
