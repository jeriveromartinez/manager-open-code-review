<script setup lang="ts">
import { computed } from 'vue'
import type { DotTone } from '../dots'

const props = defineProps<{
  segments: { count: number; tone: DotTone }[]
  label: string
  large?: boolean
}>()

const dots = computed(() =>
  props.segments.flatMap((segment) =>
    Array.from({ length: Math.max(0, segment.count) }, () => segment.tone),
  ),
)
</script>

<template>
  <div class="dots" :class="{ 'dots--large': large }" role="img" :aria-label="label">
    <span v-for="(tone, index) in dots" :key="index" class="dot" :class="`dot--${tone}`" />
  </div>
</template>
