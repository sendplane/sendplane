import { SendplaneError, type Sender, type Transport, type Whoami } from '@sendplane/api'
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'

import { createToastApi, provideToastApi, type ToastApi } from '../src/composables/useToast.js'
import { provideSendplane } from '../src/context.js'
import SenderEditPage from '../src/pages/SenderEditPage.vue'
import SenderListPage from '../src/pages/SenderListPage.vue'
import TransportListPage from '../src/pages/TransportListPage.vue'
import { fakeClient, flush } from './helpers.js'

const tenantWhoami: Whoami = {
  principal_id: 'p1',
  tenant_id: 't1',
  system_tenant: false,
  can_switch_tenant: false,
}

const systemWhoami: Whoami = {
  principal_id: 'p1',
  tenant_id: '_system',
  system_tenant: true,
  can_switch_tenant: true,
}

const sharedSender: Sender = {
  id: 'sys:default',
  shared: true,
  name: 'Platform sender',
  from_name: '{{ tenant.name }}',
  from_email: 'sender+{{ tenant.slug }}@mail.example.com',
  uses: ['campaign', 'transactional'],
  version: 1,
}

const ownSender: Sender = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a55',
  name: 'Acme mail',
  from_email: 'hello@acme.test',
  transport_id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a01',
  health: 'green',
  version: 2,
}

const sharedTransport: Transport = {
  id: 'sys:relay',
  shared: true,
  name: 'Platform relay',
  host: 'relay.example.com',
  port: 587,
  status: 'healthy',
  version: 1,
}

const ownTransport: Transport = {
  id: '0191f3d2-9c4e-7a1b-8f00-2b6c1d8e4a01',
  name: 'Own relay',
  host: 'smtp.acme.test',
  port: 587,
  status: 'healthy',
  version: 3,
}

/**
 * Like `withProvider`, plus a real toast API, so the tests can read what a
 * failure actually told the operator instead of only that it failed.
 */
function host(
  page: Component,
  options: { client: ReturnType<typeof fakeClient>; whoami?: Whoami; props?: object },
): { component: Component; toast: ToastApi } {
  const toast = createToastApi()
  const component = defineComponent({
    name: 'ToastHost',
    setup() {
      provideSendplane({
        client: options.client,
        locale: 'en',
        navigate: vi.fn(),
        ...(options.whoami ? { whoami: options.whoami } : {}),
      })
      provideToastApi(toast)
      return () => h(page, options.props)
    },
  })
  return { component, toast }
}

beforeEach(() => {
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true),
  )
})

describe('shared rows in the sender list', () => {
  it('badges a shared sender, lists its uses and offers no delete', async () => {
    const get = vi.fn(async () => ({ items: [sharedSender, ownSender] }))
    const { component } = host(SenderListPage, {
      client: fakeClient({ get }),
      whoami: tenantWhoami,
    })
    const wrapper = mount(component)
    await flush()

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0]!.text()).toContain('Shared')
    expect(rows[0]!.text()).toContain('campaign, transactional')
    expect(rows[0]!.findAll('button')).toHaveLength(0)

    // The tenant's own sender keeps its delete.
    expect(rows[1]!.text()).not.toContain('Shared')
    expect(rows[1]!.findAll('button').map((b) => b.text())).toContain('Delete')
  })

  it('hides "new sender" in the system tenant view', async () => {
    const get = vi.fn(async () => ({ items: [sharedSender] }))
    const { component } = host(SenderListPage, {
      client: fakeClient({ get }),
      whoami: systemWhoami,
    })
    const wrapper = mount(component)
    await flush()

    expect(wrapper.findAll('button').map((b) => b.text())).not.toContain('New sender')
  })

  it('renders a 403 platform_read_only as the sentence, not the error code', async () => {
    const get = vi.fn(async () => ({ items: [ownSender] }))
    const del = vi.fn(async () => {
      throw new SendplaneError({
        message: 'transport sys:relay: read-only',
        code: 'platform_read_only',
        status: 403,
      })
    })
    const { component, toast } = host(SenderListPage, {
      client: fakeClient({ get, del }),
      whoami: tenantWhoami,
    })
    const wrapper = mount(component)
    await flush()

    await wrapper.findAll('tbody tr')[0]!.findAll('button')[0]!.trigger('click')
    await flush()

    expect(toast.toasts.value[0]!.message).toBe(
      'This item is managed by the operator through configuration.',
    )
    expect(toast.toasts.value[0]!.message).not.toContain('platform_read_only')
  })
})

describe('shared rows in the transport list', () => {
  it('badges the platform transport and hides its delete', async () => {
    const get = vi.fn(async () => ({ items: [sharedTransport, ownTransport] }))
    const { component } = host(TransportListPage, {
      client: fakeClient({ get }),
      whoami: systemWhoami,
    })
    const wrapper = mount(component)
    await flush()

    const rows = wrapper.findAll('tbody tr')
    expect(rows[0]!.text()).toContain('Shared')
    expect(rows[0]!.findAll('button')).toHaveLength(0)
    expect(rows[1]!.findAll('button').map((b) => b.text())).toContain('Delete')
  })
})

describe("a tenant's view of a shared sender", () => {
  it('shows the From templates and uses, and nothing to edit or probe', async () => {
    const get = vi.fn(async (path: string) => {
      if (path === '/api/v1/senders/{senderId}') return sharedSender
      if (path === '/api/v1/transports') return { items: [] }
      if (path === '/api/v1/sending-domains') return { items: [] }
      throw new Error(`unexpected GET ${path}`)
    })
    const { component } = host(SenderEditPage, {
      client: fakeClient({ get }),
      whoami: tenantWhoami,
      props: { senderId: 'sys:default' },
    })
    const wrapper = mount(component)
    await flush(6)

    const text = wrapper.text()
    expect(text).toContain('Template')
    expect(text).toContain('sender+{{ tenant.slug }}@mail.example.com')
    expect(text).toContain('campaign, transactional')

    const buttons = wrapper.findAll('button').map((b) => b.text())
    expect(buttons).not.toContain('Save')
    expect(buttons).not.toContain('Run probe now')

    // Health and the probe history are the platform's, never the tenant's.
    expect(text).not.toContain('Probe history')
    expect(get).not.toHaveBeenCalledWith('/api/v1/senders/{senderId}/health', expect.anything())
    // No transport or domain field either: the API omits both for a tenant.
    expect(wrapper.findAll('select')).toHaveLength(0)
  })
})
