import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import PrDetailView from './PrDetailView.vue'
import type { PRDetail } from '../api'

const mocks = vi.hoisted(() => ({
  api: {
    prDetail: vi.fn(),
    retryPR: vi.fn(),
    withdrawFinding: vi.fn(),
    confirmFinding: vi.fn(),
  },
}))

vi.mock('../api', () => ({ api: mocks.api }))
vi.mock('vue-router', () => ({ useRoute: () => ({ params: { id: '42' } }) }))

const detail: PRDetail = {
  id: 42,
  repo: 'backend-api',
  number: 142,
  title: 'Corrige el login',
  author_login: 'octocat',
  html_url: 'https://github.com/acme/backend-api/pull/142',
  qa_status: 'changes_requested',
  group: 'blocked',
  validations: { passed: 3, total: 5 },
  updated_at: '2026-11-19T10:00:00Z',
  runs: [
    {
      id: 9,
      profile_id: 7,
      head_sha: 'abc123def',
      base_sha: '0000000',
      trigger: 'manual',
      status: 'failed',
      attempt: 2,
      summary: 'falló la revisión',
      coverage: { reviewed_files: 2, skipped_files: 1 },
      usage: { input_tokens: 100 },
      error_code: 'LLM_TIMEOUT',
      error_message: 'timeout contacting provider',
      started_at: '2026-11-19T09:00:00Z',
      finished_at: '2026-11-19T09:05:00Z',
      created_at: '2026-11-19T09:00:00Z',
      updated_at: '2026-11-19T09:05:00Z',
    },
  ],
  findings: [
    {
      id: 'f1',
      fingerprint: 'fp1',
      path: 'internal/auth.go',
      start_line: 10,
      end_line: 12,
      severity: 'blocker',
      category: 'security',
      title: 'Fallo bloqueante',
      body: 'Explica el bloqueo',
      suggestion: 'Corrige el token',
      confidence: 'high',
      status: 'open',
      first_seen_run_id: 9,
      last_seen_run_id: 9,
      created_at: '2026-11-19T10:00:00Z',
      updated_at: '2026-11-19T10:00:00Z',
    },
    {
      id: 'f2',
      fingerprint: 'fp2',
      path: 'web/src/Login.vue',
      start_line: 20,
      end_line: 20,
      severity: 'major',
      category: 'bug',
      title: 'Fallo mayor',
      body: 'Explica el defecto',
      suggestion: 'Ajusta el formulario',
      confidence: 'high',
      status: 'open',
      first_seen_run_id: 9,
      last_seen_run_id: 9,
      created_at: '2026-11-19T10:00:00Z',
      updated_at: '2026-11-19T10:00:00Z',
    },
    {
      id: 'f3',
      fingerprint: 'fp3',
      path: 'README.md',
      start_line: 4,
      end_line: 4,
      severity: 'minor',
      category: 'docs',
      title: 'Fallo menor',
      body: 'Explica el detalle',
      suggestion: 'Actualiza la doc',
      confidence: 'medium',
      status: 'open',
      first_seen_run_id: 9,
      last_seen_run_id: 9,
      created_at: '2026-11-19T10:00:00Z',
      updated_at: '2026-11-19T10:00:00Z',
    },
  ],
}

function mountDetail(): VueWrapper {
  return mount(PrDetailView)
}

