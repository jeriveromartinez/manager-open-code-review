<script setup lang="ts">
import type { PRGroup } from '../api'

const props = defineProps<{
  modelValue: PRGroup
}>()

const emit = defineEmits<{
  'update:modelValue': [value: PRGroup]
}>()

const tabs: { label: string; value: PRGroup }[] = [
  { label: 'Todos', value: '' },
  { label: 'En revisión', value: 'in_review' },
  { label: 'Bloqueados', value: 'blocked' },
  { label: 'Listos', value: 'ready' },
]
</script>

<template>
  <div class="filter-tabs" role="tablist" aria-label="Filtrar Pull Requests">
    <button
      v-for="tab in tabs"
      :key="tab.label"
      type="button"
      role="tab"
      class="filter-tab"
      :class="{ 'filter-tab--active': props.modelValue === tab.value }"
      :aria-selected="props.modelValue === tab.value"
      @click="emit('update:modelValue', tab.value)"
    >
      {{ tab.label }}
    </button>
  </div>
</template>
