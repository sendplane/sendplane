import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

import {
  BLOCK_EDITOR,
  BLOCK_EDITOR_VERSION,
  blockDefinitions,
  extractI18nKeys,
  finishExport,
  GRAPESJS_MJML_VERSION,
  GRAPESJS_VERSION,
  i18nTag,
  isMjmlDocument,
  makeBlockProject,
  readBlockProject,
  replaceI18nKey,
  SP_I18N_CLASS,
  unescapeLiquid,
} from '../src/lib/mjml-blocks.js'

describe('unescapeLiquid', () => {
  it('undoes the escaping GrapesJS applies inside Liquid tags', () => {
    // This is verbatim what GrapesJS exports for `{% if a > b %}`.
    const exported = '<mj-text>{% if a &gt; b %}yes{% endif %}</mj-text>'
    expect(unescapeLiquid(exported)).toBe('<mj-text>{% if a > b %}yes{% endif %}</mj-text>')
  })

  it('handles <, & and the quote forms', () => {
    expect(unescapeLiquid('{% if a &lt; b and c != &quot;x&quot; %}')).toBe(
      '{% if a < b and c != "x" %}',
    )
    expect(unescapeLiquid('{{ x | default: &quot;a&amp;b&quot; }}')).toBe(
      '{{ x | default: "a&b" }}',
    )
    expect(unescapeLiquid('{% t &#39;k&#39; %}')).toBe("{% t 'k' %}")
    expect(unescapeLiquid('{% t &#x27;k&#x27; %}')).toBe("{% t 'k' %}")
  })

  it('leaves escaping outside Liquid alone', () => {
    const mjml = '<mj-text>AT&amp;T &lt;b&gt; {{ recipient.name }}</mj-text>'
    expect(unescapeLiquid(mjml)).toBe(mjml)
  })

  it('unescapes exactly one level, so &amp;gt; stays an entity', () => {
    expect(unescapeLiquid('{% if a &amp;gt; b %}')).toBe('{% if a &gt; b %}')
  })

  it('is idempotent once nothing is escaped', () => {
    const once = unescapeLiquid('{% if a &gt; b %}')
    expect(unescapeLiquid(once)).toBe(once)
  })

  it('does not touch an unterminated delimiter', () => {
    expect(unescapeLiquid('{% if a &gt; b')).toBe('{% if a &gt; b')
  })

  it('preserves the i18n tag quotes GrapesJS already leaves alone', () => {
    const mjml = '<mj-text css-class="sp-i18n">{% t "hero.title" %}</mj-text>'
    expect(finishExport(mjml)).toBe(mjml)
  })
})

describe('extractI18nKeys', () => {
  it('reads both the tag and the filter form', () => {
    const mjml =
      '<mj-text>{% t "welcome.title" %}</mj-text>' +
      "<mj-text>{% t 'cta.label', count: 2 %}</mj-text>" +
      '<mj-text>{{ "footer.legal" | t }}</mj-text>'
    expect(extractI18nKeys(mjml)).toEqual(['cta.label', 'footer.legal', 'welcome.title'])
  })

  it('deduplicates and ignores other tags', () => {
    expect(extractI18nKeys('{% t "a" %}{% t "a" %}{% if x %}{{ y }}')).toEqual(['a'])
  })

  it('survives a scan repeated on the same module instance', () => {
    // Guards the global-regex `lastIndex` trap: the second call must agree.
    const mjml = '{% t "a" %}{% t "b" %}'
    expect(extractI18nKeys(mjml)).toEqual(extractI18nKeys(mjml))
  })
})

describe('i18n tags', () => {
  it('quotes the key', () => {
    expect(i18nTag('hero.title')).toBe('{% t "hero.title" %}')
  })

  it('replaces an existing key in place', () => {
    expect(replaceI18nKey('{% t "old.key" %}', 'new.key')).toBe('{% t "new.key" %}')
  })

  it('replaces non-tag content outright', () => {
    expect(replaceI18nKey('plain text', 'new.key')).toBe('{% t "new.key" %}')
  })

  it('does not carry regex state between calls', () => {
    expect(replaceI18nKey('{% t "a" %}', 'x')).toBe('{% t "x" %}')
    expect(replaceI18nKey('{% t "b" %}', 'y')).toBe('{% t "y" %}')
  })
})

