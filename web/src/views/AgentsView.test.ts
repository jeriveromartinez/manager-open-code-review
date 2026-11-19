import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
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
    created_at: '2026-11-02T10:00:00Z',
    updated_at: '2026-11-18T10:00:00Z',
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

  it('creates a profile, never renders the api key and shows api_key_set', async () => {
    const secret = 'sk-super-secret-123'
    const wrapper = mountAgents()
    await flushPromises()

    expect(wrapper.text()).toContain('Clave configurada')
    expect(wrapper.text()).toContain('Sin clave')

    await wrapper.get('[data-open-profile]').trigger('click')
    await wrapper.get('[data-profile-name]').setValue('Nuevo perfil')
    await wrapper.get('[data-profile-provider]').setValue('anthropic')
    await wrapper.get('[data-profile-model]').setValue('claude-3')
    await wrapper.get('[data-profile-api-key]').setValue(secret)
    expect(wrapper.html()).not.toContain(secret)
    await wrapper.get('[data-profile-submit]').trigger('click')
    await flushPromises()

    expect(mocks.api.createLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'Nuevo perfil',
        provider: 'anthropic',
        model: 'claude-3',
        api_key: secret,
      }),
    )
    expect(wrapper.text()).not.toContain(secret)
    expect(wrapper.html()).not.toContain(secret)
    expect(wrapper.find('[data-profile-api-key]').exists()).toBe(false)

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
})
