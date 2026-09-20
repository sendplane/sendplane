import type { I18nBundle, I18nKeyList, Template } from '@sendplane/api'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { h } from 'vue'

import TemplateEditorPage from '../src/pages/TemplateEditorPage.vue'
import { fakeClient, flush, waitFor, withProvider } from './helpers.js'

const template: Template = {
  id: 't1',
  name: 'Welcome',
  subject: '{% t "welcome.title" %}',
  mode: 'mjml',
  body: '<mjml><mj-body><mj-text>{{ "cta.label" | t }}</mj-text></mj-body></mjml>',
  default_locale: 'en',
  version: 2,
}

const keys: I18nKeyList = {
  default_locale: 'en',
  locales: ['en', 'ko'],
  items: [
    { key: 'welcome.title', used_in: ['subject'], missing_locales: [] },
    { key: 'cta.label', used_in: ['html'], missing_locales: ['ko'] },
  ],
}

const bundle: I18nBundle = {
  default_locale: 'en',
  locales: {
    en: { 'welcome.title': 'Welcome', 'cta.label': 'Open dashboard' },
    ko: { 'welcome.title': '환영합니다' },
  },
}

function build(overrides: Partial<Record<string, unknown>> = {}) {
  const get = vi.fn(async (path: string) => {
    switch (path) {
      case '/api/v1/templates/{templateId}':
        return template
      case '/api/v1/templates/{templateId}/i18n/keys':
        return keys
      case '/api/v1/templates/{templateId}/i18n':
        return bundle
      case '/api/v1/layouts':
        return { items: [{ id: 'l1', name: 'Base', mode: 'mjml', version: 1 }] }
      default:
        throw new Error(`unexpected GET ${path}`)
    }
  })
  const client = fakeClient({ get, ...overrides })
  const wrapper = mount(
    withProvider(TemplateEditorPage, {
      client,
      locale: 'en',
      navigate: vi.fn(),
      props: { templateId: 't1' },
      // The real block editor needs a canvas; the page's `#block-editor` slot
      // is the seam a host uses for its own, and keeps GrapesJS out of the test.
      slots: { 'block-editor': () => h('div', { class: 'host-block-editor' }, 'host editor') },
    }),
  )
  return { client, get, wrapper }
}

function matrixRows(wrapper: ReturnType<typeof mount>) {
  return wrapper.findAll('.sp-i18n__table tbody tr')
}

describe('TemplateEditorPage i18n table', () => {
  it('renders one row per key used by the template, with its locale columns', async () => {
    const { wrapper } = build()
    await flush()

    const headers = wrapper.findAll('.sp-i18n__table thead th').map((th) => th.text())
    expect(headers).toEqual(['Key', 'en', 'ko'])

    const rows = matrixRows(wrapper)
    expect(rows).toHaveLength(2)
    expect(rows[0]!.find('th').text()).toContain('welcome.title')
    expect(rows[0]!.find('th').text()).toContain('subject')
  })

  it('shows a translated value in its own locale without highlighting', async () => {
    const { wrapper } = build()
    await flush()

    const ko = matrixRows(wrapper)[0]!.findAll('td')[1]!
    expect(ko.text()).toBe('환영합니다')
    expect(ko.classes()).not.toContain('is-missing')
    expect(ko.classes()).not.toContain('is-inherited')
  })

  it('marks a value that only arrives through the fallback chain as inherited', async () => {
    const { wrapper } = build()
    await flush()

    // `cta.label` has no `ko` entry, but the bundle default `en` does, so it
    // renders greyed rather than as a publish-blocking hole.
    const ko = matrixRows(wrapper)[1]!.findAll('td')[1]!
    expect(ko.classes()).toContain('is-inherited')
    expect(ko.attributes('title')).toBe('← en')
    expect(wrapper.text()).toContain('Every key is translated.')
  })

  it('highlights a key that nothing in the chain defines and counts it', async () => {
    const sparseKeys: I18nKeyList = {
      locales: ['en', 'ko'],
      items: [{ key: 'cta.label', used_in: ['html'], missing_locales: ['en', 'ko'] }],
    }
    const get = vi.fn(async (path: string) => {
      if (path === '/api/v1/templates/{templateId}') return template
      if (path === '/api/v1/templates/{templateId}/i18n/keys') return sparseKeys
      if (path === '/api/v1/templates/{templateId}/i18n') return { locales: { en: {}, ko: {} } }
      return { items: [] }
    })
    const { wrapper } = build({ get })
    await flush()

    const cells = matrixRows(wrapper)[0]!.findAll('td')
    expect(cells[0]!.classes()).toContain('is-missing')
    expect(cells[1]!.classes()).toContain('is-missing')
    expect(cells[0]!.text()).toBe('missing')
    expect(wrapper.text()).toContain('2 untranslated')
  })
})

