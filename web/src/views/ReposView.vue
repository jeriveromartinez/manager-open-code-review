<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  api,
  ApiError,
  type Installation,
  type QAPolicy,
  type Repository,
  type RepositoryPolicy,
  type RepositoryPolicyInput,
  type RepositoryPolicyOverrides,
  type RequiredCheck,
  type SetupStatus,
  type Team,
} from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const repos = ref<Repository[]>([])
const teams = ref<Team[]>([])
const installations = ref<Installation[]>([])
const setupStatus = ref<SetupStatus | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)
const refreshing = ref(false)
const syncPending = ref<number[]>([])
const syncingAll = ref(false)
const syncMessage = ref<string | null>(null)
const syncError = ref<string | null>(null)

/** Solo los administradores pueden cambiar la configuración de los repositorios. */
const canEdit = computed(() => session.me?.is_admin === true)

/** Un lead del equipo del repositorio también puede editar sus overrides. */
function canManageRepo(repo: Repository): boolean {
  if (canEdit.value) return true
  if (repo.team_id === null) return false
  return session.me?.teams.some((team) => team.id === repo.team_id && team.role === 'lead') ?? false
}

function teamName(id: number | null): string {
  if (id === null) return 'Sin equipo'
  return teams.value.find((team) => team.id === id)?.name ?? 'Sin equipo'
}

async function load({ silent = false } = {}): Promise<void> {
  if (!silent) loading.value = true
  error.value = null
  try {
    await session.load()
    const [repoList, teamList] = await Promise.all([api.repos(), api.teams()])
    repos.value = repoList
    teams.value = teamList

    if (canEdit.value) {
      const [installationsResult, setupResult] = await Promise.allSettled([
        api.installations(),
        api.setupStatus(),
      ])
      installations.value =
        installationsResult.status === 'fulfilled' ? installationsResult.value : []
      setupStatus.value = setupResult.status === 'fulfilled' ? setupResult.value : null
    } else {
      installations.value = []
      setupStatus.value = null
    }
  } catch {
    error.value = 'No se pudieron cargar los repositorios'
  } finally {
    loading.value = false
  }
}

function isSyncing(id: number): boolean {
  return syncPending.value.includes(id)
}

function clearSyncFeedback(): void {
  syncMessage.value = null
  syncError.value = null
}

async function refresh(): Promise<void> {
  refreshing.value = true
  clearSyncFeedback()
  try {
    await load()
  } finally {
    refreshing.value = false
  }
}

async function syncInstallation(id: number): Promise<void> {
  if (isSyncing(id)) return
  clearSyncFeedback()
  syncPending.value = [...syncPending.value, id]
  try {
    await api.syncInstallation(id)
    syncMessage.value = 'Sincronización encolada. Los repositorios aparecerán en unos segundos.'
  } catch {
    syncError.value = 'No se pudo encolar la sincronización.'
  } finally {
    syncPending.value = syncPending.value.filter((pending) => pending !== id)
  }
}

async function syncAll(): Promise<void> {
  if (syncingAll.value || installations.value.length === 0) return
  clearSyncFeedback()
  syncingAll.value = true
  try {
    const results = await Promise.allSettled(
      installations.value.map((installation) => api.syncInstallation(installation.id)),
    )
    if (results.some((result) => result.status === 'rejected')) {
      syncError.value = 'No se pudo encolar la sincronización.'
    } else {
      syncMessage.value = 'Sincronización encolada. Los repositorios aparecerán en unos segundos.'
    }
  } finally {
    syncingAll.value = false
  }
}

async function setEnabled(repo: Repository, event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const enabled = input.checked
  try {
    await api.patchRepo(repo.id, { enabled })
    repo.enabled = enabled
  } catch {
    input.checked = repo.enabled
    error.value = 'No se pudo actualizar la revisión automática'
  }
}

async function setTeam(repo: Repository, event: Event): Promise<void> {
  const select = event.target as HTMLSelectElement
  const raw = select.value
  const team_id = raw === '' ? null : Number(raw)
  try {
    await api.patchRepo(repo.id, { team_id })
    repo.team_id = team_id
  } catch {
    select.value = repo.team_id === null ? '' : String(repo.team_id)
    error.value = 'No se pudo asignar el equipo'
  }
}

