<script setup lang="ts">
import { computed } from 'vue'
import type { DotTone } from '../dots'

const props = withDefaults(
  defineProps<{
    count: number
    tone: DotTone
    // Without a label the bar is decoration next to text that already says it.
    label?: string
    max?: number
  }>(),
  { label: undefined, max: 40 },
)

const PITCH = 7

const width = computed(() => `${Math.min(Math.max(0, props.count), props.max) * PITCH}px`)
</script>

<template>
  <span v-if="label" class="dot-bar" role="img" :aria-label="label">
    <span class="dot-bar__fill" :class="`dot-bar__fill--${tone}`" :style="{ width }" />
  </span>
  <span v-else class="dot-bar" aria-hidden="true">
    <span class="dot-bar__fill" :class="`dot-bar__fill--${tone}`" :style="{ width }" />
  </span>
</template>
