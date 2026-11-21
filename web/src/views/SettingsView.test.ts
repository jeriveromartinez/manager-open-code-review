import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type SettingsField } from '../api'
import SettingsView from './SettingsView.vue'

vi.mock('../api', () => ({ api: { settings: vi.fn(), saveSettings: vi.fn() } }))
type LiveEvent = { type: string; id: string }
const live = vi.hoisted(() => ({ reload: null as ((events: LiveEvent[]) => unknown) | null, types: [] as string[] }))
vi.mock('../composables/useLiveRefresh', () => ({
  useLiveRefresh: (types: string[], reload: (events: LiveEvent[]) => unknown) => {
    live.types = types
    live.reload = reload
  },
}))


function field(
  overrides: Partial<SettingsField> & Pick<SettingsField, 'key' | 'group'>,
): SettingsField {
  return {
    label: overrides.key,
    help: 'Ayuda del ajuste.',
    kind: 'text',
    unit: '',
    default: '',
    restart: '',
    secret: false,
    ...overrides,
  }
}

const fields: SettingsField[] = [
  field({
    key: 'public_url',
    group: 'general',
    label: 'URL pública',
    help: 'Dirección con la que GitHub entrega los webhooks.',
    kind: 'url',
  }),
  field({
    key: 'webhooks_enabled',
    group: 'general',
    label: 'Recibir webhooks',
    help: 'Desactivado, el panel solo descubre cambios en cada reconciliación.',
    kind: 'bool',
    default: 'true',
    restart: 'app',
  }),
  field({
    key: 'review_timeout',
    group: 'review',
    label: 'Tiempo máximo por revisión',
    help: 'Al superarlo, el run falla y el PR queda inconcluso.',
    kind: 'duration',
    unit: 'm',
    default: '20m',
    restart: 'worker',
  }),
  field({
    key: 'ocr_bin',
    group: 'worker',
    label: 'Binario ocr',
    help: 'Nombre o ruta del ejecutable ocr dentro del worker.',
    default: 'ocr',
    restart: 'worker',
  }),
  field({
    key: 'fetch_depth_max',
    group: 'worker',
    label: 'Profundidad de fetch',
    help: 'Commits que se descargan del repositorio para calcular el diff.',
    kind: 'int',
    default: '500',
    restart: 'worker',
  }),
  field({
    key: 'alert_webhook_url',
    group: 'alerts',
    label: 'Webhook de alertas',
    help: 'POST opcional para las alertas.',
    kind: 'url',
    restart: 'monitor',
    secret: true,
  }),
]

const stored = {
  public_url: '',
  webhooks_enabled: 'true',
  review_timeout: '20m0s',
  ocr_bin: 'ocr',
  fetch_depth_max: '500',
  alert_webhook_url: '',
}

const groupsWithFields = new Set(fields.map((item) => item.group)).size

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

beforeEach(() => {
  live.reload = null
  vi.mocked(api.settings).mockReset()
  vi.mocked(api.settings).mockResolvedValue({ values: { ...stored }, fields })
  vi.mocked(api.saveSettings).mockReset()
  vi.mocked(api.saveSettings).mockResolvedValue({
    values: { ...stored, review_timeout: '30m0s' },
    fields,
  })
})

