<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type Installation, type Repository, type SetupStatus, type Team } from '../api'
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

async function load(): Promise<void> {
  loading.value = true
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

    <section v-if="canEdit" class="accounts" aria-label="Cuentas conectadas">
      <div class="accounts__header">
        <h2 class="accounts__title">Cuentas conectadas</h2>
        <div class="accounts__actions">
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
            class="button button--ghost"
            data-sync-all
            :disabled="syncingAll || syncPending.length > 0 || installations.length === 0"
            @click="syncAll"
          >
            {{ syncingAll ? 'Sincronizando…' : 'Sincronizar todo' }}
          </button>
          <a
            v-if="setupStatus?.install_url"
            class="accounts__add"
            :href="setupStatus.install_url"
            target="_blank"
            rel="noreferrer"
          >
            Añadir cuenta
          </a>
        </div>
      </div>
      <ul v-if="installations.length > 0" class="accounts__list">
        <li v-for="installation in installations" :key="installation.id" class="accounts__item">
          <span>{{ installation.account_login }}</span>
          <button
            type="button"
            class="button button--ghost"
            :data-sync-installation="installation.id"
            :disabled="isSyncing(installation.id) || syncingAll"
            @click="syncInstallation(installation.id)"
          >
            {{ isSyncing(installation.id) ? 'Sincronizando…' : 'Sincronizar' }}
          </button>
        </li>
      </ul>
      <p v-else class="accounts__empty">Todavía no hay cuentas conectadas</p>
      <p v-if="syncError" class="form-error" role="alert">{{ syncError }}</p>
      <p v-else-if="syncMessage" class="accounts__message">{{ syncMessage }}</p>
    </section>

    <p v-if="error" class="empty-state">{{ error }}</p>
    <p v-else-if="!loading && repos.length === 0" class="empty-state">
      No hay repositorios disponibles
    </p>
    <div v-else class="table-wrap">
      <table class="repo-table">
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
            <td class="repo-table__name">{{ repo.full_name }}</td>
            <td>{{ repo.account_login }}</td>
            <td>{{ repo.private ? 'Privado' : 'Público' }}</td>
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
                <option v-for="team in teams" :key="team.id" :value="team.id">{{ team.name }}</option>
              </select>
              <span v-else :data-repo-team="repo.id">{{ teamName(repo.team_id) }}</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<style scoped>
.repos {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.accounts {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
  padding: var(--space-4);
}

.accounts__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  margin-bottom: var(--space-3);
}

.accounts__title {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
}

.accounts__actions {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.accounts__add {
  color: var(--accent, #0969da);
  font-size: 14px;
  font-weight: 500;
  text-decoration: none;
}

.accounts__add:hover {
  text-decoration: underline;
}

.accounts__list {
  margin: 0;
  padding-left: var(--space-5);
  color: var(--text);
}

.accounts__item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}

.accounts__empty {
  margin: 0;
  color: var(--text-muted);
  font-size: 14px;
}

.accounts__message {
  margin-top: var(--space-2);
  color: var(--ok);
  font-size: 13px;
}

.table-wrap {
  overflow-x: auto;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
}

.repo-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 14px;
}

.repo-table th,
.repo-table td {
  padding: var(--space-3) var(--space-4);
  text-align: left;
  border-bottom: 1px solid var(--border);
}

.repo-table th {
  color: var(--text-muted);
  font-size: 12.5px;
  font-weight: 600;
}

.repo-table tbody tr:last-child td {
  border-bottom: 0;
}

.repo-table__name {
  font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
  font-weight: 600;
}

.repo-table__select {
  padding: 5px 8px;
  font-size: 13px;
}

.switch {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  cursor: pointer;
}

.switch input {
  width: 18px;
  height: 18px;
  accent-color: var(--accent);
  cursor: pointer;
}

.switch input:disabled {
  cursor: not-allowed;
  opacity: 0.6;
}

.repo-table__warning {
  margin-top: var(--space-1);
  color: var(--bad);
  font-size: 12.5px;
}

.repo-table__hint {
  margin-top: var(--space-1);
  color: var(--text-muted);
  font-size: 12.5px;
}
</style>
