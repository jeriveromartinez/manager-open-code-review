<script setup lang="ts">
withDefaults(defineProps<{ label?: string }>(), { label: 'Cargando…' })
</script>

<template>
  <div class="loader" role="status">
    <span class="loader__dots" aria-hidden="true">
      <span class="loader__dot" />
      <span class="loader__dot" />
      <span class="loader__dot" />
    </span>
    {{ label }}
  </div>
</template>
