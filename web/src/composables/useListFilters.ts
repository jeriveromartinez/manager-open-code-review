import { computed, ref, watch, type ComputedRef } from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'

export type FacetValues = Record<string, string[]>

export interface ListFiltersOptions {
  facets: readonly string[]
  sorts?: readonly string[]
  defaultSort?: string
}

type QueryValue = string | (string | null)[] | null | undefined

function firstString(value: QueryValue): string | undefined {
  const entries = Array.isArray(value) ? value : [value]
  return entries.find((entry): entry is string => typeof entry === 'string')
}

function toValues(value: QueryValue): string[] {
  const entries = Array.isArray(value) ? value : [value]
  const seen = new Set<string>()
  const values: string[] = []
  for (const entry of entries) {
    if (typeof entry !== 'string' || entry === '' || seen.has(entry)) continue
    seen.add(entry)
    values.push(entry)
  }
  return values
}

function emptyFacets(facets: readonly string[]): FacetValues {
  return Object.fromEntries(facets.map((facet) => [facet, []]))
}

/**
 * Filtros de lista compartidos por las vistas del panel. `q`, las facetas y el
 * orden viven en la ref y se reflejan en la URL con `replace`, de modo que una
 * recarga o un enlace compartido recuperan la misma vista sin recargar.
 */
export function useListFilters(options: ListFiltersOptions) {
  const route = useRoute()
  const router = useRouter()

  const sorts = options.sorts ?? []
  const defaultSort = options.defaultSort

  const q = ref(firstString(route.query.q as QueryValue) ?? '')
  const facets = ref<FacetValues>(
    Object.fromEntries(
      options.facets.map((facet) => [facet, toValues(route.query[facet] as QueryValue)]),
    ),
  )

  function isValidSort(value: string | undefined): value is string {
    return value !== undefined && (sorts.includes(value) || value === defaultSort)
  }

  const querySort = firstString(route.query.sort as QueryValue)
  const sort = ref<string | undefined>(isValidSort(querySort) ? querySort : defaultSort)

  const activeCount = computed(() =>
    options.facets.reduce((total, facet) => total + (facets.value[facet]?.length ?? 0), 0),
  )
  const isActive = computed(() => q.value.trim() !== '' || activeCount.value > 0)

  function currentQuery(): LocationQueryRaw {
    const next: LocationQueryRaw = {}
    for (const [key, value] of Object.entries(route.query)) {
      if (key === 'q' || key === 'sort' || options.facets.includes(key)) continue
      next[key] = value
    }
    if (q.value.trim() !== '') next.q = q.value
    for (const facet of options.facets) {
      const values = facets.value[facet] ?? []
      if (values.length > 0) next[facet] = values
    }
    if (sort.value !== undefined && sort.value !== defaultSort) next.sort = sort.value
    return next
  }

  watch([q, facets, sort], () => void router.replace({ query: currentQuery() }), { deep: true })

  function setFacet(facet: string, values: readonly string[]): void {
    if (!options.facets.includes(facet)) return
    facets.value = { ...facets.value, [facet]: toValues([...values]) }
  }

  function toggleFacet(facet: string, value: string): void {
    if (!options.facets.includes(facet)) return
    const current = facets.value[facet] ?? []
    setFacet(facet, current.includes(value) ? current.filter((entry) => entry !== value) : [...current, value])
  }

  function setSort(value: string | undefined): void {
    sort.value = isValidSort(value) ? value : defaultSort
  }

  function clear(): void {
    q.value = ''
    facets.value = emptyFacets(options.facets)
    sort.value = defaultSort
  }

  function apply<T>(
    items: T[],
    text: (item: T) => string,
    match: (item: T, facets: FacetValues) => boolean,
  ): ComputedRef<T[]> {
    return computed(() => {
      const needle = q.value.trim().toLowerCase()
      return items.filter((item) => {
        if (needle !== '' && !text(item).toLowerCase().includes(needle)) return false
        return match(item, facets.value)
      })
    })
  }

  /**
   * Cuenta cada valor de cada faceta contra el texto y las demás dimensiones:
   * la faceta que se cuenta se ignora a sí misma para que una opción elegida
   * nunca desaparezca del desplegable.
   */
  function facetCounts<T>(
    items: T[],
    valuesOf: (item: T, facet: string) => string[],
    text?: (item: T) => string,
  ): ComputedRef<Record<string, Record<string, number>>> {
    return computed(() => {
      const needle = q.value.trim().toLowerCase()
      const counts: Record<string, Record<string, number>> = {}
      for (const facet of options.facets) {
        const perValue: Record<string, number> = {}
        for (const selected of facets.value[facet] ?? []) perValue[selected] = 0
        const others = options.facets.filter((other) => other !== facet)
        for (const item of items) {
          if (needle !== '' && text && !text(item).toLowerCase().includes(needle)) continue
          const matchesOthers = others.every((other) => {
            const selected = facets.value[other] ?? []
            if (selected.length === 0) return true
            return valuesOf(item, other).some((value) => selected.includes(value))
          })
          if (!matchesOthers) continue
          for (const value of valuesOf(item, facet)) {
            perValue[value] = (perValue[value] ?? 0) + 1
          }
        }
        counts[facet] = perValue
      }
      return counts
    })
  }

  return {
    q,
    facets,
    sort,
    isActive,
    activeCount,
    setFacet,
    toggleFacet,
    setSort,
    clear,
    apply,
    facetCounts,
  }
}
