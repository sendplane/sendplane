import type { TenantVars } from '@sendplane/api'
import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'

import TenantVarsEditor from '../src/components/TenantVarsEditor.vue'
import { provideSendplane } from '../src/context.js'
import { renderTenantTemplate } from '../src/lib/platform.js'
import { fakeClient, flush } from './helpers.js'

function build(options: { initial?: TenantVars; tenantId?: string; templates?: object } = {}) {
  const model = ref<TenantVars>(options.initial ?? {})
  const Host = defineComponent({
    name: 'VarsHost',
    setup() {
      provideSendplane({ client: fakeClient(), locale: 'en', navigate: vi.fn() })
      return () =>
        h(TenantVarsEditor, {
          modelValue: model.value,
          'onUpdate:modelValue': (next: TenantVars) => {
            model.value = next
          },
          tenantId: options.tenantId ?? 't1',
          ...(options.templates ? { senderTemplates: options.templates } : {}),
        })
    },
  })
  return { wrapper: mount(Host), model }
}

beforeEach(() => {
  localStorage.clear()
})

describe('TenantVarsEditor', () => {
  it('edits keys as a table and emits the object', async () => {
    const { wrapper, model } = build()
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Add variable')!
      .trigger('click')
    await wrapper.find('input[aria-label="Key"]').setValue('slug')
    await wrapper.find('input[aria-label="Value"]').setValue('acme')
    await flush()

    expect(model.value).toEqual({ slug: 'acme' })
  })

  it('renders the approximate From result for a shared sender', async () => {
    const { wrapper } = build({
      initial: { slug: 'acme', name: 'Acme, Inc.' },
      templates: {
        from_name: '{{ tenant.name }}',
        from_email: 'sender+{{ tenant.slug }}@mail.example.com',
      },
    })
    await flush()

    const text = wrapper.text()
    expect(text).toContain('Rendered result')
    expect(text).toContain('sender+acme@mail.example.com')
    expect(text).toContain('Acme, Inc.')
    // Labelled as an approximation; the server renders the real Liquid.
    expect(text).toContain('Approximate')
  })

  it('names the variables a template still needs', async () => {
    const { wrapper } = build({
      initial: { name: 'Acme, Inc.' },
      templates: { from_email: 'sender+{{ tenant.slug }}@mail.example.com' },
    })
    await flush()
    expect(wrapper.text()).toContain('Not supplied yet: slug')
  })

  it('highlights the keys a tenant_vars_missing named', async () => {
    const model = ref<TenantVars>({})
    const Host = defineComponent({
      setup() {
        provideSendplane({ client: fakeClient(), locale: 'en', navigate: vi.fn() })
        return () => h(TenantVarsEditor, { modelValue: model.value, missingKeys: ['slug', 'plan'] })
      },
    })
    const wrapper = mount(Host)
    await flush()
    expect(wrapper.text()).toContain('The server needs these tenant variables: slug, plan')
  })

  it('remembers the last values per tenant and does not leak them to another', async () => {
    const first = build({ tenantId: 't1' })
    await first.wrapper
      .findAll('button')
      .find((b) => b.text() === 'Add variable')!
      .trigger('click')
    await first.wrapper.find('input[aria-label="Key"]').setValue('slug')
    await first.wrapper.find('input[aria-label="Value"]').setValue('acme')
    await flush()

    const again = build({ tenantId: 't1' })
    await flush()
    expect(again.model.value).toEqual({ slug: 'acme' })

    const other = build({ tenantId: 't2' })
    await flush()
    expect(other.model.value).toEqual({})
  })

  it('switches to JSON and back', async () => {
    const { wrapper, model } = build()
    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Edit as JSON')!
      .trigger('click')
    await wrapper.find('textarea').setValue('{"plan":"pro"}')
    await flush()
    expect(model.value).toEqual({ plan: 'pro' })

    await wrapper
      .findAll('button')
      .find((b) => b.text() === 'Edit as a table')!
      .trigger('click')
    await flush()
    expect(wrapper.find('input[aria-label="Key"]').attributes('value')).toBe('plan')
  })
})

describe('renderTenantTemplate', () => {
  it('substitutes only bare tenant variables and reports what is missing', () => {
    expect(renderTenantTemplate('a+{{ tenant.slug }}@x', { slug: 'acme' })).toEqual({
      text: 'a+acme@x',
      missing: [],
      exact: true,
    })

    const partial = renderTenantTemplate('{{ tenant.name }} <{{ tenant.slug }}>', { slug: 'acme' })
    expect(partial.missing).toEqual(['name'])
    expect(partial.exact).toBe(false)

    // A filter is left alone: this is a hint, not a Liquid implementation.
    const filtered = renderTenantTemplate('{{ tenant.name | upcase }}', { name: 'acme' })
    expect(filtered.text).toBe('{{ tenant.name | upcase }}')
    expect(filtered.exact).toBe(false)
  })
})
