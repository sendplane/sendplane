import type { Whoami } from '@sendplane/api'
import { routes as manifest } from '@sendplane/ui'
import { mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '../src/App.vue'
import { clearApiKey, setApiKey, setLocale } from '../src/auth.js'
import { router } from '../src/router.js'

/**
 * Answers `/whoami` with the given identity and every other call with an empty
 * page, and records the requests so a test can read the headers that went out.
 */
function stubFetch(whoami?: Whoami) {
  const calls: Request[] = []
  const fetchMock = vi.fn(async (input: Request | string) => {
    const request = typeof input === 'string' ? new Request(input) : input
    calls.push(request)
    if (request.url.includes('/api/v1/whoami')) {
      return new Response(JSON.stringify(whoami ?? {}), {
        status: whoami ? 200 : 404,
        headers: { 'Content-Type': 'application/json' },
      })
    }
    return new Response(JSON.stringify({ items: [] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return calls
}

beforeEach(() => {
  clearApiKey()
  sessionStorage.clear()
  // The locale ref is module state shared by every test in this file.
  setLocale('en')
  stubFetch()
})

function mountApp() {
  return mount(App, { global: { plugins: [router] } })
}

function selectWithOption(wrapper: VueWrapper, text: string) {
  return wrapper
    .findAll('select')
    .find((select) => select.findAll('option').some((option) => option.text().includes(text)))
}

async function settle(times = 6) {
  for (let i = 0; i < times; i++) await new Promise((resolve) => setTimeout(resolve, 20))
}

describe('router built from the ui manifest', () => {
  it('registers every manifest route under the name the pages navigate by', () => {
    const registered = new Set(router.getRoutes().map((route) => route.name))
    for (const route of manifest) expect(registered.has(route.name)).toBe(true)
  })

  it('resolves a parameterised route to the path the manifest declares', () => {
    expect(router.resolve({ name: 'campaign', params: { campaignId: 'c1' } }).path).toBe(
      '/campaigns/c1',
    )
    expect(router.resolve({ name: 'settings' }).path).toBe('/settings')
  })

  it('sends an unknown path and the root to the campaign list', async () => {
    // `resolve` reports the matched record; the redirect only happens on a
    // navigation, so this has to push.
    await router.push('/nope/at/all')
    expect(router.currentRoute.value.name).toBe('campaigns')
    await router.push('/')
    expect(router.currentRoute.value.name).toBe('campaigns')
  })
})

describe('App', () => {
  it('shows the API key gate until a key is entered', async () => {
    const wrapper = mountApp()
    await router.isReady()
    expect(wrapper.text()).toContain('API key')
    expect(wrapper.find('nav').exists()).toBe(false)
  })

  it('renders the nav and the routed page once a key is stored', async () => {
    setApiKey('k-123')
    await router.push({ name: 'campaigns' })
    const wrapper = mountApp()
    await router.isReady()
    await new Promise((resolve) => setTimeout(resolve, 200))

    // Nav labels come from the ui message bundle, proving the provider is wired.
    expect(wrapper.find('nav').text()).toContain('Campaigns')
    expect(wrapper.find('nav').text()).toContain('Settings')
    expect(wrapper.text()).toContain('New campaign')
  })

  it('switches the whole console to Korean', async () => {
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.isReady()
    wrapper.findAll('select')[0]!.setValue('ko')
    await new Promise((resolve) => setTimeout(resolve, 200))
    expect(wrapper.find('nav').text()).toContain('캠페인')
  })
})

describe('whoami and the tenant switcher', () => {
  const switchable: Whoami = {
    principal_id: 'ops@example.com',
    tenant_id: 'acme',
    system_tenant: false,
    can_switch_tenant: true,
  }

  it('offers no switcher when the resolver would not honour the header', async () => {
    stubFetch({ ...switchable, can_switch_tenant: false })
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.isReady()
    await settle()

    expect(selectWithOption(wrapper, 'My tenant')).toBeUndefined()
  })

  it('offers both tenants once whoami says the caller may switch', async () => {
    stubFetch(switchable)
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.isReady()
    await settle()

    const switcher = selectWithOption(wrapper, 'My tenant')!
    const labels = switcher.findAll('option').map((option) => option.text().trim())
    expect(labels[0]).toBe('My tenant (acme)')
    expect(labels[1]).toBe('System tenant (_system)')
  })

  it('sends X-Sendplane-Tenant on every request after the switch, and re-asks whoami', async () => {
    const calls = stubFetch(switchable)
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.push({ name: 'campaigns' })
    await router.isReady()
    await settle()

    expect(calls.every((call) => !call.headers.get('X-Sendplane-Tenant'))).toBe(true)
    const before = calls.length

    stubFetch({ ...switchable, tenant_id: '_system', system_tenant: true })
    await selectWithOption(wrapper, 'My tenant')!.setValue('_system')
    await settle()

    // The switch is stored, so a reload of the tab stays in the operator view.
    expect(sessionStorage.getItem('sendplane.tenant')).toBe('_system')
    // whoami was asked again: whether the header is honoured is the server's call.
    const after = (globalThis.fetch as unknown as { mock: { calls: [Request][] } }).mock.calls.map(
      ([request]) => request,
    )
    expect(after.length).toBeGreaterThan(0)
    expect(after.some((request) => request.url.includes('/api/v1/whoami'))).toBe(true)
    expect(after.every((request) => request.headers.get('X-Sendplane-Tenant') === '_system')).toBe(
      true,
    )
    expect(before).toBeGreaterThan(0)
  })

  it('shows the operator banner and hides the send screen in the system tenant', async () => {
    stubFetch({ ...switchable, tenant_id: '_system', system_tenant: true })
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.push({ name: 'campaigns' })
    await router.isReady()
    await settle()

    expect(wrapper.text()).toContain('Operator (system tenant) view')
    expect(wrapper.find('nav').text()).not.toContain('Send a message')
    // The operator pages are still there.
    expect(wrapper.find('nav').text()).toContain('Transports')
    expect(wrapper.find('nav').text()).toContain('Probe mailboxes')
    // And the campaign list offers no create action.
    expect(wrapper.findAll('button').map((button) => button.text())).not.toContain('New campaign')
  })

  it('keeps the send screen and the create action in a tenant view', async () => {
    stubFetch(switchable)
    setApiKey('k-123')
    const wrapper = mountApp()
    await router.push({ name: 'campaigns' })
    await router.isReady()
    await settle()

    expect(wrapper.text()).not.toContain('Operator (system tenant) view')
    expect(wrapper.find('nav').text()).toContain('Send a message')
    expect(wrapper.findAll('button').map((button) => button.text())).toContain('New campaign')
  })
})
