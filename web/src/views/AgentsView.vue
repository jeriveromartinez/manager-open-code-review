<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import {
  api,
  type LLMProfile,
  type LLMProfileInput,
  type LLMProfilePatch,
  type QAPolicy,
  type QAPolicyInput,
  type ReviewEvent,
  type Team,
} from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { useSessionStore } from '../stores/session'
import { llmPlatforms, type LLMPlatform, type LLMPlatformModel } from '../llmPlatforms'

type ProfileTab = 'platform' | 'custom'

const CUSTOM_MODEL = '__custom__'

type ProfileForm = {
  name: string
  provider: string
  model: string
  variant: string
  base_url: string
  api_key: string
  concurrency: number
  enabled: boolean
}

// Suggested efforts. A value the model's own table does not contain is
// silently ignored by --variant, so the free text stays but is guided (#65).
const reasoningVariants = ['none', 'minimal', 'low', 'medium', 'high']

function emptyForm(): ProfileForm {
  return {
    name: '',
    provider: '',
    model: '',
    variant: '',
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
const tab = ref<ProfileTab>('platform')
const modelSelect = ref('')
const modelOther = ref('')
const remoteModels = ref<LLMPlatformModel[]>([])
const modelsLoading = ref(false)
const modelsNotice = ref<string | null>(null)
let modelsTag = ''

function platformFor(provider: string): LLMPlatform | undefined {
  return llmPlatforms.find((platform) => platform.id === provider)
}

const selectedPlatform = computed<LLMPlatform | null>(
  () => platformFor(form.value.provider) ?? null,
)
const modelIsCustom = computed(() => modelSelect.value === CUSTOM_MODEL)
const platformModelOptions = computed<LLMPlatformModel[]>(() => {
  const platform = selectedPlatform.value
  if (platform === null) return []
  if (remoteModels.value.length === 0) return platform.models
  const curated = platform.models.filter((model) =>
    remoteModels.value.some((remote) => remote.id === model.id),
  )
  const extra = remoteModels.value.filter(
    (remote) => !platform.models.some((model) => model.id === remote.id),
  )
  return [...curated, ...extra]
})

const selectedTeamId = ref<number | null>(null)
const policy = ref<QAPolicy | null>(null)
const policyProfileId = ref<number | null>(null)
const policyRequireGreenCI = ref(false)
const policyExtraInstructions = ref('')
// One glob per line: a comma inside a pattern must survive editing.
const policyExcludeGlobs = ref('')
const policyError = ref<string | null>(null)
const policyMessage = ref<string | null>(null)
// Set when someone else changed what is on screen while it had unsaved edits.
const staleProfile = ref<'' | 'changed' | 'deleted'>('')
const stalePolicy = ref(false)
// The profile as it was when its dialog opened, to tell a real edit elsewhere
// from a health check.
const editingProfile = ref<LLMProfile | null>(null)

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

function applyPolicy(saved: QAPolicy): void {
  policy.value = saved
  policyProfileId.value = saved.llm_profile_id || null
  policyRequireGreenCI.value = saved.require_green_ci
  policyExtraInstructions.value = saved.extra_instructions
  policyExcludeGlobs.value = saved.exclude_globs.join('\n')
  stalePolicy.value = false
}

function parseGlobs(raw: string): string[] {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

async function loadPolicy(): Promise<void> {
  if (selectedTeamId.value === null) return
  policyError.value = null
  policyMessage.value = null
  try {
    applyPolicy(await api.getPolicy(selectedTeamId.value))
  } catch {
    policyError.value = 'No se pudo cargar la política del equipo'
  }
}

const policyDirty = computed(
  () =>
    policy.value !== null &&
    (policyProfileId.value !== (policy.value.llm_profile_id || null) ||
      policyRequireGreenCI.value !== policy.value.require_green_ci ||
      policyExtraInstructions.value !== policy.value.extra_instructions ||
      policyExcludeGlobs.value !== policy.value.exclude_globs.join('\n')),
)

// Health checks also announce a profile; only these fields are someone's edit.
function editableOf(profile: LLMProfile): string {
  const { name, provider, model, variant, base_url, concurrency, enabled, api_key_set } = profile
  return JSON.stringify({ name, provider, model, variant, base_url, concurrency, enabled, api_key_set })
}

async function onLiveChanges(events: ReviewEvent[]): Promise<void> {
  const touched = (type: string, id: number | null) =>
    id !== null && events.some((event) => event.type === type && Number(event.id) === id)

  const editing = editingProfile.value
  try {
    profiles.value = await api.llmProfiles()
  } catch {
    error.value = 'No se pudieron actualizar los perfiles LLM'
  }
  if (modalOpen.value && editing && editingId.value === editing.id && touched('llm_profile.updated', editing.id)) {
    const fresh = profiles.value.find((profile) => profile.id === editing.id)
    if (!fresh) staleProfile.value = 'deleted'
    else if (editableOf(fresh) !== editableOf(editing)) staleProfile.value = 'changed'
  }

  const team = selectedTeamId.value
  const resync = events.some((event) => event.type === 'resync')
  if (team === null || !(resync || touched('team.updated', team))) return
  let server: QAPolicy
  try {
    server = await api.getPolicy(team)
  } catch {
    policyError.value = 'No se pudo comprobar la política del equipo'
    return
  }
  if (selectedTeamId.value !== team) return
  // Checked after the request, so edits made while it was in flight count.
  if (!policyDirty.value) {
    applyPolicy(server)
  } else if (JSON.stringify(server) !== JSON.stringify(policy.value)) {
    // Our own save notifies too; only a policy that differs is someone else's.
    stalePolicy.value = true
  }
}

function reloadProfile(): void {
  const fresh = profiles.value.find((profile) => profile.id === editingId.value)
  if (fresh) openEdit(fresh)
}

useLiveRefresh(['llm_profile.updated', 'team.updated'], onLiveChanges)

function requestedTeamId(): number | null {
  const raw = new URLSearchParams(window.location.search).get('team')
  if (raw === null || raw.trim() === '') return null
  const id = Number(raw)
  return Number.isInteger(id) && id > 0 ? id : null
}

// TeamsView links here with the team preselected and #policy, so the policy
// section must be in view once the page has rendered.
async function focusPolicy(): Promise<void> {
  if (window.location.hash !== '#policy') return
  await nextTick()
  const section = document.getElementById('policy')
  if (section && typeof section.scrollIntoView === 'function') {
    section.scrollIntoView({ block: 'start' })
  }
}

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    const [profileList, teamList] = await Promise.all([api.llmProfiles(), api.teams()])
    profiles.value = profileList
    teams.value = teamList
    const requested = requestedTeamId()
    if (requested !== null && teamList.some((team) => team.id === requested)) {
      selectedTeamId.value = requested
    }
    if (selectedTeamId.value === null && teamList.length > 0) {
      selectedTeamId.value = teamList[0].id
    }
    if (selectedTeamId.value !== null) {
      await loadPolicy()
    }
    await focusPolicy()
  } catch {
    error.value = 'No se pudieron cargar los perfiles LLM'
  } finally {
    loading.value = false
  }
}

function selectPlatform(platform: LLMPlatform): void {
  resetRemoteModels()
  form.value.provider = platform.id
  const model = platform.models[0]
  form.value.model = model.id
  modelSelect.value = model.id
  modelOther.value = ''
}

function resetRemoteModels(): void {
  remoteModels.value = []
  modelsLoading.value = false
  modelsNotice.value = null
  modelsTag = ''
}

function onAPIKeyInput(): void {
  resetRemoteModels()
  // The remote list may have offered the current model; when it disappears
  // the select must still show it instead of going blank.
  syncPlatformFromForm()
}

async function loadPlatformModels(): Promise<void> {
  if (tab.value !== 'platform') return
  const platform = selectedPlatform.value
  if (platform === null) return
  const key = form.value.api_key.trim()
  if (key === '') return

  const tag = `${platform.id}\n${key}`
  modelsTag = tag
  modelsLoading.value = true
  modelsNotice.value = null
  try {
    const response = await api.llmModels(platform.id, key)
    if (modelsTag !== tag) return
    const models = response.models
      .map((model) => ({
        id: model.id.trim(),
        label: (model.name || model.id).trim(),
      }))
      .filter((model) => model.id !== '' && model.label !== '')
    if (models.length === 0) {
      throw new Error('empty model list')
    }
    remoteModels.value = models
    const current = form.value.model
    if (models.some((model) => model.id === current)) {
      modelSelect.value = current
    } else if (editingId.value === null) {
      form.value.model = models[0].id
      modelSelect.value = models[0].id
    } else {
      modelSelect.value = CUSTOM_MODEL
      modelOther.value = current
    }
  } catch {
    if (modelsTag !== tag) return
    remoteModels.value = []
    modelsNotice.value =
      'No se pudieron cargar los modelos de la plataforma; se muestran los recomendados'
  } finally {
    if (modelsTag === tag) {
      modelsLoading.value = false
    }
  }
}

function syncPlatformFromForm(): void {
  const platform = selectedPlatform.value
  if (platform === null) return
  const known = platform.models.some((model) => model.id === form.value.model)
  modelSelect.value = known ? form.value.model : CUSTOM_MODEL
  modelOther.value = known ? '' : form.value.model
}

function setTab(next: ProfileTab): void {
  tab.value = next
  if (next === 'platform') syncPlatformFromForm()
}

function onModelSelectChange(event: Event): void {
  modelSelect.value = (event.target as HTMLSelectElement).value
  if (modelSelect.value === CUSTOM_MODEL) {
    modelOther.value = form.value.model
    return
  }
  form.value.model = modelSelect.value
}

function onModelOtherInput(event: Event): void {
  modelOther.value = (event.target as HTMLInputElement).value
  form.value.model = modelOther.value.trim()
}

function defaultProfileName(): string {
  if (tab.value === 'platform' && selectedPlatform.value !== null) {
    const platform = selectedPlatform.value
    const label = platformModelOptions.value.find((model) => model.id === form.value.model)?.label
    return `${platform.name} ${label ?? form.value.model}`.trim()
  }
  return `${form.value.provider} ${form.value.model}`.trim()
}

function openCreate(): void {
  editingId.value = null
  form.value = emptyForm()
  formError.value = null
  tab.value = 'platform'
  modelSelect.value = ''
  modelOther.value = ''
  resetRemoteModels()
  modalOpen.value = true
}

function openEdit(profile: LLMProfile): void {
  staleProfile.value = ''
  editingProfile.value = { ...profile }
  editingId.value = profile.id
  form.value = {
    name: profile.name,
    provider: profile.provider,
    model: profile.model,
    variant: profile.variant ?? '',
    base_url: profile.base_url ?? '',
    // La clave nunca se recibe del backend: se deja vacía para conservarla.
    api_key: '',
    concurrency: profile.concurrency,
    enabled: profile.enabled,
  }
  formError.value = null
  modelSelect.value = ''
  modelOther.value = ''
  resetRemoteModels()
  const platform = platformFor(profile.provider)
  if (platform && !profile.base_url) {
    tab.value = 'platform'
    syncPlatformFromForm()
  } else {
    tab.value = 'custom'
  }
  modalOpen.value = true
}

function closeModal(): void {
  staleProfile.value = ''
  editingProfile.value = null
  modalOpen.value = false
  editingId.value = null
  form.value = emptyForm()
  tab.value = 'platform'
  modelSelect.value = ''
  modelOther.value = ''
  resetRemoteModels()
}

async function submitProfile(): Promise<void> {
  formError.value = null
  const current = form.value
  const provider = current.provider.trim()
  const model = current.model.trim()
  if (tab.value === 'platform' && selectedPlatform.value === null) {
    formError.value = 'Elige una plataforma'
    return
  }
  if (!provider || !model) {
    formError.value = 'Selecciona una plataforma y un modelo'
    return
  }
  if (editingId.value === null && !current.api_key) {
    formError.value = 'La clave API es obligatoria'
    return
  }
  const body: LLMProfileInput = {
    name: current.name.trim() || defaultProfileName(),
    provider,
    model,
    concurrency: current.concurrency,
    enabled: current.enabled,
    api_key: current.api_key,
  }
  // On edit an empty variant must clear the stored one, so null is sent
  // explicitly; on create it is simply omitted like the other optional fields.
  const variant = current.variant.trim()
  if (variant) body.variant = variant
  else if (editingId.value !== null) body.variant = null
  if (tab.value === 'custom') {
    body.base_url = current.base_url.trim() || null
  }

  try {
    if (editingId.value === null) {
      await api.createLLMProfile(body)
    } else {
      const patch: LLMProfilePatch = { ...body }
      if (!current.api_key) delete patch.api_key
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
    extra_instructions: policyExtraInstructions.value.trim(),
    exclude_globs: parseGlobs(policyExcludeGlobs.value),
    max_files: policy.value?.max_files ?? 40,
    max_diff_lines: policy.value?.max_diff_lines ?? 3000,
    max_passes: policy.value?.max_passes ?? 4,
    require_green_ci: policyRequireGreenCI.value,
  }
  try {
    applyPolicy(await api.savePolicy(selectedTeamId.value, body))
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

    <div class="split split--end">
      <div class="split__main">
        <section class="register" aria-labelledby="profiles-title">
          <div class="register__head">
            <h2 id="profiles-title" class="register__title">Registro de perfiles</h2>
            <p class="register__count">
              {{ profiles.length }} {{ profiles.length === 1 ? 'perfil' : 'perfiles' }}
            </p>
          </div>

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
                  <th scope="col" class="profile-table__actions-head">Acciones</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="profile in profiles" :key="profile.id" :data-profile-row="profile.id">
                  <td class="profile-table__name">{{ profile.name }}</td>
                  <td class="profile-table__model">
                    <span class="profile-table__provider">{{ profile.provider }}</span>
                    <span class="profile-table__separator" aria-hidden="true">/</span>
                    <span>{{ profile.model }}</span>
                    <span v-if="profile.variant" class="profile-table__variant" data-profile-variant-label>
                      ({{ profile.variant }})
                    </span>
                  </td>
                  <td class="profile-table__health">
                    <div class="health" data-health-cell>
                      <div class="health__verdict">
                        <span
                          class="badge"
                          :class="healthTone(profile.health_status)"
                          data-health-badge
                        >
                          {{ healthLabel(profile.health_status) }}
                        </span>
                        <span v-if="profile.health_stale" class="badge badge--bad" data-health-stale>
                          Desactualizado
                        </span>
                      </div>
                      <span class="health__age">{{ healthAge(profile) }}</span>
                      <span v-if="profile.health_error" class="health__error">
                        {{ profile.health_error }}
                      </span>
                    </div>
                  </td>
                  <td class="profile-table__state">
                    {{ profile.enabled ? 'Activado' : 'Desactivado' }}
                  </td>
                  <td class="profile-table__key">
                    {{ profile.api_key_set ? 'Clave configurada' : 'Sin clave' }}
                  </td>
                  <td class="profile-table__actions">
                    <div class="profile-table__action-row">
                      <button
                        v-if="isAdmin"
                        class="button button--ghost profile-table__action"
                        type="button"
                        data-health-button
                        :aria-label="`Probar salud de ${profile.name}`"
                        @click="checkHealth(profile)"
                      >
                        Probar
                      </button>
                      <button
                        v-if="isAdmin"
                        class="button button--ghost profile-table__action"
                        type="button"
                        data-edit-profile
                        :aria-label="`Editar ${profile.name}`"
                        @click="openEdit(profile)"
                      >
                        Editar
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>

      <div class="split__aside">
        <section
          v-if="isAdmin && teams.length > 0"
          id="policy"
          class="policy"
          aria-labelledby="policy-title"
        >
          <header class="policy__head">
            <h2 id="policy-title" class="policy__title">Política por equipo</h2>
            <p class="policy__hint">Se guarda por equipo y se aplica a todas sus revisiones.</p>
          </header>

          <div aria-live="polite">
            <p v-if="stalePolicy" class="stale-notice" data-stale-policy>
              Otra persona cambió esta política.
              <button class="button button--ghost" type="button" @click="loadPolicy">Recargar</button>
            </p>
          </div>

          <div class="policy__fields">
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
              <span class="field__hint">Equipo al que se aplica la política.</span>
            </label>
            <label class="field">
              <span class="field__label">Perfil LLM</span>
              <select v-model.number="policyProfileId" class="field__input" data-policy-profile>
                <option :value="null">Sin perfil</option>
                <option v-for="profile in profiles" :key="profile.id" :value="profile.id">
                  {{ profile.name }}
                </option>
              </select>
              <span class="field__hint">Perfil que ejecuta las revisiones del equipo.</span>
            </label>
            <button
              class="button button--primary policy__save"
              type="button"
              data-policy-save
              @click="savePolicy"
            >
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

          <div class="policy__extras">
            <label class="field">
              <span class="field__label">Instrucciones extra</span>
              <textarea
                v-model="policyExtraInstructions"
                class="field__input"
                rows="4"
                data-policy-extra
                placeholder="Ej.: no comentes estilo; sigue las convenciones del repo"
              ></textarea>
              <span class="field__hint">
                Se envían al revisor como datos no confiables, en una sección aparte del diff.
              </span>
            </label>
            <label class="field">
              <span class="field__label">Archivos excluidos</span>
              <textarea
                v-model="policyExcludeGlobs"
                class="field__input"
                rows="4"
                data-policy-exclude
                placeholder="**/*.pb.go&#10;vendor/**"
              ></textarea>
              <span class="field__hint">
                Un patrón por línea, estilo gitignore. No se envían al modelo ni generan hallazgos.
                Los patrones con comas (por ejemplo <code>**/*.{go,js}</code>) no se pueden pasar a OCR y se ignoran.
              </span>
            </label>
          </div>

          <p v-if="policyError" class="form-error" role="alert">{{ policyError }}</p>
          <p v-else-if="policyMessage" class="policy__message" role="status">{{ policyMessage }}</p>
        </section>
      </div>
    </div>

    <div v-if="modalOpen" class="modal-backdrop" @keydown.esc="closeModal">
      <div
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="profile-dialog-title"
        aria-describedby="profile-dialog-hint"
      >
        <form class="modal__form" @submit.prevent="submitProfile">
          <header class="modal__head">
            <h2 id="profile-dialog-title" class="modal__title">
              {{ editingId === null ? 'Nuevo perfil LLM' : 'Editar perfil LLM' }}
            </h2>
            <p id="profile-dialog-hint" class="field__hint modal__hint">
              La clave se guarda cifrada y nunca se muestra. Déjala vacía para conservar la actual.
            </p>
          </header>

          <div class="modal__body">
            <div aria-live="polite">
              <p v-if="staleProfile === 'changed'" class="stale-notice" data-stale-profile>
                Otra persona cambió este perfil.
                <button class="button button--ghost" type="button" @click="reloadProfile">Recargar</button>
              </p>
              <p v-else-if="staleProfile === 'deleted'" class="stale-notice" data-stale-profile>
                Otra persona eliminó este perfil. Guardar no lo recuperará.
              </p>
            </div>
            <div class="profile-tabs" role="tablist" aria-label="Tipo de conexión">
              <button
                class="profile-tabs__tab"
                type="button"
                role="tab"
                :aria-selected="tab === 'platform'"
                data-profile-tab-platform
                @click="setTab('platform')"
              >
                Plataforma conocida
              </button>
              <button
                class="profile-tabs__tab"
                type="button"
                role="tab"
                :aria-selected="tab === 'custom'"
                data-profile-tab-custom
                @click="setTab('custom')"
              >
                Conexión personalizada
              </button>
            </div>

            <div v-if="tab === 'platform'" role="tabpanel" aria-label="Plataforma conocida">
              <div class="platform-grid" role="group" aria-label="Plataformas disponibles">
                <button
                  v-for="platform in llmPlatforms"
                  :key="platform.id"
                  class="platform-card"
                  type="button"
                  :aria-pressed="selectedPlatform?.id === platform.id"
                  :data-platform="platform.id"
                  @click="selectPlatform(platform)"
                >
                  {{ platform.name }}
                </button>
              </div>

              <div v-if="selectedPlatform" class="modal__fields">
                <label class="field modal__field--wide">
                  <span class="field__label">Clave API</span>
                  <input
                    v-model="form.api_key"
                    class="field__input"
                    type="password"
                    autocomplete="new-password"
                    data-profile-api-key
                    @input="onAPIKeyInput"
                    @blur="loadPlatformModels"
                  />
                  <span class="field__hint">
                    Se guarda cifrada, se comprueba al guardar y no vuelve a mostrarse.
                  </span>
                </label>
                <label class="field modal__field--wide">
                  <span class="field__label">Modelo</span>
                  <select
                    class="field__input"
                    :value="modelSelect"
                    data-profile-model
                    @change="onModelSelectChange"
                  >
                    <option
                      v-for="model in platformModelOptions"
                      :key="model.id"
                      :value="model.id"
                    >
                      {{ model.label }}
                    </option>
                    <option :value="CUSTOM_MODEL">Otro modelo…</option>
                  </select>
                  <input
                    v-if="modelIsCustom"
                    v-model="modelOther"
                    class="field__input modal__model-custom"
                    type="text"
                    placeholder="Identificador del modelo"
                    aria-label="Identificador del modelo"
                    data-profile-model-custom
                    @input="onModelOtherInput"
                  />
                </label>
                <p
                  v-if="modelsLoading"
                  class="field__hint"
                  role="status"
                  data-profile-models-loading
                >
                  Cargando modelos…
                </p>
                <p
                  v-else-if="modelsNotice"
                  class="field__hint"
                  role="status"
                  data-profile-models-notice
                >
                  {{ modelsNotice }}
                </p>
              </div>
              <p v-else class="modal__platform-hint">
                Elige una plataforma para añadir su clave API.
              </p>
            </div>

            <div
              v-else
              class="modal__fields"
              role="tabpanel"
              aria-label="Conexión personalizada"
            >
              <label class="field">
                <span class="field__label">Proveedor</span>
                <input
                  v-model="form.provider"
                  class="field__input"
                  type="text"
                  data-profile-provider
                />
                <span class="field__hint">
                  Identificador del proveedor de OpenCode; por ejemplo openai.
                </span>
              </label>
              <label class="field">
                <span class="field__label">Modelo</span>
                <input
                  v-model="form.model"
                  class="field__input"
                  type="text"
                  data-profile-model
                />
                <span class="field__hint">Identificador del modelo; por ejemplo gpt-5.</span>
              </label>
              <label class="field modal__field--wide">
                <span class="field__label">Clave API</span>
                <input
                  v-model="form.api_key"
                  class="field__input"
                  type="password"
                  autocomplete="new-password"
                  data-profile-api-key
                />
                <span class="field__hint">Cifrada; no vuelve a mostrarse después de guardar.</span>
              </label>
              <label class="field modal__field--wide">
                <span class="field__label">URL base (opcional)</span>
                <input
                  v-model="form.base_url"
                  class="field__input"
                  type="text"
                  data-profile-base-url
                />
                <span class="field__hint">Endpoint compatible; opcional.</span>
              </label>
            </div>

            <details class="modal__advanced" data-profile-advanced>
              <summary class="modal__advanced-summary">Opciones avanzadas</summary>
              <div class="modal__fields modal__advanced-fields">
                <label class="field modal__field--wide">
                  <span class="field__label">Nombre</span>
                  <input v-model="form.name" class="field__input" type="text" data-profile-name />
                  <span class="field__hint">
                    Si lo dejas vacío se usa
                    «{{ defaultProfileName() || 'plataforma y modelo' }}».
                  </span>
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
                  <span class="field__hint">Solicitudes simultáneas (mínimo 1).</span>
                </label>
                <label class="field">
                  <span class="field__label">Variante de razonamiento</span>
                  <input
                    v-model="form.variant"
                    class="field__input"
                    type="text"
                    list="reasoning-variants"
                    data-profile-variant
                  />
                  <datalist id="reasoning-variants">
                    <option v-for="variant in reasoningVariants" :key="variant" :value="variant" />
                  </datalist>
                  <span class="field__hint">
                    «none», «minimal» o «low» reducen los tokens de razonamiento y la latencia; el
                    modelo debe aceptar la variante.
                  </span>
                </label>
                <label class="modal__enabled">
                  <input v-model="form.enabled" type="checkbox" data-profile-enabled />
                  <span class="modal__enabled-text">
                    <span class="field__label">Perfil activado</span>
                    <span class="field__hint">Si se desactiva, no se usa para revisiones.</span>
                  </span>
                </label>
              </div>
            </details>
          </div>

          <p v-if="formError" class="form-error modal__error" role="alert">{{ formError }}</p>
          <footer class="modal__actions">
            <button
              class="button button--ghost"
              type="button"
              data-profile-cancel
              @click="closeModal"
            >
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
          </footer>
        </form>
      </div>
    </div>
  </section>
</template>

<style scoped>
/* Hallmark · waive 26b · no aria-disabled in the modal markup · the perfil form never disables a control; global .button:disabled covers it */
.agents {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.agents .button {
  min-height: 40px;
}

.register {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.register__head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-hair) solid var(--border-strong);
}

.register__title {
  font-size: 17px;
}

.register__count {
  color: var(--text-faint);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.profile-table {
  width: 100%;
  min-width: 820px;
  border-collapse: collapse;
}

.profile-table th,
.profile-table td {
  padding: var(--space-2) var(--space-3);
  text-align: left;
  vertical-align: top;
  border-bottom: var(--rule-hair) solid var(--border);
}

.profile-table th {
  color: var(--text-muted);
  font-size: 13px;
  font-weight: 500;
  border-bottom: var(--rule-heavy) solid var(--text);
}

.profile-table__name {
  font-weight: 600;
}

.profile-table__model {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  white-space: nowrap;
}

.profile-table__provider {
  color: var(--text);
}

.profile-table__separator {
  color: var(--text-faint);
}

.profile-table__health {
  min-width: 220px;
}

.health {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.health__verdict {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-1);
}

.health__age {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}

.health__error {
  color: var(--text-faint);
  font-family: var(--font-mono);
  font-size: 13px;
  overflow-wrap: anywhere;
}

.profile-table__state,
.profile-table__key {
  font-size: 13px;
  white-space: nowrap;
}

.profile-table__actions-head,
.profile-table__actions {
  text-align: right;
}

.profile-table__action-row {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-1);
}

.profile-table__action {
  padding: var(--space-1) var(--space-3);
  font-size: 13px;
}

.policy {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  padding: var(--space-5);
  color: var(--text);
  background: var(--surface);
  border: var(--rule-hair) solid var(--border-strong);
}

.policy__head {
  padding-bottom: var(--space-3);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.policy__title {
  font-size: 18px;
}

.policy__hint {
  margin-top: var(--space-1);
  max-width: 70ch;
  color: var(--text-muted);
  font-size: 13px;
}

.policy__fields {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) auto;
  align-items: end;
  gap: var(--space-4);
}

.policy__fields .field__input {
  width: 100%;
  min-width: 0;
}

.policy__save {
  white-space: nowrap;
}

.policy__extras {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-4);
  margin-top: var(--space-4);
}

.policy__extras .field__input {
  width: 100%;
  min-width: 0;
  resize: vertical;
}

.policy-ci {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
  min-height: 40px;
  padding-top: var(--space-3);
  border-top: var(--rule-hair) solid var(--border);
  cursor: pointer;
}

.policy-ci input {
  flex: none;
  width: 20px;
  height: 20px;
  margin: 0;
  accent-color: var(--accent);
  cursor: pointer;
}

.policy-ci__text {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.policy__message {
  color: var(--ok);
  font-size: 13px;
}

.modal-backdrop {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--space-4);
  background: var(--scrim);
  overflow-y: auto;
}

.modal {
  position: relative;
  display: flex;
  flex-direction: column;
  width: min(640px, 100%);
  max-height: calc(100dvh - var(--space-6));
  color: var(--text);
  background: var(--surface);
  border: var(--rule-hair) solid var(--border-strong);
  overflow: hidden;
}

.modal::after {
  content: '';
  position: absolute;
  inset: var(--space-1);
  border: var(--rule-hair) solid var(--border);
  pointer-events: none;
}

.modal__form {
  display: flex;
  flex-direction: column;
  min-height: 0;
  max-height: inherit;
}

.modal__head {
  flex: none;
  padding: var(--space-5) var(--space-5) var(--space-4);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.modal__title {
  font-size: 22px;
}

.modal__hint {
  margin-top: var(--space-2);
  max-width: 60ch;
}

.modal__body {
  flex: 1 1 auto;
  min-height: 0;
  padding: var(--space-4) var(--space-5);
  overflow-y: auto;
}

.modal__fields {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-4);
}

.modal__field--wide {
  grid-column: 1 / -1;
}

.modal__fields .field__input {
  width: 100%;
  min-width: 0;
}

.modal__fields input[type='number'] {
  max-width: 160px;
  font-family: var(--font-mono);
}

.modal__enabled {
  display: flex;
  grid-column: 1 / -1;
  align-items: flex-start;
  gap: var(--space-3);
  min-height: 40px;
  cursor: pointer;
}

.modal__enabled input {
  flex: none;
  width: 20px;
  height: 20px;
  margin: 0;
  accent-color: var(--accent);
  cursor: pointer;
}

.modal__enabled-text {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.profile-tabs {
  display: flex;
  gap: var(--space-4);
  margin-bottom: var(--space-4);
  border-bottom: var(--rule-hair) solid var(--border);
}

.profile-tabs__tab {
  min-height: 40px;
  padding: var(--space-2) 0;
  color: var(--text-muted);
  font: inherit;
  font-size: 14px;
  background: none;
  border: 0;
  border-bottom: var(--rule-heavy) solid transparent;
  cursor: pointer;
}

.profile-tabs__tab:hover {
  color: var(--text);
}

.profile-tabs__tab:active {
  color: var(--text);
  border-bottom-color: var(--border-strong);
}

.profile-tabs__tab[aria-selected='true'] {
  color: var(--text);
  border-bottom-color: var(--text);
}

.profile-tabs__tab:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--rule-hair);
}

.platform-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-2);
  margin-bottom: var(--space-4);
}

.platform-card {
  min-height: 44px;
  padding: var(--space-2) var(--space-3);
  color: var(--text);
  font: inherit;
  font-size: 14px;
  text-align: left;
  background: var(--surface-2);
  border: var(--rule-hair) solid var(--border);
  cursor: pointer;
}

.platform-card:hover {
  border-color: var(--text-muted);
}

.platform-card:active {
  border-color: var(--text);
}

.platform-card[aria-pressed='true'] {
  border-color: var(--border-strong);
  background: var(--accent-soft);
  font-weight: 600;
}

.platform-card:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--rule-hair);
}

