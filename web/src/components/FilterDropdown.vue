<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'

type FilterOption = { value: string; label: string; count?: number }

const props = withDefaults(
  defineProps<{
    label: string
    options: FilterOption[]
    modelValue: string[]
    multiple?: boolean
    clearLabel?: string
  }>(),
  { clearLabel: 'Todas' },
)

const emit = defineEmits<{
  'update:modelValue': [value: string[]]
}>()

const open = ref(false)
const activeIndex = ref(0)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLButtonElement | null>(null)
const listbox = ref<HTMLElement | null>(null)
const dropdownId = useId()

const entries = computed<FilterOption[]>(() =>
  props.multiple
    ? [
        // In multiple mode the empty option clears the filter and always comes first.
        { value: '', label: props.clearLabel },
        ...props.options,
      ]
    : props.options,
)

const selectedLabel = computed(() => {
  if (props.multiple) return ''
  const value = props.modelValue[0]
  if (value === undefined || value === '') return ''
  return props.options.find((option) => option.value === value)?.label ?? value
})

const hasSelection = computed(() => props.modelValue.some((value) => value !== ''))

const activeOptionId = computed(() => (open.value ? optionId(activeIndex.value) : undefined))

function optionId(index: number): string {
  return `${dropdownId}-option-${index}`
}

function isSelected(value: string): boolean {
  if (value === '') return props.multiple === true && props.modelValue.length === 0
  return props.modelValue.includes(value)
}

function openPopup(fromEnd = false): void {
  open.value = true
  const selectedIndex = entries.value.findIndex((option) => props.modelValue.includes(option.value))
  activeIndex.value = fromEnd
    ? entries.value.length - 1
    : selectedIndex >= 0
      ? selectedIndex
      : 0
  void nextTick(() => listbox.value?.focus())
}

function close(returnFocus = false): void {
  if (!open.value) return
  open.value = false
  if (returnFocus) void nextTick(() => trigger.value?.focus())
}

function toggle(): void {
  if (open.value) close(true)
  else openPopup()
}

function moveActive(delta: number): void {
  const total = entries.value.length
  if (total === 0) return
  activeIndex.value = (activeIndex.value + delta + total) % total
}

function select(value: string): void {
  if (props.multiple) {
    if (value === '') {
      emit('update:modelValue', [])
      return
    }
    const next = props.modelValue.includes(value)
      ? props.modelValue.filter((item) => item !== value)
      : [...props.modelValue, value]
    emit('update:modelValue', next)
    return
  }

  emit('update:modelValue', value === '' ? [] : [value])
  close(true)
}

function selectActive(): void {
  const option = entries.value[activeIndex.value]
  if (option) select(option.value)
}

function onKeydown(event: KeyboardEvent): void {
  if (!open.value) {
    if (['Enter', ' ', 'ArrowDown', 'ArrowUp'].includes(event.key)) {
      event.preventDefault()
      openPopup(event.key === 'ArrowUp')
    }
    return
  }

  switch (event.key) {
    case 'ArrowDown':
      event.preventDefault()
      moveActive(1)
      break
    case 'ArrowUp':
      event.preventDefault()
      moveActive(-1)
      break
    case 'Home':
      event.preventDefault()
      activeIndex.value = 0
      break
    case 'End':
      event.preventDefault()
      activeIndex.value = Math.max(0, entries.value.length - 1)
      break
    case 'Enter':
    case ' ':
      event.preventDefault()
      selectActive()
      break
    case 'Escape':
      event.preventDefault()
      event.stopPropagation()
      close(true)
      break
    case 'Tab':
      close()
      break
  }
}

function onFocusOut(event: FocusEvent): void {
  if (!open.value) return
  const next = event.relatedTarget
  if (next instanceof Node && root.value?.contains(next)) return
  close()
}

function onDocumentPointerDown(event: Event): void {
  if (event.target instanceof Node && !root.value?.contains(event.target)) close()
}

watch(open, (isOpen) => {
  if (isOpen) document.addEventListener('pointerdown', onDocumentPointerDown)
  else document.removeEventListener('pointerdown', onDocumentPointerDown)
})

watch(entries, (list) => {
  if (activeIndex.value >= list.length) activeIndex.value = Math.max(0, list.length - 1)
})

onBeforeUnmount(() => document.removeEventListener('pointerdown', onDocumentPointerDown))
</script>

<template>
  <div ref="root" class="filter-dropdown" @focusout="onFocusOut">
    <button
      ref="trigger"
      type="button"
      class="filter-dropdown__pill"
      :class="{ 'filter-dropdown__pill--active': hasSelection }"
      aria-haspopup="listbox"
      :aria-expanded="open"
      :aria-controls="open ? dropdownId : undefined"
      @click="toggle"
      @keydown="onKeydown"
    >
      <span class="filter-dropdown__label">{{ label }}</span>
      <span v-if="selectedLabel" class="filter-dropdown__value">{{ selectedLabel }}</span>
      <span class="filter-dropdown__chevron" aria-hidden="true">▾</span>
    </button>

    <ul
      v-if="open"
      :id="dropdownId"
      ref="listbox"
      class="filter-dropdown__list"
      role="listbox"
      :aria-label="label"
      :aria-multiselectable="multiple || undefined"
      tabindex="-1"
      :aria-activedescendant="activeOptionId"
      @keydown="onKeydown"
      @mousedown.prevent
    >
      <li
        v-for="(option, index) in entries"
        :id="optionId(index)"
        :key="option.value"
        role="option"
        class="filter-dropdown__option"
        :class="{ 'filter-dropdown__option--active': index === activeIndex }"
        :aria-selected="isSelected(option.value)"
        @click="select(option.value)"
        @mouseenter="activeIndex = index"
      >
        <span class="filter-dropdown__check" aria-hidden="true">{{ isSelected(option.value) ? '✓' : '' }}</span>
        <span class="filter-dropdown__text">{{ option.label }}</span>
        <span v-if="option.count !== undefined" class="filter-dropdown__count">{{ option.count }}</span>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.filter-dropdown {
  position: relative;
  display: inline-flex;
}

.filter-dropdown__pill {
  appearance: none;
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
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

.filter-dropdown__pill:hover {
  border-color: var(--border-strong);
  color: var(--text);
}

.filter-dropdown__pill--active {
  border-color: var(--text);
  color: var(--text);
}

.filter-dropdown__value {
  color: var(--text);
  font-weight: 700;
  letter-spacing: normal;
  text-transform: none;
}

.filter-dropdown__chevron {
  color: var(--text-faint);
  font-size: 10px;
}

.filter-dropdown__list {
  position: absolute;
  top: calc(100% + var(--space-1));
  left: 0;
  z-index: 20;
  min-width: 100%;
  margin: 0;
  padding: var(--space-1);
  border: var(--rule-hair) solid var(--border-strong);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  box-shadow: 0 12px 32px rgb(0 0 0 / 45%);
  list-style: none;
}

.filter-dropdown__option {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 40px;
  padding: 0 var(--space-3);
  border-radius: calc(var(--radius-sm) - 2px);
  color: var(--text-muted);
  font-size: 13px;
  white-space: nowrap;
  cursor: pointer;
}

.filter-dropdown__option--active {
  background: var(--surface-3);
  color: var(--text);
}

.filter-dropdown__check {
  width: 1em;
  color: var(--accent);
  font-weight: 700;
}

.filter-dropdown__text {
  flex: 1;
}

.filter-dropdown__count {
  color: var(--text-faint);
  font-family: var(--font-display);
  font-size: 15px;
  font-weight: 900;
  font-variant-numeric: tabular-nums;
}
</style>
