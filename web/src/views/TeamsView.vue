<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import {
  api,
  ApiError,
  type Repository,
  type Team,
  type TeamMember,
  type TeamRole,
  type User,
} from '../api'
import { useLiveRefresh } from '../composables/useLiveRefresh'
import { useSessionStore } from '../stores/session'

const session = useSessionStore()

const teams = ref<Team[]>([])
const users = ref<User[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const usersError = ref<string | null>(null)
const usersLoaded = ref(false)
const usersLoading = ref(false)
const formError = ref<string | null>(null)
const newTeamName = ref('')
const drafts = ref<Record<number, { userId: number | null; role: TeamRole }>>({})

type BlockingRepository = { id: number; full_name: string }

const renameTarget = ref<Team | null>(null)
const renameName = ref('')
const renameError = ref<string | null>(null)
const renameSaving = ref(false)

const deleteTarget = ref<Team | null>(null)
const deleteRepos = ref<Repository[]>([])
const deleteLoading = ref(false)
const deleteConfirmName = ref('')
const deleteError = ref<string | null>(null)
const deleteBlocked = ref<BlockingRepository[]>([])
const deleteSaving = ref(false)

const roleLabels: Record<TeamRole, string> = {
  lead: 'Lead',
  qa: 'QA',
  developer: 'Desarrollador',
}

const isAdmin = computed(() => session.me?.is_admin === true)

const otherTeams = computed(() =>
  deleteTarget.value === null
    ? []
    : teams.value.filter((team) => team.id !== deleteTarget.value?.id),
)

const canDelete = computed(() => {
  const team = deleteTarget.value
  return (
    team !== null &&
    !deleteLoading.value &&
    !deleteSaving.value &&
    deleteRepos.value.length === 0 &&
    deleteConfirmName.value === team.name
  )
})

/** Solo quien gestiona miembros necesita el listado de usuarios. */
const needsUsers = computed(
  () => isAdmin.value || (session.me?.teams.some((team) => team.role === 'lead') ?? false),
)

/** Un administrador gestiona todos los equipos; un lead solo el suyo. */
function canManage(team: Team): boolean {
  if (isAdmin.value) return true
  return (
    session.me?.teams.some((membership) => membership.id === team.id && membership.role === 'lead') ??
    false
  )
}

function policyHref(team: Team): string {
  return `/agents?team=${team.id}#policy`
}

function teamCounts(team: Team): string {
  const repositories = `${team.repository_count} repo${team.repository_count === 1 ? '' : 's'}`
  const members = `${team.members.length} miembro${team.members.length === 1 ? '' : 's'}`
  return `${repositories} · ${members}`
}

function ensureDraft(teamId: number): void {
  if (!drafts.value[teamId]) {
    drafts.value[teamId] = { userId: null, role: 'qa' }
  }
}

async function loadUsers({ force = false } = {}): Promise<void> {
  if (!needsUsers.value || usersLoading.value || (usersLoaded.value && !force)) return
  usersLoading.value = true
  usersError.value = null
  try {
    users.value = await api.users()
    usersLoaded.value = true
  } catch {
    // No es fatal: los equipos y sus controles siguen disponibles.
    usersError.value = 'No se pudo cargar la lista de usuarios; el selector puede estar vacío'
  } finally {
    usersLoading.value = false
  }
}

async function load({ silent = false } = {}): Promise<void> {
  if (!silent) loading.value = true
  error.value = null
  try {
    teams.value = await api.teams()
    teams.value.forEach((team) => ensureDraft(team.id))
  } catch {
    error.value = 'No se pudieron cargar los equipos'
  } finally {
    loading.value = false
  }
  await loadUsers({ force: silent })
}

// Drafts live in their own ref keyed by team, so a refetch leaves a
// half-filled "add member" form alone.
useLiveRefresh(['team.updated', 'user.updated'], () => load({ silent: true }))

// La sesión se carga en paralelo; cuando llega, habilitamos el listado si el
// usuario es admin o lead de algún equipo.
watch(
  () => session.me,
  () => {
    void loadUsers()
  },
)

async function createTeam(): Promise<void> {
  const name = newTeamName.value.trim()
  if (!name) return
  formError.value = null
  try {
    const team = await api.createTeam(name)
    teams.value.push(team)
    ensureDraft(team.id)
    newTeamName.value = ''
  } catch {
    formError.value = 'No se pudo crear el equipo'
  }
}

async function addMember(team: Team): Promise<void> {
  const draft = drafts.value[team.id]
  if (!draft?.userId) return
  const userId = Number(draft.userId)
  formError.value = null
  try {
    await api.setMember(team.id, userId, draft.role)
    const existing = team.members.find((member) => member.user_id === userId)
    if (existing) {
      existing.role = draft.role
    } else {
      const user = users.value.find((candidate) => candidate.id === userId)
      team.members.push({
        user_id: userId,
        login: user?.login ?? String(userId),
        avatar_url: user?.avatar_url ?? '',
        role: draft.role,
      })
    }
    draft.userId = null
  } catch {
    formError.value = 'No se pudo añadir el miembro'
  }
}

async function changeRole(team: Team, member: TeamMember, event: Event): Promise<void> {
  const role = (event.target as HTMLSelectElement).value as TeamRole
  formError.value = null
  try {
    await api.setMember(team.id, member.user_id, role)
    member.role = role
  } catch {
    formError.value = 'No se pudo cambiar el rol'
    void load()
  }
}

async function removeMember(team: Team, member: TeamMember): Promise<void> {
  formError.value = null
  try {
    await api.removeMember(team.id, member.user_id)
    team.members = team.members.filter((candidate) => candidate.user_id !== member.user_id)
  } catch {
    formError.value = 'No se pudo quitar el miembro'
    void load()
  }
}

function openRename(team: Team): void {
  renameTarget.value = team
  renameName.value = team.name
  renameError.value = null
}

function closeRename(): void {
  renameTarget.value = null
  renameName.value = ''
  renameError.value = null
}

async function submitRename(): Promise<void> {
  const team = renameTarget.value
  const name = renameName.value.trim()
  if (!team || !name || renameSaving.value) return
  renameSaving.value = true
  renameError.value = null
  try {
    await api.renameTeam(team.id, name)
    closeRename()
    await load()
  } catch {
    renameError.value = 'No se pudo renombrar el equipo'
  } finally {
    renameSaving.value = false
  }
}

async function openDelete(team: Team): Promise<void> {
  deleteTarget.value = team
  deleteConfirmName.value = ''
  deleteError.value = null
  deleteBlocked.value = []
  deleteRepos.value = []
  deleteLoading.value = false
  if (team.repository_count === 0) return

  deleteLoading.value = true
  try {
    const assigned = (await api.repos()).filter((repo) => repo.team_id === team.id)
    if (deleteTarget.value?.id === team.id) deleteRepos.value = assigned
  } catch {
    if (deleteTarget.value?.id === team.id) {
      deleteError.value = 'No se pudieron cargar los repositorios del equipo'
    }
  } finally {
    if (deleteTarget.value?.id === team.id) deleteLoading.value = false
  }
}

function closeDelete(): void {
  deleteTarget.value = null
  deleteRepos.value = []
  deleteConfirmName.value = ''
  deleteError.value = null
  deleteBlocked.value = []
}

async function reassignRepository(repo: Repository, event: Event): Promise<void> {
  const select = event.target as HTMLSelectElement
  const teamId = select.value === '' ? null : Number(select.value)
  deleteError.value = null
  try {
    await api.patchRepo(repo.id, { team_id: teamId })
    deleteRepos.value = deleteRepos.value.filter((candidate) => candidate.id !== repo.id)
    if (deleteTarget.value && deleteTarget.value.repository_count > 0) {
      deleteTarget.value.repository_count -= 1
    }
  } catch {
    select.value = ''
    deleteError.value = 'No se pudo reasignar el repositorio'
  }
}

function conflictRepositories(err: unknown): BlockingRepository[] {
  if (!(err instanceof ApiError) || err.status !== 409) return []
  const repositories = (err.payload as { repositories?: BlockingRepository[] } | undefined)
    ?.repositories
  return Array.isArray(repositories) ? repositories : []
}

async function submitDelete(): Promise<void> {
  const team = deleteTarget.value
  if (!team || !canDelete.value) return
  deleteSaving.value = true
  deleteError.value = null
  deleteBlocked.value = []
  try {
    await api.deleteTeam(team.id)
    closeDelete()
    await load()
  } catch (err) {
    const repositories = conflictRepositories(err)
    if (repositories.length > 0) {
      deleteBlocked.value = repositories
      deleteError.value = 'El servidor rechazó la eliminación: quedan repositorios asignados'
    } else {
      deleteError.value = 'No se pudo eliminar el equipo'
    }
  } finally {
    deleteSaving.value = false
  }
}

onMounted(() => {
  void session.load()
  void load()
})
</script>

<template>
  <section class="teams">
    <header class="page-header">
      <div>
        <h1 class="page-title">Equipos</h1>
        <p class="page-subtitle">Equipos, miembros y responsabilidades</p>
      </div>
      <form v-if="isAdmin" class="create-team" @submit.prevent="createTeam">
        <label class="field create-team__field">
          <span class="field__label">Nombre del equipo</span>
          <input
            v-model="newTeamName"
            class="field__input"
            type="text"
            placeholder="Nombre del equipo"
            aria-label="Nombre del equipo"
            data-create-team-name
          />
        </label>
        <button class="button button--primary" type="button" data-create-team @click="createTeam">
          Crear equipo
        </button>
      </form>
    </header>

    <p v-if="formError" class="form-error" role="alert">{{ formError }}</p>

    <p v-if="error" class="empty-state">{{ error }}</p>
    <p v-else-if="!loading && teams.length === 0" class="empty-state">No hay equipos todavía</p>
    <div v-else class="team-register card-grid">
      <article
        v-for="team in teams"
        :key="team.id"
        class="team-roster"
        :data-team-id="team.id"
        :data-team-name="team.name"
      >
        <header class="team-roster__header">
          <div class="team-roster__identity">
            <h2 class="team-roster__title">{{ team.name }}</h2>
            <p class="team-roster__slug">{{ team.slug }}</p>
          </div>
          <div class="team-roster__meta">
            <p class="team-roster__count">{{ teamCounts(team) }}</p>
            <a
              v-if="isAdmin"
              class="button button--ghost team-roster__policy"
              :href="policyHref(team)"
              data-team-policy
            >
              Política de revisión
            </a>
            <button
              v-if="isAdmin"
              class="button button--ghost team-roster__policy"
              type="button"
              data-rename-team
              @click="openRename(team)"
            >
              Renombrar
            </button>
            <button
              v-if="isAdmin"
              class="button button--ghost team-roster__policy"
              type="button"
              data-delete-team
              @click="openDelete(team)"
            >
              Eliminar
            </button>
          </div>
        </header>

        <div class="table-wrap team-roster__table">
          <table :aria-label="`Miembros de ${team.name}`">
            <thead>
              <tr>
                <th scope="col">Miembro</th>
                <th scope="col">Rol</th>
                <th v-if="canManage(team)" scope="col">Acciones</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="member in team.members" :key="member.user_id">
                <td class="team-roster__member">{{ member.login }}</td>
                <td class="team-roster__role-cell">
                  <select
                    v-if="canManage(team)"
                    class="field__input team-roster__role"
                    :value="member.role"
                    :aria-label="`Rol de ${member.login}`"
                    data-change-role
                    @change="changeRole(team, member, $event)"
                  >
                    <option value="lead">Lead</option>
                    <option value="qa">QA</option>
                    <option value="developer">Desarrollador</option>
                  </select>
                  <span v-else class="team-roster__role-read">{{ roleLabels[member.role] }}</span>
                </td>
                <td v-if="canManage(team)" class="team-roster__actions">
                  <button
                    class="button button--ghost team-roster__remove"
                    type="button"
                    :aria-label="`Quitar a ${member.login}`"
                    data-remove-member
                    @click="removeMember(team, member)"
                  >
                    Quitar
                  </button>
                </td>
              </tr>
              <tr v-if="team.members.length === 0">
                <td :colspan="canManage(team) ? 3 : 2" class="team-roster__empty">
                  Este equipo no tiene miembros todavía
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <form
          v-if="canManage(team)"
          class="member-add"
          data-add-member
          @submit.prevent="addMember(team)"
        >
          <label class="field member-add__field">
            <span class="field__label">Usuario</span>
            <select
              v-model.number="drafts[team.id].userId"
              class="field__input"
              aria-label="Selecciona un usuario para añadir"
              data-add-member-user
            >
              <option :value="null">Selecciona un usuario</option>
              <option v-for="user in users" :key="user.id" :value="user.id">{{ user.login }}</option>
            </select>
          </label>
          <label class="field member-add__field">
            <span class="field__label">Rol</span>
            <select
              v-model="drafts[team.id].role"
              class="field__input"
              aria-label="Rol del nuevo miembro"
              data-add-member-role
            >
              <option value="lead">Lead</option>
              <option value="qa">QA</option>
              <option value="developer">Desarrollador</option>
            </select>
          </label>
          <button
            class="button button--primary member-add__submit"
            type="button"
            data-add-member-submit
            @click="addMember(team)"
          >
            Añadir miembro
          </button>
          <p v-if="usersError" class="form-error member-add__error" data-users-error>
            {{ usersError }}
          </p>
        </form>
      </article>
    </div>

    <div v-if="renameTarget" class="modal-backdrop" @keydown.esc="closeRename">
      <div
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="rename-dialog-title"
        data-rename-dialog
      >
        <form class="modal__form" @submit.prevent="submitRename">
          <header class="modal__head">
            <h2 id="rename-dialog-title" class="modal__title">Renombrar equipo</h2>
            <p class="field__hint modal__hint">
              El identificador (slug) se recalcula con el nuevo nombre.
            </p>
          </header>
          <div class="modal__body">
            <label class="field">
              <span class="field__label">Nombre del equipo</span>
              <input
                v-model="renameName"
                class="field__input"
                type="text"
                :aria-label="`Nombre de ${renameTarget?.name ?? ''}`"
                data-rename-name
              />
            </label>
            <p v-if="renameError" class="form-error" role="alert">{{ renameError }}</p>
          </div>
          <footer class="modal__actions">
            <button class="button button--ghost" type="button" @click="closeRename">
              Cancelar
            </button>
            <button
              class="button button--primary"
              type="submit"
              :disabled="renameSaving || !renameName.trim()"
              data-rename-submit
            >
              {{ renameSaving ? 'Guardando…' : 'Guardar' }}
            </button>
          </footer>
        </form>
      </div>
    </div>

    <div v-if="deleteTarget" class="modal-backdrop" @keydown.esc="closeDelete">
      <div
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="delete-dialog-title"
        data-delete-dialog
      >
        <form class="modal__form" @submit.prevent="submitDelete">
          <header class="modal__head">
            <h2 id="delete-dialog-title" class="modal__title">Eliminar equipo</h2>
            <p class="field__hint modal__hint">
              Se elimina «{{ deleteTarget?.name ?? '' }}» junto con sus miembros. Esta acción no se
              puede deshacer.
            </p>
          </header>
          <div class="modal__body">
            <p v-if="deleteLoading" class="field__hint">Cargando repositorios…</p>
            <div v-else-if="deleteRepos.length > 0" class="delete-repos" data-delete-repos>
              <p class="field__hint">Reasigna cada repositorio antes de eliminar el equipo.</p>
              <ul class="delete-repos__list">
                <li v-for="repo in deleteRepos" :key="repo.id" class="delete-repos__row">
                  <span class="delete-repos__name">{{ repo.full_name }}</span>
                  <select
                    class="field__input delete-repos__select"
                    :aria-label="`Reasignar ${repo.full_name}`"
                    :data-delete-repo="repo.id"
                    @change="reassignRepository(repo, $event)"
                  >
                    <option value="">Sin equipo</option>
                    <option
                      v-for="candidate in otherTeams"
                      :key="candidate.id"
                      :value="candidate.id"
                    >
                      {{ candidate.name }}
                    </option>
                  </select>
                </li>
              </ul>
            </div>
            <div
              v-if="deleteBlocked.length > 0"
              class="delete-blocked"
              role="alert"
              data-delete-blocked
            >
              <p>El servidor rechazó la eliminación: estos repositorios siguen asignados.</p>
              <ul class="delete-repos__list">
                <li v-for="repo in deleteBlocked" :key="repo.id">{{ repo.full_name }}</li>
              </ul>
            </div>
            <label class="field">
              <span class="field__label">
                Escribe «{{ deleteTarget?.name ?? '' }}» para confirmar
              </span>
              <input
                v-model="deleteConfirmName"
                class="field__input"
                type="text"
                autocomplete="off"
                :aria-label="`Confirma el nombre de ${deleteTarget?.name ?? ''}`"
                data-delete-confirm
              />
            </label>
            <p v-if="deleteError" class="form-error" role="alert">{{ deleteError }}</p>
          </div>
          <footer class="modal__actions">
            <button class="button button--ghost" type="button" @click="closeDelete">
              Cancelar
            </button>
            <button
              class="button button--primary"
              type="submit"
              :disabled="!canDelete"
              data-delete-submit
            >
              {{ deleteSaving ? 'Eliminando…' : 'Eliminar' }}
            </button>
          </footer>
        </form>
      </div>
    </div>
  </section>
</template>

<style scoped>
.teams {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}

.create-team {
  display: flex;
  align-items: flex-end;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.create-team__field {
  flex: 1 1 220px;
  min-width: 0;
}

.create-team__field .field__input {
  width: 100%;
}

.team-roster {
  display: flex;
  flex-direction: column;
  border-top: var(--rule-heavy) solid var(--text);
  border-bottom: var(--rule-hair) solid var(--border-strong);
}

.team-roster__header {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-4);
  padding: var(--space-3) 0;
}

.team-roster__identity {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: var(--space-1) var(--space-3);
  min-width: 0;
}

.team-roster__title {
  font-size: 20px;
}

.team-roster__slug {
  overflow-wrap: anywhere;
  color: var(--text-faint);
  font-family: var(--font-mono);
  font-size: 13px;
  line-height: 1.5;
}

.team-roster__meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-3);
}

