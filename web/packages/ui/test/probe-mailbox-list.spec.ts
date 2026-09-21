import type { ProbeMailbox } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import ProbeMailboxListPage from '../src/pages/ProbeMailboxListPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const mailbox: ProbeMailbox = {
  id: 'm1',
  name: 'Gmail probe',
  address: 'probe@gmail.example',
  host: 'imap.gmail.example',
  port: 993,
  tls: 'tls',
  username: 'probe@gmail.example',
  has_password: true,
  inbox_folder: 'INBOX',
  enabled: true,
  version: 1,
  health: { status: 'unknown' },
}

function build(overrides: { get?: ReturnType<typeof vi.fn> } = {}) {
  const get = overrides.get ?? vi.fn(async () => ({ items: [mailbox] }))
  const post = vi.fn(async () => ({ ok: false }) as unknown)
  const put = vi.fn(async () => mailbox)
  const client = fakeClient({ get, post, put })
  const wrapper = mount(withProvider(ProbeMailboxListPage, { client, locale: 'en', navigate: vi.fn() }))
  return { wrapper, get, post, put, client }
}

function fieldInput(wrapper: VueWrapper, labelText: string) {
  const field = wrapper.findAll('.sp-field').find((f) => f.find('label').text().startsWith(labelText))!
  return field.find('input')
}

function buttonByText(wrapper: VueWrapper, text: string) {
  return wrapper.findAll('button').find((b) => b.text() === text)!
}

describe('ProbeMailboxListPage', () => {
  it('posts the entered input to /probe-mailboxes/test and renders auth-failure guidance', async () => {
    const { wrapper, post } = build()
    await flush()

    post.mockResolvedValueOnce({
      ok: false,
      stage: 'auth',
      error: 'LOGIN failed: invalid credentials',
      latency_ms: 42,
      folders: {},
    })

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    await fieldInput(wrapper, 'Probe address').setValue('probe@example.com')
    await fieldInput(wrapper, 'IMAP host').setValue('imap.example.com')
    await fieldInput(wrapper, 'Username').setValue('probe@example.com')
    await fieldInput(wrapper, 'Password').setValue('secret')
    await flush()

    await buttonByText(wrapper, 'Test').trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith(
      '/api/v1/probe-mailboxes/test',
      expect.objectContaining({
        body: expect.objectContaining({
          host: 'imap.example.com',
          username: 'probe@example.com',
          password: 'secret',
        }),
      }),
    )

    expect(wrapper.text()).toContain('Connection failed')
    expect(wrapper.text()).toContain('auth')
    expect(wrapper.text()).toContain('Check the account and password')
    expect(wrapper.text()).toContain('LOGIN failed: invalid credentials')
  })

  it('disables the form Test button until host, username and password are all filled', async () => {
    const { wrapper } = build()
    await flush()

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    expect((buttonByText(wrapper, 'Test').element as HTMLButtonElement).disabled).toBe(true)

    await fieldInput(wrapper, 'IMAP host').setValue('imap.example.com')
    await fieldInput(wrapper, 'Username').setValue('probe@example.com')
    await flush()
    expect((buttonByText(wrapper, 'Test').element as HTMLButtonElement).disabled).toBe(true)

    await fieldInput(wrapper, 'Password').setValue('secret')
    await flush()
    expect((buttonByText(wrapper, 'Test').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('a stored-row test without a body calls {id}/test and re-renders the health badge from the refetch', async () => {
    let checked = false
    const get = vi.fn(async () => ({
      items: [
        checked
          ? { ...mailbox, health: { status: 'ok', checked_at: '2026-09-22T10:00:00Z', last_ok_at: '2026-09-22T10:00:00Z' } }
          : mailbox,
      ],
    }))
    const { wrapper, post } = build({ get })
    await flush()

    expect(wrapper.text()).toContain('Not checked')

    post.mockResolvedValueOnce({ ok: true, stage: 'ok', latency_ms: 10, folders: {} })
    checked = true

    await buttonByText(wrapper, 'Test now').trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith(
      '/api/v1/probe-mailboxes/{mailboxId}/test',
      expect.objectContaining({ params: { path: { mailboxId: 'm1' } } }),
    )
    expect((post.mock.calls[0] as unknown[])[1]).not.toHaveProperty('body')
    expect(get).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('OK')
  })

  it('a typed-but-unsaved password sends {password} and does not trigger a refetch', async () => {
    const { wrapper, post, get } = build()
    await flush()
    get.mockClear()

    await buttonByText(wrapper, 'Edit').trigger('click')
    await flush()

    await fieldInput(wrapper, 'Password').setValue('new-candidate-password')
    await flush()

    post.mockResolvedValueOnce({ ok: true, stage: 'ok', latency_ms: 5, folders: {} })

    await buttonByText(wrapper, 'Test').trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/probe-mailboxes/{mailboxId}/test', {
      params: { path: { mailboxId: 'm1' } },
      body: { password: 'new-candidate-password' },
    })
    expect(get).not.toHaveBeenCalled()
  })
})
