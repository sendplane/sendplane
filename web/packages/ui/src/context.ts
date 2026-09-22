import { SYSTEM_TENANT_ID, type SendplaneClient, type Whoami } from '@sendplane/api'
import { computed, inject, provide, ref, type ComputedRef, type InjectionKey, type Ref } from 'vue'

import { createMessages, type LocaleMessages } from './i18n/index.js'
import { resolvePath } from './routes.js'

/**
 * Where a page wants the host to go. Names come from the `routes.ts` manifest,
 * never from a path, so the host is free to mount the console anywhere.
 */
export interface NavigateTarget {
  name: string
  params?: Record<string, string | number>
  query?: Record<string, string | number | undefined>
}

export type NavigateFn = (to: NavigateTarget) => void

export type TranslateFn = (key: string, named?: Record<string, unknown>) => string

export interface SendplaneContext {
  /** The typed REST client every page fetches through. */
  client: SendplaneClient
  /** Route-name navigation. The host owns the router; this package has none. */
  navigate: NavigateFn
  /**
   * An `href` for the same target, so links are real anchors (middle-click,
   * open-in-new-tab, screen-reader link semantics) rather than click handlers.
   */
  href: (to: NavigateTarget) => string
  t: TranslateFn
  locale: Ref<string>
  /** Locales the merged message bundle covers. */
  availableLocales: Ref<string[]>
  /**
   * `GET /api/v1/whoami`, as the host loaded it: who the caller is and which
   * tenant this view is bound to. The host owns the call (it also owns the
   * credential and the tenant header), so this is `undefined` until it lands
   * and pages must render without it.
   */
  whoami: Ref<Whoami | undefined>
  /**
   * True in the operator's system-tenant view, the only one that sees platform
   * resources and their state — and the one that cannot send (ADR-0017).
   */
  systemTenant: ComputedRef<boolean>
  /** The tenant this view is bound to, or `''` before `whoami` lands. */
  tenantId: ComputedRef<string>
}

export const SENDPLANE_KEY: InjectionKey<SendplaneContext> = Symbol('sendplane')

export interface ProvideSendplaneOptions {
  client: SendplaneClient
  navigate?: NavigateFn
  /**
   * Builds an href for a target. Defaults to the manifest path with a `#`
   * prefix stripped, which suits a history-mode router mounted at the root.
   */
  href?: (to: NavigateTarget) => string
  /** Initial UI locale; `en` and `ko` ship with the package. */
  locale?: string | Ref<string>
  /** Extra or replacement vue-i18n messages, merged over the bundled ones. */
  messages?: LocaleMessages
  /** Replaces the bundled translator wholesale, e.g. with the host's own. */
  t?: TranslateFn
  /**
   * The result of `GET /api/v1/whoami`. Pass a ref to keep it live across a
   * tenant switch; this package never fetches it itself, because the host is
   * the side that knows when the credential is ready.
   */
  whoami?: Whoami | Ref<Whoami | undefined>
}

/**
 * Installs the sendplane context on the current component instance. Call it in
 * a host component's `setup()` when `<SendplaneProvider>` does not fit, for
 * example because the host wants the provider to be its own layout root.
 */
export function provideSendplane(options: ProvideSendplaneOptions): SendplaneContext {
  const context = createSendplaneContext(options)
  provide(SENDPLANE_KEY, context)
  return context
}

/** Builds a context without providing it, for tests and manual wiring. */
export function createSendplaneContext(options: ProvideSendplaneOptions): SendplaneContext {
  const locale = isRef(options.locale)
    ? options.locale
    : ref(options.locale ?? defaultLocale(options.messages))

  const whoami: Ref<Whoami | undefined> = isWhoamiRef(options.whoami)
    ? options.whoami
    : ref(options.whoami)

  const composer = createMessages(locale, options.messages)
  const navigate =
    options.navigate ??
    ((to: NavigateTarget) => {
      // Without a host router the best available fallback is a full page load.
      if (typeof window !== 'undefined') window.location.assign(hrefFor(to))
    })

  return {
    client: options.client,
    navigate,
    href: options.href ?? hrefFor,
    t: options.t ?? ((key, named) => composer.translate(key, named)),
    locale,
    availableLocales: computed(() => composer.locales.value),
    whoami,
    // `system_tenant` is the server's answer, not a string comparison the
    // console makes up; the id is only the fallback for a host that filled the
    // ref by hand.
    systemTenant: computed(
      () => whoami.value?.system_tenant === true || whoami.value?.tenant_id === SYSTEM_TENANT_ID,
    ),
    tenantId: computed(() => whoami.value?.tenant_id ?? ''),
  }
}

export function useSendplane(): SendplaneContext {
  const context = inject(SENDPLANE_KEY, null)
  if (!context) {
    throw new Error(
      '[@sendplane/ui] no sendplane context: wrap the page in <SendplaneProvider> or call provideSendplane() in an ancestor.',
    )
  }
  return context
}

/** Reads the context without throwing, for components that can do without it. */
export function useSendplaneOptional(): SendplaneContext | null {
  return inject(SENDPLANE_KEY, null)
}

function hrefFor(to: NavigateTarget): string {
  const path = resolvePath(to.name, to.params)
  const entries = Object.entries(to.query ?? {}).filter(([, value]) => value !== undefined)
  if (entries.length === 0) return path
  const query = new URLSearchParams(entries.map(([key, value]) => [key, String(value)]))
  return `${path}?${query.toString()}`
}

function defaultLocale(messages?: LocaleMessages): string {
  if (typeof navigator === 'undefined') return 'en'
  const preferred = navigator.language?.toLowerCase() ?? 'en'
  const available = new Set(['en', 'ko', ...Object.keys(messages ?? {})])
  if (available.has(preferred)) return preferred
  const language = preferred.split('-')[0] ?? 'en'
  return available.has(language) ? language : 'en'
}

function isRef(value: unknown): value is Ref<string> {
  return typeof value === 'object' && value !== null && 'value' in value
}

function isWhoamiRef(value: unknown): value is Ref<Whoami | undefined> {
  return typeof value === 'object' && value !== null && 'value' in value
}
