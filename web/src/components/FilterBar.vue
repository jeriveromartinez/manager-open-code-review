<script setup lang="ts">
import { computed } from 'vue'

type FilterChip = { key: string; label: string }

const props = withDefaults(
  defineProps<{
    q: string
    shown: number
    total: number
    chips: FilterChip[]
    activeCount?: number
    sheetOpen?: boolean
    sheetId?: string
  }>(),
  { activeCount: 0, sheetOpen: false },
)

const emit = defineEmits<{
  'update:q': [value: string]
  clear: []
  remove: [key: string]
  'open-sheet': []
}>()

// Single source of truth for "filters are on": the counter tint and LIMPIAR
// eligibility both read it, and plan B must reuse this exact rule.
const hasFilters = computed(() => props.q.trim() !== '' || props.chips.length > 0)

// The parent must mirror the input back into `q` synchronously. Any debounce
// belongs downstream so the counter and LIMPIAR track what the user sees.
function onSearch(event: Event): void {
  emit('update:q', (event.target as HTMLInputElement).value)
}

function onClear(): void {
  emit('clear')
}

function onRemove(key: string): void {
  emit('remove', key)
}

function onOpenSheet(): void {
  emit('open-sheet')
}
</script>

<template>
  <div class="filter-bar">
    <label class="filter-bar__search">
      <svg class="filter-bar__search-icon" viewBox="0 0 16 16" aria-hidden="true" focusable="false">
        <circle cx="7" cy="7" r="4.5" />
        <path d="M10.5 10.5 14 14" />
      </svg>
      <input
        class="filter-bar__input"
        type="search"
        placeholder="Buscar"
        aria-label="Buscar"
        maxlength="120"
        :value="q"
        @input="onSearch"
      />
    </label>

    <button
      type="button"
      class="filter-bar__sheet"
      aria-haspopup="dialog"
      :aria-expanded="sheetOpen"
      :aria-controls="sheetId"
      @click="onOpenSheet"
    >
      FILTROS · {{ activeCount }}
    </button>

    <!-- Hidden below 760px but kept mounted: views must render their own
         dropdowns inside the mobile sheet; this bar only owns the trigger. -->
    <div class="filter-bar__filters">
      <slot />
    </div>

    <div class="filter-bar__tail">
      <span
        class="filter-bar__count"
        :class="{ 'filter-bar__count--active': hasFilters }"
        aria-live="polite"
      >
        {{ shown }} DE {{ total }}
      </span>
      <button
        type="button"
        class="filter-bar__clear"
        :disabled="!hasFilters"
        @click="onClear"
      >
        LIMPIAR
      </button>
    </div>

    <ul v-if="chips.length > 0" class="filter-bar__chips" aria-label="Filtros activos">
      <li v-for="chip in chips" :key="chip.key" class="filter-bar__chip">
        <button
          type="button"
          class="filter-bar__chip-remove"
          :aria-label="`Quitar filtro ${chip.label}`"
          @click="onRemove(chip.key)"
        >
          <span class="filter-bar__chip-label">{{ chip.label }}</span>
          <span class="filter-bar__chip-close" aria-hidden="true">×</span>
        </button>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.filter-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-2) var(--space-3);
  width: 100%;
}

.filter-bar__search {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  flex: 1 1 200px;
  max-width: 320px;
  min-height: 44px;
  padding: 0 var(--space-4);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
  color: var(--text-muted);
  transition:
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease;
}

.filter-bar__search:hover {
  border-color: var(--border-strong);
}

.filter-bar__search:focus-within {
  border-color: var(--text);
  color: var(--text);
}

.filter-bar__search-icon {
  flex: none;
  width: 14px;
  height: 14px;
  fill: none;
  stroke: currentcolor;
  stroke-linecap: round;
  stroke-width: 1.5;
}

.filter-bar__input {
  min-width: 0;
  width: 100%;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--text);
  font: inherit;
  font-size: 13px;
}

.filter-bar__input::placeholder {
  color: var(--text-faint);
}

.filter-bar__input:focus {
  outline: none;
}

.filter-bar__filters {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
  min-width: 0;
}

.filter-bar__tail {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  margin-left: auto;
}

.filter-bar__count {
  color: var(--text-muted);
  font-family: var(--font-display);
  font-size: 18px;
  font-weight: 900;
  font-variant-numeric: tabular-nums;
  letter-spacing: 0.04em;
  white-space: nowrap;
}

.filter-bar__count--active {
  color: var(--accent);
}

.filter-bar__clear {
  appearance: none;
  min-height: 44px;
  padding: 0 var(--space-3);
  border: 0;
  background: none;
  color: var(--text-muted);
  font: inherit;
  font-size: 12px;
  font-weight: 500;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  white-space: nowrap;
  cursor: pointer;
  transition: color var(--dur-micro) ease;
}

.filter-bar__clear:hover:not(:disabled) {
  color: var(--text);
}

.filter-bar__clear:disabled {
  color: var(--text-faint);
  cursor: default;
  opacity: 0.6;
}

.filter-bar__sheet {
  appearance: none;
  display: none;
  align-items: center;
  min-height: 44px;
  padding: 0 var(--space-4);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-pill);
  background: transparent;
  color: var(--text-muted);
  font: inherit;
  font-size: 12px;
  font-weight: 500;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  white-space: nowrap;
  cursor: pointer;
  transition:
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease;
}

.filter-bar__sheet:hover {
  border-color: var(--border-strong);
  color: var(--text);
}

.filter-bar__chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
  width: 100%;
  margin: 0;
  padding: 0;
  list-style: none;
}

.filter-bar__chip {
  display: inline-flex;
}

.filter-bar__chip-remove {
  appearance: none;
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 32px;
  padding: 0 var(--space-3);
  border: var(--rule-hair) solid var(--accent);
  border-radius: var(--radius-pill);
  background: var(--accent-soft);
  color: var(--accent);
  font: inherit;
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
  transition:
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease;
}

.filter-bar__chip-remove:hover {
  border-color: var(--accent-strong);
  color: var(--accent-strong);
}

.filter-bar__chip-close {
  font-size: 14px;
  line-height: 1;
}

@media (max-width: 760px) {
  .filter-bar__filters {
    display: none;
  }

  .filter-bar__sheet {
    display: inline-flex;
  }
}
</style>
