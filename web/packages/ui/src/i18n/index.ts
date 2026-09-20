import { computed, watch, type ComputedRef, type Ref } from 'vue'
import { createI18n, type I18n } from 'vue-i18n'

import en from './en.json'
import ko from './ko.json'

export type MessageTree = { [key: string]: string | MessageTree }
export type LocaleMessages = Record<string, MessageTree>

/** The message bundles shipped with the package. */
export const bundledMessages: LocaleMessages = { en, ko } as LocaleMessages

export interface MessageSource {
  translate: (key: string, named?: Record<string, unknown>) => string
  locales: ComputedRef<string[]>
  /** The vue-i18n instance, should a host want to `app.use()` it. */
  i18n: I18n<
    Record<string, MessageTree>,
    Record<string, unknown>,
    Record<string, unknown>,
    string,
    false
  >
}

/**
 * Builds the translator the context hands to components.
 *
 * The instance is deliberately *not* installed as a Vue plugin: a host app very
 * likely has its own vue-i18n, and installing a second one would fight over the
 * global injection. Components call `t` from the sendplane context instead
 * (ADR-0010).
 */
export function createMessages(locale: Ref<string>, overrides?: LocaleMessages): MessageSource {
  const messages = mergeMessages(bundledMessages, overrides)

  const i18n = createI18n<false>({
    legacy: false,
    globalInjection: false,
    locale: locale.value,
    fallbackLocale: 'en',
    // Missing keys are an authoring bug, not a runtime one: warn in dev, render
    // the key so the gap is visible rather than blank.
    missingWarn: false,
    fallbackWarn: false,
    messages,
  })

  watch(
    locale,
    (next) => {
      i18n.global.locale.value = next
    },
    { immediate: true },
  )

  return {
    translate: (key, named) =>
      named ? i18n.global.t(key, named as Record<string, unknown>) : i18n.global.t(key),
    locales: computed(() => Object.keys(messages)),
    i18n: i18n as MessageSource['i18n'],
  }
}

/** Deep merge, with the host's messages winning over the bundled ones. */
export function mergeMessages(base: LocaleMessages, overrides?: LocaleMessages): LocaleMessages {
  if (!overrides) return { ...base }
  const out: LocaleMessages = { ...base }
  for (const [locale, tree] of Object.entries(overrides)) {
    out[locale] = deepMerge(out[locale] ?? {}, tree)
  }
  return out
}

function deepMerge(base: MessageTree, override: MessageTree): MessageTree {
  const out: MessageTree = { ...base }
  for (const [key, value] of Object.entries(override)) {
    const existing = out[key]
    out[key] =
      typeof value === 'object' &&
      value !== null &&
      typeof existing === 'object' &&
      existing !== null
        ? deepMerge(existing, value)
        : value
  }
  return out
}
