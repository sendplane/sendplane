import type { Layout, MessageResult, Sender, Template, Whoami } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { h } from 'vue'

import CampaignListPage from '../src/pages/CampaignListPage.vue'
import LayoutEditorPage from '../src/pages/LayoutEditorPage.vue'
import LayoutListPage from '../src/pages/LayoutListPage.vue'
import MessageSendPage from '../src/pages/MessageSendPage.vue'
import TemplateEditorPage from '../src/pages/TemplateEditorPage.vue'
import TemplateListPage from '../src/pages/TemplateListPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const tenantWhoami: Whoami = {
  principal_id: 'p1',
  tenant_id: 't1',
  system_tenant: false,
  can_switch_tenant: true,
}

const systemWhoami: Whoami = {
  principal_id: 'p1',
  tenant_id: '_system',
  system_tenant: true,
  can_switch_tenant: true,
}

const sharedWelcome: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a01',
  name: 'Welcome',
  key: 'welcome',
  shared: true,
  subject: 'Hi',
  mode: 'mjml',
  body: '<mjml><mj-body></mj-body></mjml>',
  published_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5c01',
  version: 3,
}

const sharedNewsletter: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a02',
  name: 'Newsletter',
  key: 'newsletter',
  shared: true,
  uses: ['campaign'],
  subject: 'News',
  mode: 'mjml',
  published_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5c02',
  version: 1,
}

const overriddenReceipt: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a03',
  name: 'Receipt',
  key: 'receipt',
  overridden: true,
  shared_updated_since_override: true,
  overridden_from_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5c03',
  subject: 'Your receipt',
  mode: 'mjml',
  body: '<mjml><mj-body></mj-body></mjml>',
  published_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5c03',
  version: 2,
}

const ownPlain: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a04',
  name: 'Plain',
  subject: 'Plain',
  mode: 'html',
  published_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5c04',
  version: 1,
}

const sharedLayout: Layout = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e6a01',
  name: 'Brand frame',
  key: 'brand',
  shared: true,
  mode: 'mjml',
  body: '<mjml><mj-body>{{ content }}</mj-body></mjml>',
  version: 1,
}

const overriddenLayout: Layout = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e6a02',
  name: 'Footer frame',
  key: 'footer',
  overridden: true,
  mode: 'mjml',
  body: '<mjml><mj-body>{{ content }}</mj-body></mjml>',
  version: 4,
}

const sender: Sender = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a55',
  name: 'Acme mail',
  from_email: 'hello@acme.test',
  version: 1,
}

function buttonTexts(wrapper: VueWrapper): string[] {
  return wrapper.findAll('button').map((b) => b.text())
}

function buttonByText(wrapper: VueWrapper, text: string) {
  return wrapper.findAll('button').find((b) => b.text() === text)
}

function fieldByLabel(wrapper: VueWrapper, label: string) {
  return wrapper.findAll('.sp-field').find((field) => field.find('label').text().includes(label))!
}

beforeEach(() => {
  localStorage.clear()
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true),
  )
})

describe('template and layout lists', () => {
  it('shows the key, badges shared and overridden rows, and hides delete on shared ones', async () => {
    const get = vi.fn(async () => ({ items: [sharedWelcome, overriddenReceipt, ownPlain] }))
    const wrapper = mount(
      withProvider(TemplateListPage, {
        client: fakeClient({ get }),
        locale: 'en',
        whoami: tenantWhoami,
      }),
    )
    await flush()

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(3)

    expect(rows[0]!.text()).toContain('welcome')
    expect(rows[0]!.find('.sp-shared-badge').text()).toBe('Shared')
    expect(rows[0]!.findAll('button')).toHaveLength(0)

    expect(rows[1]!.text()).toContain('receipt')
    expect(rows[1]!.text()).toContain('Overridden')
    expect(rows[1]!.text()).toContain('Shared copy changed')
    expect(rows[1]!.find('.sp-shared-badge').exists()).toBe(false)
    expect(rows[1]!.findAll('button').map((b) => b.text())).toContain('Delete')

    expect(rows[2]!.text()).not.toContain('Overridden')
    expect(rows[2]!.text()).not.toContain('Shared')
    expect(rows[2]!.text()).toContain('—')
  })

  it('lets the system tenant create templates and delete its shared ones', async () => {
    const get = vi.fn(async () => ({ items: [sharedWelcome] }))
    const wrapper = mount(
      withProvider(TemplateListPage, {
        client: fakeClient({ get }),
        locale: 'en',
        whoami: systemWhoami,
      }),
    )
    await flush()

    expect(buttonTexts(wrapper)).toContain('New template')
    expect(
      wrapper
        .find('tbody tr')
        .findAll('button')
        .map((b) => b.text()),
    ).toContain('Delete')
  })

  it('badges layouts the same way', async () => {
    const get = vi.fn(async () => ({ items: [sharedLayout, overriddenLayout] }))
    const wrapper = mount(
      withProvider(LayoutListPage, {
        client: fakeClient({ get }),
        locale: 'en',
        whoami: tenantWhoami,
      }),
    )
    await flush()

    const rows = wrapper.findAll('tbody tr')
    expect(rows[0]!.text()).toContain('brand')
    expect(rows[0]!.text()).toContain('Shared')
    expect(rows[0]!.findAll('button')).toHaveLength(0)
    expect(rows[1]!.text()).toContain('Overridden')
    // Only templates carry a published version to fall behind.
    expect(rows[1]!.text()).not.toContain('Shared copy changed')
  })
})

