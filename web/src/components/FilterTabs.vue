<script setup lang="ts">
import type { BucketFilter } from '../stores/prs'

const props = defineProps<{
  modelValue: BucketFilter
  counts: Record<BucketFilter, number>
}>()

const emit = defineEmits<{
  'update:modelValue': [value: BucketFilter]
}>()

const tabs: { label: string; value: BucketFilter }[] = [
  { label: 'Para validar', value: 'ready' },
  { label: 'Con defectos', value: 'defects' },
  { label: 'Inconclusos', value: 'inconclusive' },
  { label: 'En revisión IA', value: 'ai' },
  { label: 'En pausa o aprobación', value: 'other' },
  { label: 'Todos', value: '' },
]
</script>

<template>
  <div class="filter-tabs" role="tablist" aria-label="Filtrar pull requests">
    <button
      v-for="tab in tabs"
      :key="tab.label"
      type="button"
      role="tab"
      class="filter-tab"
      :class="{ 'filter-tab--active': props.modelValue === tab.value }"
      :aria-selected="props.modelValue === tab.value"
      :data-tab="tab.value || 'all'"
      @click="emit('update:modelValue', tab.value)"
    >
      {{ tab.label }}
      <span class="filter-tab__count">{{ props.counts[tab.value] }}</span>
    </button>
  </div>
</template>
