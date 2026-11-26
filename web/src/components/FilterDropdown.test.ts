import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { afterEach, describe, expect, it } from 'vitest'
import FilterDropdown from './FilterDropdown.vue'

const options = [
  { value: 'open', label: 'Abiertas', count: 4 },
  { value: 'closed', label: 'Cerradas', count: 2 },
]

const mounted: { unmount: () => void }[] = []

afterEach(() => {
  mounted.forEach((wrapper) => wrapper.unmount())
  mounted.length = 0
  document.body.innerHTML = ''
})

function mountDropdown(props: {
  label: string
  options: (typeof options)[number][]
  modelValue: string[]
  multiple?: boolean
  clearLabel?: string
  active?: boolean
}) {
  const wrapper = mount(FilterDropdown, { attachTo: document.body, props })
  mounted.push(wrapper)
  return wrapper
}

describe('FilterDropdown', () => {
  it('opens with Enter and selects with the keyboard', async () => {
    const wrapper = mountDropdown({ label: 'Estado', options, modelValue: [] })
    const pill = wrapper.get('button')
    expect(pill.attributes('aria-expanded')).toBe('false')

    await pill.trigger('keydown', { key: 'Enter' })
    expect(pill.attributes('aria-expanded')).toBe('true')
    const listbox = wrapper.get('[role="listbox"]')
    expect(listbox.attributes('aria-label')).toBe('Estado')
    expect(listbox.attributes('aria-multiselectable')).toBeUndefined()

    await listbox.trigger('keydown', { key: 'ArrowDown' })
    await listbox.trigger('keydown', { key: 'Enter' })

    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([['closed']])
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)
    expect(pill.attributes('aria-expanded')).toBe('false')
  })

  it('does not change the selection when Escape closes', async () => {
    const wrapper = mountDropdown({ label: 'Estado', options, modelValue: ['open'] })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    expect(wrapper.find('[role="listbox"]').exists()).toBe(true)

    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'Escape' })
    await nextTick()

    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(document.activeElement).toBe(wrapper.get('button').element)
  })

  it('shows a check and the count for each option', async () => {
    const wrapper = mountDropdown({ label: 'Estado', options, modelValue: ['open'] })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    const rendered = wrapper.findAll('[role="option"]')

    expect(rendered).toHaveLength(2)
    expect(rendered[0].attributes('aria-selected')).toBe('true')
    expect(rendered[0].text()).toContain('✓')
    expect(rendered[0].get('.filter-dropdown__count').text()).toBe('4')
    expect(rendered[1].attributes('aria-selected')).toBe('false')
    expect(rendered[1].text()).not.toContain('✓')
    expect(rendered[1].get('.filter-dropdown__count').text()).toBe('2')
  })

  it('lets the parent keep the pill quiet for a default value', async () => {
    const wrapper = mountDropdown({ label: 'Orden', options, modelValue: ['open'], active: false })
    const pill = wrapper.get('button')

    expect(pill.classes()).not.toContain('filter-dropdown__pill--active')
    expect(wrapper.get('.filter-dropdown__value').text()).toBe('Abiertas')

    await wrapper.setProps({ active: true })
    expect(pill.classes()).toContain('filter-dropdown__pill--active')

    await wrapper.setProps({ active: undefined })
    expect(pill.classes()).toContain('filter-dropdown__pill--active')
  })

  it('single mode replaces the selection; multiple mode toggles', async () => {
    const single = mountDropdown({ label: 'Estado', options, modelValue: ['open'] })

    await single.get('button').trigger('keydown', { key: 'Enter' })
    await single.findAll('[role="option"]')[1].trigger('click')

    expect(single.emitted('update:modelValue')?.[0]).toEqual([['closed']])
    expect(single.find('[role="listbox"]').exists()).toBe(false)

    const multiple = mountDropdown({ label: 'Estado', options, modelValue: ['open'], multiple: true })

    await multiple.get('button').trigger('keydown', { key: 'Enter' })
    expect(multiple.get('[role="listbox"]').attributes('aria-multiselectable')).toBe('true')
    expect(multiple.findAll('[role="option"]')).toHaveLength(3)
    expect(multiple.findAll('[role="option"]')[0].text()).toContain('Todas')

    await multiple.findAll('[role="option"]')[2].trigger('click')
    expect(multiple.emitted('update:modelValue')?.[0]).toEqual([['open', 'closed']])

    await multiple.setProps({ modelValue: ['open', 'closed'] })
    await multiple.findAll('[role="option"]')[1].trigger('click')
    expect(multiple.emitted('update:modelValue')?.[1]).toEqual([['closed']])

    await multiple.setProps({ modelValue: ['closed'] })
    await multiple.findAll('[role="option"]')[0].trigger('click')
    expect(multiple.emitted('update:modelValue')?.[2]).toEqual([[]])
  })

  it('uses a custom clear label in multiple mode', async () => {
    const wrapper = mountDropdown({
      label: 'Repo',
      options,
      modelValue: [],
      multiple: true,
      clearLabel: 'Todos',
    })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })

    expect(wrapper.findAll('[role="option"]')[0].text()).toContain('Todos')
  })

  it('wraps keyboard navigation in both directions', async () => {
    const wrapper = mountDropdown({ label: 'Estado', options, modelValue: [] })
    const pill = wrapper.get('button')

    await pill.trigger('keydown', { key: 'Enter' })
    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'ArrowUp' })
    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([['closed']])

    await wrapper.setProps({ modelValue: ['closed'] })
    await pill.trigger('keydown', { key: 'Enter' })
    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'ArrowDown' })
    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('update:modelValue')?.[1]).toEqual([['open']])
  })

  it('closes on focus loss and outside click without emitting', async () => {
    const wrapper = mountDropdown({ label: 'Estado', options, modelValue: [] })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    const outside = document.createElement('button')
    document.body.appendChild(outside)

    await wrapper.get('[role="listbox"]').trigger('focusout', { relatedTarget: outside })
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    expect(wrapper.find('[role="listbox"]').exists()).toBe(true)
    document.body.dispatchEvent(new Event('pointerdown', { bubbles: true }))
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)

    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })
})
