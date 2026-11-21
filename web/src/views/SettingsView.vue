<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type SettingsField } from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'

type DurationUnit = 's' | 'm' | 'h'
type DurationPart = { amount: string; unit: DurationUnit }

const unitSeconds: Record<DurationUnit, number> = { s: 1, m: 60, h: 3600 }
const unitLabels: Record<DurationUnit, string> = { s: 'segundos', m: 'minutos', h: 'horas' }
const durationUnits: DurationUnit[] = ['s', 'm', 'h']

const values = ref<Record<string, string>>({})
const fields = ref<SettingsField[]>([])
const durations = ref<Record<string, DurationPart>>({})
const savedSnapshot = ref<Record<string, string>>({})
const revealed = ref<Record<string, boolean>>({})
const saving = ref('')
const loading = ref(false)
const loadFailed = ref(false)
const error = ref('')
const saved = ref('')
// Groups someone else changed while they had unsaved edits here.
const staleGroups = ref<SettingsField['group'][]>([])

const groups: Array<{ id: SettingsField['group']; title: string; hint: string }> = [
  { id: 'general', title: 'General', hint: 'Dirección pública, webhooks y reconciliación.' },
  { id: 'review', title: 'Revisión', hint: 'Cuánto explora y cuánto tarda una revisión.' },
  { id: 'worker', title: 'Worker', hint: 'Rutas y puertos del contenedor que ejecuta las revisiones.' },
  { id: 'alerts', title: 'Alertas', hint: 'Perfil monitor: a quién vigila y a dónde avisa.' },
  { id: 'backups', title: 'Backups', hint: 'Cuántos dumps conserva el perfil backup.' },
]

function fieldsOf(group: SettingsField['group']): SettingsField[] {
  return fields.value.filter((field) => field.group === group)
}

const restartTargets = ['app', 'worker', 'monitor'] as const

function restartLabel(group: SettingsField['group']): string {
  const present = new Set(fieldsOf(group).map((field) => field.restart))
  const names = restartTargets.filter((target) => present.has(target))
  return names.length > 0 ? `requiere reiniciar ${names.join(' y ')}` : ''
}

function parseSeconds(raw: string): number | null {
  const text = raw.trim()
  if (!text) return null
  const parts = /(\d+(?:\.\d+)?)(h|m|s)/g
  let consumed = 0
  let total = 0
  for (let match = parts.exec(text); match; match = parts.exec(text)) {
    consumed += match[0].length
    total += Number(match[1]) * unitSeconds[match[2] as DurationUnit]
  }
  return consumed === text.length && total > 0 ? total : null
}

// Go devuelve "20m0s"; la persona edita 20 y minutos. Se elige la unidad mayor
// que divide exacto para que el número se lea de un vistazo.
function splitDuration(raw: string, preferred: SettingsField['unit']): DurationPart {
  const seconds = parseSeconds(raw)
  if (seconds === null) {
    return { amount: raw, unit: preferred || 'm' }
  }
  for (const unit of ['h', 'm', 's'] as const) {
    if (seconds % unitSeconds[unit] === 0) {
      return { amount: String(seconds / unitSeconds[unit]), unit }
    }
  }
  return { amount: String(seconds), unit: 's' }
}

function syncDurations(): void {
  const next: Record<string, DurationPart> = {}
  for (const field of fields.value) {
    if (field.kind === 'duration') {
      next[field.key] = splitDuration(values.value[field.key] ?? '', field.unit)
    }
  }
  durations.value = next
}

function fieldValue(field: SettingsField): string {
  if (field.kind === 'duration') {
    const part = durations.value[field.key]
    return part ? `${part.amount.trim()}${part.unit}` : (values.value[field.key] ?? '')
  }
  return values.value[field.key] ?? ''
}

function payloadOf(group: SettingsField['group']): Record<string, string> {
  const payload: Record<string, string> = {}
  for (const field of fieldsOf(group)) {
    payload[field.key] = fieldValue(field)
  }
  return payload
}

function allPayload(): Record<string, string> {
  const payload: Record<string, string> = {}
  for (const field of fields.value) {
    payload[field.key] = fieldValue(field)
  }
  return payload
}

