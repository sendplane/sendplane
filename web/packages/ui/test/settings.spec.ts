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
  const container = wrapper
    .findAll('.sp-check')
    .find((c) => c.find('label').text() === labelText)!
  return container.find('input[type="checkbox"]')
}

function build(overrides: Partial<TenantSettings> = {}) {
  const get = vi.fn(async () => ({ ...settings, ...overrides }))
  const put = vi.fn(async () => ({ ...settings, ...overrides, version: 4 }))
  const client = fakeClient({ get, put })
  const wrapper = mount(
    withProvider(SettingsPage, { client, locale: 'en', navigate: vi.fn() }),
  )
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
})
