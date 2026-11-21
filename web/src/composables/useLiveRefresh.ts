import { onBeforeUnmount } from 'vue'
import type { ReviewEvent } from '../api'
import { RESYNC, useLiveStore } from '../stores/live'

// Writes often come in bursts (a review run touches the PR several times), so
// a view waits for the burst to settle and refetches once.
export const LIVE_REFRESH_DELAY = 300

// reload receives every event of the burst, for views that must know which
// item changed (to warn about it) rather than only that something did.
export function useLiveRefresh(
  types: string[],
  reload: (events: ReviewEvent[]) => unknown,
  filter?: (event: ReviewEvent) => boolean,
): void {
  const live = useLiveStore()
  let timer: ReturnType<typeof setTimeout> | null = null
  let burst: ReviewEvent[] = []

  const unsubscribe = live.subscribe(types, (event) => {
    if (event.type !== RESYNC.type && filter && !filter(event)) return
    burst.push(event)
    if (timer !== null) clearTimeout(timer)
    timer = setTimeout(() => {
      const events = burst
      burst = []
      timer = null
      void reload(events)
    }, LIVE_REFRESH_DELAY)
  })

  onBeforeUnmount(() => {
    unsubscribe()
    if (timer !== null) clearTimeout(timer)
  })
}
