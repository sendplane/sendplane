import type { I18nBundle, I18nKeyList } from '@sendplane/api'

export interface I18nMatrixCell {
  locale: string
  /** The value that would actually be rendered, after the fallback chain. */
  value: string
  /** The value stored against this exact locale tag, if any. */
  own: string
  /** Nothing in the fallback chain defines the key: a publish blocker. */
  missing: boolean
  /** Resolved through the fallback chain rather than this locale's own entry. */
  inherited: boolean
  /** Which locale the value actually came from. */
  from?: string
}

export interface I18nMatrixRow {
  key: string
  usedIn: string[]
  cells: I18nMatrixCell[]
  missingCount: number
}

export interface I18nMatrix {
  locales: string[]
  defaultLocale?: string
  rows: I18nMatrixRow[]
  /** Total (key, locale) pairs with no value anywhere in the chain. */
  missingTotal: number
  /** Keys defined in the bundle that the template does not reference. */
  unusedKeys: string[]
}

/**
 * Builds the key x locale table the template editor renders.
 *
 * The fallback chain is the one sending uses (architecture 6.2): the exact
 * locale tag, then the language alone (`ko-KR` to `ko`), then the bundle's
 * default locale. A cell only counts as *missing* when nothing in that chain
 * has a value, because that is what blocks a publish; a value that arrives
 * through the chain is flagged `inherited` so the operator can still see it is
 * not a real translation.
 */
export function buildI18nMatrix(
  keys: I18nKeyList | undefined,
  bundle: I18nBundle | undefined,
): I18nMatrix {
  const locales = collectLocales(keys, bundle)
  const defaultLocale = bundle?.default_locale ?? keys?.default_locale
  const table = bundle?.locales ?? {}

  const usages = keys?.items ?? []
  const rows: I18nMatrixRow[] = []
  let missingTotal = 0

  for (const usage of usages) {
    const cells: I18nMatrixCell[] = []
    let missingCount = 0
    for (const locale of locales) {
      const own = table[locale]?.[usage.key] ?? ''
      const resolved = own
        ? { value: own, from: locale }
        : resolve(table, locale, usage.key, defaultLocale)
      const missing = resolved === undefined
      if (missing) missingCount++
      cells.push({
        locale,
        own,
        value: resolved?.value ?? '',
        missing,
        inherited: !missing && resolved!.from !== locale,
        from: resolved?.from,
      })
    }
    missingTotal += missingCount
    rows.push({ key: usage.key, usedIn: [...(usage.used_in ?? [])], cells, missingCount })
  }

  return { locales, defaultLocale, rows, missingTotal, unusedKeys: unusedKeys(usages, table) }
}

/** Locale tags to show as columns: everything the server or the bundle knows. */
export function collectLocales(
  keys: I18nKeyList | undefined,
  bundle: I18nBundle | undefined,
): string[] {
  const seen = new Set<string>()
  for (const locale of keys?.locales ?? []) seen.add(locale)
  for (const locale of Object.keys(bundle?.locales ?? {})) seen.add(locale)
  for (const usage of keys?.items ?? []) {
    for (const locale of usage.missing_locales ?? []) seen.add(locale)
  }
  const defaultLocale = bundle?.default_locale ?? keys?.default_locale
  if (defaultLocale) seen.add(defaultLocale)
  // Default locale first, then alphabetical: the reference column belongs left.
  return [...seen].sort((a, b) => {
    if (a === defaultLocale) return -1
    if (b === defaultLocale) return 1
    return a.localeCompare(b)
  })
}

function resolve(
  table: NonNullable<I18nBundle['locales']>,
  locale: string,
  key: string,
  defaultLocale: string | undefined,
): { value: string; from: string } | undefined {
  for (const candidate of fallbackChain(locale, defaultLocale)) {
    const value = table[candidate]?.[key]
    if (value) return { value, from: candidate }
  }
  return undefined
}

/** `ko-KR` resolves through `ko` and then the bundle default. */
export function fallbackChain(locale: string, defaultLocale?: string): string[] {
  const chain = [locale]
  const language = locale.split('-')[0]
  if (language && language !== locale) chain.push(language)
  if (defaultLocale && !chain.includes(defaultLocale)) chain.push(defaultLocale)
  return chain
}

function unusedKeys(
  usages: NonNullable<I18nKeyList['items']>,
  table: NonNullable<I18nBundle['locales']>,
): string[] {
  const used = new Set(usages.map((usage) => usage.key))
  const defined = new Set<string>()
  for (const entries of Object.values(table)) {
    for (const key of Object.keys(entries)) defined.add(key)
  }
  return [...defined].filter((key) => !used.has(key)).sort()
}