type OverrideKind = 'number' | 'select' | 'boolean' | 'textarea'

type OverrideField = {
  key: string
  label: string
  kind: OverrideKind
  options?: { value: string; label: string }[]
}

// Campos que se pueden sobrescribir por repositorio. El alcance y la
// profundidad viven aquí; el perfil LLM sigue siendo del equipo (#153).
const policyFields: OverrideField[] = [
  {
    key: 'blocking_threshold',
    label: 'Umbral bloqueante',
    kind: 'select',
    options: [
      { value: 'blocker', label: 'Bloqueante' },
      { value: 'major', label: 'Mayor' },
      { value: 'minor', label: 'Menor' },
      { value: 'nit', label: 'Nit' },
    ],
  },
  {
    key: 'review_effort',
    label: 'Profundidad (esfuerzo)',
    kind: 'select',
    options: [
      { value: 'low', label: 'Bajo · 1 ronda' },
      { value: 'medium', label: 'Medio · 2 rondas' },
      { value: 'high', label: 'Alto · 3 rondas' },
    ],
  },
  { key: 'max_files', label: 'Máx. archivos por pasada', kind: 'number' },
  { key: 'max_diff_lines', label: 'Máx. líneas de diff por pasada', kind: 'number' },
  { key: 'max_passes', label: 'Máx. pasadas', kind: 'number' },
  {
    key: 'extra_instructions',
    label: 'Instrucciones extra',
    kind: 'textarea',
  },
  { key: 'review_context_ceiling', label: 'Techo de contexto', kind: 'number' },
  { key: 'review_token_budget', label: 'Tope de gasto', kind: 'number' },
  { key: 'require_green_ci', label: 'Exigir CI en verde', kind: 'boolean' },
  {
    key: 'auto_triage_mode',
    label: 'Verificación automática',
    kind: 'select',
    options: [
      { value: 'off', label: 'Apagada' },
      { value: 'dry_run', label: 'Solo simular' },
      { value: 'apply', label: 'Aplicar' },
    ],
  },
  { key: 'auto_withdraw', label: 'Descartar automáticamente', kind: 'boolean' },
  { key: 'verify_max_findings', label: 'Hallazgos por verificar', kind: 'number' },
  { key: 'verify_max_files', label: 'Archivos por verificar', kind: 'number' },
  { key: 'verify_token_share', label: '% de tokens para verificar', kind: 'number' },
  { key: 'verify_time_share', label: '% de tiempo para verificar', kind: 'number' },
  { key: 'verify_token_max', label: 'Tope de tokens de verificación', kind: 'number' },
]

const policyOpen = ref(false)
const policyRepo = ref<Repository | null>(null)
const repoPolicy = ref<RepositoryPolicy | null>(null)
const policyLoading = ref(false)
const policySaving = ref(false)
const policyError = ref<string | null>(null)
const policyMessage = ref<string | null>(null)
const observationMode = ref(false)
const fieldEnabled = ref<Record<string, boolean>>({})
const fieldValues = ref<Record<string, string | number>>({})
const globsEnabled = ref(false)
const globsRaw = ref('')
const checksEnabled = ref(false)
const checksRaw = ref('')

function effectiveValue(policy: QAPolicy, key: string): string {
  const values: Record<string, string | number | boolean | null> = {
    blocking_threshold: policy.blocking_threshold,
    review_effort: policy.review_effort,
    max_files: policy.max_files,
    max_diff_lines: policy.max_diff_lines,
    max_passes: policy.max_passes,
    extra_instructions: policy.extra_instructions,
    review_context_ceiling: policy.review_context_ceiling,
    review_token_budget: policy.review_token_budget,
    require_green_ci: policy.require_green_ci,
    auto_triage_mode: policy.auto_triage_mode,
    auto_withdraw: policy.auto_withdraw,
    verify_max_findings: policy.verify_max_findings,
    verify_max_files: policy.verify_max_files,
    verify_token_share: policy.verify_token_share,
    verify_time_share: policy.verify_time_share,
    verify_token_max: policy.verify_token_max,
  }
  const value = values[key]
  return value === null || value === undefined ? '' : String(value)
}

