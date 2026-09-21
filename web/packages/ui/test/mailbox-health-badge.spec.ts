import type { components } from '@sendplane/api'
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { defineComponent, h } from 'vue'

import MailboxHealthBadge from '../src/components/MailboxHealthBadge.vue'
import { provideSendplane } from '../src/context.js'
import { fakeClient } from './helpers.js'

type MailboxHealth = components['schemas']['MailboxHealth']

function mountBadge(health: MailboxHealth | undefined, locale = 'en') {
  const Host = defineComponent({
    setup() {
      provideSendplane({ client: fakeClient(), locale })
      return () => h(MailboxHealthBadge, { health } as never)
    },
  })
  return mount(Host)
}

describe('MailboxHealthBadge', () => {
  it('renders neutral "not checked" for an unknown mailbox', () => {
    const wrapper = mountBadge({ status: 'unknown' })
    expect(wrapper.find('.sp-badge').classes()).toContain('sp-badge--neutral')
    expect(wrapper.text()).toContain('Not checked')
  })

  it('renders a green badge for ok', () => {
    const wrapper = mountBadge({ status: 'ok' })
    expect(wrapper.find('.sp-badge').classes()).toContain('sp-badge--ok')
    expect(wrapper.text()).toContain('OK')
  })

  it('renders a red badge with the stage and reason for an error', () => {
    const wrapper = mountBadge({
      status: 'error',
      stage: 'auth',
      reason: 'invalid credentials',
      consecutive_failures: 2,
    })
    expect(wrapper.find('.sp-badge').classes()).toContain('sp-badge--danger')
    expect(wrapper.text()).toContain('Error')
    expect(wrapper.text()).toContain('auth')
    expect(wrapper.text()).toContain('invalid credentials')
    expect(wrapper.find('.sp-badge').attributes('title')).toBe('auth: invalid credentials')
  })

  it('renders the webhook stage and reason for a webhook-kind mailbox that timed out', () => {
    const wrapper = mountBadge({
      status: 'error',
      stage: 'webhook',
      reason: 'no webhook received within timeout',
    })
    expect(wrapper.find('.sp-badge').classes()).toContain('sp-badge--danger')
    expect(wrapper.text()).toContain('webhook')
    expect(wrapper.text()).toContain('no webhook received within timeout')
  })

  it('renders an em dash badge when there is no health yet', () => {
    const wrapper = mountBadge(undefined)
    expect(wrapper.find('.sp-badge').classes()).toContain('sp-badge--neutral')
    expect(wrapper.find('.sp-badge').text()).toBe('—')
  })

  it('translates into Korean', () => {
    const wrapper = mountBadge({ status: 'error', stage: 'auth' }, 'ko')
    expect(wrapper.text()).toContain('오류')
  })
})
