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
  <span
    class="dot-bar"
    :role="label ? 'img' : undefined"
    :aria-label="label"
    :aria-hidden="label ? undefined : 'true'"
  >
    <span class="dot-bar__fill" :class="`dot-bar__fill--${tone}`" :style="{ width }" />
  </span>
</template>