describe('PrDetailView', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.api.prDetail.mockResolvedValue(detail)
    mocks.api.retryPR.mockResolvedValue({ status: 'queued' })
    mocks.api.withdrawFinding.mockResolvedValue({ ...detail.findings[0], status: 'withdrawn' })
    mocks.api.confirmFinding.mockResolvedValue({ ...detail.findings[0], status: 'open' })
  })

  it('shows findings grouped by severity and retries the review', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.get('[data-severity="blocker"]').text()).toContain('Fallo bloqueante')
    expect(wrapper.get('[data-severity="major"]').text()).toContain('Fallo mayor')
    expect(wrapper.get('[data-severity="minor"]').text()).toContain('Fallo menor')
    expect(wrapper.get('[data-severity="major"]').text()).not.toContain('Fallo menor')

    await wrapper.get('[data-retry]').trigger('click')
    await flushPromises()

    expect(mocks.api.retryPR).toHaveBeenCalledWith(42)
    expect(mocks.api.prDetail).toHaveBeenCalledTimes(2)

    wrapper.unmount()
  })

  it('uses the newest run for the SHA and coverage counts', async () => {
    const olderRun = {
      ...detail.runs[0],
      id: 8,
      head_sha: 'oldsha111',
      coverage: { reviewed_files: 1, skipped_files: 5 },
    }
    mocks.api.prDetail.mockResolvedValue({ ...detail, runs: [olderRun, ...detail.runs] })

    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.text()).toContain('abc123def')
    expect(wrapper.text()).not.toContain('oldsha111')
    expect(wrapper.get('[data-coverage-reviewed]').text()).toContain('2')
    expect(wrapper.get('[data-coverage-skipped]').text()).toContain('1')

    wrapper.unmount()
  })

  it('shows the header, run details and coverage counts', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.get('.page-title').text()).toContain('Corrige el login')
    expect(wrapper.text()).toContain('abc123def')
    expect(wrapper.text()).toContain('octocat')
    expect(wrapper.get('[data-qa-status]').text()).toContain('Con defectos')
    expect(wrapper.get('[data-run]').text()).toContain('manual')
    expect(wrapper.get('[data-run]').text()).toContain('timeout contacting provider')
    expect(wrapper.get('[data-coverage-reviewed]').text()).toContain('2')
    expect(wrapper.get('[data-coverage-skipped]').text()).toContain('1')

    wrapper.unmount()
  })

  it('withdraws a finding with a reason after confirming', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    await wrapper.get('[data-withdraw][data-finding-id="f1"]').trigger('click')
    expect(wrapper.find('[data-confirm-dialog]').exists()).toBe(true)
    expect(mocks.api.withdrawFinding).not.toHaveBeenCalled()

    await wrapper.get('[data-finding-reason]').setValue('falso positivo')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(mocks.api.withdrawFinding).toHaveBeenCalledWith(42, 'f1', 'falso positivo')
    expect(wrapper.find('[data-confirm-dialog]').exists()).toBe(false)
    expect(mocks.api.prDetail).toHaveBeenCalledTimes(2)

    wrapper.unmount()
  })

  it('does nothing when the confirmation is cancelled', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    await wrapper.get('[data-withdraw][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-cancel]').trigger('click')

    expect(wrapper.find('[data-confirm-dialog]').exists()).toBe(false)
    expect(mocks.api.withdrawFinding).not.toHaveBeenCalled()

    wrapper.unmount()
  })

  it('lists withdrawn findings separately and confirms them', async () => {
    const withdrawn = { ...detail.findings[0], status: 'withdrawn' }
    mocks.api.prDetail.mockResolvedValue({
      ...detail,
      findings: [withdrawn, ...detail.findings.slice(1)],
    })

    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.find('[data-severity="blocker"]').exists()).toBe(false)
    expect(wrapper.get('[data-withdrawn]').text()).toContain('Fallo bloqueante')

    await wrapper.get('[data-confirm][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(mocks.api.confirmFinding).toHaveBeenCalledWith(42, 'f1')

    wrapper.unmount()
  })

  it('restores the finding when the action fails', async () => {
    mocks.api.withdrawFinding.mockRejectedValue(new Error('boom'))
    const wrapper = mountDetail()
    await flushPromises()

    await wrapper.get('[data-withdraw][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudo descartar el hallazgo')
    expect(wrapper.get('[data-severity="blocker"]').text()).toContain('Fallo bloqueante')

    wrapper.unmount()
  })
})