.team-roster__count {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.team-roster__policy {
  min-height: 40px;
  padding: var(--space-1) var(--space-2);
  font-size: 13px;
}

.team-roster__table {
  border: 0;
  border-top: var(--rule-hair) solid var(--border-strong);
}

.team-roster__table th {
  color: var(--text-muted);
}

.team-roster__table th,
.team-roster__table td {
  padding: var(--space-2) var(--space-3);
  vertical-align: middle;
}

.team-roster__member {
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: 14px;
  font-weight: 500;
}

.team-roster__role {
  width: 100%;
  max-width: 220px;
  padding: var(--space-1) var(--space-2);
  font-size: 14px;
}

.team-roster__role-read {
  display: inline-block;
  color: var(--text);
  font-size: 14px;
}

.team-roster__actions {
  text-align: right;
  white-space: nowrap;
}

.team-roster__remove {
  min-height: 40px;
  padding: var(--space-1) var(--space-2);
  font-size: 13px;
}

.team-roster__empty {
  color: var(--text-muted);
  font-size: 14px;
}

.member-add {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(0, 1fr) auto;
  align-items: end;
  gap: var(--space-2) var(--space-3);
  padding: var(--space-3) 0;
  border-top: var(--rule-hair) solid var(--border);
}

.member-add__field {
  min-width: 0;
}

.member-add__submit {
  min-height: 40px;
  white-space: nowrap;
}

.member-add__error {
  grid-column: 1 / -1;
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
  width: min(560px, 100%);
  max-height: calc(100dvh - var(--space-6));
  color: var(--text);
  background: var(--surface);
  border: var(--rule-hair) solid var(--border-strong);
  overflow: hidden;
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
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-height: 0;
  padding: var(--space-4) var(--space-5);
  overflow-y: auto;
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

.delete-repos__list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  margin: 0;
  padding: 0;
  list-style: none;
}

.delete-repos__row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 220px);
  align-items: center;
  gap: var(--space-2) var(--space-3);
}