function templateEditor(
  template: Template,
  options: { whoami: Whoami; overrides?: Partial<Record<string, unknown>> },
) {
  const navigate = vi.fn()
  const get = vi.fn(async (path: string) => {
    switch (path) {
      case '/api/v1/templates/{templateId}':
        return template
      case '/api/v1/templates/{templateId}/i18n/keys':
        return { default_locale: 'en', locales: ['en'], items: [] }
      case '/api/v1/templates/{templateId}/i18n':
        return { default_locale: 'en', locales: { en: {} } }
      case '/api/v1/layouts':
        return { items: [sharedLayout] }
      case '/api/v1/templates':
        return { items: [sharedWelcome, ownPlain] }
      default:
        throw new Error(`unexpected GET ${path}`)
    }
  })
  const client = fakeClient({ get, ...options.overrides })
  const wrapper = mount(
    withProvider(TemplateEditorPage, {
      client,
      locale: 'en',
      navigate,
      whoami: options.whoami,
      props: { templateId: template.id },
      slots: { 'block-editor': () => h('div', { class: 'host-block-editor' }, 'host editor') },
    }),
  )
  return { wrapper, navigate, get }
}

describe("a tenant's view of a shared template", () => {
  it('is read-only: no save, publish or translation writes, but preview and override', async () => {
    const { wrapper } = templateEditor(sharedWelcome, { whoami: tenantWhoami })
    await flush()

    const buttons = buttonTexts(wrapper)
    expect(buttons).not.toContain('Save')
    expect(buttons).not.toContain('Publish')
    expect(buttons).not.toContain('Save translations')
    expect(buttons).not.toContain('Import YAML')
    expect(buttons).toContain('Override for this tenant')

    expect(wrapper.text()).toContain('read-only in this tenant')
    const name = fieldByLabel(wrapper, 'Name').find('input')
    expect(name.attributes('disabled')).toBeDefined()

    // Preview is a read, so it stays.
    expect(buttonByText(wrapper, 'Render')!.attributes('disabled')).toBeUndefined()
  })

  it('overrides through POST .../override and opens the copy', async () => {
    const copy: Template = {
      ...sharedWelcome,
      id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a99',
      shared: false,
      overridden: true,
    }
    const post = vi.fn(async () => copy)
    const { wrapper, navigate } = templateEditor(sharedWelcome, {
      whoami: tenantWhoami,
      overrides: { post },
    })
    await flush()

    await buttonByText(wrapper, 'Override for this tenant')!.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/templates/{templateId}/override', {
      params: { path: { templateId: sharedWelcome.id } },
    })
    expect(navigate).toHaveBeenCalledWith({
      name: 'template',
      params: { templateId: copy.id },
    })
  })
})