function isDirty(group: SettingsField['group']): boolean {
  return fieldsOf(group).some((field) => fieldValue(field) !== savedSnapshot.value[field.key])
}

function markDirty(): void {
  saved.value = ''
}

function setBool(key: string, checked: boolean): void {
  values.value[key] = checked ? 'true' : 'false'
  markDirty()
}

function toggleReveal(key: string): void {
  revealed.value[key] = !revealed.value[key]
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const response = await api.settings()
    values.value = { ...response.values }
    fields.value = response.fields
    syncDurations()
    savedSnapshot.value = allPayload()
    loadFailed.value = false
    error.value = ''
  } catch {
    loadFailed.value = true
    error.value = 'No se pudieron cargar los ajustes.'
  } finally {
    loading.value = false
  }
}

function applyGroup(group: SettingsField['group'], server: Record<string, string>): void {
  for (const field of fieldsOf(group)) {
    values.value[field.key] = server[field.key] ?? ''
    if (field.kind === 'duration') {
      durations.value[field.key] = splitDuration(values.value[field.key], field.unit)
    }
    savedSnapshot.value[field.key] = fieldValue(field)
  }
  staleGroups.value = staleGroups.value.filter((item) => item !== group)
}

// Compared in the snapshot's shape, so "20m0s" from the server and "20m" in
// the form count as the same value.
function changedOnServer(group: SettingsField['group'], server: Record<string, string>): boolean {
  return fieldsOf(group).some((field) => {
    const raw = server[field.key] ?? ''
    if (field.kind !== 'duration') return raw !== savedSnapshot.value[field.key]
    const part = splitDuration(raw, field.unit)
    return `${part.amount.trim()}${part.unit}` !== savedSnapshot.value[field.key]
  })
}

async function onLiveChanges(): Promise<void> {
  try {
    const response = await api.settings()
    for (const group of groups) {
      if (!isDirty(group.id)) {
        applyGroup(group.id, response.values)
      } else if (changedOnServer(group.id, response.values) && !staleGroups.value.includes(group.id)) {
        staleGroups.value = [...staleGroups.value, group.id]
      }
    }
  } catch {
    error.value = 'No se pudieron actualizar los ajustes.'
  }
}

async function reloadGroup(group: SettingsField['group']): Promise<void> {
  try {
    applyGroup(group, (await api.settings()).values)
  } catch {
    error.value = 'No se pudieron cargar los ajustes.'
  }
}

useLiveRefresh(['settings.updated'], onLiveChanges)

async function save(group: SettingsField['group']): Promise<void> {
  saving.value = group
  error.value = ''
  saved.value = ''
  const payload = payloadOf(group)
  try {
    const response = await api.saveSettings(payload)
    // La respuesta trae todos los valores: reemplazarlos borraría lo que se esté
    // escribiendo en otra sección, así que solo se refresca lo que se guardó.
    for (const key of Object.keys(payload)) {
      const value = response.values[key]
      if (value !== undefined) values.value[key] = value
    }
    syncDurations()
    for (const field of fieldsOf(group)) {
      savedSnapshot.value[field.key] = fieldValue(field)
    }
    staleGroups.value = staleGroups.value.filter((item) => item !== group)
    saved.value = 'Cambios guardados.'
  } catch (err) {
    error.value = err instanceof Error ? err.message : 'No se pudieron guardar los ajustes.'
  } finally {
    saving.value = ''
  }
}

onMounted(load)
</script>

