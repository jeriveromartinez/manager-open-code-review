import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AgentsView from './AgentsView.vue'
import type { LLMProfile, Me, QAPolicy, Team } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    llmProfiles: vi.fn(),
    llmProfileUsage: vi.fn(),
    createLLMProfile: vi.fn(),
    updateLLMProfile: vi.fn(),
    checkLLMHealth: vi.fn(),
    llmModels: vi.fn(),
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
type LiveEvent = { type: string; id: string }
const live = vi.hoisted(() => ({ reload: null as ((events: LiveEvent[]) => unknown) | null, types: [] as string[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: (events: LiveEvent[]) => unknown) => {
    live.types = types
    live.reload = reload
  },
}))

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
  review_context_ceiling: null,
  global_context_ceiling: 270000,
  effective_context_ceiling: 270000,
  review_effort: 'low',
  review_token_budget: 0,
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
    mocks.api.llmProfileUsage.mockResolvedValue({
      window_days: 7,
      has_data: false,
      profiles: [],
    })
    mocks.api.createLLMProfile.mockResolvedValue({ ...profiles[0] })
    mocks.api.updateLLMProfile.mockResolvedValue({ ...profiles[1] })
    mocks.api.checkLLMHealth.mockResolvedValue({ status: 'queued' })
    mocks.api.llmModels.mockResolvedValue({
      models: [
        { id: 'claude-sonnet-5-5', name: 'Claude Sonnet 5.5' },
        { id: 'claude-new-7', name: 'Claude New 7' },
      ],
    })
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

  it('refreshes the profile list when a profile changes elsewhere', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    expect(live.types).toEqual(['llm_profile.updated', 'team.updated', 'settings.updated'])
    mocks.api.llmProfiles.mockResolvedValue(profiles.map((profile) => ({ ...profile, name: `${profile.name} v2` })))
    await live.reload!([{ type: 'llm_profile.updated', id: '7' }])
    await flushPromises()

    expect(wrapper.text()).toContain(`${profiles[0].name} v2`)
    wrapper.unmount()
  })

  it('still refreshes the profiles when the usage aggregate fails', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    mocks.api.llmProfiles.mockResolvedValue(
      profiles.map((profile) => ({ ...profile, name: `${profile.name} v2` })),
    )
    mocks.api.llmProfileUsage.mockRejectedValue(new Error('usage aggregate timed out'))

    await live.reload!([{ type: 'llm_profile.updated', id: '7' }])
    await flushPromises()

    expect(wrapper.find('[data-profile-row="7"]').exists()).toBe(true)
    expect(wrapper.text()).toContain(`${profiles[0].name} v2`)
    expect(wrapper.get('[data-usage-error]').text()).toContain('consumos')

    wrapper.unmount()
  })

  it('warns inside the dialog when the profile being edited changes elsewhere', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
    expect(wrapper.find('[data-stale-profile]').exists()).toBe(false)
    await live.reload!([{ type: 'llm_profile.updated', id: '7' }])
    await flushPromises()
    expect(wrapper.find('[data-stale-profile]').exists()).toBe(false)

    // A finished health check notifies too, but leaves the editable fields alone.
    mocks.api.llmProfiles.mockResolvedValue(profiles.map((profile) => ({ ...profile, health_status: 'ok' })))
    await live.reload!([{ type: 'llm_profile.updated', id: '11' }])
    await flushPromises()
    expect(wrapper.find('[data-stale-profile]').exists()).toBe(false)

    mocks.api.llmProfiles.mockResolvedValue(
      profiles.map((profile) => (profile.id === 11 ? { ...profile, model: 'other-model' } : { ...profile })),
    )
    await live.reload!([{ type: 'llm_profile.updated', id: '11' }])
    await flushPromises()
    expect(wrapper.get('[data-stale-profile]').text()).toContain('Otra persona cambió este perfil')
    wrapper.unmount()
  })

  it('says so when the profile being edited was deleted elsewhere', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
    mocks.api.llmProfiles.mockResolvedValue(profiles.filter((profile) => profile.id !== 11))
    await live.reload!([{ type: 'llm_profile.updated', id: '11' }])
    await flushPromises()

    expect(wrapper.get('[data-stale-profile]').text()).toContain('Otra persona eliminó este perfil')
    expect(wrapper.find('[data-stale-profile] button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps the saved confirmation when its own save comes back as an event', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Política guardada')
    await live.reload!([{ type: 'team.updated', id: '1' }])
    await flushPromises()

    expect(wrapper.text()).toContain('Política guardada')
    wrapper.unmount()
  })

  it('does not overwrite policy edits made while the live check is in flight', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    let release: (value: typeof policy) => void = () => {}
    mocks.api.getPolicy.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = resolve
        }),
    )
    const pending = live.reload!([{ type: 'team.updated', id: '1' }])
    await flushPromises()
    await wrapper.get('[data-policy-require-green-ci]').setValue(true)
    release({ ...policy, max_passes: 2 })
    await pending
    await flushPromises()

    expect((wrapper.get('[data-policy-require-green-ci]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.find('[data-stale-policy]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('reloads a clean policy and protects one with unsaved changes', async () => {
    const wrapper = mountAgents()
    await flushPromises()
    const checkbox = () => wrapper.get('[data-policy-require-green-ci]').element as HTMLInputElement

    mocks.api.getPolicy.mockResolvedValue({ ...policy, require_green_ci: true })
    await live.reload!([{ type: 'team.updated', id: '1' }])
    await flushPromises()
    expect(checkbox().checked).toBe(true)

    await wrapper.get('[data-policy-require-green-ci]').setValue(false)
    mocks.api.getPolicy.mockResolvedValue({ ...policy, require_green_ci: true, max_passes: 2 })
    await live.reload!([{ type: 'team.updated', id: '1' }])
    await flushPromises()
    expect(checkbox().checked).toBe(false)
    expect(wrapper.get('[data-stale-policy]').text()).toContain('Otra persona cambió esta política')

    await wrapper.get('[data-stale-policy] button').trigger('click')
    await flushPromises()
    expect(checkbox().checked).toBe(true)
    expect(wrapper.find('[data-stale-policy]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('sends the inherited defaults and turns an empty ceiling into null', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    const ceiling = wrapper.get('[data-policy-ceiling]').element as HTMLInputElement
    expect(ceiling.value).toBe('')
    expect(ceiling.placeholder).toBe('270000')
    expect(wrapper.get('[data-policy-cost]').text()).toContain('270.0k')
    expect((wrapper.get('[data-policy-effort]').element as HTMLSelectElement).value).toBe('low')

    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      1,
      expect.objectContaining({
        review_context_ceiling: null,
        review_effort: 'low',
        review_token_budget: 0,
      }),
    )
    wrapper.unmount()
  })

  it('shows the global ceiling in the hint even when a team override wins', async () => {
    mocks.api.getPolicy.mockResolvedValue({
      ...policy,
      review_context_ceiling: 120000,
      global_context_ceiling: 270000,
      effective_context_ceiling: 120000,
    })
    const wrapper = mountAgents()
    await flushPromises()

    expect(wrapper.get('[data-policy-cost]').text()).toContain('270.0k')
    expect(wrapper.get('[data-policy-cost]').text()).not.toContain('120.0k')
    expect((wrapper.get('[data-policy-ceiling]').element as HTMLInputElement).placeholder).toBe('270000')
    wrapper.unmount()
  })

  it('refreshes the policy when the global settings change elsewhere', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    mocks.api.getPolicy.mockResolvedValue({
      ...policy,
      global_context_ceiling: 300000,
      effective_context_ceiling: 300000,
    })
    await live.reload!([{ type: 'settings.updated', id: '' }])
    await flushPromises()

    expect(mocks.api.getPolicy).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-policy-cost]').text()).toContain('300.0k')
    expect((wrapper.get('[data-policy-ceiling]').element as HTMLInputElement).placeholder).toBe('300000')
    wrapper.unmount()
  })

  it('sends numeric context controls and the selected effort', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-ceiling]').setValue('150000')
    await wrapper.get('[data-policy-effort]').setValue('high')
    await wrapper.get('[data-policy-budget]').setValue('250000')
    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      1,
      expect.objectContaining({
        review_context_ceiling: 150000,
        review_effort: 'high',
        review_token_budget: 250000,
      }),
    )
    wrapper.unmount()
  })

  it('treats an edit of the context controls as unsaved work', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-policy-ceiling]').setValue('150000')
    mocks.api.getPolicy.mockResolvedValue({ ...policy, effective_context_ceiling: 300000 })
    await live.reload!([{ type: 'team.updated', id: '1' }])
    await flushPromises()

    expect((wrapper.get('[data-policy-ceiling]').element as HTMLInputElement).value).toBe('150000')
    expect(wrapper.get('[data-stale-policy]').text()).toContain('Otra persona cambió esta política')
    wrapper.unmount()
  })

  it('puts the profile register beside the team policy', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    expect(wrapper.get('.split__main').find('[data-profile-row="7"]').exists()).toBe(true)
    expect(wrapper.get('.split__aside').find('#policy').exists()).toBe(true)
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

  it('refuses to create without choosing a platform', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('plataforma')

    wrapper.unmount()
  })

  it('refuses to submit hidden custom values from the platform tab', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-profile-tab-custom]').trigger('click')
    await wrapper.get('[data-profile-provider]').setValue('ollama')
    await wrapper.get('[data-profile-model]').setValue('llama3')
    await wrapper.get('[data-profile-api-key]').setValue('local-key')
    await wrapper.get('[data-profile-base-url]').setValue('http://localhost:11434')
    await wrapper.get('[data-profile-tab-platform]').trigger('click')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('plataforma')

    wrapper.unmount()
  })

  it('does not send a custom base URL from the platform tab', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-profile-tab-custom]').trigger('click')
    await wrapper.get('[data-profile-provider]').setValue('openai')
    await wrapper.get('[data-profile-model]').setValue('gpt-5')
    await wrapper.get('[data-profile-api-key]').setValue('sk-test')
    await wrapper.get('[data-profile-base-url]').setValue('http://proxy.local')
    await wrapper.get('[data-profile-tab-platform]').trigger('click')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith({
      name: 'OpenAI gpt-5',
      provider: 'openai',
      model: 'gpt-5',
      concurrency: 1,
      enabled: true,
      api_key: 'sk-test',
    })

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

  it('edits extra instructions and one exclude glob per line', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    expect((wrapper.get('[data-policy-exclude]').element as HTMLTextAreaElement).value).toBe('*.pb.go')

    await wrapper.get('[data-policy-extra]').setValue('  No comentes estilo  ')
    await wrapper.get('[data-policy-exclude]').setValue('*.pb.go\n\n  vendor/**  \n')
    await wrapper.get('[data-policy-save]').trigger('click')
    await flushPromises()

    expect(mocks.api.savePolicy).toHaveBeenCalledWith(
      1,
      expect.objectContaining({
        extra_instructions: 'No comentes estilo',
        exclude_globs: ['*.pb.go', 'vendor/**'],
      }),
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
    expect(mocks.api.updateLLMProfile.mock.calls[0][1]).toMatchObject({
      base_url: 'http://localhost:11434',
    })

    wrapper.unmount()
  })

  it('sends and clears the reasoning variant', async () => {
    const wrapper = mountAgents()
    await flushPromises()

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="openai"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-test')
    await wrapper.get('[data-profile-variant]').setValue('minimal')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({ variant: 'minimal' }),
    )

    await wrapper.get('[data-profile-row="11"] [data-edit-profile]').trigger('click')
    expect((wrapper.get('[data-profile-variant]').element as HTMLInputElement).value).toBe('70b')
    await wrapper.get('[data-profile-variant]').setValue('')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.updateLLMProfile).toHaveBeenCalledWith(
      11,
      expect.objectContaining({ variant: null }),
    )

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

  it('shows the token consumption of each profile and sin datos when absent', async () => {
    mocks.api.llmProfileUsage.mockResolvedValue({
      window_days: 7,
      has_data: true,
      profiles: [
        {
          profile_id: 7,
          name: 'GPT principal',
          provider: 'openai',
          model: 'gpt-5',
          has_data: true,
          runs: 4,
          checks: 2,
          input_tokens: 300000,
          output_tokens: 20000,
          cache_read_tokens: 80000,
          cache_write_tokens: 0,
          total_tokens: 400000,
          average_tokens: 66666,
        },
        {
          profile_id: 11,
          name: 'Local llama',
          provider: 'ollama',
          model: 'llama3',
          has_data: false,
          runs: 0,
          checks: 0,
          input_tokens: null,
          output_tokens: null,
          cache_read_tokens: null,
          cache_write_tokens: null,
          total_tokens: null,
          average_tokens: null,
        },
      ],
    })

    const wrapper = mountAgents()
    await flushPromises()

    const used = wrapper.get('[data-profile-row="7"] [data-profile-usage]')
    expect(used.get('[data-profile-usage-total]').text()).toContain('400.0k')
    expect(used.get('[data-profile-usage-io]').text()).toContain('300.0k')
    expect(used.get('[data-profile-usage-io]').text()).toContain('20.0k')
    expect(used.get('[data-profile-usage-cache]').text()).toContain('20 %')
    expect(used.get('[data-profile-usage-cache]').text()).toContain('2 chequeos')
    expect(used.get('[data-profile-usage-cache]').text()).toContain('4 ejecuciones')

    const empty = wrapper.get('[data-profile-row="11"] [data-profile-usage]')
    expect(empty.get('[data-profile-usage-empty]').text()).toContain('sin datos')

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

  it('loads platform models on key blur with curated ids first', async () => {
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    expect(mocks.api.llmModels).toHaveBeenCalledWith('anthropic', 'sk-ant')
    const values = wrapper
      .findAll('[data-profile-model] option')
      .map((option) => (option.element as HTMLOptionElement).value)
    expect(values).toEqual(['claude-sonnet-5-5', 'claude-new-7', '__custom__'])
    wrapper.unmount()
  })

  it('keeps curated models and shows a notice when the platform call fails', async () => {
    mocks.api.llmModels.mockRejectedValue(new Error('upstream'))
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    expect(wrapper.get('[data-profile-models-notice]').text()).toContain(
      'se muestran los recomendados',
    )
    const values = wrapper
      .findAll('[data-profile-model] option')
      .map((option) => (option.element as HTMLOptionElement).value)
    expect(values).toContain('claude-opus-5-5')
    wrapper.unmount()
  })

  it('treats an empty model list as a failure', async () => {
    mocks.api.llmModels.mockResolvedValue({ models: [] })
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    expect(wrapper.find('[data-profile-models-notice]').exists()).toBe(true)
    expect(wrapper.findAll('[data-profile-model] option')).toHaveLength(4)
    wrapper.unmount()
  })

  it('ignores a late response for a previous provider and key', async () => {
    let resolveFirst: (value: { models: { id: string; name: string }[] }) => void = () => {}
    mocks.api.llmModels
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveFirst = resolve
          }),
      )
      .mockResolvedValueOnce({ models: [{ id: 'fresh-model', name: 'Fresh' }] })

    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')

    await wrapper.get('[data-platform="openai"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-openai')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    resolveFirst({ models: [{ id: 'claude-old', name: 'Claude Old' }] })
    await flushPromises()

    const values = wrapper
      .findAll('[data-profile-model] option')
      .map((option) => (option.element as HTMLOptionElement).value)
    expect(values).toEqual(['fresh-model', '__custom__'])
    wrapper.unmount()
  })

  it('saves while the model list is still loading', async () => {
    mocks.api.llmModels.mockImplementation(() => new Promise(() => {}))
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({ provider: 'anthropic', api_key: 'sk-ant' }),
    )
    wrapper.unmount()
  })

  it('does not query models when editing with an empty key', async () => {
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    expect(mocks.api.llmModels).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('keeps the stored model in the custom option when the platform no longer lists it', async () => {
    mocks.api.llmModels.mockResolvedValue({ models: [{ id: 'other-model', name: 'Other' }] })
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-profile-row="13"] [data-edit-profile]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-new')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      '__custom__',
    )
    expect((wrapper.get('[data-profile-model-custom]').element as HTMLInputElement).value).toBe(
      'claude-sonnet-5-5',
    )
    wrapper.unmount()
  })

  it('resets to the curated list when the platform changes', async () => {
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()

    await wrapper.get('[data-platform="openai"]').trigger('click')
    const values = wrapper
      .findAll('[data-profile-model] option')
      .map((option) => (option.element as HTMLOptionElement).value)
    expect(values).toEqual(['gpt-6.2-sol', 'gpt-6.2-astra', 'gpt-5.4-nano', '__custom__'])
    wrapper.unmount()
  })

  it('keeps the visible selection in sync when the key changes', async () => {
    mocks.api.llmModels.mockResolvedValue({ models: [{ id: 'remote-only', name: 'Remote Only' }] })
    const wrapper = mountAgents()
    await flushPromises()
    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-platform="anthropic"]').trigger('click')
    await wrapper.get('[data-profile-api-key]').setValue('sk-ant')
    await wrapper.get('[data-profile-api-key]').trigger('blur')
    await flushPromises()
    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      'remote-only',
    )

    await wrapper.get('[data-profile-api-key]').setValue('sk-ant-2')

    expect((wrapper.get('[data-profile-model]').element as HTMLSelectElement).value).toBe(
      '__custom__',
    )
    expect((wrapper.get('[data-profile-model-custom]').element as HTMLInputElement).value).toBe(
      'remote-only',
    )
    wrapper.unmount()
  })
})
