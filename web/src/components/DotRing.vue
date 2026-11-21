<script setup lang="ts">
import { computed } from 'vue'
import { ringDots, type DotSegment } from '../dots'

const props = withDefaults(
  defineProps<{
    segments: DotSegment[]
    label: string
    size?: number
    count?: number
  }>(),
  { size: 168, count: 54 },
)

const DOT = 5

const dots = computed(() => ringDots(props.segments, props.count, props.size / 2 - 8, props.size, DOT))
</script>

<template>
  <div class="dot-ring" role="img" :aria-label="label" :style="{ width: `${size}px`, height: `${size}px` }">
    <span
      v-for="(dot, index) in dots"
      :key="index"
      class="dot dot-ring__dot"
      :class="`dot--${dot.tone}`"
      :style="{ left: `${dot.x}px`, top: `${dot.y}px` }"
    />
    <div class="dot-ring__centre">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.dot-ring {
  position: relative;
  flex: none;
}

.dot-ring__dot {
  position: absolute;
  width: 5px;
  height: 5px;
}

.dot-ring__centre {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--space-1);
  text-align: center;
}
</style>
