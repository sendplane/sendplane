import type { SendplaneClient } from '@sendplane/api'
import { vi } from 'vitest'
import { defineComponent, h, type Component } from 'vue'

import {
  provideSendplane,
  type NavigateTarget,
  type ProvideSendplaneOptions,
} from '../src/context.js'

/** A `SendplaneClient` whose methods are spies, for page-level tests. */
export function fakeClient(overrides: Partial<Record<string, unknown>> = {}): SendplaneClient {
  const notStubbed = (name: string) => () => {
    throw new Error(`fakeClient: ${name} was called but not stubbed`)
  }
  return {
    baseUrl: 'https://app.test',
    GET: vi.fn(notStubbed('GET')),
    PUT: vi.fn(notStubbed('PUT')),
    POST: vi.fn(notStubbed('POST')),
    DELETE: vi.fn(notStubbed('DELETE')),
    OPTIONS: vi.fn(),
    HEAD: vi.fn(),
    PATCH: vi.fn(),
    TRACE: vi.fn(),
    request: vi.fn(),
    use: vi.fn(),
    eject: vi.fn(),
    get: vi.fn(notStubbed('get')),
    post: vi.fn(notStubbed('post')),
    put: vi.fn(notStubbed('put')),
    del: vi.fn(notStubbed('del')),
    ingestRecipients: vi.fn(notStubbed('ingestRecipients')),
    getI18nYaml: vi.fn(notStubbed('getI18nYaml')),
    putI18nYaml: vi.fn(notStubbed('putI18nYaml')),
    ...overrides,
  } as unknown as SendplaneClient
}

/**
 * Wraps a component in a host that provides the sendplane context, which is how
 * a real host app mounts a page.
 */
export function withProvider(
  page: Component,
  options: Omit<ProvideSendplaneOptions, 'client'> & {
    client: SendplaneClient
    props?: Record<string, unknown>
    /** Named slots, the way a host fills e.g. `#block-editor`. */
    slots?: Record<string, () => unknown>
  },
): Component {
  const { props, slots, ...contextOptions } = options
  return defineComponent({
    name: 'TestHost',
    setup() {
      provideSendplane(contextOptions)
      return () => h(page, props, slots as never)
    },
  })
}

export const noopNavigate = (_to: NavigateTarget): void => {}

/** Lets pending promise callbacks and Vue's scheduler run. */
export async function flush(times = 3): Promise<void> {
  for (let i = 0; i < times; i++) await Promise.resolve()
  await new Promise((resolve) => setTimeout(resolve, 0))
}

/**
 * Polls until `predicate` holds. Dynamically imported components resolve on a
 * real module load, which takes longer than a microtask flush.
 */
export async function waitFor(predicate: () => boolean, timeoutMs = 4000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error('waitFor: condition never became true')
    await new Promise((resolve) => setTimeout(resolve, 20))
  }
  await flush()
}