describe('TemplateEditorPage i18n YAML', () => {
  it('exports the bundle through the YAML wrapper', async () => {
    const getI18nYaml = vi.fn(async () => 'default_locale: en\n')
    const { wrapper } = build({ getI18nYaml })
    await flush()

    const exportButton = wrapper.findAll('button').find((b) => b.text() === 'Export YAML')!
    await exportButton.trigger('click')
    await flush()

    expect(getI18nYaml).toHaveBeenCalledWith('t1')
  })
})

describe('TemplateEditorPage editor tabs', () => {
  it('offers blocks, MJML and HTML, starting on the template mode', async () => {
    const { wrapper } = build()
    await flush()

    const tabs = wrapper.findAll('[role="tab"]')
    expect(tabs.map((tab) => tab.text())).toEqual(['Blocks', 'MJML', 'HTML'])
    expect(tabs[1]!.attributes('aria-selected')).toBe('true')
  })

  it('confirms before a mode switch hands the body to another editor', async () => {
    const { wrapper } = build()
    await flush()
    const ask = vi.fn(() => false)
    Object.defineProperty(window, 'confirm', { value: ask, configurable: true, writable: true })

    await wrapper.findAll('[role="tab"]')[0]!.trigger('click')
    await flush()
    expect(ask).toHaveBeenCalledTimes(1)
    // Declining leaves the template on MJML: the body has not been re-imported.
    expect(wrapper.findAll('[role="tab"]')[1]!.attributes('aria-selected')).toBe('true')

    ask.mockReturnValue(true)
    await wrapper.findAll('[role="tab"]')[0]!.trigger('click')
    await waitFor(() => wrapper.find('.host-block-editor').exists())

    expect(wrapper.findAll('[role="tab"]')[0]!.attributes('aria-selected')).toBe('true')
    expect(wrapper.text()).toContain('host editor')
  })
})

describe('TemplateEditorPage preview', () => {
  it('renders the server HTML into a sandboxed iframe', async () => {
    const post = vi.fn(async () => ({
      locale: 'ko',
      subject: '환영합니다',
      html: '<p>hi</p>',
      missing_keys: [{ key: 'cta.label', locale: 'ko' }],
      warnings: [],
    }))
    const { wrapper } = build({ post })
    await flush()

    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Render')!
      .trigger('click')
    await flush()

    expect(post).toHaveBeenCalledWith('/api/v1/templates/{templateId}/preview', {
      params: { path: { templateId: 't1' } },
      body: {
        vars: {},
        recipient: { email: 'sample@example.com', name: 'Sample' },
      },
    })

    const frame = wrapper.find('iframe')
    expect(frame.attributes('srcdoc')).toBe('<p>hi</p>')
    // No scripts, no same-origin: rendered mail is untrusted markup.
    expect(frame.attributes('sandbox')).toBe('')
    expect(wrapper.text()).toContain('Rendered as ko')
    expect(wrapper.text()).toContain('cta.label')
  })

  it('reports malformed sample JSON instead of sending it', async () => {
    const { wrapper } = build()
    await flush()

    const varsField = wrapper.findAll('textarea')[1]!
    await varsField.setValue('{ nope')
    await flush()

    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
  })
})
