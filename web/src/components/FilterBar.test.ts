import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import FilterBar from './FilterBar.vue'

const chips = [
  { key: 'status:open', label: 'Abiertas' },
  { key: 'team:pagos', label: 'Pagos' },
]

describe('FilterBar', () => {
  it('emits the search text', async () => {
    const wrapper = mount(FilterBar, {
      props: { q: '', shown: 4, total: 12, chips: [] },
    })

    const input = wrapper.get('input[type="search"]')
    expect(input.attributes('placeholder')).toBe('Buscar')
    expect(input.attributes('maxlength')).toBe('120')
    expect(wrapper.find('form').exists()).toBe(false)

    await input.setValue('api')
    await input.trigger('keydown', { key: 'Enter' })

    expect(wrapper.emitted('update:q')).toEqual([['api']])
    expect(wrapper.emitted('clear')).toBeUndefined()
    expect(wrapper.emitted('remove')).toBeUndefined()
  })

  it('renders N DE M with the active style when filters are on', () => {
    const idle = mount(FilterBar, {
      props: { q: '', shown: 4, total: 12, chips: [] },
    })
    expect(idle.get('.filter-bar__count').text()).toBe('4 DE 12')
    expect(idle.get('.filter-bar__count').classes()).not.toContain('filter-bar__count--active')

    const searching = mount(FilterBar, {
      props: { q: 'api', shown: 1, total: 12, chips: [] },
    })
    expect(searching.get('.filter-bar__count').text()).toBe('1 DE 12')
    expect(searching.get('.filter-bar__count').classes()).toContain('filter-bar__count--active')

    const filtered = mount(FilterBar, {
      props: { q: '', shown: 2, total: 12, chips },
    })
    expect(filtered.get('.filter-bar__count').classes()).toContain('filter-bar__count--active')
  })

  it('emits remove for a chip and clear for LIMPIAR', async () => {
    const wrapper = mount(FilterBar, {
      props: { q: 'api', shown: 2, total: 12, chips },
    })

    const remove = wrapper.findAll('.filter-bar__chip-remove')
    expect(remove).toHaveLength(2)
    expect(remove[0].attributes('aria-label')).toBe('Quitar filtro Abiertas')
    expect(remove[0].text()).toContain('×')

    await remove[1].trigger('click')
    expect(wrapper.emitted('remove')).toEqual([['team:pagos']])

    const clear = wrapper.get('.filter-bar__clear')
    expect(clear.text()).toBe('LIMPIAR')
    expect(clear.attributes('disabled')).toBeUndefined()

    await clear.trigger('click')
    expect(wrapper.emitted('clear')).toHaveLength(1)

    const idle = mount(FilterBar, {
      props: { q: '', shown: 12, total: 12, chips: [] },
    })
    expect(idle.get('.filter-bar__clear').attributes('disabled')).toBeDefined()
  })

  it('keeps the bar in normal flow at narrow widths', () => {
    const originalWidth = window.innerWidth
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 480 })

    const wrapper = mount(FilterBar, {
      props: { q: '', shown: 3, total: 8, chips: [] },
      slots: {
        default: '<button type="button" data-desktop-filter>Estado</button>',
        sheet: '<button type="button" data-sheet-trigger>FILTROS · 0</button>',
      },
    })

    const bar = wrapper.get('.filter-bar')
    expect(bar.element).toBe(wrapper.element)
    expect(bar.attributes('style')).toBeUndefined()
    expect(bar.find('[data-desktop-filter]').exists()).toBe(true)
    expect(bar.find('[data-sheet-trigger]').exists()).toBe(true)
    expect(bar.element.contains(bar.get('[data-desktop-filter]').element)).toBe(true)
    expect(bar.element.contains(bar.get('[data-sheet-trigger]').element)).toBe(true)

    Object.defineProperty(window, 'innerWidth', { configurable: true, value: originalWidth })
  })
})
