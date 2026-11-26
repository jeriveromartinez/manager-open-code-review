import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { describe, expect, it } from 'vitest'
import FilterSheet from './FilterSheet.vue'

const slot = `
  <button type="button" data-test="first">Estado</button>
  <button type="button" data-test="second">Equipo</button>
`

function getDialog(): HTMLElement | null {
  return document.body.querySelector('[role="dialog"]')
}

function press(target: EventTarget, key: string, shiftKey = false): void {
  target.dispatchEvent(
    new KeyboardEvent('keydown', { key, shiftKey, bubbles: true, cancelable: true }),
  )
}

describe('FilterSheet', () => {
  it('traps focus and restores it on close', async () => {
    const trigger = document.createElement('button')
    trigger.textContent = 'FILTROS · 2'
    document.body.appendChild(trigger)
    trigger.focus()
    expect(document.activeElement).toBe(trigger)

    const wrapper = mount(FilterSheet, {
      attachTo: document.body,
      props: { open: false, activeCount: 2, shown: 5 },
      slots: { default: slot },
    })

    await wrapper.setProps({ open: true })
    await nextTick()

    const dialog = getDialog()
    expect(dialog).not.toBeNull()
    expect(dialog?.getAttribute('aria-modal')).toBe('true')
    const labelledBy = dialog?.getAttribute('aria-labelledby')
    expect(labelledBy).toBeTruthy()
    expect(document.getElementById(labelledBy as string)?.textContent).toBe('FILTROS')

    const shell = document.body.querySelector('.filter-sheet')
    expect(shell?.parentElement).toBe(document.body)
    expect(document.body.contains(dialog)).toBe(true)

    const first = document.body.querySelector('[data-test="first"]') as HTMLElement
    const apply = document.body.querySelector('.filter-sheet__apply') as HTMLElement

    expect(document.activeElement).toBe(first)

    press(apply, 'Tab')
    expect(document.activeElement).toBe(first)

    press(first, 'Tab', true)
    expect(document.activeElement).toBe(apply)

    await wrapper.setProps({ open: false })
    await nextTick()

    expect(getDialog()).toBeNull()
    expect(document.activeElement).toBe(trigger)

    wrapper.unmount()
    trigger.remove()
  })

  it('closes with Escape and with the backdrop', async () => {
    const wrapper = mount(FilterSheet, {
      attachTo: document.body,
      props: { open: true, activeCount: 1, shown: 3 },
      slots: { default: slot },
    })
    await nextTick()

    const dialog = getDialog() as HTMLElement
    press(dialog, 'Escape')
    expect(wrapper.emitted('close')).toHaveLength(1)

    const backdrop = document.body.querySelector('.filter-sheet__backdrop') as HTMLElement
    backdrop.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    expect(wrapper.emitted('close')).toHaveLength(2)

    dialog.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    expect(wrapper.emitted('close')).toHaveLength(2)

    await wrapper.setProps({ open: false })
    wrapper.unmount()
  })

  it('does not close on Escape when an inner control already handled it', async () => {
    const wrapper = mount(FilterSheet, {
      attachTo: document.body,
      props: { open: true, activeCount: 1, shown: 3 },
      slots: {
        default: '<button type="button" data-test="inner" @keydown.esc.prevent>Estado</button>',
      },
    })
    await nextTick()

    const inner = document.body.querySelector('[data-test="inner"]') as HTMLElement
    press(inner, 'Escape')

    expect(wrapper.emitted('close')).toBeUndefined()

    await wrapper.setProps({ open: false })
    wrapper.unmount()
  })

  it('shows LIMPIAR / VER n PRs and emits clear', async () => {
    const wrapper = mount(FilterSheet, {
      attachTo: document.body,
      props: { open: true, activeCount: 2, shown: 7 },
      slots: { default: slot },
    })
    await nextTick()

    const clear = document.body.querySelector('.filter-sheet__clear') as HTMLButtonElement
    const apply = document.body.querySelector('.filter-sheet__apply') as HTMLButtonElement

    expect(clear.textContent?.trim()).toBe('LIMPIAR')
    expect(apply.textContent?.trim()).toBe('VER 7 PRs')

    clear.click()
    expect(wrapper.emitted('clear')).toHaveLength(1)
    expect(wrapper.emitted('close')).toBeUndefined()

    apply.click()
    expect(wrapper.emitted('close')).toHaveLength(1)

    await wrapper.setProps({ activeCount: 0 })
    expect(clear.disabled).toBe(true)

    await wrapper.setProps({ open: false })
    wrapper.unmount()
  })

  it('locks body scroll while open and restores it on close', async () => {
    document.body.style.overflow = 'auto'

    const wrapper = mount(FilterSheet, {
      attachTo: document.body,
      props: { open: false, activeCount: 0, shown: 4 },
    })

    await wrapper.setProps({ open: true })
    expect(document.body.style.overflow).toBe('hidden')

    await wrapper.setProps({ open: false })
    expect(document.body.style.overflow).toBe('auto')

    document.body.style.overflow = ''
    wrapper.unmount()
  })
})
