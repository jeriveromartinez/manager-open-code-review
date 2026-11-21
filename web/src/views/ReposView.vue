<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type Installation, type Repository, type SetupStatus, type Team } from '../api'
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
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
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
</style>
