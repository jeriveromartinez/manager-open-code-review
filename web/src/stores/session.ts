import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, type Me } from '../api'

export const useSessionStore = defineStore('session', () => {
  const me = ref<Me | null>(null)
  const loading = ref(false)
  const loaded = ref(false)
  // Pages and the shell all ask for the session on mount; they share one request.
  let pending: Promise<Me | null> | null = null

  const isAdmin = computed(() => me.value?.is_admin === true)

  function load(force = false): Promise<Me | null> {
    if (loaded.value && !force) {
      return Promise.resolve(me.value)
    }
    if (pending && !force) {
      return pending
    }

    loading.value = true
    const request: Promise<Me | null> = fetchMe().finally(() => {
      if (pending === request) pending = null
    })
    pending = request
    return request
  }

  async function fetchMe(): Promise<Me | null> {
    try {
      me.value = await api.me()
    } catch {
      // 401: el handler global del API ya redirige a /login.
      me.value = null
    } finally {
      loaded.value = true
      loading.value = false
    }
    return me.value
  }

  function reset(): void {
    me.value = null
    loaded.value = false
  }

  return { me, loading, loaded, isAdmin, load, reset }
})