describe("a tenant's override of a shared template", () => {
  it('names the shared key, flags the republished original, and stays editable', async () => {
    const { wrapper } = templateEditor(overriddenReceipt, { whoami: tenantWhoami })
    await flush()

    expect(wrapper.text()).toContain('override of the shared template receipt')
    expect(wrapper.text()).toContain('published again after this copy was made')
    expect(buttonTexts(wrapper)).toContain('Save')
    expect(buttonTexts(wrapper)).toContain('Delete override (revert to shared)')
  })

  it('deleting the override goes back to the shared template, found by key', async () => {
    const del = vi.fn(async () => undefined)
    const sharedReceipt: Template = {
      ...overriddenReceipt,
      id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a77',
      shared: true,
      overridden: false,
    }
    const { wrapper, navigate, get } = templateEditor(overriddenReceipt, {
      whoami: tenantWhoami,
      overrides: { del },
    })
    get.mockImplementation(async (path: string) => {
      if (path === '/api/v1/templates') return { items: [sharedWelcome, sharedReceipt] }
      if (path === '/api/v1/templates/{templateId}') return overriddenReceipt
      if (path === '/api/v1/layouts') return { items: [] }
      return { items: [] }
    })
    await flush()

    await buttonByText(wrapper, 'Delete override (revert to shared)')!.trigger('click')
    await flush()

    expect(del).toHaveBeenCalledWith('/api/v1/templates/{templateId}', {
      params: { path: { templateId: overriddenReceipt.id } },
    })
    expect(navigate).toHaveBeenCalledWith({
      name: 'template',
      params: { templateId: sharedReceipt.id },
    })
  })
})

describe("the system tenant's template editor", () => {
  it('offers sharing and uses, and saves key, shared and uses on every PUT', async () => {
    const put = vi.fn(async () => sharedNewsletter)
    const { wrapper } = templateEditor(sharedNewsletter, {
      whoami: systemWhoami,
      overrides: { put },
    })
    await flush()

    expect(wrapper.text()).not.toContain('read-only in this tenant')
    const checkboxes = wrapper.findAll('input[type="checkbox"]')
    const labels = wrapper.findAll('.sp-check label').map((l) => l.text())
    expect(labels).toEqual(['Share with every tenant', 'Campaigns', 'Transactional sends'])
    expect((checkboxes[0]!.element as HTMLInputElement).checked).toBe(true)
    expect((checkboxes[1]!.element as HTMLInputElement).checked).toBe(true)
    expect((checkboxes[2]!.element as HTMLInputElement).checked).toBe(false)

    await checkboxes[2]!.setValue(true)
    await buttonByText(wrapper, 'Save')!.trigger('click')
    await flush()

    expect(put).toHaveBeenCalledWith(
      '/api/v1/templates/{templateId}',
      expect.objectContaining({
        body: expect.objectContaining({
          key: 'newsletter',
          shared: true,
          uses: ['campaign', 'transactional'],
          version: 1,
        }),
      }),
    )
  })

  it('rejects a malformed key before it reaches the server', async () => {
    const { wrapper } = templateEditor(sharedNewsletter, { whoami: systemWhoami })
    await flush()

    await fieldByLabel(wrapper, 'Key').find('input').setValue('Bad Key')
    await flush()

    expect(fieldByLabel(wrapper, 'Key').find('[role="alert"]').text()).toContain('1-64 characters')
    expect(buttonByText(wrapper, 'Save')!.attributes('disabled')).toBeDefined()
  })
})

describe("a tenant's view of a shared layout", () => {
  it('is read-only with an override that opens the copy', async () => {
    const navigate = vi.fn()
    const copy: Layout = { ...sharedLayout, id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e6a99' }
    const post = vi.fn(async () => copy)
    const get = vi.fn(async () => sharedLayout)
    const wrapper = mount(
      withProvider(LayoutEditorPage, {
        client: fakeClient({ get, post }),
        locale: 'en',
        navigate,
        whoami: tenantWhoami,
        props: { layoutId: sharedLayout.id },
      }),
    )
    await flush()

    expect(buttonTexts(wrapper)).not.toContain('Save')
    await buttonByText(wrapper, 'Override for this tenant')!.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/layouts/{layoutId}/override', {
      params: { path: { layoutId: sharedLayout.id } },
    })
    expect(navigate).toHaveBeenCalledWith({ name: 'layout', params: { layoutId: copy.id } })
  })
})

