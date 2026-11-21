<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import DotBar from '../components/DotBar.vue'
import DotLoader from '../components/DotLoader.vue'
import DotRing from '../components/DotRing.vue'
import KpiCard from '../components/KpiCard.vue'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { api, type PRView, type QAPolicy, type QASummary, type Repository } from '../api'
import type { DotTone } from '../dots'
import { isForbidden } from '../errors'
import { blockedAt, decisionRows, thresholds, type Threshold } from '../leadPanel'
import { formatWait, isOverSla, qaBucket, waitMinutes } from '../qaQueue'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const teams = ref<{ id: number; name: string }[]>([])
const teamId = ref<number | null>(null)
const prs = ref<PRView[]>([])
const repos = ref<Repository[]>([])
const summary = ref<QASummary | null>(null)
const policy = ref<QAPolicy | null>(null)
const loading = ref(true)
const error = ref<string | null>(null)
const now = ref(new Date())
let clock: ReturnType<typeof setInterval> | null = null

const thresholdLabels: Record<Threshold, string> = {
  blocker: 'Bloqueante',
  major: 'Mayor',
  minor: 'Menor',
  nit: 'Nit',
}

const team = computed(() => teams.value.find((item) => item.id === teamId.value) ?? null)
const slaSeconds = computed(() => summary.value?.validation_sla_seconds ?? 0)
// getPolicy serves the defaults with id 0 when the team never saved a policy.
const hasPolicy = computed(() => (policy.value?.id ?? 0) !== 0)

const buckets = computed(() => {
  const counts = { ready: 0, ai: 0, problems: 0 }
  for (const pr of prs.value) {
    const bucket = qaBucket(pr)
    if (bucket === 'ready') counts.ready++
    else if (bucket === 'ai') counts.ai++
    else counts.problems++
  }
  return counts
})

const qaWait = computed(() => {
  const ready = prs.value.filter((pr) => qaBucket(pr) === 'ready')
  const oldest = Math.max(0, ...ready.map((pr) => waitMinutes(pr, now.value)))
  return {
    ready: ready.length,
    late: ready.filter((pr) => isOverSla(pr, slaSeconds.value, now.value)).length,
    oldest: ready.length > 0 ? formatWait(oldest) : '—',
  }
})

const simulation = computed(() =>
  thresholds.map((threshold) => ({
    threshold,
    label: thresholdLabels[threshold],
    blocked: blockedAt(prs.value, threshold),
    current: policy.value?.blocking_threshold === threshold,
  })),
)

const precision = computed(() => {
  const confirmed = summary.value?.confirmed ?? 0
  const falsePositive = summary.value?.false_positive ?? 0
  const decided = confirmed + falsePositive
  return { confirmed, falsePositive, decided, percent: decided === 0 ? 0 : Math.round((confirmed / decided) * 100) }
})

// Same window as the per-repo rows, not the precision window.
const confirmedInRepos = computed(() =>
  (summary.value?.confirmed_by_repo ?? []).reduce((sum, item) => sum + item.count, 0),
)

const byRepo = computed(() =>
  (summary.value?.confirmed_by_repo ?? []).map((item, index) => ({
    ...item,
    tone: (index === 0 ? 'bad' : 'on') as DotTone,
  })),
)

const decisions = computed(() => decisionRows(prs.value, slaSeconds.value, now.value))

async function loadTeams(): Promise<void> {
  const me = await session.load()
  if (!me) return
  if (me.is_admin) {
    teams.value = (await api.teams()).map((item) => ({ id: item.id, name: item.name }))
  } else {
    teams.value = me.teams.filter((item) => item.role === 'lead').map((item) => ({ id: item.id, name: item.name }))
  }
  teamId.value = teams.value[0]?.id ?? null
}

// Each load gets a number; an answer that arrives after a newer load started
// belongs to a team that is no longer on screen and is dropped.
let latestLoad = 0

