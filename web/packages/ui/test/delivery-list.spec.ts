import type { Delivery, DeliveryList } from '@sendplane/api'
import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import DeliveryListPage from '../src/pages/DeliveryListPage.vue'
import { fakeClient, flush, withProvider } from './helpers.js'

const delivery: Delivery = {
  id: 'd1',
  email: 'a@example.com',
  lane: 'bulk',
  status: 'sent',
  sender_id: 's1',
  version_id: 'v1',
  updated_at: '2026-09-01T10:00:00Z',
}

function build(props: { campaignId?: string } = {}) {
  const listDeliveries = vi.fn(async () => ({ items: [delivery] }) as DeliveryList)
  const client = fakeClient({ listDeliveries })
  const wrapper = mount(
    withProvider(DeliveryListPage, {
      client,
      locale: 'en',
      navigate: vi.fn(),
      props,
    }),
  )
  return { wrapper, listDeliveries, client }
}

/** Finds the checkbox for one option of an `SpMultiFilter` by its legend and label. */
function filterCheckbox(wrapper: VueWrapper, legend: string, optionLabel: string) {
  const fieldset = wrapper
    .findAll('fieldset.sp-filter')
    .find((f) => f.find('.sp-filter__legend').text() === legend)!
  const chip = fieldset.findAll('.sp-filter__chip').find((c) => c.text() === optionLabel)!
  return chip.find('input[type="checkbox"]')
}

describe('DeliveryListPage', () => {
  it('lists every tenant delivery through client.listDeliveries with no campaign filter by default', async () => {
    const { wrapper, listDeliveries } = build()
    await flush()

    expect(listDeliveries).toHaveBeenCalledWith({ limit: 50 }, expect.anything())
    expect(wrapper.text()).toContain('a@example.com')
  })

  it('pre-fills and scopes the query by campaignId when the prop is set', async () => {
    const { listDeliveries } = build({ campaignId: 'c1' })
    await flush()

    expect(listDeliveries).toHaveBeenCalledWith(
      { limit: 50, campaign_id: 'c1' },
      expect.anything(),
    )
  })

  it('shows a breadcrumb back to the campaign only when campaignId is set', async () => {
    const scoped = build({ campaignId: 'c1' })
    await flush()
    expect(scoped.wrapper.find('a').exists()).toBe(true)

    const tenantWide = build()
    await flush()
    expect(tenantWide.wrapper.findAll('a')).toHaveLength(1) // just the email link, no breadcrumb
  })

  it('applies the lane filter', async () => {
    const { wrapper, listDeliveries } = build()
    await flush()
    listDeliveries.mockClear()

    await filterCheckbox(wrapper, 'Lane', 'transactional').setValue(true)
    await flush()

    expect(listDeliveries).toHaveBeenCalledWith(
      { limit: 50, lane: ['transactional'] },
      expect.anything(),
    )
  })

  it('applies the status and error class filters', async () => {
    const { wrapper, listDeliveries } = build()
    await flush()
    listDeliveries.mockClear()

    await filterCheckbox(wrapper, 'Status', 'Failed').setValue(true)
    await flush()
    await filterCheckbox(wrapper, 'Error class', 'Permanent').setValue(true)
    await flush()

    expect(listDeliveries).toHaveBeenLastCalledWith(
      { limit: 50, status: ['failed'], error_class: ['permanent'] },
      expect.anything(),
    )
  })

  it('applies campaign id, email, since and until together on search', async () => {
    const { wrapper, listDeliveries } = build()
    await flush()
    listDeliveries.mockClear()

    const form = wrapper.find('form.sp-toolbar')
    const inputs = form.findAll('input')
    await inputs[0]!.setValue('c9')
    await inputs[1]!.setValue('a@x.com')
    await inputs[2]!.setValue('2026-01-01T00:00')
    await inputs[3]!.setValue('2026-02-01T00:00')
    await form.trigger('submit')
    await flush()

    expect(listDeliveries).toHaveBeenCalledWith(
      {
        limit: 50,
        campaign_id: 'c9',
        email: 'a@x.com',
        since: new Date('2026-01-01T00:00').toISOString(),
        until: new Date('2026-02-01T00:00').toISOString(),
      },
      expect.anything(),
    )
  })

  it('clears every filter and reloads the first page', async () => {
    const { wrapper, listDeliveries } = build()
    await flush()

    await filterCheckbox(wrapper, 'Lane', 'bulk').setValue(true)
    await flush()
    listDeliveries.mockClear()

    const clear = wrapper.findAll('button').find((b) => b.text() === 'Clear')!
    await clear.trigger('click')
    await flush()

    expect(listDeliveries).toHaveBeenCalledWith({ limit: 50 }, expect.anything())
  })
})
