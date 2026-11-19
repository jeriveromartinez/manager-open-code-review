import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, subscribeEvents, type PRGroup, type PRView, type Stats } from '../api'

const REFRESH_DELAY = 300

function emptyStats(): Stats {
  return { open: 0, awaiting_human: 0, in_evaluation: 0, with_problems: 0 }
}

export const usePrsStore = defineStore('prs', () => {
  const stats = ref<Stats>(emptyStats())
  const prs = ref<PRView[]>([])
  const group = ref<PRGroup>('')
  const loading = ref(false)
  const connected = ref(false)
  const error = ref<string | null>(null)

  let refreshTimer: ReturnType<typeof setTimeout> | null = null
  let unsubscribe: (() => void) | null = null

  const repoCount = computed(() => new Set(prs.value.map((pr) => pr.repo)).size)

  async function loadStats(): Promise<void> {
    stats.value = await api.stats()
  }

  async function loadPrs(): Promise<void> {
    prs.value = await api.prs(group.value === '' ? undefined : group.value)
  }

  async function refresh(): Promise<void> {
    loading.value = true
    error.value = null
    try {
      await Promise.all([loadStats(), loadPrs()])
    } catch (err) {
      error.value = err instanceof Error ? err.message : 'No se pudieron cargar los PRs'
    } finally {
      loading.value = false
    }
  }

  async function setGroup(next: PRGroup): Promise<void> {
    group.value = next
    loading.value = true
    error.value = null
    try {
      await loadPrs()
    } catch (err) {
      error.value = err instanceof Error ? err.message : 'No se pudieron cargar los PRs'
    } finally {
      loading.value = false
    }
  }

  /** Debounce de 300 ms para ráfagas de eventos SSE. */
  function scheduleRefresh(): void {
    if (refreshTimer !== null) {
      clearTimeout(refreshTimer)
    }
    refreshTimer = setTimeout(() => {
      refreshTimer = null
      void refresh()
    }, REFRESH_DELAY)
  }

  function start(): void {
    if (unsubscribe) {
      return
    }
    unsubscribe = subscribeEvents(
      (event) => {
        if (event.type === 'pr.updated' || event.type === 'repo.updated') {
          scheduleRefresh()
        }
      },
      (isConnected) => {
        connected.value = isConnected
      },
    )
  }

  function stop(): void {
    if (refreshTimer !== null) {
      clearTimeout(refreshTimer)
      refreshTimer = null
    }
    if (unsubscribe) {
      unsubscribe()
      unsubscribe = null
    }
    connected.value = false
  }

  return {
    stats,
    prs,
    group,
    loading,
    connected,
    error,
    repoCount,
    refresh,
    setGroup,
    scheduleRefresh,
    start,
    stop,
  }
})
