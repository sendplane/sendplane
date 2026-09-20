import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import SpBlockEditorSlot from '../src/components/SpBlockEditorSlot.vue'
import { makeBlockProject } from '../src/lib/mjml-blocks.js'

/**
 * `MjmlBlockEditor.vue` itself is not mounted here.
 *
 * GrapesJS boots under happy-dom, but happy-dom's `Attr.nodeName` returns `''`
 * (and `Attr.nodeValue` returns `null`) instead of the attribute's name, which
 * is what GrapesJS's HTML parser reads. Every authored attribute — `href`,
 * `css-class`, every MJML style attribute — therefore collapses into a single
 * empty key, and an export assertion in this environment would be testing
 * happy-dom rather than the editor. The behaviour is instead verified against
 * real Chromium; see `docs/block-editor-spike.md`.
 *
 * Re-enable this block once happy-dom implements `Attr.nodeName`, or once the
 * package gains a browser-mode vitest project.
 */
describe.skip('MjmlBlockEditor (needs a real DOM: happy-dom Attr.nodeName is empty)', () => {
  it('exports MJML with Liquid intact', () => {
    expect.fail('not run')
  })
})

describe('SpBlockEditorSlot', () => {
  it('prefers the host-provided editor over the bundled one', () => {
    const wrapper = mount(SpBlockEditorSlot, {
      props: { modelValue: undefined, mjml: '' },
      slots: { default: '<div class="host-editor" />' },
    })
    expect(wrapper.find('.host-editor').exists()).toBe(true)
  })

  it('unwraps the ADR-0009 envelope for the editor and rewraps what comes back', async () => {
    const project = { pages: [{ frames: [] }] }
    const wrapper = mount(SpBlockEditorSlot, {
      props: {
        modelValue: makeBlockProject(project, '<mjml>old</mjml>') as unknown as Record<
          string,
          unknown
        >,
        mjml: '<mjml>old</mjml>',
      },
      // The real editor needs a canvas; a stub is enough to check the adapter.
      global: {
        stubs: {
          MjmlBlockEditor: {
            name: 'MjmlBlockEditor',
            props: ['modelValue', 'locale', 'i18nKeys'],
            emits: ['update:modelValue'],
            template: '<div />',
          },
        },
      },
    })

    const child = wrapper.findComponent({ name: 'MjmlBlockEditor' })
    expect(child.exists()).toBe(true)
    // What the editor hands back after an edit settles.
    const next = { project: { pages: [] }, mjml: '<mjml>new</mjml>' }
    expect(child.props('modelValue')).toEqual({ project, mjml: '<mjml>old</mjml>' })
    child.vm.$emit('update:modelValue', next)
    await wrapper.vm.$nextTick()

    const blocks = wrapper.emitted('update:modelValue')?.[0]?.[0] as Record<string, unknown>
    expect(blocks).toMatchObject({
      editor: 'grapesjs-mjml',
      project: next.project,
      mjml: next.mjml,
    })
    expect(blocks.editor_version).toMatch(/^grapesjs@/)
    expect(wrapper.emitted('update:mjml')?.[0]?.[0]).toBe('<mjml>new</mjml>')
  })
})