async function loadTeam({ silent = false } = {}): Promise<void> {
  const id = teamId.value
  if (id === null) return
  const load = ++latestLoad
  if (!silent) loading.value = true
  error.value = null
  try {
    const [allPrs, nextSummary, nextPolicy, allRepos] = await Promise.all([
      api.prs(),
      api.qaSummary(id),
      api.getPolicy(id),
      api.repos(),
    ])
    if (load !== latestLoad) return
    prs.value = allPrs.filter((pr) => pr.team_id === id)
    repos.value = allRepos.filter((repo) => repo.team_id === id)
    summary.value = nextSummary
    policy.value = nextPolicy
  } catch (err) {
    if (load !== latestLoad) return
    prs.value = []
    repos.value = []
    summary.value = null
    policy.value = null
    error.value = isForbidden(err)
      ? 'Solo el lead del equipo o un administrador pueden ver este panel'
      : 'No se pudo cargar el equipo'
  } finally {
    if (load === latestLoad) loading.value = false
  }
}

useLiveRefresh(['pr.updated', 'repo.updated', 'team.updated'], () => loadTeam({ silent: true }))

async function selectTeam(id: number): Promise<void> {
  teamId.value = id
  await loadTeam()
}

onMounted(async () => {
  // Started before any await so onBeforeUnmount always finds it to clear.
  clock = setInterval(() => {
    now.value = new Date()
  }, 60_000)
  try {
    await loadTeams()
  } catch {
    error.value = 'No se pudieron cargar tus equipos'
  }
  if (teamId.value === null) {
    loading.value = false
    return
  }
  await loadTeam()
})

onBeforeUnmount(() => {
  if (clock !== null) clearInterval(clock)
})
</script>

