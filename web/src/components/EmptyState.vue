<script setup lang="ts">
defineProps<{
  title: string
  actionLabel?: string
}>()

const emit = defineEmits<{
  action: []
}>()
</script>

<template>
  <div class="empty-state">
    <p class="empty-state__title">{{ title }}</p>
    <button
      v-if="actionLabel"
      type="button"
      class="button button--light"
      @click="emit('action')"
    >
      {{ actionLabel }}
    </button>
  </div>
</template>

<style scoped>
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-4);
}

.empty-state__title {
  font-family: var(--font-display);
  font-size: 24px;
  font-weight: 900;
  letter-spacing: 0.02em;
  text-transform: uppercase;
}
</style>
