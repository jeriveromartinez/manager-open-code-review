import { defineStore } from 'pinia'
import { ref } from 'vue'
import { openEventStream, type ReviewEvent } from '../api'

// Sent to every subscriber when the stream comes back after a drop: events
// emitted while it was down are lost, so each view refetches what it shows.
export const RESYNC: ReviewEvent = { type: 'resync', id: '' }

type Subscription = { types: Set<string>; handler: (event: ReviewEvent) => void }

export const useLiveStore = defineStore('live', () => {
  const connected = ref(false)
  const subscriptions = new Set<Subscription>()
  let close: (() => void) | null = null
  let dropped = false

  function dispatch(event: ReviewEvent): void {
    for (const subscription of subscriptions) {
      if (event.type === RESYNC.type || subscription.types.has(event.type)) {
        subscription.handler(event)
      }
    }
  }

  function onState(isConnected: boolean): void {
    connected.value = isConnected
    if (!isConnected) {
      dropped = true
      return
    }
    if (dropped) {
      dropped = false
      dispatch(RESYNC)
    }
  }

  function start(): void {
    if (close) return
    close = openEventStream(dispatch, onState)
  }

  function stop(): void {
    close?.()
    close = null
    dropped = false
    connected.value = false
  }

  function subscribe(types: string[], handler: (event: ReviewEvent) => void): () => void {
    const subscription = { types: new Set(types), handler }
    subscriptions.add(subscription)
    return () => {
      subscriptions.delete(subscription)
    }
  }

  return { connected, start, stop, subscribe }
})
