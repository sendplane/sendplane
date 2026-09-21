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

function findField(wrapper: VueWrapper, labelText: string) {
  return wrapper.findAll('.sp-field').find((f) => f.find('label').text().startsWith(labelText))
}

function fieldSelect(wrapper: VueWrapper, labelText: string) {
  return findField(wrapper, labelText)!.find('select')
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

  it('defaults new mailboxes to imap kind with the IMAP fields visible', async () => {
    const { wrapper } = build()
    await flush()

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    expect((fieldSelect(wrapper, 'Kind').element as HTMLSelectElement).value).toBe('imap')
    expect(findField(wrapper, 'IMAP host')).toBeTruthy()
  })

  it('switching to webhook kind hides the IMAP fields and strips them from the create payload', async () => {
    const { wrapper, post } = build()
    await flush()

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    await fieldInput(wrapper, 'Name').setValue('Conduit probe')
    await fieldInput(wrapper, 'Probe address').setValue('probe@conduit.example')
    await fieldSelect(wrapper, 'Kind').setValue('webhook')
    await flush()

    for (const label of [
      'IMAP host',
      'Port',
      'TLS',
      'Username',
      'Password',
      'Inbox folder',
      'Spam folder',
    ]) {
      expect(findField(wrapper, label)).toBeUndefined()
    }
    expect(wrapper.text()).toContain("inbound webhook")

    post.mockResolvedValueOnce({ ...mailbox, kind: 'webhook' })
    await wrapper.find('form').trigger('submit')
    await flush()

    expect(post).toHaveBeenCalledWith(
      '/api/v1/probe-mailboxes',
      expect.objectContaining({
        body: expect.objectContaining({
          kind: 'webhook',
          name: 'Conduit probe',
          address: 'probe@conduit.example',
        }),
      }),
    )
    const sentBody = ((post.mock.calls[0] as unknown[])[1] as { body: Record<string, unknown> }).body
    for (const key of ['host', 'port', 'tls', 'username', 'password', 'inbox_folder', 'spam_folder']) {
      expect(sentBody).not.toHaveProperty(key)
    }
  })

  it('enables the webhook-kind form Test button as soon as only the address is filled', async () => {
    const { wrapper } = build()
    await flush()

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    await fieldSelect(wrapper, 'Kind').setValue('webhook')
    await flush()
    expect((buttonByText(wrapper, 'Test').element as HTMLButtonElement).disabled).toBe(true)

    await fieldInput(wrapper, 'Probe address').setValue('probe@conduit.example')
    await flush()
    expect((buttonByText(wrapper, 'Test').element as HTMLButtonElement).disabled).toBe(false)
  })

  it('shows the inbound webhook provider instead of a capability list', async () => {
    const { wrapper, post } = build()
    await flush()

    await buttonByText(wrapper, 'New probe mailbox').trigger('click')
    await flush()

    await fieldSelect(wrapper, 'Kind').setValue('webhook')
    await fieldInput(wrapper, 'Probe address').setValue('probe@conduit.example')
    await flush()

    post.mockResolvedValueOnce({ ok: true, stage: 'ok', latency_ms: 8, server: 'webhook:sendplane', folders: {} })
    await buttonByText(wrapper, 'Test').trigger('click')
    await flush()

    expect(wrapper.text()).toContain('Inbound webhook provider: sendplane')
    expect(wrapper.text()).not.toContain('Server capabilities')
  })

  it('shows a kind column and keeps "Test now" available for a webhook-kind row', async () => {
    const webhookMailbox: ProbeMailbox = {
      ...mailbox,
      id: 'm2',
      kind: 'webhook',
      host: '',
      port: 0,
      health: { status: 'error', stage: 'webhook', reason: 'no webhook received within timeout' },
    }
    const get = vi.fn(async () => ({ items: [webhookMailbox] }))
    const { wrapper } = build({ get })
    await flush()

    expect(wrapper.text()).toContain('Webhook')
    expect(wrapper.text()).toContain('no webhook received within timeout')
    expect(buttonByText(wrapper, 'Test now')).toBeTruthy()
  })
})
