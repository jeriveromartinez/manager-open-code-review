<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  api,
  type LLMProfile,
  type LLMProfileInput,
  type LLMProfilePatch,
  type QAPolicy,
  type QAPolicyInput,
  type Team,
} from '../api'
import { useSessionStore } from '../stores/session'

type ProfileForm = {
  name: string
  provider: string
  model: string
  mode: string
  base_url: string
  api_key: string
  concurrency: number
  enabled: boolean
}

function emptyForm(): ProfileForm {
  return {
    name: '',
    provider: '',
    model: '',
    mode: '',
    base_url: '',
    api_key: '',
    concurrency: 1,
    enabled: true,
  }
}

const session = useSessionStore()
const isAdmin = computed(() => session.me?.is_admin === true)

const profiles = ref<LLMProfile[]>([])
const teams = ref<Team[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

const modalOpen = ref(false)
const editingId = ref<number | null>(null)
const form = ref<ProfileForm>(emptyForm())
const formError = ref<string | null>(null)

const selectedTeamId = ref<number | null>(null)
const policy = ref<QAPolicy | null>(null)
const policyProfileId = ref<number | null>(null)
const policyRequireGreenCI = ref(false)
const policyError = ref<string | null>(null)
const policyMessage = ref<string | null>(null)

function healthLabel(status: string): string {
  if (status === 'ok') return 'Saludable'
  if (status === 'error') return 'Con error'
  if (status === 'checking') return 'Verificando'
  if (status === 'unknown') return 'Sin verificar'
  return status
}

function healthTone(status: string): string {
  if (status === 'ok') return 'badge--ok'
  if (status === 'error') return 'badge--bad'
  if (status === 'checking') return 'badge--busy'
  return 'badge--muted'
}

function healthAge(profile: LLMProfile): string {
  if (profile.health_checked_at === null) return 'nunca comprobado'
  const seconds = profile.health_age_seconds
  if (seconds < 60) return 'comprobado hace menos de 1 min'
  if (seconds < 3600) return `comprobado hace ${Math.floor(seconds / 60)} min`
  if (seconds < 86400) return `comprobado hace ${Math.floor(seconds / 3600)} h`
  return `comprobado hace ${Math.floor(seconds / 86400)} d`
}

async function refreshProfiles(): Promise<void> {
  profiles.value = await api.llmProfiles()
}

async function loadPolicy(): Promise<void> {
  if (selectedTeamId.value === null) return
  policyError.value = null
  policyMessage.value = null
  try {
    const saved = await api.getPolicy(selectedTeamId.value)
    policy.value = saved
    policyProfileId.value = saved.llm_profile_id || null
    policyRequireGreenCI.value = saved.require_green_ci
  } catch {
    policyError.value = 'No se pudo cargar la política del equipo'
  }
}

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const [profileList, teamList] = await Promise.all([api.llmProfiles(), api.teams()])
    profiles.value = profileList
    teams.value = teamList
    if (selectedTeamId.value === null && teamList.length > 0) {
      selectedTeamId.value = teamList[0].id
      await loadPolicy()
    }
  } catch {
    error.value = 'No se pudieron cargar los perfiles LLM'
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  editingId.value = null
  form.value = emptyForm()
  formError.value = null
  modalOpen.value = true
}

function openEdit(profile: LLMProfile): void {
  editingId.value = profile.id
  form.value = {
    name: profile.name,
    provider: profile.provider,
    model: profile.model,
    mode: profile.mode,
    base_url: profile.base_url ?? '',
    // La clave nunca se recibe del backend: se deja vacía para conservarla.
    api_key: '',
    concurrency: profile.concurrency,
    enabled: profile.enabled,
  }
  formError.value = null
  modalOpen.value = true
}

function closeModal(): void {
  modalOpen.value = false
  editingId.value = null
  form.value = emptyForm()
}

async function submitProfile(): Promise<void> {
  formError.value = null
  const current = form.value
  const common = {
    name: current.name.trim(),
    provider: current.provider.trim(),
    model: current.model.trim(),
    mode: current.mode.trim(),
    base_url: current.base_url.trim() || null,
    concurrency: current.concurrency,
    enabled: current.enabled,
  }

  try {
    if (editingId.value === null) {
      if (!current.api_key) {
        formError.value = 'La clave API es obligatoria'
        return
      }
      const body: LLMProfileInput = { ...common, api_key: current.api_key }
      await api.createLLMProfile(body)
    } else {
      const patch: LLMProfilePatch = { ...common }
      if (current.api_key) patch.api_key = current.api_key
      await api.updateLLMProfile(editingId.value, patch)
    }
    await refreshProfiles()
    closeModal()
  } catch {
    formError.value = 'No se pudo guardar el perfil'
  }
}

