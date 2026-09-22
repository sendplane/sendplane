import type { TenantSettings } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import SettingsPage from '../src/pages/SettingsPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const settings: TenantSettings = {
  tenant_id: 't1',
  version: 3,
  retention_days: 90,
  suppression_enabled: true,
  unsubscribe_mode: 'host',
  unsubscribe_url_template: '',
  unsubscribe_one_click: false,
  bounce_retain_raw: false,
  default_locale: 'en',
  tracking: { domain: 't.example.com', opens: true, clicks: true },
}

function checkboxByLabel(wrapper: VueWrapper, labelText: string) {
  const container = wrapper.findAll('.sp-check').find((c) => c.find('label').text() === labelText)!
  return container.find('input[type="checkbox"]')
}

// Mirrors SettingsPage's own list (api/openapi.yaml `OutboxEvent.type`).
const ALL_EVENT_TYPES = [
  'delivery.sent',
  'delivery.deferred',
  'delivery.failed',
  'delivery.bounced',
  'delivery.complained',
  'delivery.suppressed',
  'campaign.started',
  'campaign.paused',
  'campaign.completed',
  'campaign.cancelled',
  'transport.unhealthy',
  'transport.recovered',
  'sender.health_changed',
  'mailbox.unhealthy',
  'mailbox.recovered',
  'recipient.unsubscribed',
  'delivery.opened',
  'delivery.clicked',
  'i18n.missing_key',
]
const DEFAULT_EVENT_TYPES = ALL_EVENT_TYPES.filter(
  (t) => !['delivery.sent', 'delivery.opened', 'delivery.clicked'].includes(t),
)

function fieldByLabel(wrapper: VueWrapper, label: string) {
  return wrapper.findAll('.sp-field').find((field) => field.find('label').text().includes(label))!
}

function build(overrides: Partial<TenantSettings> = {}) {
  const get = vi.fn(async () => ({ ...settings, ...overrides }))
  const put = vi.fn(async () => ({ ...settings, ...overrides, version: 4 }))
  const client = fakeClient({ get, put })
  const wrapper = mount(withProvider(SettingsPage, { client, locale: 'en', navigate: vi.fn() }))
  return { wrapper, get, put }
}

describe('SettingsPage', () => {
  it('reflects the server value of both toggles', async () => {
    const { wrapper } = build({ unsubscribe_one_click: true, bounce_retain_raw: true })
    await flush()

    const oneClick = checkboxByLabel(wrapper, 'RFC 8058 one-click')
    const retainRaw = checkboxByLabel(wrapper, 'Keep the raw bounce message')
    expect((oneClick.element as HTMLInputElement).checked).toBe(true)
    expect((retainRaw.element as HTMLInputElement).checked).toBe(true)
  })

  it('round-trips both toggles through save', async () => {
    const { wrapper, put } = build()
    await flush()

    await checkboxByLabel(wrapper, 'RFC 8058 one-click').setValue(true)
    await checkboxByLabel(wrapper, 'Keep the raw bounce message').setValue(true)

    const save = wrapper.findAll('button').find((b) => b.text() === 'Save')!
    await save.trigger('click')
    await flush()

    expect(put).toHaveBeenCalledWith(
      '/api/v1/settings',
      expect.objectContaining({
        body: expect.objectContaining({
          version: 3,
          unsubscribe_one_click: true,
          bounce_retain_raw: true,
        }),
      }),
    )
  })

  it('disables the one-click toggle outside host mode, since it only matters there', async () => {
    const { wrapper } = build({ unsubscribe_mode: 'sendplane' })
    await flush()

    const oneClick = checkboxByLabel(wrapper, 'RFC 8058 one-click')
    expect((oneClick.element as HTMLInputElement).disabled).toBe(true)
  })

  it('renders the event type checkboxes from an explicit server list', async () => {
    const { wrapper } = build({ event_types: ['campaign.started', 'delivery.bounced'] })
    await flush()

    expect((checkboxByLabel(wrapper, 'campaign.started').element as HTMLInputElement).checked).toBe(
      true,
    )
    expect((checkboxByLabel(wrapper, 'delivery.bounced').element as HTMLInputElement).checked).toBe(
      true,
    )
    // Not in the explicit list, so unchecked even though it's part of the default set.
    expect((checkboxByLabel(wrapper, 'campaign.paused').element as HTMLInputElement).checked).toBe(
      false,
    )
    expect((checkboxByLabel(wrapper, 'delivery.sent').element as HTMLInputElement).checked).toBe(
      false,
    )
  })

  it('shows the default set checked when event_types is empty', async () => {
    const { wrapper } = build({ event_types: [] })
    await flush()

    expect((checkboxByLabel(wrapper, 'delivery.sent').element as HTMLInputElement).checked).toBe(
      false,
    )
    expect((checkboxByLabel(wrapper, 'delivery.opened').element as HTMLInputElement).checked).toBe(
      false,
    )
    expect((checkboxByLabel(wrapper, 'delivery.clicked').element as HTMLInputElement).checked).toBe(
      false,
    )
    expect((checkboxByLabel(wrapper, 'campaign.started').element as HTMLInputElement).checked).toBe(
      true,
    )
  })

  it('editing a checkbox from the default set and saving sends the explicit array', async () => {
    const { wrapper, put } = build({ event_types: [] })
    await flush()

    await checkboxByLabel(wrapper, 'campaign.started').setValue(false)

    const save = wrapper.findAll('button').find((b) => b.text() === 'Save')!
    await save.trigger('click')
    await flush()

    expect(put).toHaveBeenCalledWith(
      '/api/v1/settings',
      expect.objectContaining({
        body: expect.objectContaining({
          event_types: DEFAULT_EVENT_TYPES.filter((t) => t !== 'campaign.started'),
        }),
      }),
    )
  })

  it('offers the mailbox health event types, on by default', async () => {
    const { wrapper } = build({ event_types: [] })
    await flush()

    expect(
      (checkboxByLabel(wrapper, 'mailbox.unhealthy').element as HTMLInputElement).checked,
    ).toBe(true)
    expect(
      (checkboxByLabel(wrapper, 'mailbox.recovered').element as HTMLInputElement).checked,
    ).toBe(true)
  })

  it('reset to default clears the array', async () => {
    const { wrapper, put } = build({ event_types: ['campaign.started'] })
    await flush()

    const reset = wrapper.findAll('button').find((b) => b.text() === 'Reset to default')!
    await reset.trigger('click')
    await flush()

    // Cleared draft falls back to showing the default set.
    expect((checkboxByLabel(wrapper, 'campaign.started').element as HTMLInputElement).checked).toBe(
      true,
    )
    expect((checkboxByLabel(wrapper, 'campaign.paused').element as HTMLInputElement).checked).toBe(
      true,
    )

    const save = wrapper.findAll('button').find((b) => b.text() === 'Save')!
    await save.trigger('click')
    await flush()

    expect(put).toHaveBeenCalledWith(
      '/api/v1/settings',
      expect.objectContaining({
        body: expect.objectContaining({ event_types: [] }),
      }),
    )
  })
})

