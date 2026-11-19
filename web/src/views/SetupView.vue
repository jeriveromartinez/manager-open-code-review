<script setup lang="ts">
import { ref } from 'vue'
import { ApiError, api, type SetupManifest } from '../api'

const setupToken = ref('')
const org = ref('')
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
    const result = await api.setupManifest({
      setup_token: setupToken.value.trim(),
      org: org.value.trim(),
    })
    postManifest(result)
  } catch (err) {
    if (err instanceof ApiError && err.status === 403) {
      error.value = 'El token de setup no es válido. Revisa los logs del servidor.'
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
        Crea la GitHub App de la instancia. Necesitas el token de setup que aparece en los logs del
        servidor.
      </p>

      <form class="setup-form" @submit.prevent="submit">
        <label class="field">
          <span class="field__label">Token de setup</span>
          <input
            v-model="setupToken"
            class="field__input"
            type="text"
            name="setup_token"
            autocomplete="off"
            required
          />
        </label>

        <label class="field">
          <span class="field__label">Organización (opcional)</span>
          <input
            v-model="org"
            class="field__input"
            type="text"
            name="org"
            autocomplete="off"
            placeholder="mi-organizacion"
          />
          <span class="field__hint">Vacío para crear la App en tu cuenta personal.</span>
        </label>

        <p v-if="error" class="form-error" role="alert">{{ error }}</p>

        <button type="submit" class="button button--primary button--block" :disabled="busy">
          {{ busy ? 'Creando GitHub App…' : 'Crear GitHub App' }}
        </button>
      </form>
    </div>
  </section>
</template>