describe('SettingsView live updates', () => {
  it('takes new values into groups without unsaved changes', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(live.types).toEqual(['settings.updated'])
    vi.mocked(api.settings).mockResolvedValue({ values: { ...stored, ocr_bin: 'ocr2' }, fields })
    await live.reload!([{ type: 'settings.updated', id: '' }])
    await flushPromises()

    expect((wrapper.find('input[name="ocr_bin"]').element as HTMLInputElement).value).toBe('ocr2')
    expect(wrapper.find('[data-stale-group]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('keeps unsaved edits, warns, and applies the server values on reload', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="ocr_bin"]').setValue('mine')
    vi.mocked(api.settings).mockResolvedValue({ values: { ...stored, ocr_bin: 'theirs' }, fields })
    await live.reload!([{ type: 'settings.updated', id: '' }])
    await flushPromises()

    expect((wrapper.find('input[name="ocr_bin"]').element as HTMLInputElement).value).toBe('mine')
    const notice = wrapper.get('[data-group="worker"] [data-stale-group]')
    expect(notice.text()).toContain('Otra persona cambió estos ajustes')

    await notice.get('button').trigger('click')
    await flushPromises()
    expect((wrapper.find('input[name="ocr_bin"]').element as HTMLInputElement).value).toBe('theirs')
    expect(wrapper.find('[data-stale-group]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('clears the notice once the group is saved', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="ocr_bin"]').setValue('mine')
    vi.mocked(api.settings).mockResolvedValue({ values: { ...stored, ocr_bin: 'theirs' }, fields })
    await live.reload!([{ type: 'settings.updated', id: '' }])
    await flushPromises()
    expect(wrapper.find('[data-group="worker"] [data-stale-group]').exists()).toBe(true)

    vi.mocked(api.saveSettings).mockResolvedValue({ values: { ...stored, ocr_bin: 'mine' }, fields })
    await wrapper.get('[data-group="worker"]').trigger('submit')
    await flushPromises()

    expect(wrapper.find('[data-stale-group]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('does not warn about a group whose server values did not change', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="ocr_bin"]').setValue('mine')
    await live.reload!([{ type: 'settings.updated', id: '' }])
    await flushPromises()

    expect(wrapper.find('[data-stale-group]').exists()).toBe(false)
    expect((wrapper.find('input[name="ocr_bin"]').element as HTMLInputElement).value).toBe('mine')
    wrapper.unmount()
  })
})

describe('SettingsView', () => {
  it('renders a section per group with the stored value', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.text()).toContain('Revisión')
    expect(
      (wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value,
    ).toBe('20')
  })

  it('shows the human label and help instead of the raw key', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const timeout = wrapper.find('[data-field="review_timeout"]')
    expect(timeout.text()).toContain('Tiempo máximo por revisión')
    expect(timeout.text()).toContain('Al superarlo, el run falla')
    expect(timeout.text()).toContain('Predeterminado: 20m')
    expect(timeout.text()).not.toContain('review_timeout')
  })

  it('renders a duration as amount plus unit and saves a Go duration string', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const amount = wrapper.find('[data-field="review_timeout"] input[name="review_timeout"]')
    const unit = wrapper.find('[data-field="review_timeout"] select')
    expect(amount.attributes('type')).toBe('number')
    expect((amount.element as HTMLInputElement).value).toBe('20')
    expect((unit.element as HTMLSelectElement).value).toBe('m')

    await amount.setValue('30')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
  })

  it('saves a boolean as true or false', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const box = wrapper.find('[data-field="webhooks_enabled"] input[type="checkbox"]')
    expect((box.element as HTMLInputElement).checked).toBe(true)

    await box.setValue(false)
    await wrapper.find('form[data-group="general"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({
      public_url: '',
      webhooks_enabled: 'false',
    })
  })

  it('renders an integer field as a number input', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    const depth = wrapper.find('[data-field="fetch_depth_max"] input')
    expect(depth.attributes('type')).toBe('number')
    expect((depth.element as HTMLInputElement).value).toBe('500')
  })

  it('saves only the edited section', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
  })

  it('keeps unsaved drafts in other sections when one section is saved', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="public_url"]').setValue('https://qa.example.com')
    await wrapper.find('input[name="review_timeout"]').setValue('30')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
    expect(
      (wrapper.find('input[name="public_url"]').element as HTMLInputElement).value,
    ).toBe('https://qa.example.com')
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
        field({ key: 'app_value', group: 'worker', restart: 'app' }),
        field({ key: 'worker_value', group: 'worker', restart: 'worker' }),
        field({ key: 'monitor_value', group: 'worker', restart: 'monitor' }),
      ],
    })
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.find('form[data-group="worker"]').text()).toContain(
      'requiere reiniciar app y worker y monitor',
    )
  })

  it('renders only the groups that have fields', async () => {
    vi.mocked(api.settings).mockResolvedValue({
      values: { review_timeout: '20m0s' },
      fields: [field({ key: 'review_timeout', group: 'review', kind: 'duration', unit: 'm' })],
    })
    const wrapper = mount(SettingsView)
    await flushPromises()

    const forms = wrapper.findAll('form[data-group]')
    expect(forms).toHaveLength(1)
    expect(forms[0].attributes('data-group')).toBe('review')
  })

  it('shows no restart label when no field of the section needs a restart', async () => {
    vi.mocked(api.settings).mockResolvedValue({
      values: {},
      fields: [field({ key: 'public_url', group: 'general', kind: 'url' })],
    })
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
    expect(plain.attributes('type')).toBe('number')
    expect(plain.attributes('autocomplete')).toBe('off')
  })

  it('reveals a secret only when asked', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('[data-field="alert_webhook_url"] .field__reveal').trigger('click')

    const secret = wrapper.find('input[name="alert_webhook_url"]')
    expect(secret.attributes('type')).toBe('text')
    expect(wrapper.find('[data-field="alert_webhook_url"] .field__reveal').text()).toBe('Ocultar')
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
    expect(
      (wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value,
    ).toBe('20')
  })

  it('renders only the error and one retry control when the load fails', async () => {
    vi.mocked(api.settings).mockRejectedValueOnce(new Error('boom'))
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.text()).toContain('No se pudieron cargar los ajustes.')
    expect(wrapper.findAll('form[data-group]')).toHaveLength(0)
    expect(wrapper.findAll('[data-retry]')).toHaveLength(1)
  })

  it('renders the sections again when the retry succeeds', async () => {
    vi.mocked(api.settings).mockRejectedValueOnce(new Error('boom'))
    const wrapper = mount(SettingsView)
    await flushPromises()
    expect(wrapper.findAll('form[data-group]')).toHaveLength(0)

    await wrapper.get('[data-retry]').trigger('click')
    await flushPromises()

    expect(api.settings).toHaveBeenCalledTimes(2)
    expect(wrapper.findAll('form[data-group]')).toHaveLength(groupsWithFields)
    expect(wrapper.find('[data-retry]').exists()).toBe(false)
  })

  it('keeps the retry control disabled and the error visible while the retry is pending', async () => {
    vi.mocked(api.settings).mockRejectedValueOnce(new Error('boom'))
    const retry = deferred<{ values: Record<string, string>; fields: SettingsField[] }>()
    vi.mocked(api.settings).mockReturnValueOnce(retry.promise)
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.get('[data-retry]').trigger('click')

    const button = wrapper.get('[data-retry]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toBe('Cargando…')
    expect(wrapper.text()).toContain('No se pudieron cargar los ajustes.')
    expect(wrapper.findAll('form[data-group]')).toHaveLength(0)

    retry.resolve({ values: { ...stored }, fields })
    await flushPromises()

    expect(wrapper.findAll('form[data-group]')).toHaveLength(groupsWithFields)
    expect(wrapper.find('[data-retry]').exists()).toBe(false)
  })

  it('shows a disabled loading control before the first response arrives', async () => {
    const first = deferred<{ values: Record<string, string>; fields: SettingsField[] }>()
    vi.mocked(api.settings).mockReturnValueOnce(first.promise)
    const wrapper = mount(SettingsView)
    await nextTick()

    const button = wrapper.get('[data-retry]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.text()).toBe('Cargando…')
    expect(wrapper.findAll('form[data-group]')).toHaveLength(0)

    first.resolve({ values: { ...stored }, fields })
    await flushPromises()

    expect(wrapper.findAll('form[data-group]')).toHaveLength(groupsWithFields)
    expect(wrapper.find('[data-retry]').exists()).toBe(false)
  })

  it('clears the success notice as soon as any field changes', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30')
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

    await wrapper.find('input[name="review_timeout"]').setValue('30')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('review_stale_after')
    expect(
      (wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value,
    ).toBe('30')
  })
})
