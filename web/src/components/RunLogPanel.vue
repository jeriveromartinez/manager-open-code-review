<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, type ReviewRunLogEntry } from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'

const props = defineProps<{
  runId: number
  /** true mientras la ejecución puede seguir escribiendo líneas. */
  live: boolean
}>()

const PAGE = 500
// El tope de páginas evita que un log enorme bloquee el navegador al abrirlo.
const MAX_PAGES = 20
const POLL_INTERVAL = 3000

const entries = ref<ReviewRunLogEntry[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const autoScroll = ref(true)
const box = ref<HTMLElement | null>(null)

let cursor = 0
let request = 0
let pollTimer: ReturnType<typeof setInterval> | null = null

async function fetchTail(silent = false): Promise<void> {
  const current = ++request
  if (!silent) loading.value = true
  try {
    for (let page = 0; page < MAX_PAGES; page++) {
      const next = await api.reviewRunLogs(props.runId, cursor, PAGE)
      if (current !== request) return
      if (next.entries.length > 0) entries.value.push(...next.entries)
      cursor = next.next_after
      if (!next.has_more) break
    }
    error.value = null
    await scrollToEnd()
  } catch {
    if (current === request) error.value = 'No se pudo cargar el log de la ejecución'
  } finally {
    if (current === request) loading.value = false
  }
}

async function scrollToEnd(): Promise<void> {
  if (!autoScroll.value) return
  await nextTick()
  const el = box.value
  if (el) el.scrollTop = el.scrollHeight
}

// Soltar el seguimiento al subir permite leer mientras llegan líneas nuevas; el
// botón de Seguir vuelve a bajar en el siguiente sondeo.
function onScroll(): void {
  const el = box.value
  if (!el) return
  autoScroll.value = el.scrollTop + el.clientHeight >= el.scrollHeight - 24
}

function stopPolling(): void {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPolling(): void {
  stopPolling()
  if (props.live) pollTimer = setInterval(() => void fetchTail(true), POLL_INTERVAL)
}

function reset(): void {
  cursor = 0
  entries.value = []
  error.value = null
  void fetchTail()
}

// El SSE avisa en cuanto hay líneas nuevas; el sondeo es solo la red de
// seguridad cuando el evento se pierde o la conexión se cae.
useLiveRefresh(
  ['run.log'],
  () => void fetchTail(true),
  (event) => event.id === String(props.runId),
)

watch(() => props.runId, reset)
watch(() => props.live, startPolling)

onMounted(() => {
  void fetchTail()
  startPolling()
})

onBeforeUnmount(stopPolling)

function shortTime(ts: string): string {
  const date = new Date(ts)
  return Number.isNaN(date.getTime()) ? ts : date.toLocaleTimeString([], { hour12: false })
}
</script>

<template>
  <section class="run-log" data-run-log>
    <header class="run-log__head">
      <span class="run-log__count" data-run-log-count>{{ entries.length }} líneas</span>
      <label class="run-log__follow">
        <input v-model="autoScroll" type="checkbox" data-run-log-follow />
        Seguir
      </label>
      <button
        class="button button--ghost run-log__refresh"
        type="button"
        data-run-log-refresh
        :disabled="loading"
        @click="fetchTail()"
      >
        {{ loading ? 'Cargando…' : 'Actualizar' }}
      </button>
    </header>
    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <p v-else-if="entries.length === 0" class="run-log__empty">
      {{ loading ? 'Cargando el log…' : 'Todavía no hay líneas: la revisión aún no ha empezado.' }}
    </p>
    <ol v-show="entries.length > 0" ref="box" class="run-log__lines" data-run-log-lines @scroll="onScroll">
      <li
        v-for="entry in entries"
        :key="entry.seq"
        class="run-log__line"
        :class="`is-${entry.level}`"
        data-run-log-line
      >
        <span class="run-log__ts">{{ shortTime(entry.ts) }}</span>
        <span class="run-log__origin">
          <span class="run-log__source">{{ entry.source }}</span>
          <span v-if="entry.invocation" class="run-log__invocation">{{ entry.invocation }}</span>
          <span v-if="entry.phase" class="run-log__phase">{{ entry.phase }}</span>
        </span>
        <span class="run-log__text">{{ entry.line }}</span>
      </li>
    </ol>
  </section>
</template>

<style scoped>
.run-log {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3);
  background: var(--surface-2);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-sm);
}

.run-log__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.run-log__count {
  color: var(--text-faint);
  font-size: 13px;
}

.run-log__follow {
  display: flex;
  align-items: center;
  gap: var(--space-1);
  margin-left: auto;
  color: var(--text-muted);
  font-size: 13px;
  cursor: pointer;
}

.run-log__refresh {
  font-size: 13px;
}

.run-log__empty {
  margin: 0;
  color: var(--text-faint);
  font-size: 13px;
}

.run-log__lines {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 420px;
  margin: 0;
  padding: var(--space-2);
  overflow-y: auto;
  background: var(--bg);
  border: var(--rule-hair) solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: 12.5px;
  list-style: none;
}

.run-log__line {
  display: grid;
  grid-template-columns: 68px minmax(90px, max-content) 1fr;
  gap: var(--space-2);
  align-items: baseline;
  color: var(--text-muted);
}

.run-log__line.is-warn {
  color: var(--accent);
}

.run-log__line.is-error {
  color: var(--bad);
  background: var(--bad-soft);
}

.run-log__ts {
  color: var(--text-faint);
}

.run-log__origin {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1);
}

.run-log__source,
.run-log__invocation,
.run-log__phase {
  padding: 0 4px;
  color: var(--text-faint);
  border: var(--rule-hair) solid var(--border);
  border-radius: 4px;
  font-size: 11.5px;
}

.run-log__text {
  min-width: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}
</style>
