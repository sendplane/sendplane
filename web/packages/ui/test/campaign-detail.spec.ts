import type { Campaign, LinkClickList } from '@sendplane/api'
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import CampaignDetailPage from '../src/pages/CampaignDetailPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const campaign: Campaign = {
  id: 'c1',
  name: 'Spring launch',
  sender_id: 's1',
  version_id: 'v1',
  status: 'running',
  version: 3,
  started_at: '2026-09-01T10:00:00Z',
  stats: {
    by_status: { sent: 1000, failed: 25, bounced: 10, queued: 5 },
    sent: 1000,
    total: 1040,
    unique_opens: 500,
    unique_clicks: 120,
    unsubscribed: 4,
    unsubscribe_clicked: 11,
    computed_at: '2026-09-01T12:00:00Z',
  },
}

const links: LinkClickList = {
  items: [
    { link_no: 0, url: 'https://example.com/a', clicks: 80, unique_clicks: 60 },
    { link_no: 1, url: 'https://example.com/b', clicks: 40, unique_clicks: 30 },
  ],
}

function build(overrides: { get?: unknown; post?: unknown } = {}) {
  const get = vi.fn(async (path: string) => {
    if (path === '/api/v1/campaigns/{campaignId}') return campaign
    if (path === '/api/v1/campaigns/{campaignId}/links') return links
    throw new Error(`unexpected GET ${path}`)
  })
  const post = vi.fn(async () => ({ requeued: 25 }))
  const client = fakeClient({ get: overrides.get ?? get, post: overrides.post ?? post })
  return {
    client,
    get,
    post,
    wrapper: mount(
      withProvider(CampaignDetailPage, {
        client,
        locale: 'en',
        navigate: vi.fn(),
        props: { campaignId: 'c1' },
      }),
    ),
  }
}

beforeEach(() => {
  // `confirm` backs the dialog when no ConfirmDialog host is mounted.
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true),
  )
})

describe('CampaignDetailPage', () => {
  it('shows the campaign name, status and per-status delivery counts', async () => {
    const { wrapper } = build()
    await flush()

    expect(wrapper.text()).toContain('Spring launch')
    expect(wrapper.find('.sp-badge').text()).toBe('Running')

    const stats = wrapper.findAll('.sp-stat')
    const labelled = (label: string) =>
      stats.find((stat) => stat.find('.sp-stat__label').text().startsWith(label))!

    expect(labelled('Sent').find('.sp-stat__value').text()).toBe('1,000')
    expect(labelled('Failed').find('.sp-stat__value').text()).toBe('25')
    expect(labelled('Bounced').find('.sp-stat__value').text()).toBe('10')
  })

  it('shows the total ingest count with its accepted rate, from stats.total/stats.sent', async () => {
    const { wrapper } = build()
    await flush()

    const stats = wrapper.findAll('.sp-stat')
    const labelled = (label: string) =>
      stats.find((stat) => stat.find('.sp-stat__label').text().startsWith(label))!

    expect(labelled('Total').find('.sp-stat__value').text()).toBe('1,040')
    expect(labelled('Total').find('.sp-stat__sub').text()).toBe('96.15% accepted')
  })

  it('shows unique engagement metrics with their rate against sent', async () => {
    const { wrapper } = build()
    await flush()

    const stats = wrapper.findAll('.sp-stat')
    const labelled = (label: string) =>
      stats.find((stat) => stat.find('.sp-stat__label').text().startsWith(label))!

    expect(labelled('Unique opens').find('.sp-stat__value').text()).toBe('500')
    expect(labelled('Unique opens').find('.sp-stat__sub').text()).toBe('50% of sent')
    expect(labelled('Unique clicks').find('.sp-stat__sub').text()).toBe('12% of sent')
    expect(labelled('Unsubscribed').find('.sp-stat__sub').text()).toBe('0.4% of sent')
  })

  it('warns that opens are over-estimated', async () => {
    const { wrapper } = build()
    await flush()
    expect(wrapper.text()).toContain('Opens are over-estimated')

    const stats = wrapper.findAll('.sp-stat')
    const opens = stats.find((stat) => stat.find('.sp-stat__label').text().startsWith('Unique opens'))!
    expect(opens.find('.sp-stat__hint').attributes('title')).toContain('over-estimated')
  })

  it('lists link clicks from the published version', async () => {
    const { wrapper } = build()
    await flush()
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0]!.text()).toContain('https://example.com/a')
    expect(rows[0]!.text()).toContain('80')
    expect(rows[0]!.text()).toContain('60')
  })

  it('offers pause and cancel while running, and not start', async () => {
    const { wrapper } = build()
    await flush()
    const labels = wrapper.findAll('.sp-actions-row button').map((button) => button.text())
    expect(labels).toContain('Pause')
    expect(labels).toContain('Cancel')
    expect(labels).toContain('Retry failed')
    expect(labels).not.toContain('Start')
  })

  it('confirms before pausing and calls the pause operation', async () => {
    const { wrapper, post } = build()
    await flush()

    const pause = wrapper.findAll('.sp-actions-row button').find((b) => b.text() === 'Pause')!
    await pause.trigger('click')
    await flush()

    expect(globalThis.confirm).toHaveBeenCalledWith(expect.stringContaining('Pause sending?'))
    expect(post).toHaveBeenCalledWith('/api/v1/campaigns/{campaignId}/pause', {
      params: { path: { campaignId: 'c1' } },
    })
  })

  it('does not call the API when the operator cancels the confirmation', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => false),
    )
    const { wrapper, post } = build()
    await flush()

    const cancel = wrapper.findAll('.sp-actions-row button').find((b) => b.text() === 'Cancel')!
    await cancel.trigger('click')
    await flush()

    expect(post).not.toHaveBeenCalled()
  })

  it('retries only failed deliveries and reports how many were requeued', async () => {
    const { wrapper, post } = build()
    await flush()

    const retry = wrapper
      .findAll('.sp-actions-row button')
      .find((b) => b.text() === 'Retry failed')!
    await retry.trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/campaigns/{campaignId}/retry', {
      params: { path: { campaignId: 'c1' } },
      body: { status: ['failed'] },
    })
  })

  it('shows a start button and enables the upload panel while the campaign is a draft', async () => {
    const draft = { ...campaign, status: 'draft' as const }
    const get = vi.fn(async (path: string) =>
      path === '/api/v1/campaigns/{campaignId}' ? draft : links,
    )
    const { wrapper } = build({ get })
    await flush()

    const labels = wrapper.findAll('.sp-actions-row button').map((button) => button.text())
    expect(labels).toContain('Start')
    expect(labels).not.toContain('Pause')
    expect(wrapper.find('input[type="file"]').attributes('disabled')).toBeUndefined()
  })

  it('surfaces a load failure with a retry affordance', async () => {
    const get = vi.fn(async () => {
      throw new Error('boom')
    })
    const { wrapper } = build({ get })
    await flush()
    expect(wrapper.find('[role="alert"]').text()).toContain('boom')
  })
})