describe('choosing a template to send', () => {
  const result: MessageResult = {
    version_id: sharedWelcome.published_version_id!,
    deliveries: [
      {
        delivery_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4d01',
        email: 'a@example.com',
        status: 'queued',
      },
    ],
  }

  function sendPage() {
    const get = vi.fn(async (path: string) => {
      if (path === '/api/v1/senders') return { items: [sender] }
      if (path === '/api/v1/templates')
        return { items: [sharedWelcome, sharedNewsletter, ownPlain] }
      throw new Error(`unexpected GET ${path}`)
    })
    const post = vi.fn(async () => result)
    const wrapper = mount(
      withProvider(MessageSendPage, {
        client: fakeClient({ get, post }),
        locale: 'en',
        navigate: vi.fn(),
        whoami: tenantWhoami,
      }),
    )
    return { wrapper, post }
  }

  function templateOptions(wrapper: VueWrapper) {
    return fieldByLabel(wrapper, 'Template')
      .findAll('option')
      .filter((o) => o.attributes('value'))
  }

  it('marks shared templates and disables one restricted to campaigns', async () => {
    const { wrapper } = sendPage()
    await flush()

    const options = templateOptions(wrapper)
    const welcome = options.find((o) => o.text().startsWith('Welcome'))!
    const newsletter = options.find((o) => o.text().startsWith('Newsletter'))!
    expect(welcome.text()).toContain('[welcome]')
    expect(welcome.text()).toContain('Shared')
    expect(welcome.attributes('disabled')).toBeUndefined()
    expect(newsletter.text()).toContain('only for campaign')
    expect(newsletter.attributes('disabled')).toBeDefined()
  })

  it('sends template_key instead of template_id when selecting by key', async () => {
    const { wrapper, post } = sendPage()
    await flush()

    await fieldByLabel(wrapper, 'Sender').find('select').setValue(sender.id)
    await fieldByLabel(wrapper, 'Template').find('select').setValue(sharedWelcome.id)
    const byKey = wrapper
      .findAll('.sp-check')
      .find((c) => c.text().includes('Select by key'))!
      .find('input')
    await byKey.setValue(true)
    await flush()

    // A template without a key cannot be named by one.
    const plain = templateOptions(wrapper).find((o) => o.text().startsWith('Plain'))!
    expect(plain.attributes('disabled')).toBeDefined()

    await wrapper.find('input[aria-label="Email"]').setValue('a@example.com')
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledTimes(1)
    const body = (post.mock.calls[0] as unknown as [string, { body: Record<string, unknown> }])[1]
      .body
    expect(body.template_key).toBe('welcome')
    expect(body).not.toHaveProperty('template_id')
  })

  it('disables a shared template restricted to transactional sends on campaign create', async () => {
    const transactionalOnly: Template = {
      ...sharedWelcome,
      id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e5a55',
      name: 'Reset',
      key: 'reset',
      uses: ['transactional'],
    }
    const get = vi.fn(async (path: string) => {
      if (path === '/api/v1/campaigns') return { items: [] }
      if (path === '/api/v1/senders') return { items: [sender] }
      if (path === '/api/v1/templates') return { items: [sharedWelcome, transactionalOnly] }
      throw new Error(`unexpected GET ${path}`)
    })
    const post = vi.fn(async () => ({ id: 'c1', name: 'Launch', status: 'draft' }))
    const navigate = vi.fn()
    const wrapper = mount(
      withProvider(CampaignListPage, {
        client: fakeClient({ get, post }),
        locale: 'en',
        navigate,
        whoami: tenantWhoami,
      }),
    )
    await flush()
    await buttonByText(wrapper, 'New campaign')!.trigger('click')
    await flush()

    const options = templateOptions(wrapper)
    expect(options.find((o) => o.text().startsWith('Reset'))!.attributes('disabled')).toBeDefined()
    expect(
      options.find((o) => o.text().startsWith('Welcome'))!.attributes('disabled'),
    ).toBeUndefined()

    await fieldByLabel(wrapper, 'Name').find('input').setValue('Launch')
    await fieldByLabel(wrapper, 'Sender').find('select').setValue(sender.id)
    await fieldByLabel(wrapper, 'Template').find('select').setValue(sharedWelcome.id)
    await wrapper
      .findAll('.sp-check')
      .find((c) => c.text().includes('Select by key'))!
      .find('input')
      .setValue(true)
    await wrapper.find('form').trigger('submit')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/campaigns', {
      body: { name: 'Launch', sender_id: sender.id, template_key: 'welcome' },
    })
  })
})
