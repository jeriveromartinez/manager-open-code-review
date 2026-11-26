import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, type PRView, type QASummary } from '../api'
import { qaBucket, sortForQueue, type QaBucket } from '../qaQueue'

export type BucketFilter = QaBucket | ''

export const usePrsStore = defineStore('prs', () => {
  const summary = ref<QASummary | null>(null)
  const prs = ref<PRView[]>([])
  // QA opens the panel to validate, so that is the view it starts on.
  const bucket = ref<BucketFilter>('ready')
  const loading = ref(false)
  const error = ref<string | null>(null)

  const queue = computed(() => sortForQueue(prs.value))
  const visible = computed(() =>
    bucket.value === '' ? queue.value : queue.value.filter((pr) => qaBucket(pr) === bucket.value),
  )

  // A silent refresh (live events) leaves loading alone so the empty state
  // does not flicker while the list is fetched again.
  async function refresh({ silent = false } = {}): Promise<void> {
    if (!silent) loading.value = true
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

  return {
    summary,
    prs,
    queue,
    visible,
    bucket,
    loading,
    error,
    refresh,
    setBucket,
  }
})
