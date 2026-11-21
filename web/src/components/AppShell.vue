<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import RoleSwitch from './RoleSwitch.vue'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()
const route = useRoute()

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
</script>

<template>
  <div class="shell" :class="{ 'shell--wide': route.meta.wide }">
    <header class="shell__bar">
      <RouterLink to="/" class="shell__brand" data-brand>QA Control Center</RouterLink>

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
