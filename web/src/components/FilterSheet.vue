<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'

const props = defineProps<{
  open: boolean
  activeCount: number
  shown: number
}>()

const emit = defineEmits<{
  close: []
  clear: []
}>()

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

const sheet = ref<HTMLElement | null>(null)
const titleId = useId()

let previousOverflow = ''
let restoreFocusTo: HTMLElement | null = null
let wasOpen = false

function focusables(): HTMLElement[] {
  if (!sheet.value) return []
  return Array.from(sheet.value.querySelectorAll<HTMLElement>(FOCUSABLE))
}

function focusFirst(): void {
  const [first] = focusables()
  if (first) first.focus()
  else sheet.value?.focus()
}

// Follow-up: a shared lock counter is needed if another overlay starts locking body scroll.
function lockScroll(): void {
  previousOverflow = document.body.style.overflow
  document.body.style.overflow = 'hidden'
}

function unlockScroll(): void {
  document.body.style.overflow = previousOverflow
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') {
    // A nested control (e.g. a dropdown) may have consumed the key first.
    if (event.defaultPrevented) return
    event.preventDefault()
    emit('close')
    return
  }

  if (event.key !== 'Tab') return

  const items = focusables()
  if (items.length === 0) {
    event.preventDefault()
    sheet.value?.focus()
    return
  }

  const first = items[0]
  const last = items[items.length - 1]
  const active = document.activeElement
  const inside = active instanceof Node && sheet.value?.contains(active) === true

  if (event.shiftKey) {
    // The panel itself is tabindex="-1": treat it as the start of the trap.
    if (!inside || active === first || active === sheet.value) {
      event.preventDefault()
      last.focus()
    }
    return
  }

  if (!inside || active === last) {
    event.preventDefault()
    first.focus()
  }
}

function onBackdrop(): void {
  emit('close')
}

watch(
  () => props.open,
  (isOpen) => {
    if (isOpen === wasOpen) return
    wasOpen = isOpen

    if (isOpen) {
      lockScroll()
      restoreFocusTo = document.activeElement instanceof HTMLElement ? document.activeElement : null
      void nextTick(focusFirst)
      return
    }

    unlockScroll()
    const target = restoreFocusTo
    restoreFocusTo = null
    if (target?.isConnected) void nextTick(() => target.focus())
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  if (props.open) unlockScroll()
})
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="filter-sheet">
      <div class="filter-sheet__backdrop" aria-hidden="true" @click="onBackdrop" />

      <div
        ref="sheet"
        class="filter-sheet__panel"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="titleId"
        tabindex="-1"
        @keydown="onKeydown"
      >
        <header class="filter-sheet__head">
          <h2 :id="titleId" class="filter-sheet__title">FILTROS</h2>
        </header>

        <div class="filter-sheet__body">
          <slot />
        </div>

        <footer class="filter-sheet__foot">
          <button
            type="button"
            class="filter-sheet__clear"
            :disabled="activeCount === 0"
            @click="emit('clear')"
          >
            LIMPIAR
          </button>
          <button type="button" class="filter-sheet__apply" @click="emit('close')">
            VER {{ shown }} PRs
          </button>
        </footer>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.filter-sheet {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: flex;
  align-items: flex-end;
}

.filter-sheet__backdrop {
  position: absolute;
  inset: 0;
  background: var(--scrim);
}

.filter-sheet__panel {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  width: 100%;
  max-height: 85vh;
  padding: var(--space-5) var(--space-4) var(--space-4);
  border-top: var(--rule-hair) solid var(--border-strong);
  border-radius: var(--radius-lg) var(--radius-lg) 0 0;
  background: var(--surface);
  overflow-y: auto;
}

.filter-sheet__title {
  margin: 0;
  color: var(--text-muted);
  font: 500 12px var(--font-sans);
  letter-spacing: 0.08em;
  text-transform: uppercase;
}

.filter-sheet__body {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

/* Follow-up: dropdowns opened near the footer can overlap it; they need upward placement or a fixed layer. */
.filter-sheet__body :deep(.filter-dropdown) {
  display: flex;
  width: 100%;
}

.filter-sheet__body :deep(.filter-dropdown__pill) {
  width: 100%;
  justify-content: space-between;
}

.filter-sheet__foot {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
  padding-top: var(--space-3);
  border-top: var(--rule-hair) solid var(--border);
}

.filter-sheet__clear,
.filter-sheet__apply {
  appearance: none;
  min-height: 48px;
  border-radius: var(--radius-pill);
  font: inherit;
  font-size: 12px;
  font-weight: 500;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  cursor: pointer;
  transition:
    border-color var(--dur-micro) ease,
    color var(--dur-micro) ease,
    opacity var(--dur-micro) ease;
}

.filter-sheet__clear {
  border: var(--rule-hair) solid var(--border);
  background: transparent;
  color: var(--text-muted);
}

.filter-sheet__clear:hover:not(:disabled) {
  border-color: var(--border-strong);
  color: var(--text);
}

.filter-sheet__clear:disabled {
  color: var(--text-faint);
  cursor: default;
  opacity: 0.6;
}

.filter-sheet__apply {
  border: var(--rule-hair) solid var(--accent);
  background: var(--accent);
  color: var(--accent-ink);
}

.filter-sheet__apply:hover {
  border-color: var(--accent-strong);
  background: var(--accent-strong);
}

.filter-sheet__panel:focus {
  outline: none;
}

.filter-sheet__panel :focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}
</style>
