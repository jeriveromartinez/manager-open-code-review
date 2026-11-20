import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type SettingsField } from '../api'
import SettingsView from './SettingsView.vue'

vi.mock('../api', () => ({ api: { settings: vi.fn(), saveSettings: vi.fn() } }))

const fields: SettingsField[] = [
  { key: 'review_timeout', group: 'review', default: '20m', restart: 'worker', secret: false },
  { key: 'ocr_bin', group: 'worker', default: 'ocr', restart: 'worker', secret: false },
  { key: 'alert_webhook_url', group: 'alerts', default: '', restart: 'monitor', secret: true },
]

beforeEach(() => {
  vi.mocked(api.settings).mockResolvedValue({
    values: { review_timeout: '20m0s', ocr_bin: 'ocr', alert_webhook_url: '' },
    fields,
  })
  vi.mocked(api.saveSettings).mockResolvedValue({
    values: { review_timeout: '30m0s', ocr_bin: 'ocr', alert_webhook_url: '' },
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

  it('shows which process has to restart', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()
    expect(wrapper.text()).toContain('requiere reiniciar worker')
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
