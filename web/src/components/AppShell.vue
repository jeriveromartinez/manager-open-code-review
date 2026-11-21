<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, watch } from 'vue'
import { RouterLink } from 'vue-router'
import RoleSwitch from './RoleSwitch.vue'
import { useLiveStore } from '../stores/live'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()
const live = useLiveStore()

const links = computed(() => {
  const base = [
    { to: '/', label: 'Mi cola' },
    { to: '/repos', label: 'Repositorios' },
    { to: '/teams', label: 'Equipos' },
  ]
  if (!session.me?.is_admin) return base
  return [...base, { to: '/agents', label: 'Agentes LLM' }, { to: '/settings', label: 'Ajustes' }]
})

async function logout(): Promise<void> {
  live.stop()
  try {
    await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' })
  } finally {
    session.reset()
    window.location.assign('/login')
  }
}

// Every page needs the session for the navigation, not only the ones that read it.
onMounted(() => {
  void session.load()
})

// One stream per tab, open only while someone is signed in.
watch(
  () => session.me,
  (me) => (me ? live.start() : live.stop()),
  { immediate: true },
)

onBeforeUnmount(() => {
  live.stop()
})
</script>

<template>
  <div class="shell">
    <header class="shell__bar">
      <div class="shell__identity">
        <span
          v-if="session.me"
          class="live"
          :class="{ 'live--on': live.connected }"
          data-service-badge
          aria-live="polite"
          :title="live.connected ? 'Eventos en vivo conectados' : 'Sin conexión con el servicio de eventos'"
        ><span class="visually-hidden">{{ live.connected ? 'En vivo' : 'Reconectando…' }}</span></span>
        <RouterLink to="/" class="shell__brand" data-brand>QA Control Center</RouterLink>
      </div>

      <div v-if="session.me" class="shell__aside">
        <RoleSwitch />
        <nav class="shell__nav" aria-label="Principal">
          <RouterLink
            v-for="link in links"
            :key="link.to"
            :to="link.to"
            class="pill-link"
            :data-nav="link.to"
          >
            {{ link.label }}
          </RouterLink>
          <button
            type="button"
            class="pill-link"
            data-logout
            :aria-label="`Salir (${session.me.login})`"
            @click="logout"
          >
            Salir
          </button>
        </nav>
      </div>
    </header>

    <main class="container">
      <slot />
    </main>
  </div>
</template>