<template>
  <section class="dashboard">
    <header class="page-header">
      <div>
        <h1 class="page-title">Ajustes</h1>
        <p class="page-subtitle">
          Se guardan en la base de datos. Los cambios se aplican al reiniciar el proceso indicado.
        </p>
      </div>
      <span v-if="saved" class="badge badge--ok" role="status">{{ saved }}</span>
    </header>

    <template v-if="loadFailed || loading">
      <p v-if="loadFailed" class="form-error" role="alert">{{ error }}</p>
      <button
        type="button"
        class="button button--primary"
        data-retry
        :disabled="loading"
        @click="load"
      >
        {{ loading ? 'Cargando…' : 'Reintentar' }}
      </button>
    </template>

    <p v-else-if="error" class="form-error" role="alert">{{ error }}</p>

    <div class="card-grid">
      <template v-for="group in groups" :key="group.id">
        <form
          v-if="fieldsOf(group.id).length > 0"
          class="settings-group"
          :data-group="group.id"
          @submit.prevent="save(group.id)"
        >
          <div aria-live="polite">
            <p v-if="staleGroups.includes(group.id)" class="stale-notice" data-stale-group>
              Otra persona cambió estos ajustes.
              <button class="button button--ghost" type="button" @click="reloadGroup(group.id)">Recargar</button>
            </p>
          </div>
          <header class="settings-group__head">
            <h2 class="settings-group__title">{{ group.title }}</h2>
            <span v-if="restartLabel(group.id)" class="badge badge--busy">
              {{ restartLabel(group.id) }}
            </span>
          </header>
          <p class="settings-group__hint">{{ group.hint }}</p>

          <div class="settings-group__grid">
            <div
              v-for="field in fieldsOf(group.id)"
              :key="field.key"
              class="field"
              :data-field="field.key"
            >
              <label class="field__label" :for="`setting-${field.key}`">{{ field.label }}</label>
              <p class="field__help">
                {{ field.help }}
                <span v-if="field.default" class="field__default">
                  Predeterminado: <code>{{ field.default }}</code>.
                </span>
              </p>

              <div v-if="field.kind === 'duration'" class="field__control field__control--duration">
                <input
                  :id="`setting-${field.key}`"
                  :value="durations[field.key].amount"
                  class="field__input"
                  type="number"
                  min="0"
                  step="1"
                  inputmode="numeric"
                  autocomplete="off"
                  :name="field.key"
                  @input="
                    durations[field.key].amount = ($event.target as HTMLInputElement).value;
                    markDirty()
                  "
                />
                <select
                  v-model="durations[field.key].unit"
                  class="field__select"
                  :name="`${field.key}_unit`"
                  :aria-label="`Unidad de ${field.label}`"
                  @change="markDirty"
                >
                  <option v-for="unit in durationUnits" :key="unit" :value="unit">
                    {{ unitLabels[unit] }}
                  </option>
                </select>
              </div>

              <label v-else-if="field.kind === 'bool'" class="field__check">
                <input
                  :id="`setting-${field.key}`"
                  :name="field.key"
                  type="checkbox"
                  :checked="values[field.key] === 'true'"
                  @change="setBool(field.key, ($event.target as HTMLInputElement).checked)"
                />
                <span>{{ values[field.key] === 'true' ? 'Activado' : 'Desactivado' }}</span>
              </label>

              <div v-else class="field__control">
                <input
                  :id="`setting-${field.key}`"
                  v-model="values[field.key]"
                  class="field__input"
                  :type="
                    field.kind === 'int'
                      ? 'number'
                      : field.secret && !revealed[field.key]
                        ? 'password'
                        : 'text'
                  "
                  :name="field.key"
                  :inputmode="field.kind === 'int' ? 'numeric' : field.kind === 'url' ? 'url' : undefined"
                  :autocomplete="field.secret ? 'new-password' : 'off'"
                  @input="markDirty"
                />
                <button
                  v-if="field.secret"
                  type="button"
                  class="button button--ghost field__reveal"
                  @click="toggleReveal(field.key)"
                >
                  {{ revealed[field.key] ? 'Ocultar' : 'Mostrar' }}
                </button>
              </div>
            </div>
          </div>

          <footer class="settings-group__actions">
            <button
              type="submit"
              class="button button--primary"
              :disabled="saving === group.id || !isDirty(group.id)"
            >
              {{ saving === group.id ? 'Guardando…' : 'Guardar' }}
            </button>
            <span v-if="isDirty(group.id)" class="settings-group__dirty">Cambios sin guardar</span>
          </footer>
        </form>
      </template>
    </div>
  </section>
</template>
