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
type LiveRegistration = { types: string[]; reload: () => unknown; filter?: (event: { type: string; id: string }) => boolean }
const live = vi.hoisted(() => ({ registrations: [] as LiveRegistration[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: () => unknown, filter?: LiveRegistration['filter']) => {
    live.registrations.push({ types, reload, filter })
  },
}))

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
  review_state: 'running',
  review_reason: '',
  validations: { passed: 3, total: 5 },
  open_findings: { blocker: 1, major: 1, minor: 1, nit: 0 },
  status_since: '2026-11-19T10:00:00Z',
  team_id: 3,
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
      input_tokens: 25000,
      output_tokens: 5000,
      cache_read_tokens: 60000,
      cache_write_tokens: 10000,
      total_tokens: 100000,
      max_context_tokens: 41000,
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
      triage_verdict: '',
      triaged_by: '',
      triaged_at: null,
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
      triage_verdict: '',
      triaged_by: '',
      triaged_at: null,
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
      triage_verdict: '',
      triaged_by: '',
      triaged_at: null,
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
    live.registrations.length = 0
    mocks.api.prDetail.mockResolvedValue(detail)
    mocks.api.retryPR.mockResolvedValue({ status: 'queued' })
    mocks.api.withdrawFinding.mockResolvedValue({ ...detail.findings[0], status: 'withdrawn' })
    mocks.api.confirmFinding.mockResolvedValue({ ...detail.findings[0], status: 'open' })
  })

  it('refreshes on events for this PR only and keeps an open dialog', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    const [registration] = live.registrations
    expect(registration.types).toEqual(['pr.updated'])
    expect(registration.filter!({ type: 'pr.updated', id: '42' })).toBe(true)
    expect(registration.filter!({ type: 'pr.updated', id: '43' })).toBe(false)

    await wrapper.get('[data-withdraw][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-finding-reason]').setValue('falso positivo')
    mocks.api.prDetail.mockClear()
    const pending = registration.reload()
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).not.toContain('Cargando el detalle')
    await pending
    await flushPromises()

    expect(mocks.api.prDetail).toHaveBeenCalledTimes(1)
    expect((wrapper.get('[data-finding-reason]').element as HTMLInputElement).value).toBe('falso positivo')
    wrapper.unmount()
  })

  it('lets the newest load win over an older quiet one', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    let releaseOld: (value: typeof detail) => void = () => {}
    mocks.api.prDetail.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          releaseOld = resolve
        }),
    )
    const old = live.registrations[0].reload()
    mocks.api.prDetail.mockResolvedValueOnce({ ...detail, title: 'Título nuevo' })
    await live.registrations[0].reload()
    releaseOld({ ...detail, title: 'Título viejo' })
    await old
    await flushPromises()

    expect(wrapper.text()).toContain('Título nuevo')
    expect(wrapper.text()).not.toContain('Título viejo')
    wrapper.unmount()
  })

  it('puts the findings beside the QA checklist and the runs', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    const main = wrapper.get('.split__main')
    const aside = wrapper.get('.split__aside')
    expect(main.find('.severity-list [data-finding]').exists()).toBe(true)
    expect(aside.find('[data-qa-check]').exists()).toBe(true)
    expect(aside.text()).toContain('Ejecuciones')
    expect(main.find('[data-qa-check]').exists()).toBe(false)
    wrapper.unmount()
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

    const tokens = wrapper.get('[data-run-tokens]').text()
    expect(tokens).toContain('100.0k')
    expect(tokens).toContain('25.0k / 5.0k')
    expect(tokens).toContain('70 %')
    expect(tokens).toContain('41.0k')

    wrapper.unmount()
  })

  it('labels a run without token data instead of showing zeros', async () => {
    mocks.api.prDetail.mockResolvedValue({
      ...detail,
      runs: [
        {
          ...detail.runs[0],
          input_tokens: null,
          output_tokens: null,
          cache_read_tokens: null,
          cache_write_tokens: null,
          total_tokens: null,
          max_context_tokens: null,
        },
      ],
    })

    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.find('[data-run-tokens]').exists()).toBe(false)
    expect(wrapper.get('[data-run-no-tokens]').text()).toContain('Sin datos de tokens')

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

  it('confirms an untriaged finding and names who triaged the others', async () => {
    const triaged = {
      ...detail.findings[1],
      triage_verdict: 'confirmed' as const,
      triaged_by: 'mvega',
      triaged_at: '2026-11-19T11:00:00Z',
    }
    mocks.api.prDetail.mockResolvedValue({
      ...detail,
      findings: [detail.findings[0], triaged, detail.findings[2]],
    })

    const wrapper = mountDetail()
    await flushPromises()

    const major = wrapper.get('[data-severity="major"]')
    expect(major.get('[data-triage-note]').text()).toBe('Confirmado por mvega')
    expect(major.find('[data-confirm]').exists()).toBe(false)
    expect(wrapper.get('[data-severity="blocker"] [data-triage-note]').text()).toBe('Sin triar')

    await wrapper.get('[data-confirm][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(mocks.api.confirmFinding).toHaveBeenCalledWith(42, 'f1')

    wrapper.unmount()
  })

  it('summarises what QA must check before approving', async () => {
    const wrapper = mountDetail()
    await flushPromises()

    const check = wrapper.get('[data-qa-check]')
    expect(check.get('[data-triaged-count]').text()).toBe('0/3 triados')
    expect(check.get('[data-check="ci"]').text()).toBe('CI: 3/5 checks en verde')
    expect(check.get('[data-check="ci"]').classes()).toContain('is-bad')
    expect(check.get('[data-check="ai"]').text()).toBe('Última revisión IA: fallida')
    expect(check.get('[data-check="blockers"]').text()).toBe('1 bloqueante abierto')
    expect(check.get('[data-check="untriaged"]').text()).toBe('3 hallazgos sin triar')
    expect(check.get('a').attributes('href')).toBe(`${detail.html_url}/files`)

    wrapper.unmount()
  })

  it('explains a forbidden triage instead of a generic error', async () => {
    mocks.api.withdrawFinding.mockRejectedValue(Object.assign(new Error('forbidden'), { status: 403 }))
    const wrapper = mountDetail()
    await flushPromises()

    await wrapper.get('[data-withdraw][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toBe('Solo QA o el lead del equipo pueden triar hallazgos')

    wrapper.unmount()
  })

  it('offers no triage on a resolved finding and keeps a reopened false positive labelled', async () => {
    const resolved = { ...detail.findings[0], status: 'resolved' }
    const reopened = {
      ...detail.findings[1],
      triage_verdict: 'false_positive' as const,
      triaged_by: 'mvega',
      triaged_at: '2026-11-19T11:00:00Z',
    }
    mocks.api.prDetail.mockResolvedValue({ ...detail, findings: [resolved, reopened, detail.findings[2]] })

    const wrapper = mountDetail()
    await flushPromises()

    const blocker = wrapper.get('[data-severity="blocker"]')
    expect(blocker.get('[data-triage-note]').text()).toBe('Resuelto en el código')
    expect(blocker.find('[data-withdraw]').exists()).toBe(false)
    expect(blocker.find('[data-confirm]').exists()).toBe(false)

    const major = wrapper.get('[data-severity="major"]')
    expect(major.get('[data-triage-note]').text()).toBe('Falso positivo según mvega')
    expect(major.find('[data-confirm]').exists()).toBe(true)

    wrapper.unmount()
  })

  it('explains that a resolved finding cannot be triaged', async () => {
    mocks.api.confirmFinding.mockRejectedValue(Object.assign(new Error('conflict'), { status: 409 }))
    const wrapper = mountDetail()
    await flushPromises()

    await wrapper.get('[data-confirm][data-finding-id="f1"]').trigger('click')
    await wrapper.get('[data-apply]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toBe('El hallazgo ya está resuelto en el código')

    wrapper.unmount()
  })
})
