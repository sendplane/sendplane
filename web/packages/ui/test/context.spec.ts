import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'

import SendplaneProvider from '../src/SendplaneProvider.vue'
import { useSendplane } from '../src/context.js'
import { resolvePath } from '../src/routes.js'
import { fakeClient } from './helpers.js'

const Probe = defineComponent({
  name: 'Probe',
  setup() {
    const { t, client, navigate, href, locale } = useSendplane()
    return () =>
      h('div', [
        h('span', { class: 'label' }, t('campaign.title')),
        h('span', { class: 'locale' }, locale.value),
        h('span', { class: 'base' }, client.baseUrl),
        h(
          'button',
          {
            class: 'go',
            onClick: () => navigate({ name: 'campaign', params: { campaignId: 'c1' } }),
          },
          'go',
        ),
        h('a', { class: 'link', href: href({ name: 'campaigns' }) }, 'campaigns'),
      ])
  },
})

describe('SendplaneProvider / useSendplane', () => {
  it('provides the client, translator and navigation to descendants', () => {
    const navigate = vi.fn()
    const wrapper = mount(SendplaneProvider, {
      props: { client: fakeClient(), navigate, locale: 'en' },
      slots: { default: () => h(Probe) },
    })

    expect(wrapper.find('.label').text()).toBe('Campaigns')
    expect(wrapper.find('.base').text()).toBe('https://app.test')
    expect(wrapper.find('.link').attributes('href')).toBe('/campaigns')

    wrapper.find('.go').trigger('click')
    expect(navigate).toHaveBeenCalledWith({ name: 'campaign', params: { campaignId: 'c1' } })
  })

  it('translates through the bundled ko messages and follows the locale prop', async () => {
    const wrapper = mount(SendplaneProvider, {
      props: { client: fakeClient(), locale: 'ko' },
      slots: { default: () => h(Probe) },
    })
    expect(wrapper.find('.label').text()).toBe('캠페인')

    await wrapper.setProps({ locale: 'en' })
    expect(wrapper.find('.label').text()).toBe('Campaigns')
  })

  it('merges host messages over the bundled ones', () => {
    const wrapper = mount(SendplaneProvider, {
      props: {
        client: fakeClient(),
        locale: 'en',
        messages: { en: { campaign: { title: 'Sends' } } },
      },
      slots: { default: () => h(Probe) },
    })
    // Overridden key wins, and sibling keys from the bundle survive the merge.
    expect(wrapper.find('.label').text()).toBe('Sends')
  })

  it('accepts a host-supplied translator wholesale', () => {
    const wrapper = mount(SendplaneProvider, {
      props: { client: fakeClient(), t: (key: string) => `[[${key}]]` },
      slots: { default: () => h(Probe) },
    })
    expect(wrapper.find('.label').text()).toBe('[[campaign.title]]')
  })

  it('tracks an externally owned locale ref', () => {
    const locale = ref('ko')
    const Host = defineComponent({
      setup() {
        return () =>
          h(SendplaneProvider, { client: fakeClient(), locale: locale.value }, () => h(Probe))
      },
    })
    const wrapper = mount(Host)
    expect(wrapper.find('.locale').text()).toBe('ko')
  })

  it('throws a useful message when no provider is present', () => {
    expect(() => mount(Probe)).toThrow(/SendplaneProvider|provideSendplane/)
  })
})

describe('routes manifest', () => {
  it('fills path parameters and rejects an unknown route', () => {
    expect(resolvePath('campaign', { campaignId: 'a/b' })).toBe('/campaigns/a%2Fb')
    expect(() => resolvePath('nope')).toThrow(/unknown route/)
    expect(() => resolvePath('campaign')).toThrow(/campaignId/)
  })
})
