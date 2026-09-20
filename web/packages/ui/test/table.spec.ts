import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'

import SpTable from '../src/components/SpTable.vue'
import { useCursorList } from '../src/composables/useCursorList.js'
import { provideSendplane } from '../src/context.js'
import { fakeClient } from './helpers.js'
import { flush } from './helpers.js'

interface Row {
  id: string
  name: string
}

const columns = [
  { key: 'name', label: 'Name' },
  { key: 'id', label: 'ID' },
]

function mountTable(props: Record<string, unknown>, slots: Record<string, unknown> = {}) {
  const Host = defineComponent({
    setup() {
      provideSendplane({ client: fakeClient(), locale: 'en' })
      return () => h(SpTable, props as never, slots as never)
    },
  })
  return mount(Host)
}

describe('SpTable', () => {
  const rows: Row[] = [
    { id: 'a', name: 'Alpha' },
    { id: 'b', name: 'Beta' },
  ]

  it('renders a row per item and falls back to the raw field', () => {
    const wrapper = mountTable({ columns, rows, rowKey: (row: Row) => row.id })
    const cells = wrapper.findAll('tbody td')
    expect(wrapper.findAll('tbody tr')).toHaveLength(2)
    expect(cells[0]!.text()).toBe('Alpha')
    expect(cells[1]!.text()).toBe('a')
  })

  it('lets a cell slot override the default rendering', () => {
    const wrapper = mountTable(
      { columns, rows, rowKey: (row: Row) => row.id },
      {
        'cell-name': ({ row }: { row: Row }) => h('b', `!${row.name}`),
      },
    )
    expect(wrapper.find('tbody td b').text()).toBe('!Alpha')
  })

  it('hides the pager until there is another page', () => {
    const wrapper = mountTable({ columns, rows, rowKey: (row: Row) => row.id })
    expect(wrapper.find('nav').exists()).toBe(false)
  })

  it('emits next and previous and disables the ends', async () => {
    const wrapper = mountTable({
      columns,
      rows,
      rowKey: (row: Row) => row.id,
      hasNext: true,
      hasPrevious: false,
      pageNumber: 1,
    })
    const buttons = wrapper.findAll('nav button')
    expect((buttons[0]!.element as HTMLButtonElement).disabled).toBe(true)
    expect((buttons[1]!.element as HTMLButtonElement).disabled).toBe(false)
    expect(wrapper.find('.sp-table__pageno').text()).toBe('Page 1')

    await buttons[1]!.trigger('click')
    expect(wrapper.findComponent(SpTable).emitted('next')).toHaveLength(1)
  })

  it('shows an empty state once loading finishes with no rows', () => {
    const wrapper = mountTable({
      columns,
      rows: [],
      rowKey: (row: Row) => row.id,
      emptyTitle: 'Nothing',
    })
    expect(wrapper.text()).toContain('Nothing')
  })
})

describe('useCursorList', () => {
  it('walks pages forward on next_cursor and replays cursors going back', async () => {
    const pages: Record<string, { items: Row[]; next_cursor?: string }> = {
      first: { items: [{ id: 'a', name: 'Alpha' }], next_cursor: 'c2' },
      c2: { items: [{ id: 'b', name: 'Beta' }] },
    }
    const seen: (string | undefined)[] = []
    const fetchPage = vi.fn(async (params: { limit: number; cursor?: string }) => {
      seen.push(params.cursor)
      return pages[params.cursor ?? 'first']!
    })

    let list!: ReturnType<typeof useCursorList<Row>>
    const Host = defineComponent({
      setup() {
        list = useCursorList<Row>(fetchPage, { limit: 10 })
        return () => h('div')
      },
    })
    mount(Host)
    await flush()

    expect(list.items.value.map((row) => row.id)).toEqual(['a'])
    expect(list.hasNext.value).toBe(true)
    expect(list.hasPrevious.value).toBe(false)
    expect(list.pageNumber.value).toBe(1)

    list.next()
    await flush()
    expect(list.items.value.map((row) => row.id)).toEqual(['b'])
    expect(list.pageNumber.value).toBe(2)
    expect(list.hasNext.value).toBe(false)
    expect(list.hasPrevious.value).toBe(true)

    list.previous()
    await flush()
    expect(list.items.value.map((row) => row.id)).toEqual(['a'])
    expect(seen).toEqual([undefined, 'c2', undefined])
  })

  it('drops every held cursor when a filter changes', async () => {
    const status = ref('')
    const fetchPage = vi.fn(async () => ({ items: [] as Row[], next_cursor: 'c2' }))

    let list!: ReturnType<typeof useCursorList<Row>>
    const Host = defineComponent({
      setup() {
        list = useCursorList<Row>(fetchPage, { watch: status })
        return () => h('div')
      },
    })
    mount(Host)
    await flush()

    list.next()
    await flush()
    expect(list.pageNumber.value).toBe(2)

    status.value = 'failed'
    await flush()
    // A cursor is only meaningful for the query that produced it.
    expect(list.pageNumber.value).toBe(1)
  })
})
