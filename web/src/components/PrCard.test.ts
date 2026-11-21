import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'
import PrCard from './PrCard.vue'
import type { PRView } from '../api'

function mountCard(pr: PRView) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/prs/:id', component: { template: '<div />' } },
      { path: '/:pathMatch(.*)*', component: { template: '<div />' } },
    ],
  })
  return mount(PrCard, { props: { pr }, global: { plugins: [router] } })
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
})