function formatChecks(checks: RequiredCheck[] | null | undefined): string {
  return (checks ?? [])
    .map((check) => (check.origin ? `${check.name} | ${check.origin}` : check.name))
    .join('\n')
}

function parseChecks(raw: string): RequiredCheck[] {
  const checks: RequiredCheck[] = []
  for (const line of raw.split('\n')) {
    const [name, origin] = line.split('|').map((part) => part.trim())
    if (!name) continue
    checks.push(origin ? { name, origin } : { name })
  }
  return checks
}

function parseLines(raw: string): string[] {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

function fillFromPolicy(policy: RepositoryPolicy): void {
  repoPolicy.value = policy
  observationMode.value = policy.overrides.observation_mode
  const enabled: Record<string, boolean> = {}
  const values: Record<string, string | number> = {}
  for (const field of policyFields) {
    const override = policy.overrides[field.key as keyof RepositoryPolicyOverrides]
    enabled[field.key] = override !== null && override !== undefined
    values[field.key] = enabled[field.key] ? String(override) : effectiveValue(policy.effective, field.key)
  }
  fieldEnabled.value = enabled
  fieldValues.value = values
  globsEnabled.value = policy.overrides.exclude_globs !== null
  globsRaw.value = (policy.overrides.exclude_globs ?? policy.effective.exclude_globs ?? []).join('\n')
  checksEnabled.value = policy.overrides.required_checks !== null
  checksRaw.value = formatChecks(policy.overrides.required_checks ?? policy.effective.required_checks)
}

function applyRepoState(repo: Repository, policy: RepositoryPolicy): void {
  repo.has_policy_override = !policy.inherited
  repo.policy_observation = policy.overrides.observation_mode
}

async function openPolicy(repo: Repository): Promise<void> {
  policyRepo.value = repo
  policyOpen.value = true
  policyLoading.value = true
  policyError.value = null
  policyMessage.value = null
  try {
    fillFromPolicy(await api.getRepoPolicy(repo.id))
  } catch (err) {
    policyError.value =
      err instanceof ApiError && err.status === 409
        ? 'El equipo no tiene política: créala en Agentes antes de configurar overrides'
        : 'No se pudo cargar la política del repositorio'
  } finally {
    policyLoading.value = false
  }
}

function closePolicy(): void {
  policyOpen.value = false
  policyRepo.value = null
  repoPolicy.value = null
  policyError.value = null
  policyMessage.value = null
}

function toggleOverride(key: string, event: Event): void {
  fieldEnabled.value[key] = (event.target as HTMLInputElement).checked
  if (fieldEnabled.value[key] && !fieldValues.value[key]) {
    const policy = repoPolicy.value
    fieldValues.value[key] = policy ? effectiveValue(policy.effective, key) : ''
  }
}

function fieldOptions(field: OverrideField): { value: string; label: string }[] {
  if (field.kind === 'boolean') {
    return [
      { value: 'true', label: 'Sí' },
      { value: 'false', label: 'No' },
    ]
  }
  return field.options ?? []
}

function provenanceLabel(key: string): string {
  const source = repoPolicy.value?.provenance[key]
  if (source === 'repo') return 'Propio'
  if (source === 'global') return 'Global'
  return 'Equipo'
}

async function saveRepoPolicy(): Promise<void> {
  const repo = policyRepo.value
  if (!repo) return
  policyError.value = null
  policyMessage.value = null

  const body: RepositoryPolicyInput = {}
  if (observationMode.value) body.observation_mode = true
  for (const field of policyFields) {
    if (!fieldEnabled.value[field.key]) continue
    const value = fieldValues.value[field.key]
    const raw = value === null || value === undefined ? '' : String(value).trim()
    if (field.kind === 'number') {
      if (raw === '' || !Number.isFinite(Number(raw))) {
        policyError.value = `«${field.label}» necesita un número`
        return
      }
      ;(body as Record<string, unknown>)[field.key] = Number(raw)
    } else if (field.kind === 'boolean') {
      ;(body as Record<string, unknown>)[field.key] = raw === 'true'
    } else {
      ;(body as Record<string, unknown>)[field.key] = raw
    }
  }
  if (globsEnabled.value) body.exclude_globs = parseLines(globsRaw.value)
  if (checksEnabled.value) body.required_checks = parseChecks(checksRaw.value)

  policySaving.value = true
  try {
    const saved =
      Object.keys(body).length === 0
        ? await api.clearRepoPolicy(repo.id)
        : await api.saveRepoPolicy(repo.id, body)
    fillFromPolicy(saved)
    applyRepoState(repo, saved)
    policyMessage.value = saved.inherited ? 'Política heredada del equipo' : 'Overrides guardados'
  } catch {
    policyError.value = 'No se pudo guardar la política del repositorio'
  } finally {
    policySaving.value = false
  }
}

async function clearOverride(): Promise<void> {
  const repo = policyRepo.value
  if (!repo) return
  policyError.value = null
  policyMessage.value = null
  policySaving.value = true
  try {
    const saved = await api.clearRepoPolicy(repo.id)
    fillFromPolicy(saved)
    applyRepoState(repo, saved)
    policyMessage.value = 'Política heredada del equipo'
  } catch {
    policyError.value = 'No se pudo volver a la herencia'
  } finally {
    policySaving.value = false
  }
}

useLiveRefresh(['repo.updated', 'team.updated'], () => load({ silent: true }))

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="repos">
    <header class="page-header">
      <div>
        <h1 class="page-title">Repositorios</h1>
        <p class="page-subtitle">Revisión automática y equipo asignado por repositorio</p>
      </div>
    </header>

    <div class="split split--narrow">
      <div v-if="canEdit" class="split__aside">
        <section class="accounts register" aria-label="Cuentas conectadas">
          <div class="register__head">
            <h2 class="register__title">Cuentas conectadas</h2>
            <div class="register__actions">
              <button
                type="button"
                class="button button--ghost"
                data-refresh-repos
                :disabled="refreshing"
                @click="refresh"
              >
                {{ refreshing ? 'Actualizando…' : 'Actualizar' }}
              </button>
              <button
                type="button"
                class="button button--primary"
                data-sync-all
                :disabled="syncingAll || syncPending.length > 0 || installations.length === 0"
                @click="syncAll"
              >
                {{ syncingAll ? 'Sincronizando…' : 'Sincronizar todo' }}
              </button>
              <a
                v-if="setupStatus?.install_url"
                class="button button--ghost accounts__add"
                :href="setupStatus.install_url"
                target="_blank"
                rel="noreferrer"
              >
                Añadir cuenta
              </a>
            </div>
          </div>
          <p class="register__hint">
            Sincronizar vuelve a leer las instalaciones y los repositorios desde GitHub. Sincronizar
            todo encola una operación por cada cuenta. El proceso se ejecuta en segundo plano y tarda
            unos segundos.
          </p>
          <div v-if="installations.length > 0" class="table-wrap">
            <table class="register-table accounts__table">
              <thead>
                <tr>
                  <th scope="col">Cuenta</th>
                  <th scope="col">Tipo de cuenta</th>
                  <th scope="col" class="cell--action">Sincronización</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="installation in installations" :key="installation.id">
                  <td class="mono">{{ installation.account_login }}</td>
                  <td>{{ installation.account_type === 'Organization' ? 'Organización' : 'Usuario' }}</td>
                  <td class="cell--action">
                    <button
                      type="button"
                      class="button button--ghost"
                      :data-sync-installation="installation.id"
                      :disabled="isSyncing(installation.id) || syncingAll"
                      @click="syncInstallation(installation.id)"
                    >
                      {{ isSyncing(installation.id) ? 'Sincronizando…' : 'Sincronizar' }}
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="empty-state accounts__empty">Todavía no hay cuentas conectadas</p>
          <p v-if="syncError" class="form-error" role="alert">{{ syncError }}</p>
          <p v-else-if="syncMessage" class="register__message" role="status">{{ syncMessage }}</p>
        </section>
      </div>

      <div class="split__main">
        <section class="register" aria-label="Repositorios">
          <div class="register__head">
            <h2 class="register__title">Registro de repositorios</h2>
            <span v-if="!loading" class="register__meta">Total: {{ repos.length }}</span>
          </div>
          <p v-if="error" class="empty-state" role="alert">{{ error }}</p>
          <p v-else-if="!loading && repos.length === 0" class="empty-state">
            No hay repositorios disponibles
          </p>
          <div v-else class="table-wrap">
            <table class="repo-table register-table">
              <thead>
                <tr>
                  <th scope="col">Repositorio</th>
                  <th scope="col">Cuenta</th>
                  <th scope="col">Visibilidad</th>
                  <th scope="col">Revisión automática</th>
                  <th scope="col">Equipo</th>
                  <th scope="col">Política</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="repo in repos" :key="repo.id" :data-repo-row="repo.id">
                  <td class="repo-table__name">
                    <span class="repo-table__id">#{{ repo.id }}</span>
                    <span class="repo-table__fullname">{{ repo.full_name }}</span>
                  </td>
                  <td class="mono">{{ repo.account_login }}</td>
                  <td>
                    <span class="badge" :class="repo.private ? 'badge--muted' : 'badge--ok'">
                      {{ repo.private ? 'Privado' : 'Público' }}
                    </span>
                  </td>
                  <td>
                    <label class="switch">
                      <input
                        type="checkbox"
                        role="switch"
                        :checked="repo.enabled"
                        :disabled="!canEdit"
                        :aria-label="`Revisión automática para ${repo.full_name}`"
                        :data-repo-enabled="repo.id"
                        @change="setEnabled(repo, $event)"
                      />
                      <span>{{ repo.enabled ? 'Activada' : 'Desactivada' }}</span>
                    </label>
                    <p
                      v-if="repo.enabled && repo.team_id === null"
                      class="repo-table__warning"
                      :data-repo-warning="repo.id"
                    >
                      Habilitado sin equipo: no se revisará
                    </p>
                    <p
                      v-else-if="repo.enabled && repo.team_id !== null && !repo.has_policy"
                      class="repo-table__warning"
                      :data-repo-policy-warning="repo.id"
                    >
                      El equipo no tiene política: no se revisará
                    </p>
                    <p
                      v-else-if="canEdit && repo.team_id === null"
                      class="repo-table__hint"
                      :data-repo-hint="repo.id"
                    >
                      Sin equipo: activar no iniciará revisiones
                    </p>
                    <p
                      v-else-if="canEdit && repo.team_id !== null && !repo.has_policy"
                      class="repo-table__hint"
                      :data-repo-policy-hint="repo.id"
                    >
                      El equipo no tiene política: activar no iniciará revisiones
                    </p>
                  </td>
                  <td>
                    <select
                      v-if="canEdit"
                      class="field__input repo-table__select"
                      :value="repo.team_id ?? ''"
                      :aria-label="`Equipo de ${repo.full_name}`"
                      :data-repo-team="repo.id"
                      @change="setTeam(repo, $event)"
                    >
                      <option value="">Sin equipo</option>
                      <option v-for="team in teams" :key="team.id" :value="team.id">
                        {{ team.name }}
                      </option>
                    </select>
                    <span v-else :data-repo-team="repo.id">{{ teamName(repo.team_id) }}</span>
                  </td>
                  <td>
                    <div v-if="repo.team_id === null" class="repo-table__policy repo-table__policy--none">
                      —
                    </div>
                    <div v-else class="repo-table__policy">
                      <span
                        class="badge"
                        :class="repo.has_policy_override ? 'badge--ok' : 'badge--muted'"
                        :data-repo-policy-state="repo.id"
                      >
                        {{ repo.has_policy_override ? 'Propia' : 'Heredada' }}
                      </span>
                      <span
                        v-if="repo.policy_observation"
                        class="badge badge--busy"
                        :data-repo-policy-observation="repo.id"
                      >
                        Observación
                      </span>
                      <button
                        v-if="canManageRepo(repo) && repo.has_policy"
                        type="button"
                        class="button button--ghost repo-table__policy-edit"
                        :data-repo-policy="repo.id"
                        @click="openPolicy(repo)"
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
    </div>

    <div v-if="policyOpen" class="policy-backdrop" @keydown.esc="closePolicy">
      <section
        class="policy-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="repo-policy-title"
      >
        <header class="policy-dialog__head">
          <div>
            <h2 id="repo-policy-title" class="policy-dialog__title">
              Política de {{ policyRepo?.full_name }}
            </h2>
            <p class="policy-dialog__hint">
              Los campos desmarcados heredan la política del equipo. El perfil LLM se configura por
              equipo.
            </p>
          </div>
          <button type="button" class="button button--ghost" data-close-policy @click="closePolicy">
            Cerrar
          </button>
        </header>

        <p v-if="policyLoading" class="empty-state">Cargando política…</p>
        <p v-else-if="!repoPolicy && policyError" class="form-error" role="alert">
          {{ policyError }}
        </p>
        <template v-else-if="repoPolicy">
          <p class="policy-dialog__meta">
            Revisión
            <span class="mono" data-repo-policy-revision>{{ repoPolicy.revision }}</span>
            · {{ repoPolicy.inherited ? 'Sin overrides' : 'Con overrides' }}
          </p>

          <label class="policy-toggle">
            <input v-model="observationMode" type="checkbox" data-repo-policy-observation-mode />
            <span>
              <span class="field__label">Modo observación</span>
              <span class="field__hint">
                Calcula, expone y registra los overrides sin cambiar el comportamiento de la
                revisión; se aplica la política del equipo.
              </span>
            </span>
          </label>

          <div class="policy-dialog__fields">
            <label v-for="field in policyFields" :key="field.key" class="field policy-field">
              <span class="policy-field__head">
                <input
                  type="checkbox"
                  :checked="fieldEnabled[field.key]"
                  :data-policy-override="field.key"
                  @change="toggleOverride(field.key, $event)"
                />
                <span class="field__label">{{ field.label }}</span>
                <span class="badge badge--muted" :data-policy-source="field.key">
                  {{ provenanceLabel(field.key) }}
                </span>
              </span>
              <select
                v-if="field.kind === 'select' || field.kind === 'boolean'"
                v-model="fieldValues[field.key]"
                class="field__input"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
              >
                <option
                  v-for="option in fieldOptions(field)"
                  :key="option.value"
                  :value="option.value"
                >
                  {{ option.label }}
                </option>
              </select>
              <textarea
                v-else-if="field.kind === 'textarea'"
                v-model="fieldValues[field.key]"
                class="field__input"
                rows="3"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
                placeholder="Ej.: no comentes estilo; sigue las convenciones del repo"
              ></textarea>
              <input
                v-else
                v-model="fieldValues[field.key]"
                class="field__input"
                type="number"
                :disabled="!fieldEnabled[field.key]"
                :data-policy-field="field.key"
              />
            </label>
          </div>

          <label class="field policy-field">
            <span class="policy-field__head">
              <input v-model="globsEnabled" type="checkbox" data-policy-override="exclude_globs" />
              <span class="field__label">Archivos excluidos</span>
              <span class="badge badge--muted" :data-policy-source="'exclude_globs'">
                {{ provenanceLabel('exclude_globs') }}
              </span>
            </span>
            <textarea
              v-model="globsRaw"
              class="field__input"
              rows="3"
              :disabled="!globsEnabled"
              data-policy-field="exclude_globs"
              placeholder="vendor/**&#10;**/*.pb.go"
            ></textarea>
            <span class="field__hint">Un patrón por línea.</span>
          </label>

          <label class="field policy-field">
            <span class="policy-field__head">
              <input v-model="checksEnabled" type="checkbox" data-policy-override="required_checks" />
              <span class="field__label">Checks requeridos</span>
              <span class="badge badge--muted" :data-policy-source="'required_checks'">
                {{ provenanceLabel('required_checks') }}
              </span>
            </span>
            <textarea
              v-model="checksRaw"
              class="field__input"
              rows="3"
              :disabled="!checksEnabled"
              data-policy-field="required_checks"
              placeholder="build | app:12345&#10;ci/test | status"
            ></textarea>
            <span class="field__hint">
              Uno por línea: <code>nombre</code> o <code>nombre | origen</code>. Vacío exige todos
              los checks reportados.
            </span>
          </label>

          <p v-if="policyError" class="form-error" role="alert">{{ policyError }}</p>
          <p v-else-if="policyMessage" class="policy-message" role="status">{{ policyMessage }}</p>

          <footer class="policy-dialog__actions">
            <button
              type="button"
              class="button button--ghost"
              data-repo-policy-clear
              :disabled="policySaving"
              @click="clearOverride"
            >
              Volver a herencia
            </button>
            <button
              type="button"
              class="button button--primary"
              data-repo-policy-save
              :disabled="policySaving"
              @click="saveRepoPolicy"
            >
              Guardar overrides
            </button>
          </footer>
        </template>
      </section>
    </div>
  </section>
</template>

<style scoped>
.repos {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.register {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.register__head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--space-3);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.register__title {
  font-size: 17px;
}

.register__meta {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
}

.register__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.register .button {
  min-height: 40px;
}

.accounts__add {
  text-decoration: none;
}

.register__hint {
  max-width: 72ch;
  color: var(--text-muted);
  font-size: 13.5px;
}

.register__message {
  color: var(--ok);
  font-size: 13.5px;
}

.accounts__empty {
  color: var(--text-muted);
}

.register-table {
  font-size: 14.5px;
}

.register-table th {
  padding: var(--space-3);
  color: var(--text-muted);
  font-size: 13px;
  font-weight: 500;
}

.register-table td {
  padding: var(--space-3);
}

.register-table th + th,
.register-table td + td {
  border-left: var(--rule-hair) solid var(--border);
}

.cell--action {
  text-align: right;
  white-space: nowrap;
}

.mono,
.repo-table__name {
  font-family: var(--font-mono);
}

.repo-table__id {
  margin-right: var(--space-2);
  color: var(--text-faint);
  font-size: 13px;
}

.repo-table__fullname {
  white-space: nowrap;
}

.repo-table__select {
  min-height: 40px;
  max-width: 100%;
  padding: var(--space-1) var(--space-2);
  font-size: 14px;
}

.repo-table__select:hover {
  border-color: var(--text);
  background: var(--surface-3);
}

.repo-table__select:active {
  background: var(--surface-3);
}

.repo-table__select:focus-visible,
.switch input:focus-visible {
  outline: var(--rule-heavy) solid var(--focus);
  outline-offset: var(--space-1);
}

.switch {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 40px;
  cursor: pointer;
}

.switch input {
  width: 18px;
  height: 18px;
  accent-color: var(--accent);
  cursor: pointer;
}

.switch:hover span {
  text-decoration: underline;
  text-underline-offset: 3px;
}

.switch:active span {
  color: var(--text-muted);
}

.switch input:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}

.repo-table__warning {
  margin-top: var(--space-1);
  color: var(--bad);
  font-size: 13px;
}

.repo-table__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 13px;
}

