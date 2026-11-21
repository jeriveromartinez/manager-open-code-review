import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AgentsView from './AgentsView.vue'
import type { LLMProfile, Me, QAPolicy, Team } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    llmProfiles: vi.fn(),
    createLLMProfile: vi.fn(),
    updateLLMProfile: vi.fn(),
    checkLLMHealth: vi.fn(),
    teams: vi.fn(),
    getPolicy: vi.fn(),
    savePolicy: vi.fn(),
  },
  session: {
    me: null as Me | null,
    load: vi.fn(),
  },
}))

vi.mock('../api', () => ({ api: mocks.api }))
vi.mock('../stores/session', () => ({
  useSessionStore: () => mocks.session,
}))

const admin: Me = {
  id: 1,
  login: 'admin',
  avatar_url: '',
  is_admin: true,
  teams: [],
}

const profiles: LLMProfile[] = [
  {
    id: 7,
    name: 'GPT principal',
    provider: 'openai',
    model: 'gpt-5',
    variant: null,
    mode: 'api',
    base_url: null,
    concurrency: 2,
    enabled: true,
    api_key_set: true,
    health_status: 'ok',
    health_checked_at: '2026-11-19T10:00:00Z',
    health_error: '',
    health_age_seconds: 300,
    health_stale: false,
    created_at: '2026-11-01T10:00:00Z',
    updated_at: '2026-11-19T10:00:00Z',
  },
  {
    id: 11,
    name: 'Local llama',
    provider: 'ollama',
    model: 'llama3',
    variant: '70b',
    mode: 'local',
    base_url: 'http://localhost:11434',
    concurrency: 1,
    enabled: false,
    api_key_set: false,
    health_status: 'error',
    health_checked_at: '2026-11-18T10:00:00Z',
    health_error: 'connection refused',
    health_age_seconds: 90000,
    health_stale: true,
    created_at: '2026-11-02T10:00:00Z',
    updated_at: '2026-11-18T10:00:00Z',
  },
  {
    id: 12,
    name: 'Sin estrenar',
    provider: 'openai',
    model: 'gpt-5',
    variant: null,
    mode: 'api',
    base_url: null,
    concurrency: 1,
    enabled: true,
    api_key_set: true,
    health_status: 'unknown',
    health_checked_at: null,
    health_error: '',
    health_age_seconds: 0,
    health_stale: true,
    created_at: '2026-11-03T10:00:00Z',
    updated_at: '2026-11-03T10:00:00Z',
  },
  {
    id: 13,
    name: 'Claude QA',
    provider: 'anthropic',
    model: 'claude-sonnet-5-5',
    variant: null,
    mode: 'api',
    base_url: null,
    concurrency: 1,
    enabled: true,
    api_key_set: true,
    health_status: 'unknown',
    health_checked_at: null,
    health_error: '',
    health_age_seconds: 0,
    health_stale: true,
    created_at: '2026-11-04T10:00:00Z',
    updated_at: '2026-11-04T10:00:00Z',
  },
  {
    id: 14,
    name: 'Azure fallback',
    provider: 'azure',
    model: 'gpt-4o',
    variant: null,
    mode: 'api',
    base_url: null,
    concurrency: 1,
    enabled: true,
    api_key_set: true,
    health_status: 'unknown',
    health_checked_at: null,
    health_error: '',
    health_age_seconds: 0,
    health_stale: true,
    created_at: '2026-11-05T10:00:00Z',
    updated_at: '2026-11-05T10:00:00Z',
  },
]

const teams: Team[] = [
  { id: 1, name: 'Plataforma', slug: 'plataforma', members: [] },
  { id: 2, name: 'Mobile', slug: 'mobile', members: [] },
]

const policy: QAPolicy = {
  id: 3,
  team_id: 1,
  llm_profile_id: 7,
  blocking_threshold: 'major',
  extra_instructions: '',
  exclude_globs: ['*.pb.go'],
  max_files: 40,
  max_diff_lines: 3000,
  max_passes: 4,
  require_green_ci: false,
  updated_at: '2026-11-19T10:00:00Z',
}

function mountAgents(): VueWrapper {
  return mount(AgentsView)
}

