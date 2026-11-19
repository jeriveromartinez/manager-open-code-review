<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, type Repository, type Team } from '../api'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const repos = ref<Repository[]>([])
const teams = ref<Team[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

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
    const [repoList, teamList] = await Promise.all([api.repos(), api.teams()])
    repos.value = repoList
    teams.value = teamList
  } catch {
    error.value = 'No se pudieron cargar los repositorios'
  } finally {
    loading.value = false
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
  void session.load()
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

    <p v-if="error" class="empty-state">{{ error }}</p>
    <p v-else-if="!loading && repos.length === 0" class="empty-state">
      No hay repositorios disponibles
    </p>
    <div v-else class="table-wrap">
      <table class="repo-table">
        <thead>
          <tr>
            <th scope="col">Repositorio</th>
            <th scope="col">Visibilidad</th>
            <th scope="col">Revisión automática</th>
            <th scope="col">Equipo</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="repo in repos" :key="repo.id" :data-repo-row="repo.id">
            <td class="repo-table__name">{{ repo.full_name }}</td>
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
</style>