<template>
  <section class="lead">
    <header class="page-header">
      <div class="lead__title">
        <h1 class="page-title">{{ team ? `Equipo ${team.name}` : 'Mi equipo' }}</h1>
        <p v-if="session.me" class="page-subtitle">
          Hola, {{ session.me.login }} · {{ prs.length }} PRs abiertos · {{ repos.length }} repositorio{{ repos.length === 1 ? '' : 's' }}
        </p>
      </div>
    </header>

    <div v-if="teams.length > 1" class="filter-tabs" role="group" aria-label="Equipo">
      <button
        v-for="item in teams"
        :key="item.id"
        type="button"
        class="filter-tab"
        :class="{ 'filter-tab--active': item.id === teamId }"
        :aria-pressed="item.id === teamId"
        :data-team="item.id"
        @click="selectTeam(item.id)"
      >
        {{ item.name }}
      </button>
    </div>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <DotLoader v-else-if="loading" />
    <p v-else-if="teams.length === 0" class="empty-state" data-no-teams>
      No lideras ningún equipo. Un administrador puede asignarte como lead en Equipos.
    </p>

    <template v-else-if="team">
      <section class="tile-grid" aria-label="Resumen del equipo">
        <article class="tile" data-tile="state">
          <h2 class="tile__title"><span class="tile__index">01</span>Estado del equipo</h2>
          <div class="ring-row">
            <DotRing
              :segments="[
                { value: buckets.ready, tone: 'on' },
                { value: buckets.ai, tone: 'dim' },
                { value: buckets.problems, tone: 'bad' },
              ]"
              :label="`${prs.length} PRs abiertos: ${buckets.ready} listos, ${buckets.ai} en evaluación, ${buckets.problems} bloqueados`"
            >
              <span class="ring-row__figure">{{ prs.length }}</span>
              <span class="caption">Abiertos</span>
            </DotRing>
            <dl class="legend">
              <div>
                <dt class="caption"><span class="dot dot--on" />Listos</dt>
                <dd>{{ buckets.ready }}</dd>
              </div>
              <div>
                <dt class="caption"><span class="dot dot--dim" />Evaluando</dt>
                <dd>{{ buckets.ai }}</dd>
              </div>
              <div>
                <dt class="caption"><span class="dot dot--bad" />Bloqueados</dt>
                <dd class="is-late" data-blocked-count>{{ buckets.problems }}</dd>
              </div>
            </dl>
          </div>
        </article>

        <article class="tile" :class="{ 'tile--alert': qaWait.late > 0 }" data-tile="qa-wait">
          <h2 class="tile__title"><span class="tile__index">02</span>Espera de QA</h2>
          <KpiCard label="Listos para validar" :value="qaWait.ready" />
          <dl class="stats tile__foot">
            <div>
              <dt class="caption">Fuera de plazo</dt>
              <dd :class="{ 'is-late': qaWait.late > 0 }" data-late-count>{{ qaWait.late }}</dd>
            </div>
            <div>
              <dt class="caption">El más antiguo</dt>
              <dd>{{ qaWait.oldest }}</dd>
            </div>
          </dl>
        </article>

        <article class="tile" :class="{ 'tile--alert': !hasPolicy }" data-tile="policy">
          <h2 class="tile__title"><span class="tile__index">03</span>Política activa</h2>
          <template v-if="hasPolicy && policy">
            <p class="policy__headline">
              Bloquea desde <span class="is-late">{{ thresholdLabels[policy.blocking_threshold as Threshold] ?? policy.blocking_threshold }}</span>
            </p>
            <p class="tile__hint">
              {{ policy.max_passes }} pases como máximo · CI verde {{ policy.require_green_ci ? 'requerido' : 'no requerido' }}
            </p>
            <table class="simulation">
              <caption class="caption">PRs abiertos que quedarían bloqueados con cada umbral</caption>
              <tbody>
                <tr
                  v-for="row in simulation"
                  :key="row.threshold"
                  :class="{ 'simulation__row--current': row.current }"
                  :data-threshold="row.threshold"
                >
                  <th scope="row">{{ row.label }}{{ row.current ? ' (actual)' : '' }}</th>
                  <td>{{ row.blocked }}</td>
                </tr>
              </tbody>
            </table>
          </template>
          <template v-else>
            <p class="tile__hint" data-no-policy>Sin política: los PRs de este equipo no se revisan.</p>
          </template>
          <RouterLink v-if="session.me?.is_admin" class="button tile__foot" to="/agents">Ajustar política →</RouterLink>
        </article>

        <article class="tile" data-tile="precision">
          <h2 class="tile__title">
            <span class="tile__index">04</span>Precisión de la IA · {{ summary?.precision_window_days ?? 7 }} días
          </h2>
          <div class="ring-row">
            <DotRing
              :segments="[
                { value: precision.confirmed, tone: 'on' },
                { value: precision.falsePositive, tone: 'bad' },
              ]"
              :label="
                precision.decided > 0
                  ? `${precision.percent}% de hallazgos confirmados en el equipo`
                  : 'Acierto sin datos'
              "
            >
              <span v-if="precision.decided > 0" class="ring-row__figure" data-precision>{{ precision.percent }}%</span>
              <span v-else class="ring-row__figure" aria-hidden="true">—</span>
              <span class="caption">Acierto</span>
            </DotRing>
            <dl class="legend">
              <div>
                <dt class="caption"><span class="dot dot--on" />Confirmados</dt>
                <dd>{{ precision.confirmed }}</dd>
              </div>
              <div>
                <dt class="caption"><span class="dot dot--bad" />Falsos positivos</dt>
                <dd :class="{ 'is-late': precision.falsePositive > 0 }">{{ precision.falsePositive }}</dd>
              </div>
            </dl>
          </div>
          <p v-if="precision.decided === 0" class="tile__empty">Aún no hay hallazgos triados en este periodo.</p>
        </article>

        <article class="tile" data-tile="repos-defects">
          <h2 class="tile__title">
            <span class="tile__index">05</span>Defectos por repo · {{ summary?.category_window_days ?? 30 }} días
          </h2>
          <KpiCard label="Confirmados por QA" :value="confirmedInRepos" />
          <ul v-if="byRepo.length > 0" class="bars tile__foot">
            <li v-for="row in byRepo" :key="row.repo" class="bars__row">
              <span class="caption">{{ row.repo }}</span>
              <DotBar :count="row.count" :tone="row.tone" :label="`${row.count} defectos confirmados en ${row.repo}`" />
              <span class="bars__count">{{ row.count }}</span>
            </li>
          </ul>
          <div v-else class="empty-bar tile__foot">
            <p class="tile__empty">Sin defectos confirmados todavía.</p>
            <DotBar :count="0" tone="on" />
          </div>
        </article>

        <article class="tile" data-tile="coverage">
          <h2 class="tile__title"><span class="tile__index">06</span>Cobertura de repos</h2>
          <KpiCard
            label="Con revisión automática"
            :value="repos.filter((repo) => repo.enabled && repo.has_policy).length"
          />
          <ul v-if="repos.length > 0" class="rows tile__foot">
            <li v-for="repo in repos" :key="repo.id" class="rows__item" :data-repo="repo.full_name">
              <span>{{ repo.full_name }}</span>
              <span :class="repo.enabled && repo.has_policy ? 'is-ok' : 'is-late'">
                {{ !repo.enabled ? 'Deshabilitado' : repo.has_policy ? 'Activa' : 'Sin política' }}
              </span>
            </li>
          </ul>
          <p v-else class="tile__empty">El equipo no tiene repositorios asignados.</p>
        </article>
      </section>

      <section class="pr-section" aria-labelledby="decisions-title">
        <div class="pr-section__head">
          <h2 id="decisions-title" class="pr-section__title">Necesitan una decisión</h2>
          <span class="pr-section__count">{{ decisions.length }}</span>
        </div>
        <p v-if="decisions.length === 0" class="empty-state">Nada bloqueado ni fuera de plazo.</p>
        <div v-else class="decisions">
          <table>
            <thead>
              <tr>
                <th scope="col">PR</th>
                <th scope="col">Motivo</th>
                <th scope="col">Espera</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in decisions" :key="row.pr.id" data-decision>
                <td>
                  <RouterLink :to="`/prs/${row.pr.id}`">{{ row.pr.title }}</RouterLink>
                  <br />
                  <span class="caption">{{ row.pr.repo }} #{{ row.pr.number }} · por {{ row.pr.author_login }}</span>
                </td>
                <td class="decisions__reason">{{ row.reason }}</td>
                <td class="decisions__wait">{{ row.wait }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </template>
  </section>
</template>

<style scoped>
.lead {
  display: flex;
  flex-direction: column;
  gap: 36px;
}

.lead__title {
  min-width: 0;
}

.tile__hint {
  color: var(--text-muted);
  font-size: 13px;
}

.tile__empty {
  color: var(--text-faint);
  font-size: 13px;
}

.is-late {
  color: var(--accent);
  font-weight: 700;
}

.is-ok {
  color: var(--ok);
}

.ring-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
}

