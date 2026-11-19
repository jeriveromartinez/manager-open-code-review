<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import FilterTabs from '../components/FilterTabs.vue'
import KpiCard from '../components/KpiCard.vue'
import PrCard from '../components/PrCard.vue'
import type { PRGroup } from '../api'
import { usePrsStore } from '../stores/prs'
import { useSessionStore } from '../stores/session'

const prsStore = usePrsStore()
const session = useSessionStore()

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
  void session.load()
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
