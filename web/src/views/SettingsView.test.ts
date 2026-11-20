import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type SettingsField } from '../api'
import SettingsView from './SettingsView.vue'

vi.mock('../api', () => ({ api: { settings: vi.fn(), saveSettings: vi.fn() } }))

const fields: SettingsField[] = [
  { key: 'public_url', group: 'general', default: '', restart: '', secret: false },
  { key: 'review_timeout', group: 'review', default: '20m', restart: 'worker', secret: false },
  { key: 'ocr_bin', group: 'worker', default: 'ocr', restart: 'worker', secret: false },
  { key: 'alert_webhook_url', group: 'alerts', default: '', restart: 'monitor', secret: true },
]

const stored = { public_url: '', review_timeout: '20m0s', ocr_bin: 'ocr', alert_webhook_url: '' }

beforeEach(() => {
  vi.mocked(api.settings).mockReset()
  vi.mocked(api.settings).mockResolvedValue({ values: { ...stored }, fields })
  vi.mocked(api.saveSettings).mockReset()
  vi.mocked(api.saveSettings).mockResolvedValue({
    values: { ...stored, review_timeout: '30m0s' },
    fields,
  })
})

describe('SettingsView', () => {
  it('renders a section per group with the stored value', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.text()).toContain('Revisión')
    expect((wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value).toBe(
      '20m0s',
    )
  })

  it('saves only the edited section', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
  })

  it('keeps unsaved drafts in other sections when one section is saved', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="public_url"]').setValue('https://qa.example.com')
    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
    expect((wrapper.find('input[name="public_url"]').element as HTMLInputElement).value).toBe(
      'https://qa.example.com',
    )
  })

  it('shows which process has to restart', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()
    expect(wrapper.text()).toContain('requiere reiniciar worker')
  })

  it('names the monitor when a section needs it restarted', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()
    expect(wrapper.find('form[data-group="alerts"]').text()).toContain(
      'requiere reiniciar monitor',
    )
  })

  it('lists every restart target of a section instead of the first one', async () => {
    vi.mocked(api.settings).mockResolvedValue({
      values: {},
      fields: [
        { key: 'app_value', group: 'worker', default: '', restart: 'app', secret: false },
        { key: 'worker_value', group: 'worker', default: '', restart: 'worker', secret: false },
        { key: 'monitor_value', group: 'worker', default: '', restart: 'monitor', secret: false },
      ],
    })
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.find('form[data-group="worker"]').text()).toContain(
      'requiere reiniciar app y worker y monitor',
    )
  })

  it('shows no restart label when no field of the section needs a restart', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const general = wrapper.find('form[data-group="general"]')
    expect(general.text()).not.toContain('requiere reiniciar')
    expect(general.findAll('.badge')).toHaveLength(0)
  })

  it('masks secret fields and asks for a new password', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const secret = wrapper.find('input[name="alert_webhook_url"]')
    const plain = wrapper.find('input[name="review_timeout"]')
    expect(secret.attributes('type')).toBe('password')
    expect(secret.attributes('autocomplete')).toBe('new-password')
    expect(plain.attributes('type')).toBe('text')
    expect(plain.attributes('autocomplete')).toBe('off')
  })

  it('offers a retry when the settings fail to load', async () => {
    vi.mocked(api.settings).mockRejectedValueOnce(new Error('boom'))
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudieron cargar los ajustes.')
    expect(wrapper.find('input[name="review_timeout"]').exists()).toBe(false)

    await wrapper.get('[data-retry]').trigger('click')
    await flushPromises()

    expect(api.settings).toHaveBeenCalledTimes(2)
    expect((wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value).toBe(
      '20m0s',
    )
  })

  it('clears the success notice as soon as any field changes', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()
    expect(wrapper.text()).toContain('Cambios guardados.')

    await wrapper.find('input[name="ocr_bin"]').setValue('/usr/local/bin/ocr')
    expect(wrapper.text()).not.toContain('Cambios guardados.')
  })

  it('keeps the typed value and shows the server error when a save is rejected', async () => {
    vi.mocked(api.saveSettings).mockRejectedValue(
      new Error('review_stale_after: must exceed review_timeout (30m0s)'),
    )
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('review_stale_after')
    expect((wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value).toBe(
      '30m',
    )
  })
})