describe('SettingsPage platform defaults (ADR-0017)', () => {
  const fromPlatform: Partial<TenantSettings> = {
    tracking: {
      domain: 't.platform.example',
      domain_source: 'platform',
      opens: true,
      clicks: true,
    },
    unsubscribe_url_template: 'https://platform.example/u',
    unsubscribe_url_template_source: 'platform',
  }

  it('marks a platform-sourced value instead of prefilling it as the tenant\u2019s', async () => {
    const { wrapper } = build(fromPlatform)
    await flush()

    const tracking = fieldByLabel(wrapper, 'Tracking domain')
    expect(tracking.text()).toContain('Using the platform default')
    expect(tracking.text()).toContain('t.platform.example')
    // The field stays empty, and the default is only a placeholder: saving must
    // not freeze the operator's value as a tenant copy.
    expect((tracking.find('input').element as HTMLInputElement).value).toBe('')
    expect(tracking.find('input').attributes('placeholder')).toBe('t.platform.example')
    // Clearing the field is how you go back, so the hint says so.
    expect(tracking.text()).toContain('clear the field')

    const unsubscribe = fieldByLabel(wrapper, 'Host URL template')
    expect(unsubscribe.text()).toContain('Using the platform default')
    expect(unsubscribe.text()).toContain('https://platform.example/u')
    expect((unsubscribe.find('input').element as HTMLInputElement).value).toBe('')
  })

  it('does not demand a tracking domain that the platform already supplies', async () => {
    const { wrapper } = build({ ...fromPlatform, unsubscribe_mode: 'sendplane' })
    await flush()

    const tracking = fieldByLabel(wrapper, 'Tracking domain')
    expect(tracking.find('[role="alert"]').exists()).toBe(false)
  })

  it('shows a tenant-set value as the tenant\u2019s, with no marker', async () => {
    const { wrapper } = build({
      tracking: { domain: 't.acme.test', domain_source: 'tenant', opens: true, clicks: true },
    })
    await flush()

    const tracking = fieldByLabel(wrapper, 'Tracking domain')
    expect((tracking.find('input').element as HTMLInputElement).value).toBe('t.acme.test')
    expect(tracking.text()).not.toContain('Using the platform default')
  })
})
