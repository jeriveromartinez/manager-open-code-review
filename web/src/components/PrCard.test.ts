import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import PrCard from './PrCard.vue'
import type { PRView } from '../api'

const now = new Date('2026-11-18T12:00:00Z')
const fourHours = 4 * 3600

function mountCard(pr: PRView, slaSeconds = fourHours) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/prs/:id', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } },
    ],
  })
  return mount(PrCard, { props: { pr, slaSeconds, now }, global: { plugins: [router] } })
}

const base: PRView = {
  id: 1,
  repo: 'backend-api',
  number: 142,
  title: 'Añade endpoint de pedidos',
  author_login: 'octocat',
  html_url: 'https://github.com/acme/backend-api/pull/142',
  qa_status: 'ready_for_human',
  group: 'ready',
  review_state: '',
  review_reason: '',
  validations: { passed: 9, total: 9 },
  open_findings: { blocker: 0, major: 0, minor: 2, nit: 0 },
  status_since: '2026-11-18T10:50:00Z',
  updated_at: '2026-11-18T10:00:00Z',
}

describe('PrCard', () => {
  it('enlaza el título con el detalle interno del PR y mantiene el enlace externo', () => {
    const wrapper = mountCard(base)

    expect(wrapper.get('a.pr-card__link').attributes('href')).toBe('/prs/1')
    expect(wrapper.get('a.pr-card__github').attributes('href')).toBe(base.html_url)
    expect(wrapper.text()).toContain('backend-api #142')
    expect(wrapper.text()).toContain('Listo para ti')
    expect(wrapper.text()).toContain('9/9 validaciones correctas')
    expect(wrapper.get('[data-cta]').text()).toBe('Validar →')
    expect(wrapper.get('[data-cta]').attributes('aria-label')).toBe('Validar backend-api #142')
  })

  it('avisa cuando un PR pendiente no tiene revisión programada', () => {
    const wrapper = mountCard({
      ...base,
      qa_status: 'pending',
      group: 'in_review',
      review_state: 'blocked',
      review_reason: 'Repositorio sin equipo asignado',
    })

    expect(wrapper.text()).toContain('Sin revisión programada')
    expect(wrapper.text()).toContain('Repositorio sin equipo asignado')
    expect(wrapper.text()).not.toContain('Análisis en curso')
  })

  it('oculta la línea de validaciones cuando total es 0', () => {
    const wrapper = mountCard({ ...base, validations: { passed: 0, total: 0 } })

    expect(wrapper.text()).not.toContain('validaciones correctas')
  })

  it('cuenta la espera de QA y marca lo que se sale del plazo', () => {
    const onTime = mountCard(base)
    expect(onTime.get('[data-wait]').text()).toBe('espera validación 1h 10m')

    const late = mountCard({ ...base, status_since: '2026-11-18T06:00:00Z' })
    expect(late.get('[data-wait]').text()).toBe('espera validación 6h 00m · fuera de plazo')
    expect(late.get('[data-wait]').classes()).toContain('pr-card__late')

    const noSla = mountCard({ ...base, status_since: '2026-11-18T06:00:00Z' }, 0)
    expect(noSla.get('[data-wait]').text()).not.toContain('fuera de plazo')
  })

  it('muestra los hallazgos abiertos y resalta los bloqueantes', () => {
    const wrapper = mountCard({
      ...base,
      qa_status: 'changes_requested',
      group: 'blocked',
      open_findings: { blocker: 1, major: 2, minor: 0, nit: 0 },
    })

    expect(wrapper.get('[data-open-blockers]').text()).toBe('1')
    expect(wrapper.get('[data-open-blockers]').classes()).toContain('pr-card__count--bad')
    expect(wrapper.get('[data-cta]').text()).toBe('Ver defectos →')
    expect(wrapper.get('[data-wait]').text()).toContain('en este estado')
  })
})
