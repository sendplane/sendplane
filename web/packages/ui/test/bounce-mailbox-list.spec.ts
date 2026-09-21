import type { components } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import BounceMailboxListPage from '../src/pages/BounceMailboxListPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

type BounceMailbox = components['schemas']['BounceMailbox']

const mailbox: BounceMailbox = {
  id: 'b1',
  name: 'Bounce inbox',
  host: 'imap.example.com',
  port: 993,
  tls: 'tls',
  username: 'bounce@example.com',
  has_password: true,
  folder: 'INBOX',
  enabled: true,
  version: 1,
  health: { status: 'error', stage: 'auth', reason: 'invalid credentials', consecutive_failures: 3 },
}

function build() {
  const get = vi.fn(async () => ({ items: [mailbox] }))
  const post = vi.fn(async () => ({ ok: false }) as unknown)
  const put = vi.fn(async () => mailbox)
  const client = fakeClient({ get, post, put })
  const wrapper = mount(withProvider(BounceMailboxListPage, { client, locale: 'en', navigate: vi.fn() }))
  return { wrapper, get, post, put, client }
}

function fieldInput(wrapper: VueWrapper, labelText: string) {
  const field = wrapper.findAll('.sp-field').find((f) => f.find('label').text().startsWith(labelText))!
  return field.find('input')
}

function buttonByText(wrapper: VueWrapper, text: string) {
  return wrapper.findAll('button').find((b) => b.text() === text)!
}

describe('BounceMailboxListPage', () => {
  it('lists mailboxes with a red health badge, stage and reason for an error', async () => {
    const { wrapper } = build()
    await flush()

    expect(wrapper.text()).toContain('Bounce inbox')
    expect(wrapper.text()).toContain('Error')
    expect(wrapper.text()).toContain('auth')
    expect(wrapper.text()).toContain('invalid credentials')
  })

  it('posts the entered input to /bounce-mailboxes/test and renders the failure panel', async () => {
    const { wrapper, post } = build()
    await flush()

    post.mockResolvedValueOnce({
      ok: false,
      stage: 'dial',
      error: 'connection refused',
      latency_ms: 8,
      folders: {},
    })

    await buttonByText(wrapper, 'New bounce mailbox').trigger('click')
    await flush()

    await fieldInput(wrapper, 'Host').setValue('imap.example.com')
    await fieldInput(wrapper, 'Username').setValue('bounce@example.com')
    await fieldInput(wrapper, 'Password').setValue('secret')
    await flush()

    await buttonByText(wrapper, 'Test').trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith(
      '/api/v1/bounce-mailboxes/test',
      expect.objectContaining({
        body: expect.objectContaining({ host: 'imap.example.com', username: 'bounce@example.com' }),
      }),
    )
    expect(wrapper.text()).toContain('Connection failed')
    expect(wrapper.text()).toContain('Check the host, port and TLS mode')
    expect(wrapper.text()).toContain('connection refused')
  })

  it('a stored-row test calls {id}/test with no body and refetches the list', async () => {
    const { wrapper, post, get } = build()
    await flush()
    get.mockClear()

    post.mockResolvedValueOnce({ ok: true, stage: 'ok', latency_ms: 12, folders: {} })

    await buttonByText(wrapper, 'Test now').trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/bounce-mailboxes/{mailboxId}/test', {
      params: { path: { mailboxId: 'b1' } },
    })
    expect(get).toHaveBeenCalledTimes(1)
  })
})