.delete-repos__name {
  overflow-wrap: anywhere;
  font-family: var(--font-mono);
  font-size: 14px;
}

.delete-repos__select {
  width: 100%;
}

.delete-blocked {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  font-size: 14px;
}

.teams .field__input {
  transition:
    border-color var(--dur-micro) ease,
    background-color var(--dur-micro) ease;
}

.teams select.field__input {
  cursor: pointer;
}

.teams .field__input:hover {
  border-color: var(--text-muted);
}

.teams .field__input:active {
  border-color: var(--text);
}

.teams .field__input:focus-visible {
  border-color: var(--text);
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}

.teams .button {
  min-height: 40px;
}

.teams .button:active:not(:disabled) {
  transform: translateY(1px);
}

@media (max-width: 760px) {
  .member-add {
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }

  .member-add__submit {
    grid-column: 1 / -1;
    width: 100%;
  }
}

@media (max-width: 480px) {
  .create-team {
    width: 100%;
  }

  .create-team__field {
    flex-basis: 100%;
  }

  .create-team .button {
    width: 100%;
  }

  .member-add {
    grid-template-columns: minmax(0, 1fr);
  }

  .delete-repos__row {
    grid-template-columns: minmax(0, 1fr);
  }

  .modal__head {
    padding: var(--space-4);
  }

  .modal__body {
    padding: var(--space-4);
  }

  .modal__actions {
    flex-wrap: wrap;
    padding: var(--space-3) var(--space-4) var(--space-4);
  }

  .modal__actions .button {
    flex: 1 1 auto;
  }

  .team-roster__title {
    font-size: 18px;
  }
}

@media (pointer: coarse) {
  .teams .field__input,
  .teams .button {
    min-height: 44px;
  }
}
</style>
