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
  <div class="empty">
    <p class="empty__title">{{ title }}</p>
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
.empty {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-4);
  padding: var(--space-5);
  border: var(--rule-hair) dashed var(--border);
  border-radius: var(--radius-lg);
  color: var(--text-muted);
}

.empty__title {
  font-family: var(--font-display);
  font-size: 24px;
  font-weight: 900;
  letter-spacing: 0.02em;
  text-transform: uppercase;
}
</style>
