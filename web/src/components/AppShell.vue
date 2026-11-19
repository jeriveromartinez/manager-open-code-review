<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const initial = computed(() => (session.me?.login ?? '?').slice(0, 1).toUpperCase())

async function logout(): Promise<void> {
  try {
    await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' })
  } finally {
    session.reset()
    window.location.assign('/login')
  }
}
</script>

<template>
  <div class="shell">
    <header class="topbar">
      <RouterLink to="/" class="brand" aria-label="QA Control Center">
        <span class="brand__mark" aria-hidden="true">QA</span>
        <span class="brand__name">QA Control Center</span>
      </RouterLink>

      <nav class="topbar__nav" aria-label="Principal">
        <template v-if="session.me">
          <RouterLink to="/" class="nav-link">Panel</RouterLink>
          <RouterLink to="/repos" class="nav-link">Repositorios</RouterLink>
          <RouterLink to="/teams" class="nav-link">Equipos</RouterLink>
          <RouterLink v-if="session.me?.is_admin" to="/agents" class="nav-link">
            Agentes LLM
          </RouterLink>
        </template>
        <slot name="nav" />
      </nav>

      <div class="topbar__user">
        <img
          v-if="session.me?.avatar_url"
          class="avatar"
          :src="session.me.avatar_url"
          :alt="session.me.login"
          width="28"
          height="28"
        />
        <span v-else-if="session.me" class="avatar avatar--fallback" aria-hidden="true">{{ initial }}</span>
        <span v-if="session.me" class="topbar__login">{{ session.me.login }}</span>
        <button v-if="session.me" type="button" class="button button--ghost" @click="logout">Salir</button>
      </div>
    </header>

    <main class="container">
      <slot />
    </main>
  </div>
</template>

<style scoped>
.nav-link {
  padding: 6px 10px;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  font-size: 14px;
  font-weight: 550;
  text-decoration: none;
  white-space: nowrap;
}

.nav-link:hover {
  color: var(--text);
  background: var(--surface-2);
}

.nav-link.router-link-active {
  color: var(--accent);
  background: var(--accent-soft);
}
</style>
