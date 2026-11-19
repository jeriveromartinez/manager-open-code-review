import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import PrCard from './PrCard.vue'
import type { PRView } from '../api'

const base: PRView = {
  id: 1,
  repo: 'backend-api',
  number: 142,
  title: 'Añade endpoint de pedidos',
  author_login: 'octocat',
  html_url: 'https://github.com/acme/backend-api/pull/142',
  qa_status: 'ready_for_human',
  group: 'ready',
  validations: { passed: 9, total: 9 },
  updated_at: '2026-11-18T10:00:00Z',
}

describe('PrCard', () => {
  it('muestra repo #número, etiqueta de estado y validaciones', () => {
    const wrapper = mount(PrCard, { props: { pr: base } })

    expect(wrapper.text()).toContain('backend-api #142')
    expect(wrapper.text()).toContain('Listo para ti')
    expect(wrapper.text()).toContain('9/9 validaciones correctas')
    expect(wrapper.find('a.pr-card__link').attributes('href')).toBe(base.html_url)
  })

  it('oculta la línea de validaciones cuando total es 0', () => {
    const wrapper = mount(PrCard, {
      props: { pr: { ...base, validations: { passed: 0, total: 0 } } },
    })

    expect(wrapper.text()).not.toContain('validaciones correctas')
  })
})
