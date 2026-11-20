<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type SettingsField } from '../api'

const values = ref<Record<string, string>>({})
const fields = ref<SettingsField[]>([])
const saving = ref('')
const loading = ref(false)
const loadFailed = ref(false)
const error = ref('')
const saved = ref('')

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

async function load(): Promise<void> {
  error.value = ''
  loadFailed.value = false
  loading.value = true
  try {
    const response = await api.settings()
    values.value = { ...response.values }
    fields.value = response.fields
  } catch {
    loadFailed.value = true
    error.value = 'No se pudieron cargar los ajustes.'
  } finally {
    loading.value = false
  }
}

async function save(group: SettingsField['group']): Promise<void> {
  saving.value = group
  error.value = ''
  saved.value = ''
  const payload: Record<string, string> = {}
  for (const field of fieldsOf(group)) {
    payload[field.key] = values.value[field.key] ?? ''
  }
  try {
    const response = await api.saveSettings(payload)
    // La respuesta trae todos los valores: reemplazarlos borraría lo que se esté
    // escribiendo en otra sección, así que solo se refresca lo que se guardó.
    for (const key of Object.keys(payload)) {
      const value = response.values[key]
      if (value !== undefined) values.value[key] = value
    }
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
    </header>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <button
      v-if="loadFailed"
      type="button"
      class="button button--primary"
      data-retry
      :disabled="loading"
      @click="load"
    >
      {{ loading ? 'Cargando…' : 'Reintentar' }}
    </button>
    <span v-if="saved" class="badge badge--ok" role="status">{{ saved }}</span>

    <form
      v-for="group in groups"
      :key="group.id"
      class="pr-card"
      :data-group="group.id"
      @submit.prevent="save(group.id)"
    >
      <div class="pr-section__head">
        <h2 class="pr-section__title">{{ group.title }}</h2>
        <span v-if="restartLabel(group.id)" class="badge badge--busy">
          {{ restartLabel(group.id) }}
        </span>
      </div>
      <p class="field__hint">{{ group.hint }}</p>

      <label v-for="field in fieldsOf(group.id)" :key="field.key" class="field">
        <span class="field__label">{{ field.key }}</span>
        <input
          v-model="values[field.key]"
          class="field__input"
          :type="field.secret ? 'password' : 'text'"
          :name="field.key"
          :placeholder="field.default || 'sin valor'"
          :autocomplete="field.secret ? 'new-password' : 'off'"
          @input="saved = ''"
        />
      </label>

      <button type="submit" class="button button--primary" :disabled="saving === group.id">
        {{ saving === group.id ? 'Guardando…' : 'Guardar' }}
      </button>
    </form>
  </section>
</template>
