<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import type { PRView } from '../api'
import { prStatusLabel } from '../status'
import { formatWait, isOverSla, qaBucket, waitMinutes } from '../qaQueue'
import DotMeter from './DotMeter.vue'

const props = defineProps<{
  pr: PRView
  slaSeconds: number
  now: Date
}>()

const status = computed(() => prStatusLabel(props.pr))
const bucket = computed(() => qaBucket(props.pr))
const overSla = computed(() => isOverSla(props.pr, props.slaSeconds, props.now))
const waiting = computed(() => {
  const text = formatWait(waitMinutes(props.pr, props.now))
  return bucket.value === 'ready' ? `espera validación ${text}` : `en este estado ${text}`
})

const ctas: Partial<Record<string, { text: string; label: string }>> = {
  ready: { text: 'Validar →', label: 'Validar' },
  defects: { text: 'Ver defectos →', label: 'Ver defectos de' },
  inconclusive: { text: 'Ver motivo →', label: 'Ver motivo de' },
}
const ctaFor = computed(() => ctas[bucket.value] ?? { text: 'Ver detalle →', label: 'Ver detalle de' })
const cta = computed(() => ctaFor.value.text)
// The arrow is decoration; screen readers hear the action and the PR instead.
const ctaLabel = computed(() => ctaFor.value.label)

const validationDots = computed(() => {
  const { passed, total } = props.pr.validations
  const rest = status.value.tone === 'bad' ? 'bad' : 'off'
  return [
    { count: passed, tone: 'on' as const },
    { count: total - passed, tone: rest as 'bad' | 'off' },
  ]
})
</script>

<template>
  <article class="pr-card">
    <header class="pr-card__head">
      <span class="pr-card__repo">{{ pr.repo }} #{{ pr.number }}</span>
      <span class="badge pr-card__status" :class="`badge--${status.tone}`">{{ status.tag }}</span>
    </header>

    <RouterLink class="pr-card__link" :to="`/prs/${pr.id}`">{{ pr.title }}</RouterLink>

    <p class="pr-card__meta">
      <span class="pr-card__author">por {{ pr.author_login }}</span> ·
      <span :class="{ 'pr-card__late': overSla }" data-wait>
        {{ waiting }}{{ overSla ? ' · fuera de plazo' : '' }}
      </span>
    </p>

    <dl class="pr-card__findings">
      <div>
        <dt class="caption">Bloq.</dt>
        <dd :class="{ 'pr-card__count--bad': pr.open_findings.blocker > 0 }" data-open-blockers>
          {{ pr.open_findings.blocker }}
        </dd>
      </div>
      <div>
        <dt class="caption">Mayores</dt>
        <dd>{{ pr.open_findings.major }}</dd>
      </div>
      <div>
        <dt class="caption">Menores</dt>
        <dd>{{ pr.open_findings.minor }}</dd>
      </div>
    </dl>

    <div v-if="pr.validations.total > 0" class="pr-card__validations">
      <span class="caption">{{ pr.validations.passed }}/{{ pr.validations.total }} validaciones correctas</span>
      <DotMeter
        large
        :segments="validationDots"
        :label="`${pr.validations.passed} de ${pr.validations.total} validaciones correctas`"
      />
    </div>

    <p class="pr-card__action">{{ status.action }}</p>

    <footer class="pr-card__footer">
      <RouterLink
        class="button"
        :class="bucket === 'ready' ? 'button--light' : ''"
        :to="`/prs/${pr.id}`"
        :aria-label="`${ctaLabel} ${pr.repo} #${pr.number}`"
        data-cta
      >
        {{ cta }}
      </RouterLink>
      <a class="pr-card__github" :href="pr.html_url" target="_blank" rel="noreferrer">GitHub ↗</a>
    </footer>
  </article>
</template>

<style scoped>
.pr-card__late {
  color: var(--accent);
  font-weight: 700;
}

.pr-card__findings {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: var(--space-2);
}

.pr-card__findings > div + div {
  padding-left: var(--space-3);
  border-left: var(--rule-hair) solid var(--border);
}

.pr-card__findings dd {
  margin-top: var(--space-1);
  font-family: var(--font-display);
  font-size: 22px;
  font-weight: 900;
  font-variant-numeric: tabular-nums;
}

.pr-card__count--bad {
  color: var(--accent);
}

.pr-card__validations {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.pr-card__github {
  flex: none;
  color: var(--text-muted);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-decoration: none;
}
</style>
