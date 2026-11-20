<script setup lang="ts">
import { ref } from 'vue'
import { ApiError, api, type SetupManifest } from '../api'

const name = ref('')
const busy = ref(false)
const error = ref('')

/** Envía el manifiesto a GitHub con un POST de formulario, como exige su flujo. */
function postManifest({ action_url, manifest }: SetupManifest): void {
  const form = document.createElement('form')
  form.method = 'post'
  form.action = action_url

  const input = document.createElement('input')
  input.type = 'hidden'
  input.name = 'manifest'
  input.value = JSON.stringify(manifest)

  form.appendChild(input)
  document.body.appendChild(form)

  form.submit()
}

async function submit(): Promise<void> {
  error.value = ''
  busy.value = true
  try {
    postManifest(await api.setupManifest({ name: name.value.trim() }))
  } catch (err) {
    if (err instanceof ApiError && err.status === 403) {
      error.value = 'El token de setup del servidor no coincide.'
    } else if (err instanceof ApiError && err.status === 409) {
      error.value = 'La instancia ya está configurada.'
    } else {
      error.value = 'No se pudo crear la GitHub App. Inténtalo de nuevo.'
    }
    busy.value = false
  }
}
</script>

<template>
  <section class="auth-page">
    <div class="auth-card auth-card--wide">
      <span class="brand__mark brand__mark--large" aria-hidden="true">QA</span>
      <h1 class="auth-card__title">Configurar QA Control Center</h1>
      <p class="auth-card__subtitle">
        Elige el nombre con el que se creará la GitHub App de esta instancia.
      </p>

      <form class="setup-form" @submit.prevent="submit">
        <label class="field">
          <span class="field__label">Nombre de la App</span>
          <input
            v-model="name"
            class="field__input"
            type="text"
            name="name"
            autocomplete="off"
            placeholder="QA Control Center"
            required
          />
        </label>

        <p v-if="error" class="form-error" role="alert">{{ error }}</p>

        <button type="submit" class="button button--primary button--block" :disabled="busy">
          {{ busy ? 'Creando GitHub App…' : 'Crear GitHub App' }}
        </button>
      </form>
    </div>
  </section>
</template>