async function checkHealth(profile: LLMProfile): Promise<void> {
  error.value = null
  try {
    await api.checkLLMHealth(profile.id)
    await refreshProfiles()
  } catch {
    error.value = 'No se pudo programar la prueba de salud'
  }
}

async function savePolicy(): Promise<void> {
  if (selectedTeamId.value === null || policyProfileId.value === null) return
  policyError.value = null
  policyMessage.value = null
  const body: QAPolicyInput = {
    llm_profile_id: policyProfileId.value,
    blocking_threshold: policy.value?.blocking_threshold ?? 'major',
    extra_instructions: policy.value?.extra_instructions ?? '',
    exclude_globs: policy.value?.exclude_globs ?? [],
    max_files: policy.value?.max_files ?? 40,
    max_diff_lines: policy.value?.max_diff_lines ?? 3000,
    max_passes: policy.value?.max_passes ?? 4,
    require_green_ci: policyRequireGreenCI.value,
  }
  try {
    const saved = await api.savePolicy(selectedTeamId.value, body)
    policy.value = saved
    policyRequireGreenCI.value = saved.require_green_ci
    policyMessage.value = 'Política guardada'
  } catch {
    policyError.value = 'No se pudo guardar la política'
  }
}

onMounted(() => {
  void session.load()
  void load()
})
</script>

<template>
  <section class="agents">
    <header class="page-header">
      <div>
        <h1 class="page-title">Agentes LLM</h1>
        <p class="page-subtitle">Perfiles de modelos, salud y políticas de revisión</p>
      </div>
      <button
        v-if="isAdmin"
        class="button button--primary"
        type="button"
        data-open-profile
        @click="openCreate"
      >
        Nuevo perfil
      </button>
    </header>

    <p v-if="error" class="form-error">{{ error }}</p>
    <p v-else-if="!loading && profiles.length === 0" class="empty-state">
      No hay perfiles LLM configurados
    </p>
    <div v-else class="table-wrap">
      <table class="profile-table">
        <thead>
          <tr>
            <th scope="col">Nombre</th>
            <th scope="col">Proveedor / modelo</th>
            <th scope="col">Salud</th>
            <th scope="col">Estado</th>
            <th scope="col">Clave API</th>
            <th scope="col">Acciones</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="profile in profiles" :key="profile.id" :data-profile-row="profile.id">
            <td class="profile-table__name">{{ profile.name }}</td>
            <td>{{ profile.provider }} / {{ profile.model }}</td>
            <td>
              <div class="health" data-health-cell>
                <span class="badge" :class="healthTone(profile.health_status)" data-health-badge>
                  {{ healthLabel(profile.health_status) }}
                </span>
                <span v-if="profile.health_stale" class="badge badge--bad" data-health-stale>
                  Desactualizado
                </span>
                <span class="health__age">{{ healthAge(profile) }}</span>
              </div>
              <span v-if="profile.health_error" class="profile-table__health-error">
                {{ profile.health_error }}
              </span>
            </td>
            <td>{{ profile.enabled ? 'Activado' : 'Desactivado' }}</td>
            <td>{{ profile.api_key_set ? 'Clave configurada' : 'Sin clave' }}</td>
            <td>
              <button
                v-if="isAdmin"
                class="button button--ghost profile-table__action"
                type="button"
                data-health-button
                @click="checkHealth(profile)"
              >
                Probar
              </button>
              <button
                v-if="isAdmin"
                class="button button--ghost profile-table__action"
                type="button"
                data-edit-profile
                @click="openEdit(profile)"
              >
                Editar
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <section v-if="isAdmin && teams.length > 0" class="policy-card" aria-label="Política por equipo">
      <h2 class="policy-card__title">Política por equipo</h2>
      <div class="policy-card__fields">
        <label class="field">
          <span class="field__label">Equipo</span>
          <select
            v-model.number="selectedTeamId"
            class="field__input"
            data-policy-team
            @change="loadPolicy"
          >
            <option :value="null">Selecciona un equipo</option>
            <option v-for="team in teams" :key="team.id" :value="team.id">{{ team.name }}</option>
          </select>
        </label>
        <label class="field">
          <span class="field__label">Perfil LLM</span>
          <select v-model.number="policyProfileId" class="field__input" data-policy-profile>
            <option :value="null">Sin perfil</option>
            <option v-for="profile in profiles" :key="profile.id" :value="profile.id">
              {{ profile.name }}
            </option>
          </select>
        </label>
        <button class="button button--primary" type="button" data-policy-save @click="savePolicy">
          Guardar política
        </button>
      </div>
      <label class="policy-ci">
        <input v-model="policyRequireGreenCI" type="checkbox" data-policy-require-green-ci />
        <span class="policy-ci__text">
          <span class="field__label">Exigir CI en verde</span>
          <span class="field__hint">
            Ninguna revisión empieza hasta que el último commit tenga checks y ninguno haya fallado.
          </span>
        </span>
      </label>
      <p v-if="policyError" class="form-error">{{ policyError }}</p>
      <p v-else-if="policyMessage" class="policy-card__message">{{ policyMessage }}</p>
    </section>

    <div v-if="modalOpen" class="modal-backdrop" role="dialog" aria-modal="true" aria-label="Perfil LLM">
      <form class="modal" @submit.prevent="submitProfile">
        <h2 class="modal__title">{{ editingId === null ? 'Nuevo perfil LLM' : 'Editar perfil LLM' }}</h2>
        <p class="field__hint">
          La clave se guarda cifrada y nunca se muestra. Déjala vacía para conservar la actual.
        </p>

        <label class="field">
          <span class="field__label">Nombre</span>
          <input v-model="form.name" class="field__input" type="text" data-profile-name />
        </label>
        <label class="field">
          <span class="field__label">Proveedor</span>
          <input v-model="form.provider" class="field__input" type="text" data-profile-provider />
        </label>
        <label class="field">
          <span class="field__label">Modelo</span>
          <input v-model="form.model" class="field__input" type="text" data-profile-model />
        </label>
        <label class="field">
          <span class="field__label">Clave API</span>
          <input
            v-model="form.api_key"
            class="field__input"
            type="password"
            autocomplete="new-password"
            data-profile-api-key
          />
        </label>
        <label class="field">
          <span class="field__label">Modo</span>
          <input v-model="form.mode" class="field__input" type="text" data-profile-mode />
        </label>
        <label class="field">
          <span class="field__label">URL base (opcional)</span>
          <input v-model="form.base_url" class="field__input" type="text" data-profile-base-url />
        </label>
        <label class="field">
          <span class="field__label">Concurrencia</span>
          <input
            v-model.number="form.concurrency"
            class="field__input"
            type="number"
            min="1"
            data-profile-concurrency
          />
        </label>
        <label class="switch">
          <input v-model="form.enabled" type="checkbox" data-profile-enabled />
          <span>Perfil activado</span>
        </label>

        <p v-if="formError" class="form-error">{{ formError }}</p>
        <div class="modal__actions">
          <button class="button button--ghost" type="button" data-profile-cancel @click="closeModal">
            Cancelar
          </button>
          <button
            class="button button--primary"
            type="button"
            data-profile-submit
            @click="submitProfile"
          >
            {{ editingId === null ? 'Crear perfil' : 'Guardar cambios' }}
          </button>
        </div>
      </form>
    </div>
  </section>
