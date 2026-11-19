<script setup lang="ts">
defineProps<{
  label: string
  value: number
  tone?: 'default' | 'ok' | 'busy' | 'bad'
}>()
</script>

<template>
  <article class="kpi-card" :class="`kpi-card--${tone ?? 'default'}`">
    <span class="kpi-card__value">{{ value }}</span>
    <span class="kpi-card__label">{{ label }}</span>
  </article>
</template>
