import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import EmptyState from './EmptyState.vue'

describe('EmptyState', () => {
  it('renders the title and emits action', async () => {
    const wrapper = mount(EmptyState, {
      props: { title: 'NADA COINCIDE CON ESTOS FILTROS', actionLabel: 'LIMPIAR FILTROS' },
    })

    expect(wrapper.get('.empty__title').text()).toBe('NADA COINCIDE CON ESTOS FILTROS')
    const action = wrapper.get('button')
    expect(action.text()).toBe('LIMPIAR FILTROS')
    await action.trigger('click')
    expect(wrapper.emitted('action')).toEqual([[]])
  })

  it('hides the action when no label is given', () => {
    const wrapper = mount(EmptyState, { props: { title: 'SIN RESULTADOS' } })

    expect(wrapper.text()).toContain('SIN RESULTADOS')
    expect(wrapper.find('button').exists()).toBe(false)
  })
})
