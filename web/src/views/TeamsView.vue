<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, type Team, type TeamMember, type TeamRole, type User } from '../api'
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

const roleLabels: Record<TeamRole, string> = {
  lead: 'Lead',
  qa: 'QA',
  developer: 'Desarrollador',
}

const isAdmin = computed(() => session.me?.is_admin === true)

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

function ensureDraft(teamId: number): void {
  if (!drafts.value[teamId]) {
    drafts.value[teamId] = { userId: null, role: 'qa' }
  }
}

async function loadUsers(): Promise<void> {
  if (!needsUsers.value || usersLoaded.value || usersLoading.value) return
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

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    teams.value = await api.teams()
    teams.value.forEach((team) => ensureDraft(team.id))
  } catch {
    error.value = 'No se pudieron cargar los equipos'
  } finally {
    loading.value = false
  }
  await loadUsers()
}

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
    <div v-else class="team-register">
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
          <p class="team-roster__count">
            {{ team.members.length }} miembro{{ team.members.length === 1 ? '' : 's' }}
          </p>
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

.team-register {
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
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

.team-roster__count {
  color: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 13px;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
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