.modal__platform-hint {
  max-width: 60ch;
  color: var(--text-muted);
  font-size: 14px;
}

.modal__model-custom {
  margin-top: var(--space-2);
}

.modal__advanced {
  margin-top: var(--space-4);
  border-top: var(--rule-hair) solid var(--border);
}

.modal__advanced-summary {
  display: flex;
  align-items: center;
  min-height: 40px;
  color: var(--text-muted);
  font-size: 14px;
  cursor: pointer;
}

.modal__advanced-summary:hover {
  color: var(--text);
}

.modal__advanced-summary:active {
  color: var(--text);
}

.modal__advanced[open] .modal__advanced-summary {
  color: var(--text);
}

.modal__advanced-summary:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--rule-hair);
}

.modal__advanced-fields {
  margin-top: var(--space-3);
}

.modal__error {
  flex: none;
  padding: 0 var(--space-5) var(--space-4);
}

.modal__actions {
  flex: none;
  display: flex;
  justify-content: flex-end;
  gap: var(--space-3);
  padding: var(--space-3) var(--space-5) var(--space-5);
  border-top: var(--rule-hair) solid var(--border);
}

.modal__actions .button {
  white-space: nowrap;
}

.field__input:hover {
  border-color: var(--text-muted);
}

.field__input:active {
  border-color: var(--text);
}

