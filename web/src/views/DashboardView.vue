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
      <div class="title-block">
        <div>
          <h1 class="page-title">QA Control Center</h1>
          <p class="page-subtitle">Estado de la cola de revisión por repositorio</p>
        </div>
        <dl class="title-block__fields">
          <div class="title-block__field">
            <dt>Ámbito</dt>
            <dd>Todos los repositorios</dd>
          </div>
          <div class="title-block__field">
            <dt>Cola</dt>
            <dd>Pull requests abiertos</dd>
          </div>
          <div class="title-block__field">
            <dt>Actualización</dt>
            <dd>Eventos en vivo</dd>
          </div>
        </dl>
      </div>
      <div class="service-status">
        <span class="service-status__label">Servicio de eventos</span>
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
      </div>
    </header>

    <section
      v-if="session.me?.is_admin && readiness && !readiness.ready"
      class="readiness"
      data-readiness-banner
      aria-label="Configuración pendiente"
    >
      <header class="register__head">
        <div>
          <h2 class="readiness__title">Configuración pendiente</h2>
          <p class="readiness__hint">
            Completa estos pasos para que la revisión automática se ejecute.
          </p>
        </div>
      </header>
      <ul class="readiness__list">
        <li
          v-for="item in readiness.items"
          :key="item.key"
          class="readiness__item"
          :data-readiness-item="item.key"
        >
          <span class="readiness__label">{{ item.label }}</span>
          <RouterLink class="button readiness__link" :to="item.link">
            {{ readinessLinkLabel(item.link) }}
          </RouterLink>
        </li>
      </ul>
    </section>

    <section class="register" aria-labelledby="dashboard-kpis">
      <header class="register__head">
        <div>
          <h2 id="dashboard-kpis" class="register__title">Resumen de la cola</h2>
          <p class="register__hint">Recuento de pull requests por estado.</p>
        </div>
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
    </section>

    <section class="register" aria-labelledby="dashboard-prs">
      <header class="register__head">
        <div>
          <h2 id="dashboard-prs" class="register__title">Registro de pull requests</h2>
          <p class="register__hint">Estado, autor y validaciones de cada PR.</p>
        </div>
        <span class="pr-section__count">
          {{ prsStore.repoCount }} repositorio{{ prsStore.repoCount === 1 ? '' : 's' }}
        </span>
      </header>

      <div class="pr-toolbar">
        <span class="pr-toolbar__label">Filtrar por estado</span>
        <FilterTabs :model-value="prsStore.group" @update:model-value="changeGroup" />
      </div>

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
.title-block {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-width: 0;
}

.title-block__fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, max-content));
  gap: var(--space-2) var(--space-5);
  margin: 0;
}

.title-block__field {
  min-width: 0;
}

.title-block__field dt {
  color: var(--text-faint);
  font-size: 13px;
}

.title-block__field dd {
  margin: 0;
  color: var(--text);
  font-size: 15px;
  line-height: 1.4;
}

.service-status {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-1);
  flex: none;
}

.service-status__label {
  color: var(--text-faint);
  font-size: 13px;
}

.service-status .badge {
  font-size: 13px;
}

.register {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-width: 0;
}

.register__head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-2) var(--space-4);
  flex-wrap: wrap;
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.register__title {
  font-size: 18px;
}

.register__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 13px;
}

.readiness {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--surface);
  border: var(--rule-hair) solid var(--border-strong);
  border-left: var(--rule-heavy) solid var(--bad);
}

.readiness .register__head {
  border-bottom: var(--rule-hair) solid var(--border);
}

.readiness__title {
  color: var(--bad);
  font-size: 16px;
}

.readiness__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 13px;
}

.readiness__list {
  display: flex;
  flex-direction: column;
  margin: 0;
  padding: 0;
  list-style: none;
}

.readiness__item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-4);
  padding: var(--space-2) 0;
  border-bottom: var(--rule-hair) solid var(--border);
  color: var(--text);
  font-size: 15px;
}

.readiness__item:last-child {
  border-bottom: 0;
}

.readiness__link {
  flex: none;
  min-height: calc(var(--space-5) + var(--space-4));
}

.pr-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-4);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-hair) solid var(--border);
}

.pr-toolbar__label {
  color: var(--text-muted);
  font-size: 13px;
}

.pr-toolbar :deep(.filter-tab) {
  min-height: calc(var(--space-5) + var(--space-4));
}

.pr-toolbar :deep(.filter-tab:active:not(.filter-tab--active)) {
  background: var(--surface-3);
  color: var(--text);
}

.pr-grid :deep(.pr-card__repo),
.pr-grid :deep(.pr-card__meta),
.pr-grid :deep(.pr-card__action),
.pr-grid :deep(.badge) {
  font-size: 13px;
}

@media (pointer: coarse) {
  .readiness__link,
  .pr-toolbar :deep(.filter-tab) {
    min-height: calc(var(--space-5) + var(--space-4) + var(--space-1));
  }
}

@media (max-width: 480px) {
  .service-status {
    flex-direction: row;
    align-items: center;
    gap: var(--space-2);
  }

  .register__title {
    font-size: 16px;
  }

  .readiness__item {
    align-items: flex-start;
  }

  .readiness__link {
    width: 100%;
  }
}
</style>
