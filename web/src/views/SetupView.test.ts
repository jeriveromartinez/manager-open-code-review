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
  api: { setupManifest: vi.fn(), setupStatus: vi.fn() },
}))

beforeEach(() => {
  vi.mocked(api.setupStatus).mockReset()
  vi.mocked(api.setupStatus).mockResolvedValue({
    configured: false,
    install_url: '',
    token_required: false,
  })
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
  it('asks only for the App name', async () => {
    const wrapper = mount(SetupView)
    await flushPromises()

    const inputs = wrapper.findAll('input')
    expect(inputs).toHaveLength(1)
    expect(inputs[0].attributes('name')).toBe('name')
    expect(inputs[0].attributes('type')).toBe('text')
    expect(inputs[0].attributes('autocomplete')).toBe('off')
    expect(inputs[0].attributes('required')).toBeDefined()
  })

  it('keeps asking only for the name when the status call fails', async () => {
    vi.mocked(api.setupStatus).mockRejectedValue(new Error('red'))
    const wrapper = mount(SetupView)
    await flushPromises()

    expect(wrapper.findAll('input')).toHaveLength(1)
  })

  it('asks for the setup token when the server requires it', async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      configured: false,
      install_url: '',
      token_required: true,
    })
    const wrapper = mount(SetupView)
    await flushPromises()

    const inputs = wrapper.findAll('input')
    const token = wrapper.find('input[name="setup_token"]')
    expect(inputs).toHaveLength(2)
    expect(token.exists()).toBe(true)
    expect(wrapper.text()).toContain('QA_SETUP_TOKEN')

    await wrapper.find('input[name="name"]').setValue('Acme QA')
    await token.setValue('s3cret')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(api.setupManifest).toHaveBeenCalledWith({ name: 'Acme QA', setup_token: 's3cret' })
  })

  it('omits the setup token when the field is left empty', async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      configured: false,
      install_url: '',
      token_required: true,
    })
    const wrapper = mount(SetupView)
    await flushPromises()

    await wrapper.find('input[name="name"]').setValue('Acme QA')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(api.setupManifest).toHaveBeenCalledWith({ name: 'Acme QA' })
  })

  it('submits the manifest with the trimmed App name', async () => {
    const wrapper = mount(SetupView)
    await flushPromises()

    await wrapper.find('input[name="name"]').setValue('  Acme QA  ')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(api.setupManifest).toHaveBeenCalledWith({ name: 'Acme QA' })
  })

  it('shows the setup token field when the server rejects the submit with 403', async () => {
    vi.mocked(api.setupManifest).mockRejectedValueOnce(new ApiError(403, 'setup token required'))
    const wrapper = mount(SetupView)
    await flushPromises()

    expect(wrapper.find('input[name="setup_token"]').exists()).toBe(false)

    await wrapper.find('input[name="name"]').setValue('Acme QA')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const token = wrapper.find('input[name="setup_token"]')
    expect(token.exists()).toBe(true)
    expect(wrapper.text()).toContain('requiere el token de setup')

    await token.setValue('s3cret')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(api.setupManifest).toHaveBeenLastCalledWith({ name: 'Acme QA', setup_token: 's3cret' })
  })

  it('explains that the instance is already configured', async () => {
    vi.mocked(api.setupManifest).mockRejectedValue(new ApiError(409, 'already configured'))
    const wrapper = mount(SetupView)
    await flushPromises()

    await wrapper.find('input[name="name"]').setValue('Acme QA')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('La instancia ya está configurada.')
  })
})