</template>

<style scoped>
.agents {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.profile-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 14px;
}

.profile-table th,
.profile-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border);
}

.profile-table th {
  color: var(--text-muted);
  font-size: 12.5px;
  font-weight: 600;
}

.profile-table tbody tr:last-child td {
  border-bottom: 0;
}

.profile-table__name {
  font-weight: 600;
}

.profile-table__health-error {
  display: block;
  margin-top: var(--space-1);
  color: var(--text-faint);
  font-size: 12px;
}

.health {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}

.health__age {
  color: var(--text-faint);
  font-size: 12px;
}

.profile-table__action {
  padding: 5px 9px;
  font-size: 12.5px;
}

.policy-card {
  padding: var(--space-4);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
}

.policy-card__title {
  margin: 0 0 var(--space-3);
  font-size: 16px;
  font-weight: 600;
}

.policy-card__fields {
  display: flex;
  align-items: end;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.policy-card__message {
  margin-top: var(--space-2);
  color: var(--ok);
  font-size: 13px;
}

.policy-ci {
  display: flex;
  align-items: start;
  gap: var(--space-2);
  margin-top: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border);
  cursor: pointer;
}

.policy-ci input {
  width: 18px;
  height: 18px;
  margin: 0;
  accent-color: var(--accent);
  cursor: pointer;
}

.policy-ci__text {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.modal-backdrop {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: grid;
  place-items: center;
  padding: var(--space-4);
  background: rgb(0 0 0 / 55%);
}

.modal {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  width: min(480px, 100%);
  max-height: 90vh;
  overflow-y: auto;
  padding: var(--space-5);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-md);
}

.modal__title {
  margin: 0;
  font-size: 18px;
}

.modal__actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}
</style>