.ring-row__figure {
  font-family: var(--font-display);
  font-size: 40px;
  font-weight: 900;
  line-height: 1;
}

.legend,
.bars,
.rows {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  margin: 0;
  padding: 0;
  list-style: none;
}

.legend .dot {
  display: inline-block;
  margin-right: var(--space-2);
}

.legend dd,
.stats dd {
  margin-top: var(--space-1);
  font-family: var(--font-display);
  font-size: 26px;
  font-weight: 800;
  font-variant-numeric: tabular-nums;
}

.stats {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3);
}

.stats > div + div {
  padding-left: var(--space-3);
  border-left: var(--rule-hair) solid var(--border);
}

.policy__headline {
  font-family: var(--font-display);
  font-size: 28px;
  font-weight: 900;
  line-height: 1.1;
  text-transform: uppercase;
}

.simulation {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.simulation caption {
  padding-bottom: var(--space-2);
  text-align: left;
}

.simulation th,
.simulation td {
  padding: 6px 0;
  border-bottom: var(--rule-hair) dashed var(--border);
  background: transparent;
  font-weight: 500;
  text-align: left;
  text-transform: none;
  letter-spacing: 0;
}

.simulation td {
  text-align: right;
  font-weight: 700;
}

.simulation__row--current th,
.simulation__row--current td {
  color: var(--accent);
}

.bars__row {
  display: grid;
  grid-template-columns: minmax(0, 140px) minmax(0, 1fr) 32px;
  align-items: center;
  gap: 10px;
}

.bars__row .caption {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.bars__count {
  text-align: right;
  font-size: 12px;
  font-weight: 700;
}

.rows {
  gap: 0;
}

.rows__item {
  display: flex;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-2) 0;
  border-bottom: var(--rule-hair) dashed var(--border);
  font-size: 13px;
  overflow-wrap: anywhere;
}

.rows__item:last-child {
  border-bottom: 0;
}

.decisions {
  overflow-x: auto;
  background: var(--surface);
  border: var(--rule-hair) solid var(--tile-border);
  border-radius: var(--radius-lg);
}

.decisions table {
  min-width: 640px;
}

.decisions__reason {
  color: var(--accent);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.04em;
}

.decisions__wait {
  font-weight: 800;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
</style>
