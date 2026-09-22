import {
  SendplaneError,
  type MessageResult,
  type Sender,
  type Template,
  type Whoami,
} from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import MessageSendPage from '../src/pages/MessageSendPage.vue'
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

const ownSender: Sender = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a55',
  name: 'Acme mail',
  from_email: 'hello@acme.test',
  version: 1,
}

const sharedTransactional: Sender = {
  id: 'sys:default',
  shared: true,
  name: 'Platform sender',
  from_name: '{{ tenant.name }}',
  from_email: 'sender+{{ tenant.slug }}@mail.example.com',
  uses: ['campaign', 'transactional'],
  version: 1,
}

const sharedCampaignOnly: Sender = {
  ...sharedTransactional,
  id: 'sys:bulk',
  name: 'Bulk only',
  uses: ['campaign'],
}

const published: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4b01',
  name: 'Password reset',
  subject: 'Reset',
  mode: 'mjml',
  published_version_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4c01',
  version: 4,
}

const unpublished: Template = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4b02',
  name: 'Draft only',
  subject: 'Draft',
  mode: 'mjml',
  version: 1,
}

const result: MessageResult = {
  version_id: published.published_version_id!,
  deliveries: [
    {
      delivery_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4d01',
      email: 'a@example.com',
      status: 'queued',
    },
  ],
}

function build(
  overrides: { senders?: Sender[]; post?: (...args: any[]) => unknown; whoami?: Whoami } = {},
): {
  wrapper: VueWrapper
  post: ReturnType<typeof vi.fn>
} {
  const get = vi.fn(async (path: string) => {
    if (path === '/api/v1/senders')
      return { items: overrides.senders ?? [ownSender, sharedTransactional] }
    if (path === '/api/v1/templates') return { items: [published, unpublished] }
    throw new Error(`unexpected GET ${path}`)
  })
  const post = vi.fn(overrides.post ?? (async () => result))
  const client = fakeClient({ get, post })
  const wrapper = mount(
    withProvider(MessageSendPage, {
      client,
      locale: 'en',
      navigate: vi.fn(),
      whoami: overrides.whoami ?? tenantWhoami,
    }),
  )
  return { wrapper, post }
}

function cardByTitle(wrapper: VueWrapper, title: string) {
  return wrapper.findAll('.sp-card').find((card) => card.find('h2').text() === title)!
}

function fieldByLabel(wrapper: VueWrapper, label: string) {
  return wrapper.findAll('.sp-field').find((field) => field.find('label').text().includes(label))!
}

function buttonByText(wrapper: VueWrapper, text: string) {
  return wrapper.findAll('button').find((button) => button.text() === text)
}

beforeEach(() => {
  localStorage.clear()
})

