import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api, type Me } from '../api'

export const useSessionStore = defineStore('session', () => {
  const me = ref<Me | null>(null)
  const loading = ref(false)
  const loaded = ref(false)

  const isAdmin = computed(() => me.value?.is_admin === true)

  async function load(force = false): Promise<Me | null> {
    if (loaded.value && !force) {
      return me.value
    }

    loading.value = true
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