describe('storage envelope', () => {
  it('round-trips through the ADR-0009 shape', () => {
    const project = { pages: [{ frames: [] }] }
    const stored = makeBlockProject(project, '<mjml></mjml>')
    expect(stored).toEqual({
      editor: BLOCK_EDITOR,
      editor_version: BLOCK_EDITOR_VERSION,
      project,
      mjml: '<mjml></mjml>',
    })
    expect(readBlockProject(stored)).toMatchObject({
      project,
      mjml: '<mjml></mjml>',
      foreign: false,
    })
  })

  it('accepts a bare GrapesJS project', () => {
    const project = { pages: [], styles: [] }
    expect(readBlockProject(project)).toMatchObject({ project, mjml: '', foreign: false })
  })

  it('reports a project another editor wrote', () => {
    const read = readBlockProject({ editor: 'email-builder-js', project: { a: 1 }, mjml: '' })
    expect(read.foreign).toBe(true)
    expect(read.editor).toBe('email-builder-js')
  })

  it('treats undefined, null and junk as empty', () => {
    for (const input of [undefined, null, 'x', 42, [], {}]) {
      expect(readBlockProject(input)).toMatchObject({ project: null, foreign: false })
    }
  })
})

describe('blockDefinitions', () => {
  const blocks = blockDefinitions({ t: (key) => key })
  const byId = Object.fromEntries(blocks.map((block) => [block.id, block]))

  it('defines the three sendplane blocks', () => {
    expect(Object.keys(byId).sort()).toEqual([
      'sp-conditional-section',
      'sp-i18n-text',
      'sp-unsubscribe',
    ])
  })

  it('emits an i18n tag the server scanner recognises', () => {
    const content = byId['sp-i18n-text']!.content
    expect(content).toContain(`css-class="${SP_I18N_CLASS}"`)
    expect(extractI18nKeys(content)).toHaveLength(1)
  })

  it('wraps the conditional section in mj-raw, per architecture 6.3', () => {
    const content = byId['sp-conditional-section']!.content
    expect(content.startsWith('<mj-raw>{% if ')).toBe(true)
    expect(content.endsWith('<mj-raw>{% endif %}</mj-raw>')).toBe(true)
    expect(content).toContain('<mj-section')
  })

  it('puts the unsubscribe link in an anchor, not on an mj- tag', () => {
    const content = byId['sp-unsubscribe']!.content
    expect(content).toContain('<a href="{{ unsubscribe_url }}" data-sp-track="off">')
    // MJML's strict validator rejects unknown attributes on mj-* elements, so
    // neither marker may sit on the mj-text itself.
    expect(/<mj-[a-z-]+[^>]*\sdata-/.test(content)).toBe(false)
  })

  it('never puts a data- attribute on an mj- tag in any block', () => {
    for (const block of blocks) {
      expect(/<mj-[a-z-]+[^>]*\sdata-/.test(block.content)).toBe(false)
    }
  })

  it('survives an export round-trip unchanged', () => {
    for (const block of blocks) expect(finishExport(block.content)).toBe(block.content)
  })
})

describe('isMjmlDocument', () => {
  it('recognises a document and rejects plain HTML', () => {
    expect(isMjmlDocument('<mjml><mj-body></mj-body></mjml>')).toBe(true)
    expect(isMjmlDocument('  <MJML >\n')).toBe(true)
    expect(isMjmlDocument('<div>hi</div>')).toBe(false)
    expect(isMjmlDocument('')).toBe(false)
  })
})

describe('pinned editor versions', () => {
  it('matches the versions package.json actually installs', () => {
    // Vitest runs with the package root as its working directory.
    const pkg = JSON.parse(readFileSync(resolve(process.cwd(), 'package.json'), 'utf8')) as {
      dependencies: Record<string, string>
    }
    // The stored `editor_version` is what tells a future reader whether a saved
    // project can still be opened, so it must not drift from the dependency.
    expect(pkg.dependencies.grapesjs).toContain(GRAPESJS_VERSION)
    expect(pkg.dependencies['grapesjs-mjml']).toContain(GRAPESJS_MJML_VERSION)
  })
})
