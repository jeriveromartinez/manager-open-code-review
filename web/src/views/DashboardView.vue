<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import FilterTabs from '../components/FilterTabs.vue'
import KpiCard from '../components/KpiCard.vue'
import PrCard from '../components/PrCard.vue'
import { api, type PRGroup, type Readiness } from '../api'
import { usePrsStore } from '../stores/prs'
import { useSessionStore } from '../stores/session'

const prsStore = usePrsStore()
const session = useSessionStore()

const readiness = ref<Readiness | null>(null)

const readinessLinkLabels: Record<string, string> = {
  '/repos': 'Ir a repositorios',
  '/teams': 'Ir a equipos',
  '/agents': 'Ir a perfiles LLM',
}

function readinessLinkLabel(link: string): string {
  return readinessLinkLabels[link] ?? 'Resolver'
}

async function loadReadiness(): Promise<void> {
  const me = await session.load()
  if (!me?.is_admin) {
    readiness.value = null
    return
  }
  try {
    readiness.value = await api.readiness()
  } catch {
    readiness.value = null
  }
}

const kpis = computed(() => [
  { key: 'open', label: 'PRs abiertos', value: prsStore.stats.open, tone: 'default' as const },
  {
    key: 'awaiting_human',
    label: 'Esperando tu revisión',
    value: prsStore.stats.awaiting_human,
    tone: 'ok' as const,
  },
  {
    key: 'in_evaluation',
    label: 'En evaluación',
    value: prsStore.stats.in_evaluation,
    tone: 'busy' as const,
  },
  {
    key: 'with_problems',
    label: 'Con problemas',
    value: prsStore.stats.with_problems,
    tone: 'bad' as const,
  },
])

async function changeGroup(group: PRGroup): Promise<void> {
  await prsStore.setGroup(group)
}

onMounted(() => {
  void loadReadiness()
  void prsStore.refresh()
  prsStore.start()
})

onBeforeUnmount(() => {
  prsStore.stop()
})
</script>

<template>
  <section class="dashboard">
    <header class="page-header">
      <div>
        <h1 class="page-title">QA Control Center</h1>
        <p class="page-subtitle">Todos los repositorios</p>
      </div>
      <span
        class="badge"
        :class="prsStore.connected ? 'badge--ok' : 'badge--muted'"
        data-service-badge
        :title="
          prsStore.connected
            ? 'Eventos en vivo conectados'
            : 'Sin conexión con el servicio de eventos'
        "
      >
        {{ prsStore.connected ? 'Servicio activo' : 'Reconectando…' }}
      </span>
    </header>

    <section
      v-if="session.me?.is_admin && readiness && !readiness.ready"
      class="readiness"
      data-readiness-banner
      aria-label="Configuración pendiente"
    >
      <div>
        <h2 class="readiness__title">Falta configuración para revisar</h2>
        <p class="readiness__hint">
          Completa estos pasos para que la revisión automática se ejecute.
        </p>
      </div>
      <ul class="readiness__list">
        <li
          v-for="item in readiness.items"
          :key="item.key"
          class="readiness__item"
          :data-readiness-item="item.key"
        >
          <span>{{ item.label }}</span>
          <RouterLink class="readiness__link" :to="item.link">
            {{ readinessLinkLabel(item.link) }}
          </RouterLink>
        </li>
      </ul>
    </section>

    <div class="kpi-grid">
      <KpiCard
        v-for="kpi in kpis"
        :key="kpi.key"
        :data-kpi="kpi.key"
        :label="kpi.label"
        :value="kpi.value"
        :tone="kpi.tone"
      />
    </div>

    <FilterTabs :model-value="prsStore.group" @update:model-value="changeGroup" />

    <section class="pr-section">
      <header class="pr-section__head">
        <h2 class="pr-section__title">Pull Requests</h2>
        <span class="pr-section__count">
          {{ prsStore.repoCount }} repositorio{{ prsStore.repoCount === 1 ? '' : 's' }}
        </span>
      </header>

      <p v-if="prsStore.error" class="empty-state">{{ prsStore.error }}</p>
      <p v-else-if="!prsStore.loading && prsStore.prs.length === 0" class="empty-state">
        No hay PRs abiertos en tus repositorios
      </p>
      <div v-else class="pr-grid">
        <PrCard v-for="pr in prsStore.prs" :key="pr.id" :pr="pr" />
      </div>
    </section>
  </section>
</template>

<style scoped>
.readiness {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--bad-soft);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
}

.readiness__title {
  font-size: 16px;
  font-weight: 600;
  color: var(--bad);
}

.readiness__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 14px;
}

.readiness__list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  margin: 0;
  padding-left: var(--space-5);
}

.readiness__item {
  color: var(--text);
}

.readiness__link {
  margin-left: var(--space-2);
  color: var(--bad);
  font-weight: 600;
}
</style>
