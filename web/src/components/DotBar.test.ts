import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import DotBar from './DotBar.vue'

describe('DotBar', () => {
  it('lights one dot per unit over an unlit track', () => {
    const wrapper = mount(DotBar, { props: { count: 3, tone: 'bad', label: '3 bloqueantes' } })

    const bar = wrapper.get('[role="img"]')
    expect(bar.attributes('aria-label')).toBe('3 bloqueantes')
    expect(bar.classes()).toContain('dot-bar')
    const fill = wrapper.get('.dot-bar__fill')
    expect(fill.classes()).toContain('dot-bar__fill--bad')
    expect(fill.attributes('style')).toContain('width: 21px')
  })

  it('keeps the track when there is nothing to show', () => {
    const wrapper = mount(DotBar, { props: { count: 0, tone: 'on', label: 'ninguno' } })

    expect(wrapper.find('.dot-bar').exists()).toBe(true)
    expect(wrapper.get('.dot-bar__fill').attributes('style')).toContain('width: 0px')
  })

  it('caps the lit dots so a large count cannot overflow the tile', () => {
    const wrapper = mount(DotBar, { props: { count: 500, max: 40, tone: 'on', label: 'muchos' } })

    expect(wrapper.get('.dot-bar__fill').attributes('style')).toContain('width: 280px')
  })
})