describe('AgentsView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.session.me = admin
    mocks.session.load.mockResolvedValue(null)
    mocks.api.llmProfiles.mockResolvedValue(profiles.map((profile) => ({ ...profile })))
    mocks.api.createLLMProfile.mockResolvedValue({ ...profiles[0] })
    mocks.api.updateLLMProfile.mockResolvedValue({ ...profiles[1] })
    mocks.api.checkLLMHealth.mockResolvedValue({ status: 'queued' })
    mocks.api.teams.mockResolvedValue(teams.map((team) => ({ ...team })))
    mocks.api.getPolicy.mockResolvedValue({ ...policy })
    mocks.api.savePolicy.mockResolvedValue({ ...policy })
  })

  afterEach(() => {
    window.history.replaceState({}, '', '/')
  })

  it('preselecciona el equipo indicado en la URL y carga su política', async () => {
    window.history.replaceState({}, '', '/agents?team=2#policy')
    const wrapper = mountAgents()
    await flushPromises()

    expect(mocks.api.getPolicy).toHaveBeenCalledWith(2)
    expect((wrapper.get('[data-policy-team]').element as HTMLSelectElement).value).toBe('2')
    wrapper.unmount()
  })

  it('shows the stored key state without rendering the key', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    expect(wrapper.text()).toContain('Clave configurada')
    expect(wrapper.text()).toContain('Sin clave')

    wrapper.unmount()
  })

  it('creates a profile from a platform with only the api key', async () => {
    const secret = 'sk-ant-super-secret-456'
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue(secret)
    expect(wrapper.html()).not.toContain(secret)

    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith({
      name: 'Anthropic Claude Sonnet 5.5',
      provider: 'anthropic',
      model: 'claude-sonnet-5-5',
      concurrency: 1,
      enabled: true,
      api_key: secret,
    })
    expect(wrapper.html()).not.toContain(secret)
    expect(wrapper.find('[data-profile-api-key]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('refuses to create a platform profile without an api key', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="openai"]').trigger('click')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('clave API')

    wrapper.unmount()
  })

  it('resets the model when the platform changes', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="openai"]').trigger('click')
    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      'gpt-6.2-sol',
    )

    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      'claude-sonnet-5-5',
    )

    wrapper.unmount()
  })

  it('keeps the typed api key and platform when switching tabs', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-keep-me')

    await wrapper.get('[data-profile-tab-custom]').trigger('click')
    expect((wrapper.get('[data-profile-api-key]').element as HTMLInputElement).value).toBe(
      'sk-keep-me',
    )

    await wrapper.get('[data-profile-tab-platform]').trigger('click')
    expect(wrapper.get('[data-platform="anthropic"]').attributes('aria-pressed')).toBe('true')
    expect((wrapper.get('[data-profile-api-key]').element as HTMLInputElement).value).toBe(
      'sk-keep-me',
    )

    wrapper.unmount()
  })

  it('creates a profile through the custom connection tab', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-profile-tab-custom]').trigger('click')
    await wrapper.get('[data-profile-name]').setValue('Local llama')
    await wrapper.get('[data-profile-provider]').setValue('ollama')
    await wrapper.get('[data-profile-model]').setValue('llama3')
    await wrapper.get('[data-profile-api-key]').setValue('local-key')
    await wrapper.get('[data-profile-base-url]').setValue('http://localhost:11434')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith({
      name: 'Local llama',
      provider: 'ollama',
      model: 'llama3',
      concurrency: 1,
      enabled: true,
      api_key: 'local-key',
      base_url: 'http://localhost:11434',
    })

    wrapper.unmount()
  })

  it('Probar enqueues a health check and refreshes the profiles', async () => {
    const wrapper = mountAgents()
    await flushPromises()
    expect(mocks.api.llmProfiles).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-profile-row="7"] [data-health-button]').trigger('click')
    await flushPromises()

    expect(mocks.api.checkLLMHealth).toHaveBeenCalledWith(7)
    expect(mocks.api.llmProfiles).toHaveBeenCalledTimes(2)

    wrapper.unmount()
  })

  it('maps a team to a profile through its QA policy', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-team]').setValue('2')
    await flushPromises()
    expect(mocks.api.getPolicy).toHaveBeenCalledWith(2)

    await wrapper.get('[data-policy-profile]').setValue('11')
    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      2,
      expect.objectContaining({ llm_profile_id: 11 }),
    )

    wrapper.unmount()
  })

  it('edits a profile without sending an api_key when the field is blank', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
    const nameInput = wrapper.get('[data-profile-name]').element as HTMLInputElement
    const keyInput = wrapper.get('[data-profile-api-key]').element as HTMLInputElement
    expect(nameInput.value).toBe('Local llama')
    expect(keyInput.value).toBe('')

    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.updateLLMProfile).toHaveBeenCalledTimes(1)
    expect(mocks.api.updateLLMProfile.mock.calls[0][0]).toBe(11)
    expect(mocks.api.updateLLMProfile.mock.calls[0][1]).not.toHaveProperty('api_key')

    wrapper.unmount()
  })

  it('sends the model chosen from the catalog select', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="openai"]').trigger('click')
    await wrapper.get('[data-profile-model]').setValue('gpt-6.2-astra')
    await wrapper.get('[data-profile-api-key]').setValue('sk-test')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({ provider: 'openai', model: 'gpt-6.2-astra' }),
    )

    wrapper.unmount()
  })

  it('sends a model typed through the other-model option', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="openai"]').trigger('click')
    await wrapper.get('[data-profile-model]').setValue('__custom__')
    await wrapper.get('[data-profile-model-custom]').setValue('gpt-preview-x')
    await wrapper.get('[data-profile-api-key]').setValue('sk-test')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({ provider: 'openai', model: 'gpt-preview-x' }),
    )

    wrapper.unmount()
  })

  it('edits a catalog profile keeping its model and omitting the api key', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
    expect(wrapper.get('[data-platform="anthropic"]').attributes('aria-pressed')).toBe('true')
    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      'claude-sonnet-5-5',
    )

    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.updateLLMProfile).toHaveBeenCalledWith(
      13,
      expect.objectContaining({ provider: 'anthropic', model: 'claude-sonnet-5-5' }),
    )
    expect(mocks.api.updateLLMProfile.mock.calls[0][1]).not.toHaveProperty('api_key')
    expect(mocks.api.updateLLMProfile.mock.calls[0][1]).not.toHaveProperty('base_url')

    wrapper.unmount()
  })

  it('edits a catalog profile with an unknown model through the other-model field', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="7"] [data-edit-profile]').trigger('click')
    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      '__custom__',
    )
    expect((wrapper.get('[data-profile-model-custom]').element as HTMLInputElement).value).toBe(
      'gpt-5',
    )

    wrapper.unmount()
  })

  it('opens a profile with a base URL in the custom tab', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
    expect((wrapper.get('[data-profile-provider]').element as HTMLInputElement).value).toBe(
      'ollama',
    )
    expect((wrapper.get('[data-profile-base-url]').element as HTMLInputElement).value).toBe(
      'http://localhost:11434',
    )

    wrapper.unmount()
  })

  it('opens a profile with an unknown provider in the custom tab', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="14"] [data-edit-profile]').trigger('click')
    expect((wrapper.get('[data-profile-provider]').element as HTMLInputElement).value).toBe(
      'azure',
    )

    wrapper.unmount()
  })

  it('shows the health age of a fresh profile without a stale marker', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    const cell = wrapper.get('[data-profile-row="7"] [data-health-cell]')
    expect(cell.text()).toContain('comprobado hace 5 min')
    expect(cell.find('[data-health-stale]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('marks a stale verdict distinctly', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    const cell = wrapper.get('[data-profile-row="11"] [data-health-cell]')
    expect(cell.text()).toContain('comprobado hace 1 d')
    expect(cell.find('[data-health-stale]').exists()).toBe(true)
    expect(cell.find('[data-health-stale]').text()).toContain('Desactualizado')

    wrapper.unmount()
  })

  it('says never checked for a profile without a verdict', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    const cell = wrapper.get('[data-profile-row="12"] [data-health-cell]')
    expect(cell.text()).toContain('nunca comprobado')
    expect(cell.find('[data-health-stale]').exists()).toBe(true)

    wrapper.unmount()
  })

  it('renders the CI requirement unchecked when the policy is off', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    const checkbox = wrapper.get('[data-policy-require-green-ci]')
      .element as HTMLInputElement
    expect(checkbox.checked).toBe(false)

    wrapper.unmount()
  })

  it('renders the CI requirement checked when the policy is on', async () => {
    mocks.api.getPolicy.mockResolvedValue({ ...policy, require_green_ci: true })
    mocks.api.savePolicy.mockResolvedValue({ ...policy, require_green_ci: true })

    const wrapper = mountAgents()
    await flushPromises()

    const checkbox = wrapper.get('[data-policy-require-green-ci]')
      .element as HTMLInputElement
    expect(checkbox.checked).toBe(true)

    wrapper.unmount()
  })

  it('sends require_green_ci true when the CI requirement is enabled', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-require-green-ci]').setValue(true)
    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      1,
      expect.objectContaining({ require_green_ci: true }),
    )

    wrapper.unmount()
  })

  it('still sends require_green_ci false when the requirement is off', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      1,
      expect.objectContaining({ require_green_ci: false }),
    )

    wrapper.unmount()
  })
})
