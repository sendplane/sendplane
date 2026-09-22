import { SendplaneError, type SendingDomain, type Transport, type Whoami } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import SenderEditPage from '../src/pages/SenderEditPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const systemWhoami: Whoami = {
  principal_id: 'p1',
  tenant_id: '_system',
  system_tenant: true,
  can_switch_tenant: true,
}

const sharedTransport: Transport = {
  id: 'sys:relay',
  shared: true,
  name: 'Platform relay',
  host: 'relay.example.com',
  port: 587,
  version: 1,
}

const ownTransport: Transport = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a01',
  name: 'Own relay',
  host: 'smtp.acme.test',
  port: 587,
  version: 2,
}

const sharedDomain: SendingDomain = {
  id: 'sys:maildomain',
  shared: true,
  domain: 'mail.example.com',
  version: 1,
}

const ownDomain: SendingDomain = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a02',
  domain: 'acme.test',
  version: 1,
}

function build(overrides: { post?: (...args: any[]) => unknown; domains?: SendingDomain[] } = {}) {
  const get = vi.fn(async (path: string) => {
    if (path === '/api/v1/transports') return { items: [sharedTransport, ownTransport] }
    if (path === '/api/v1/sending-domains')
      return { items: overrides.domains ?? [sharedDomain, ownDomain] }
    throw new Error(`unexpected GET ${path}`)
  })
  const post = vi.fn(overrides.post ?? (async () => ({ id: 'new', name: 'x' })))
  const client = fakeClient({ get, post })
  const wrapper = mount(
    withProvider(SenderEditPage, {
      client,
      locale: 'en',
      navigate: vi.fn(),
      // The system tenant is the only view that even lists platform rows, so
      // it is the case where filtering them out of the selects matters.
      whoami: systemWhoami,
    }),
  )
  return { wrapper, post }
}

function fieldByLabel(wrapper: VueWrapper, label: string) {
  return wrapper.findAll('.sp-field').find((field) => field.find('label').text().includes(label))!
}

async function fillValidDraft(wrapper: VueWrapper) {
  await fieldByLabel(wrapper, 'Name').find('input').setValue('Acme mail')
  await fieldByLabel(wrapper, 'From address').find('input').setValue('hello@acme.test')
  await fieldByLabel(wrapper, 'Transport').find('select').setValue(ownTransport.id)
  await flush()
}

describe('SenderEditPage form', () => {
  it('leaves platform transports and domains out of the selects', async () => {
    const { wrapper } = build()
    await flush()

    const transportLabels = fieldByLabel(wrapper, 'Transport')
      .findAll('option')
      .map((option) => option.text())
    expect(transportLabels.some((label) => label.includes('Own relay'))).toBe(true)
    expect(transportLabels.some((label) => label.includes('Platform relay'))).toBe(false)

    const domainLabels = fieldByLabel(wrapper, 'Sending domain')
      .findAll('option')
      .map((option) => option.text())
    expect(domainLabels).toContain('acme.test')
    expect(domainLabels).not.toContain('mail.example.com')

    // And it says why, rather than leaving the missing rows unexplained.
    expect(fieldByLabel(wrapper, 'Transport').text()).toContain(
      'Shared transports and domains cannot be assigned',
    )
  })

  it('points at the sending domain when the operator has none of their own yet', async () => {
    const { wrapper } = build({ domains: [sharedDomain] })
    await flush()
    expect(fieldByLabel(wrapper, 'Sending domain').text()).toContain(
      'Register the sending domain first',
    )
  })

  it('surfaces from_domain_not_owned on the From address field', async () => {
    const { wrapper } = build({
      post: async () => {
        throw new SendplaneError({
          message: 'from_email hello@acme.test is not on a sending domain of this tenant',
          code: 'from_domain_not_owned',
          status: 422,
        })
      },
    })
    await flush()
    await fillValidDraft(wrapper)
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Save')!
      .trigger('click')
    await flush()

    expect(fieldByLabel(wrapper, 'From address').text()).toContain(
      'must be on one of your own sending domains',
    )
  })

  it('surfaces transport_not_assignable on the transport field', async () => {
    const { wrapper } = build({
      post: async () => {
        throw new SendplaneError({
          message: 'transport sys:relay cannot be assigned',
          code: 'transport_not_assignable',
          status: 422,
        })
      },
    })
    await flush()
    await fillValidDraft(wrapper)
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Save')!
      .trigger('click')
    await flush()

    expect(fieldByLabel(wrapper, 'Transport').text()).toContain('cannot be assigned')
  })
})
