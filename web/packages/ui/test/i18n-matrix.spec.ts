import type { I18nBundle, I18nKeyList } from '@sendplane/api'
import { describe, expect, it } from 'vitest'

import {
  buildI18nMatrix,
  collectLocales,
  fallbackChain,
  i18nSummaryKind,
} from '../src/lib/i18n-matrix.js'

const keys: I18nKeyList = {
  default_locale: 'en',
  locales: ['en', 'ko', 'ko-KR'],
  items: [
    { key: 'welcome.title', used_in: ['subject', 'html'], missing_locales: [] },
    { key: 'cta.label', used_in: ['html'], missing_locales: ['ko'] },
  ],
}

const bundle: I18nBundle = {
  default_locale: 'en',
  locales: {
    en: {
      'welcome.title': 'Welcome, {{ recipient.name }}',
      'cta.label': 'Open dashboard',
      legacy: 'x',
    },
    ko: { 'welcome.title': '{{ recipient.name }}님, 환영합니다' },
    'ko-KR': {},
  },
}

describe('buildI18nMatrix', () => {
  const matrix = buildI18nMatrix(keys, bundle)
  const row = (key: string) => matrix.rows.find((entry) => entry.key === key)!
  const cell = (key: string, locale: string) =>
    row(key).cells.find((entry) => entry.locale === locale)!

  it('puts the default locale first and then sorts the rest', () => {
    expect(matrix.locales).toEqual(['en', 'ko', 'ko-KR'])
  })

  it('marks a key present in the exact locale as neither missing nor inherited', () => {
    expect(cell('welcome.title', 'ko')).toMatchObject({
      missing: false,
      inherited: false,
      from: 'ko',
    })
  })

  it('resolves ko-KR through ko, flagging the value as inherited', () => {
    // An empty map for a tag means "fall back to the language" (spec 6.2), so
    // this is a real translation, not a hole a publish should block on.
    expect(cell('welcome.title', 'ko-KR')).toMatchObject({
      missing: false,
      inherited: true,
      from: 'ko',
      own: '',
    })
  })

  it('counts a key with nothing but the default locale as inherited, not missing', () => {
    expect(cell('cta.label', 'ko')).toMatchObject({ missing: false, inherited: true, from: 'en' })
  })

  it('reports a genuine hole when no locale in the chain has the key', () => {
    const sparse = buildI18nMatrix(
      { ...keys, default_locale: undefined },
      { locales: { en: { 'welcome.title': 'Welcome' }, ko: {} } },
    )
    const ko = sparse.rows
      .find((entry) => entry.key === 'cta.label')!
      .cells.find((entry) => entry.locale === 'ko')!
    expect(ko.missing).toBe(true)
    expect(sparse.missingTotal).toBeGreaterThan(0)
  })

  it('has nothing missing for a fully covered bundle', () => {
    expect(matrix.missingTotal).toBe(0)
  })

  it('lists keys the template no longer references', () => {
    expect(matrix.unusedKeys).toEqual(['legacy'])
  })

  it('survives a template whose keys have not loaded yet', () => {
    const empty = buildI18nMatrix(undefined, undefined)
    expect(empty.rows).toEqual([])
    expect(empty.locales).toEqual([])
    expect(empty.missingTotal).toBe(0)
  })
})

describe('i18nSummaryKind', () => {
  it('is "empty" when there are no keys at all, not "complete"', () => {
    expect(i18nSummaryKind(buildI18nMatrix(undefined, undefined))).toBe('empty')
    expect(i18nSummaryKind(buildI18nMatrix({ items: [] }, { locales: {} }))).toBe('empty')
  })

  it('is "incomplete" for a brand-new template: one key, an empty bundle, one default locale', () => {
    // This is the "false complete" bug: a fresh template's bundle has no
    // locales of its own, but the server (and the client) still resolve a
    // default locale, so there is one column and the key is missing from it.
    const keys: I18nKeyList = { default_locale: 'en', locales: ['en'], items: [{ key: 'hello' }] }
    const bundle: I18nBundle = { default_locale: 'en', locales: {} }
    const matrix = buildI18nMatrix(keys, bundle)
    expect(matrix.missingTotal).toBe(1)
    expect(i18nSummaryKind(matrix)).toBe('incomplete')
  })

  it('is "complete" only once every cell resolves', () => {
    const keys: I18nKeyList = { default_locale: 'en', locales: ['en'], items: [{ key: 'hello' }] }
    const bundle: I18nBundle = { default_locale: 'en', locales: { en: { hello: 'Hello' } } }
    expect(i18nSummaryKind(buildI18nMatrix(keys, bundle))).toBe('complete')
  })
})

describe('fallbackChain / collectLocales', () => {
  it('walks tag, language and default', () => {
    expect(fallbackChain('ko-KR', 'en')).toEqual(['ko-KR', 'ko', 'en'])
    expect(fallbackChain('en', 'en')).toEqual(['en'])
  })

  it('unions the locales the server and the bundle know about', () => {
    expect(
      collectLocales(
        { items: [{ key: 'a', missing_locales: ['fr'] }], locales: ['en'] },
        { default_locale: 'en', locales: { ja: {} } },
      ),
    ).toEqual(['en', 'fr', 'ja'])
  })
})