@media (pointer: coarse) {
  .register .button,
  .switch,
  .repo-table__select {
    min-height: 44px;
  }
}

.repo-table__policy {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-1) var(--space-2);
}

.repo-table__policy--none {
  color: var(--text-faint);
}

.repo-table__policy-edit {
  min-height: 32px;
  padding: 0 var(--space-2);
  font-size: 13px;
}

.policy-backdrop {
  position: fixed;
  inset: 0;
  z-index: 50;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: var(--space-5);
  overflow: auto;
  background: rgb(0 0 0 / 55%);
}

.policy-dialog {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  width: min(780px, 100%);
  max-height: calc(100vh - 2 * var(--space-5));
  padding: var(--space-4);
  overflow: auto;
  background: var(--surface);
  border: var(--rule-heavy) solid var(--text);
}

.policy-dialog__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  padding-bottom: var(--space-2);
  border-bottom: var(--rule-heavy) solid var(--text);
}

.policy-dialog__title {
  font-size: 17px;
}

.policy-dialog__hint,
.policy-dialog__meta {
  color: var(--text-muted);
  font-size: 13.5px;
}

.policy-dialog__fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: var(--space-3);
}

.policy-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.policy-field__head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.policy-toggle {
  display: flex;
  align-items: flex-start;
  gap: var(--space-2);
}

.policy-toggle .field__hint {
  display: block;
}

.policy-message {
  color: var(--ok);
  font-size: 13.5px;
}

.policy-dialog__actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: var(--space-2);
}

@media (max-width: 700px) {
  .policy-dialog__fields {
    grid-template-columns: 1fr;
  }
}
</style>
