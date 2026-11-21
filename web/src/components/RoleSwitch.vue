<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()
const route = useRoute()

// Only people who lead a team (or admins, who see every team) have a second view.
const visible = computed(() => {
  const me = session.me
  return !!me && (me.is_admin || me.teams.some((team) => team.role === 'lead'))
})
const onLead = computed(() => route.path === '/lead')
</script>

<template>
  <nav v-if="visible" class="role-switch" aria-label="Vista por rol">
    <RouterLink to="/" class="role-switch__link" :aria-current="onLead ? undefined : 'page'" data-role-qa>
      QA
    </RouterLink>
    <RouterLink to="/lead" class="role-switch__link" :aria-current="onLead ? 'page' : undefined" data-role-lead>
      Team lead
    </RouterLink>
  </nav>
</template>

<style scoped>
.role-switch {
  display: inline-flex;
  gap: var(--space-1);
  padding: var(--space-1);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
}

.role-switch__link {
  display: inline-flex;
  align-items: center;
  min-height: 36px;
  padding: 0 var(--space-4);
  border-radius: var(--radius-pill);
  color: var(--text-muted);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-decoration: none;
  text-transform: uppercase;
}

.role-switch__link:hover {
  color: var(--text);
}

.role-switch__link[aria-current='page'] {
  background: var(--accent);
  color: var(--accent-ink);
}
</style>
