import { createRouter, createWebHistory } from 'vue-router'
import { api, setUnauthorizedHandler } from './api'
import AgentsView from './views/AgentsView.vue'
import DashboardView from './views/DashboardView.vue'
import LeadView from './views/LeadView.vue'
import LoginView from './views/LoginView.vue'
import PrDetailView from './views/PrDetailView.vue'
import ReposView from './views/ReposView.vue'
import RunsView from './views/RunsView.vue'
import SettingsView from './views/SettingsView.vue'
import SetupView from './views/SetupView.vue'
import TeamsView from './views/TeamsView.vue'
import { useSessionStore } from './stores/session'

let configured: boolean | null = null

/** Estado de configuración de la instancia, cacheado entre navegaciones. */
export async function isConfigured(force = false): Promise<boolean> {
  if (configured === null || force) {
    try {
      const status = await api.setupStatus()
      configured = status.configured
    } catch {
      // Si el backend no responde, no encerramos al usuario en /setup.
      configured = true
    }
  }
  return configured
}

export function setConfigured(value: boolean): void {
  configured = value
}

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardView, meta: { flushContainer: true } },
    { path: '/runs', name: 'runs', component: RunsView, meta: { flushContainer: true } },
    { path: '/lead', name: 'lead', component: LeadView, meta: { flushContainer: true } },
    { path: '/repos', name: 'repos', component: ReposView, meta: { flushContainer: true } },
    { path: '/teams', name: 'teams', component: TeamsView },
    { path: '/agents', name: 'agents', component: AgentsView },
    { path: '/settings', name: 'settings', component: SettingsView, meta: { admin: true } },
    { path: '/prs/:id', name: 'pr-detail', component: PrDetailView, meta: { flushContainer: true } },
    { path: '/login', name: 'login', component: LoginView, meta: { layout: 'bare' } },
    { path: '/setup', name: 'setup', component: SetupView, meta: { layout: 'bare' } },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

router.beforeEach(async (to) => {
  const ready = await isConfigured()

  if (!ready && to.name !== 'setup') {
    return { name: 'setup' }
  }
  if (ready && to.name === 'setup') {
    return { name: 'dashboard' }
  }
  // El menú ya oculta Ajustes a quien no es admin, pero un no-admin que teclee
  // la ruta no debe llegar a un formulario que el backend va a rechazar.
  if (to.meta.admin) {
    const session = useSessionStore()
    const me = await session.load()
    if (!me?.is_admin) return { name: 'dashboard' }
  }
  return true
})

setUnauthorizedHandler(() => {
  const current = router.currentRoute.value
  if (current.name === 'login') {
    return
  }
  void router.push({ name: 'login', query: { redirect: current.fullPath } })
})

export default router
