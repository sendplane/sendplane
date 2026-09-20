import { routes as manifest } from '@sendplane/ui'
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import App from '../src/App.vue'
import { clearApiKey, setApiKey } from '../src/auth.js'
import { router } from '../src/router.js'

beforeEach(() => {
  clearApiKey()
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify({ items: [] }), { status: 200 })),
  )
})

function mountApp() {
  return mount(App, { global: { plugins: [router] } })
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