describe('MessageSendPage', () => {
  it('offers published templates only, with the version that will be used', async () => {
    const { wrapper } = build()
    await flush()

    const options = wrapper
      .findAll('select')[1]!
      .findAll('option')
      .map((o) => o.text())
    expect(options.some((label) => label.startsWith('Password reset'))).toBe(true)
    expect(options.some((label) => label.startsWith('Draft only'))).toBe(false)
    // The published version is shown, not just the template name.
    expect(options.find((label) => label.startsWith('Password reset'))).toContain('0191f3d2')
  })

  it('sends the payload the spec describes, with the idempotency key as a header', async () => {
    const { wrapper, post } = build()
    await flush()

    await wrapper.findAll('select')[0]!.setValue(sharedTransactional.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)

    const recipients = cardByTitle(wrapper, 'Recipients')
    await recipients.find('input[aria-label="Email"]').setValue('a@example.com')
    await recipients.find('input[aria-label="Name"]').setValue('A')
    await recipients.find('input[aria-label="Locale"]').setValue('ko')
    await recipients.find('input[aria-label="Variables (JSON)"]').setValue('{"plan":"pro"}')

    // tenant_vars, through the shared editor.
    const vars = cardByTitle(wrapper, 'Tenant variables')
    await vars
      .findAll('button')
      .find((b) => b.text() === 'Add variable')!
      .trigger('click')
    await vars.find('input[aria-label="Key"]').setValue('slug')
    await vars.find('input[aria-label="Value"]').setValue('acme')

    // One custom header.
    const headers = cardByTitle(wrapper, 'Custom headers')
    await headers
      .findAll('button')
      .find((b) => b.text() === 'Add header')!
      .trigger('click')
    await headers.find('input[aria-label="Header"]').setValue('X-Ticket')
    await headers.find('input[aria-label="Value"]').setValue('42')

    await fieldByLabel(wrapper, 'Idempotency key').find('input').setValue('key-123')
    await fieldByLabel(wrapper, 'Message variables').find('textarea').setValue('{"product":"x"}')
    await flush()

    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/messages', {
      params: { header: { 'Idempotency-Key': 'key-123' } },
      body: {
        template_id: published.id,
        sender_id: sharedTransactional.id,
        to: [{ email: 'a@example.com', name: 'A', locale: 'ko', vars: { plan: 'pro' } }],
        tenant_vars: { slug: 'acme' },
        vars: { product: 'x' },
        headers: { 'X-Ticket': '42' },
      },
    })

    // The result panel links each delivery to its detail page.
    expect(wrapper.text()).toContain('a@example.com')
    expect(
      wrapper.find('a[href="/deliveries/0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4d01"]').exists(),
    ).toBe(true)
  })

  it('rotates the idempotency key after a send, so the next one is not a replay', async () => {
    const { wrapper, post } = build()
    await flush()
    await wrapper.findAll('select')[0]!.setValue(ownSender.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    await flush()

    const before = fieldByLabel(wrapper, 'Idempotency key').find('input').element.value
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(post.mock.calls[0]![1].params.header['Idempotency-Key']).toBe(before)
    expect(fieldByLabel(wrapper, 'Idempotency key').find('input').element.value).not.toBe(before)
  })

  it('flags an idempotent replay', async () => {
    const { wrapper } = build({ post: async () => ({ ...result, idempotent_replay: true }) })
    await flush()
    await wrapper.findAll('select')[0]!.setValue(ownSender.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    await flush()
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(wrapper.text()).toContain('stored result of an earlier identical request')
  })

  it('pastes `email,name,locale` lines into recipient rows', async () => {
    const { wrapper } = build()
    await flush()

    await fieldByLabel(wrapper, 'Paste recipients')
      .find('textarea')
      .setValue('a@example.com,A,ko\nb@example.com\n\n# comment')
    await buttonByText(wrapper, 'Paste')!.trigger('click')
    await flush()

    const emails = cardByTitle(wrapper, 'Recipients')
      .findAll('input[aria-label="Email"]')
      .map((input) => (input.element as HTMLInputElement).value)
    expect(emails).toEqual(['a@example.com', 'b@example.com'])
    expect(wrapper.text()).toContain('2 of at most 1000 recipients')
  })

  it('will not send with a sender whose uses exclude transactional, and says why', async () => {
    const { wrapper, post } = build({ senders: [sharedCampaignOnly] })
    await flush()

    await wrapper.findAll('select')[0]!.setValue(sharedCampaignOnly.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    await flush()

    expect(wrapper.text()).toContain('not allowed for transactional sends')
    expect(buttonByText(wrapper, 'Send')!.attributes('disabled')).toBeDefined()
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()
    expect(post).not.toHaveBeenCalled()
  })

  it('renders tenant_vars_missing on the tenant variables field', async () => {
    const { wrapper } = build({
      post: async () => {
        throw new SendplaneError({
          message: 'sender sys:default: tenant_vars missing',
          code: 'tenant_vars_missing',
          status: 422,
          details: [
            { field: 'tenant_vars.slug', message: 'required' },
            { field: 'tenant_vars.name', message: 'required' },
          ],
        })
      },
    })
    await flush()
    await wrapper.findAll('select')[0]!.setValue(sharedTransactional.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    await flush()
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(cardByTitle(wrapper, 'Tenant variables').text()).toContain(
      'The server needs these tenant variables: slug, name',
    )
  })

  it('renders sender_use_denied inline, with the message the server sent', async () => {
    const { wrapper } = build({
      post: async () => {
        throw new SendplaneError({
          message: 'sender sys:default may not be used for transactional sends',
          code: 'sender_use_denied',
          status: 403,
        })
      },
    })
    await flush()
    await wrapper.findAll('select')[0]!.setValue(sharedTransactional.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    await flush()
    await buttonByText(wrapper, 'Send')!.trigger('click')
    await flush()

    expect(fieldByLabel(wrapper, 'Sender').text()).toContain(
      'sender sys:default may not be used for transactional sends',
    )
  })

  it('previews through the template endpoint with the first recipient and tenant_vars', async () => {
    const { wrapper, post } = build({
      post: async (path: string) =>
        path === '/api/v1/templates/{templateId}/preview'
          ? { locale: 'ko', subject: 'Reset', html: '<p>hi</p>' }
          : result,
    })
    await flush()
    await wrapper.findAll('select')[0]!.setValue(sharedTransactional.id)
    await wrapper.findAll('select')[1]!.setValue(published.id)
    await cardByTitle(wrapper, 'Recipients')
      .find('input[aria-label="Email"]')
      .setValue('a@example.com')
    const vars = cardByTitle(wrapper, 'Tenant variables')
    await vars
      .findAll('button')
      .find((b) => b.text() === 'Add variable')!
      .trigger('click')
    await vars.find('input[aria-label="Key"]').setValue('slug')
    await vars.find('input[aria-label="Value"]').setValue('acme')
    await flush()

    await buttonByText(wrapper, 'Preview')!.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/templates/{templateId}/preview', {
      params: { path: { templateId: published.id } },
      body: {
        recipient: { email: 'a@example.com' },
        tenant_vars: { slug: 'acme' },
        vars: {},
      },
    })
    // The rendered mail is sandboxed, like the template editor's preview.
    const frame = wrapper.find('iframe')
    expect(frame.attributes('sandbox')).toBe('')
    expect(frame.attributes('srcdoc')).toBe('<p>hi</p>')
  })

  it('offers no form at all in the system tenant view', async () => {
    const { wrapper } = build({ whoami: systemWhoami })
    await flush()

    expect(wrapper.text()).toContain('The system tenant cannot send')
    expect(wrapper.findAll('select')).toHaveLength(0)
    expect(buttonByText(wrapper, 'Send')!.attributes('disabled')).toBeDefined()
  })
})
