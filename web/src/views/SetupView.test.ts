import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '../api'
import SetupView from './SetupView.vue'

vi.mock('../api', () => ({
  ApiError: class ApiError extends Error {
    status: number

    constructor(status: number, message: string) {
      super(message)
      this.status = status
    }
  },
  api: { setupManifest: vi.fn() },
}))

beforeEach(() => {
  vi.mocked(api.setupManifest).mockReset()
  vi.mocked(api.setupManifest).mockResolvedValue({
    action_url: 'https://github.com/settings/apps/new',
    manifest: { name: 'Acme QA' },
  })
  // El flujo real envía un formulario a GitHub; en jsdom basta con no navegar.
  vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {})
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('SetupView', () => {
  it('asks only for the App name', () => {
    const wrapper = mount(SetupView)

    const inputs = wrapper.findAll('input')
    expect(inputs).toHaveLength(1)
    expect(inputs[0].attributes('name')).toBe('name')
    expect(inputs[0].attributes('type')).toBe('text')
    expect(inputs[0].attributes('autocomplete')).toBe('off')
    expect(inputs[0].attributes('required')).toBeDefined()
  })

  it('submits the manifest with the trimmed App name', async () => {
    const wrapper = mount(SetupView)

    await wrapper.find('input[name="name"]').setValue('  Acme QA  ')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(api.setupManifest).toHaveBeenCalledWith({ name: 'Acme QA' })
  })

  it('explains that the instance is already configured', async () => {
    vi.mocked(api.setupManifest).mockRejectedValue(new ApiError(409, 'already configured'))
    const wrapper = mount(SetupView)

    await wrapper.find('input[name="name"]').setValue('Acme QA')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('La instancia ya está configurada.')
  })
})
