import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, subscribeEvents, type PRView, type QASummary } from '../api'
import { qaBucket, sortForQueue, type QaBucket } from '../qaQueue'

const REFRESH_DELAY = 300

export type BucketFilter = QaBucket | ''

export const usePrsStore = defineStore('prs', () => {
  const summary = ref<QASummary | null>(null)
  const prs = ref<PRView[]>([])
  // QA opens the panel to validate, so that is the view it starts on.
  const bucket = ref<BucketFilter>('ready')
  const loading = ref(false)
  const connected = ref(false)
  const error = ref<string | null>(null)

  let refreshTimer: ReturnType<typeof setTimeout> | null = null
  let unsubscribe: (() => void) | null = null

  const queue = computed(() => sortForQueue(prs.value))
  const visible = computed(() =>
    bucket.value === '' ? queue.value : queue.value.filter((pr) => qaBucket(pr) === bucket.value),
  )
  const counts = computed(() => {
    const result: Record<BucketFilter, number> = {
      '': prs.value.length,
      ready: 0,
      defects: 0,
      inconclusive: 0,
      ai: 0,
      other: 0,
    }
    for (const pr of prs.value) result[qaBucket(pr)]++
    return result
  })

  async function refresh(): Promise<void> {
    loading.value = true
    error.value = null
    try {
      const [nextSummary, nextPrs] = await Promise.all([api.qaSummary(), api.prs()])
      summary.value = nextSummary
      prs.value = nextPrs
    } catch (err) {
      error.value = err instanceof Error ? err.message : 'No se pudieron cargar los PRs'
    } finally {
      loading.value = false
    }
  }

  function setBucket(next: BucketFilter): void {
    bucket.value = next
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
    summary,
    prs,
    queue,
    visible,
    counts,
    bucket,
    loading,
    connected,
    error,
    refresh,
    setBucket,
    scheduleRefresh,
    start,
    stop,
  }
})
