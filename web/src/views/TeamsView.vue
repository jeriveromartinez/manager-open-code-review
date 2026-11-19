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
        <input
          v-model="newTeamName"
          class="field__input"
          type="text"
          placeholder="Nombre del equipo"
          aria-label="Nombre del equipo"
          data-create-team-name
        />
        <button class="button button--primary" type="button" data-create-team @click="createTeam">
          Crear equipo
        </button>
      </form>
    </header>

    <p v-if="formError" class="form-error">{{ formError }}</p>

    <p v-if="error" class="empty-state">{{ error }}</p>
    <p v-else-if="!loading && teams.length === 0" class="empty-state">No hay equipos todavía</p>
    <div v-else class="team-grid">
      <article
        v-for="team in teams"
        :key="team.id"
        class="team-card"
        :data-team-id="team.id"
        :data-team-name="team.name"
      >
        <header class="team-card__head">
          <h2 class="team-card__title">{{ team.name }}</h2>
          <span class="team-card__count">
            {{ team.members.length }} miembro{{ team.members.length === 1 ? '' : 's' }}
          </span>
        </header>

        <ul v-if="team.members.length > 0" class="member-list">
          <li v-for="member in team.members" :key="member.user_id" class="member-row">
            <span class="member-row__login">{{ member.login }}</span>

            <select
              v-if="canManage(team)"
              class="field__input member-row__role"
              :value="member.role"
              :aria-label="`Rol de ${member.login}`"
              data-change-role
              @change="changeRole(team, member, $event)"
            >
              <option value="lead">Lead</option>
              <option value="qa">QA</option>
              <option value="developer">Desarrollador</option>
            </select>
            <span v-else class="member-row__role">{{ roleLabels[member.role] }}</span>

            <button
              v-if="canManage(team)"
              class="button button--ghost member-row__remove"
              type="button"
              :aria-label="`Quitar a ${member.login}`"
              data-remove-member
              @click="removeMember(team, member)"
            >
              Quitar
            </button>
          </li>
        </ul>
        <p v-else class="member-list__empty">Este equipo no tiene miembros todavía</p>

        <div v-if="canManage(team)" class="team-card__add" data-add-member>
          <p v-if="usersError" class="form-error" data-users-error>{{ usersError }}</p>
          <select
            v-model.number="drafts[team.id].userId"
            class="field__input"
            aria-label="Selecciona un usuario para añadir"
            data-add-member-user
          >
            <option :value="null">Selecciona un usuario</option>
            <option v-for="user in users" :key="user.id" :value="user.id">{{ user.login }}</option>
          </select>
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
          <button class="button button--primary" type="button" data-add-member-submit @click="addMember(team)">
            Añadir miembro
          </button>
        </div>
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
  gap: var(--space-2);
  flex-wrap: wrap;
}

.team-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: var(--space-3);
}

.team-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
}

.team-card__head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--space-3);
}

.team-card__title {
  font-size: 18px;
  letter-spacing: -0.01em;
}

.team-card__count {
  color: var(--text-faint);
  font-size: 13px;
}

.member-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.member-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding-bottom: var(--space-2);
  border-bottom: 1px dashed var(--border);
}

.member-row__login {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-weight: 600;
}

.member-row__role {
  min-width: 110px;
  padding: 5px 8px;
  font-size: 13px;
}

.member-row__remove {
  padding: 5px 9px;
  font-size: 12.5px;
}

.member-list__empty {
  color: var(--text-faint);
  font-size: 13px;
}

.team-card__add {
  display: flex;
  gap: var(--space-2);
  flex-wrap: wrap;
  padding-top: var(--space-3);
  border-top: 1px solid var(--border);
}

.team-card__add .field__input {
  padding: 5px 8px;
  font-size: 13px;
}

.team-card__add [data-users-error] {
  flex-basis: 100%;
}
</style>