.field__input:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--rule-hair);
}

.policy-ci input:focus-visible,
.modal__enabled input:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--rule-hair);
}

@media (max-width: 880px) {
  .policy__fields,
  .policy__extras {
    grid-template-columns: minmax(0, 1fr);
  }

  .policy__save {
    width: 100%;
  }

  .modal__fields {
    grid-template-columns: minmax(0, 1fr);
  }

  .modal__field--wide {
    grid-column: auto;
  }
}

@media (max-width: 560px) {
  .policy {
    padding: var(--space-4);
  }

  .modal-backdrop {
    padding: var(--space-2);
  }

  .modal {
    max-height: calc(100dvh - var(--space-4));
  }

  .modal__head {
    padding: var(--space-4);
  }

  .modal__body {
    padding: var(--space-4);
  }

  .modal__error {
    padding: 0 var(--space-4) var(--space-4);
  }

  .modal__actions {
    flex-wrap: wrap;
    padding: var(--space-3) var(--space-4) var(--space-4);
  }

  .platform-grid {
    grid-template-columns: minmax(0, 1fr);
  }

  .modal__actions .button {
    flex: 1 1 auto;
  }

  .profile-table {
    min-width: 760px;
  }
}

@media (max-width: 420px) {
  .modal__actions {
    flex-direction: column;
  }

  .modal__actions .button {
    width: 100%;
  }
}

@media (pointer: coarse) {
  .agents .button,
  .agents .field__input,
  .policy-ci,
  .modal__enabled {
    min-height: 44px;
  }
}
</style>
