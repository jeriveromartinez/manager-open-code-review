import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import FilterDropdown from './FilterDropdown.vue'

const options = [
  { value: 'open', label: 'Abiertas', count: 4 },
  { value: 'closed', label: 'Cerradas', count: 2 },
]

describe('FilterDropdown', () => {
  it('opens with Enter and selects with the keyboard', async () => {
    const wrapper = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: [] },
    })
    const pill = wrapper.get('button')
    expect(pill.attributes('aria-expanded')).toBe('false')

    await pill.trigger('keydown', { key: 'Enter' })
    expect(pill.attributes('aria-expanded')).toBe('true')
    const listbox = wrapper.get('[role="listbox"]')
    expect(listbox.attributes('aria-label')).toBe('Estado')

    await listbox.trigger('keydown', { key: 'ArrowDown' })
    await listbox.trigger('keydown', { key: 'Enter' })

    expect(wrapper.emitted('update:modelValue')?.[0]).toEqual([['closed']])
    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)
    expect(pill.attributes('aria-expanded')).toBe('false')
    wrapper.unmount()
  })

  it('does not change the selection when Escape closes', async () => {
    const wrapper = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: ['open'] },
    })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    expect(wrapper.find('[role="listbox"]').exists()).toBe(true)

    await wrapper.get('[role="listbox"]').trigger('keydown', { key: 'Escape' })

    expect(wrapper.find('[role="listbox"]').exists()).toBe(false)
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows a check and the count for each option', async () => {
    const wrapper = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: ['open'] },
    })

    await wrapper.get('button').trigger('keydown', { key: 'Enter' })
    const rendered = wrapper.findAll('[role="option"]')

    expect(rendered).toHaveLength(2)
    expect(rendered[0].attributes('aria-selected')).toBe('true')
    expect(rendered[0].text()).toContain('✓')
    expect(rendered[0].get('.filter-dropdown__count').text()).toBe('4')
    expect(rendered[1].attributes('aria-selected')).toBe('false')
    expect(rendered[1].text()).not.toContain('✓')
    expect(rendered[1].get('.filter-dropdown__count').text()).toBe('2')
    wrapper.unmount()
  })

  it('single mode replaces the selection; multiple mode toggles', async () => {
    const single = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: ['open'] },
    })

    await single.get('button').trigger('keydown', { key: 'Enter' })
    await single.findAll('[role="option"]')[1].trigger('click')

    expect(single.emitted('update:modelValue')?.[0]).toEqual([['closed']])
    expect(single.find('[role="listbox"]').exists()).toBe(false)
    single.unmount()

    const multiple = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: ['open'], multiple: true },
    })

    await multiple.get('button').trigger('keydown', { key: 'Enter' })
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
    multiple.unmount()
  })

  it('closes on focus loss and outside click without emitting', async () => {
    const wrapper = mount(FilterDropdown, {
      attachTo: document.body,
      props: { label: 'Estado', options, modelValue: [] },
    })

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
    outside.remove()
    wrapper.unmount()
  })
})
