import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import { defineComponent, h } from 'vue'

import SpStatusBadge from '../src/components/SpStatusBadge.vue'
import { provideSendplane } from '../src/context.js'
import { toneFor } from '../src/lib/status.js'
import { fakeClient } from './helpers.js'

function mountBadge(props: Record<string, unknown>, locale = 'en') {
  const Host = defineComponent({
    setup() {
      provideSendplane({ client: fakeClient(), locale })
      return () => h(SpStatusBadge, props as never)
    },
  })
  return mount(Host)
}

describe('SpStatusBadge', () => {
  it('translates the status and paints the matching tone', () => {
    const wrapper = mountBadge({ kind: 'delivery', value: 'bounced' })
    expect(wrapper.text()).toBe('Bounced')
    expect(wrapper.find('span.sp-badge').classes()).toContain('sp-badge--danger')
  })

  it('translates into the active locale', () => {
    expect(mountBadge({ kind: 'campaign', value: 'running' }, 'ko').text()).toBe('발송 중')
  })

  it('renders an em dash for a missing value', () => {
    const wrapper = mountBadge({ kind: 'health', value: undefined })
    expect(wrapper.text()).toBe('—')
    expect(wrapper.find('span.sp-badge').classes()).toContain('sp-badge--neutral')
  })

  it('shows the raw value when the vocabulary has no message for it', () => {
    // A status the server added but this build does not know about must still
    // be legible rather than showing `status.delivery.quarantined`.
    const wrapper = mountBadge({ kind: 'delivery', value: 'quarantined' })
    expect(wrapper.text()).toBe('quarantined')
  })

  it('surfaces the reason as a tooltip', () => {
    const wrapper = mountBadge({ kind: 'transport', value: 'unhealthy', title: 'auth failed x5' })
    expect(wrapper.find('span.sp-badge').attributes('title')).toBe('auth failed x5')
  })

  it('maps every vocabulary onto a tone', () => {
    expect(toneFor('health', 'green')).toBe('ok')
    expect(toneFor('health', 'yellow')).toBe('warn')
    expect(toneFor('health', 'red')).toBe('danger')
    expect(toneFor('outbox', 'failed')).toBe('danger')
    expect(toneFor('errorClass', 'rate_limited')).toBe('warn')
    expect(toneFor('delivery', undefined)).toBe('neutral')
  })
})
