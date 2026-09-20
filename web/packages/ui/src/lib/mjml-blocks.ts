/*
 * Pure helpers behind `MjmlBlockEditor.vue`.
 *
 * They are deliberately free of GrapesJS imports so that the storage envelope,
 * the Liquid-preserving export fixup and the custom block definitions can be
 * tested without a canvas (see `test/mjml-blocks.spec.ts`), and so that nothing
 * here drags the editor bundle into the main chunk.
 */

/** Editor id stored with a blocks-mode project (ADR-0009). */
export const BLOCK_EDITOR = 'grapesjs-mjml'

/**
 * Editor version stored with the project. Both packages matter for whether a
 * saved project can be reopened, so both are pinned here; the test suite
 * asserts these strings against `package.json` so they cannot silently drift.
 */
export const GRAPESJS_VERSION = '0.23.6'
export const GRAPESJS_MJML_VERSION = '1.0.8'
export const BLOCK_EDITOR_VERSION = `grapesjs@${GRAPESJS_VERSION}+grapesjs-mjml@${GRAPESJS_MJML_VERSION}`

/**
 * What the template editor persists to `Template.blocks` (ADR-0009). The server
 * only ever compiles `Template.body`; `project` exists so editing can resume,
 * and `mjml` is kept alongside it so a reader can tell which MJML this project
 * produced without booting the editor.
 */
export interface BlockEditorProject {
  editor: string
  editor_version: string
  project: unknown
  mjml: string
}

/** The editor's `v-model` payload: the project blob plus its exported MJML. */
export interface BlockEditorValue {
  project: unknown | null
  mjml: string
}

/** Seed document for a template that has no body yet. */
export const DEFAULT_MJML = [
  '<mjml>',
  '  <mj-body>',
  '    <mj-section>',
  '      <mj-column>',
  '        <mj-text>Drag a block here.</mj-text>',
  '      </mj-column>',
  '    </mj-section>',
  '  </mj-body>',
  '</mjml>',
].join('\n')

/** `true` when the string looks like an MJML document the editor can import. */
export function isMjmlDocument(source: string): boolean {
  return /<mjml[\s>]/i.test(source)
}

// --- Liquid-preserving export ----------------------------------------------

// A Liquid object (`{{ … }}`) or tag (`{% … %}`). Non-greedy so the first
// closing delimiter wins, which is also how the Go scanner reads them.
const LIQUID_SPAN = /\{\{[\s\S]*?\}\}|\{%[\s\S]*?%\}/g

const NAMED_ENTITY = /&(?:amp|lt|gt|quot|apos|nbsp|#0*(?:34|39)|#[xX]0*(?:22|27));/g

const ENTITY_VALUES: Record<string, string> = {
  '&amp;': '&',
  '&lt;': '<',
  '&gt;': '>',
  '&quot;': '"',
  '&apos;': "'",
  '&nbsp;': ' ',
  '&#34;': '"',
  '&#39;': "'",
  '&#x22;': '"',
  '&#x27;': "'",
}

function decodeEntity(entity: string): string {
  const direct = ENTITY_VALUES[entity.toLowerCase()]
  if (direct !== undefined) return direct
  // Strip leading zeros from the numeric forms (`&#039;`) and retry.
  const numeric = entity.toLowerCase().replace(/^&#(x?)0*/, '&#$1')
  return ENTITY_VALUES[numeric] ?? entity
}

/**
 * Undoes the HTML escaping GrapesJS applies to text and attribute values, but
 * only *inside* Liquid delimiters.
 *
 * GrapesJS parses the document into a DOM and serialises it back, so a text
 * node containing `{% if a > b %}` comes out as `{% if a &gt; b %}` and
 * `{{ x | default: "a&b" }}` comes out with `&amp;`. MJML passes both through
 * verbatim, so the entity would survive all the way into the HTML template and
 * Liquid would fail to parse the tag at send time.
 *
 * Only the spans between `{{`/`}}` and `{%`/`%}` are touched, so markup the
 * operator deliberately escaped in ordinary copy (`AT&T`) stays escaped. One
 * pass only, which is the exact inverse of one escaping pass: a literal
 * `&amp;gt;` inside a tag becomes `&gt;`, not `>`.
 */
export function unescapeLiquid(mjml: string): string {
  return mjml.replace(LIQUID_SPAN, (span) => span.replace(NAMED_ENTITY, decodeEntity))
}

/** Everything the editor does to its raw export before it leaves the component. */
export function finishExport(mjml: string): string {
  return unescapeLiquid(mjml).trim()
}

// --- i18n keys --------------------------------------------------------------

// `{% t "key" %}` / `{% t 'key', count: 2 %}`.
const T_TAG = /\{%-?\s*t\s+(["'])([^"']+)\1/g
// `{{ "key" | t }}` — the filter form the Go scanner also accepts.
const T_FILTER = /\{\{-?\s*(["'])([^"']+)\1\s*\|\s*t\b/g

/**
 * Best-effort list of i18n keys a document references, used to flag keys the
 * bundle does not define yet. The server's scanner is authoritative — this one
 * exists so the editor can warn before a save round-trip.
 */
export function extractI18nKeys(source: string): string[] {
  const keys = new Set<string>()
  for (const pattern of [T_TAG, T_FILTER]) {
    pattern.lastIndex = 0
    let match = pattern.exec(source)
    while (match) {
      if (match[2]) keys.add(match[2])
      match = pattern.exec(source)
    }
  }
  return [...keys].sort()
}

/** Renders an i18n key as the Liquid tag the block editor inserts. */
export function i18nTag(key: string): string {
  return `{% t ${JSON.stringify(key || 'key')} %}`
}

// --- storage envelope -------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** Wraps a project and its export in the ADR-0009 envelope. */
export function makeBlockProject(project: unknown, mjml: string): BlockEditorProject {
  return { editor: BLOCK_EDITOR, editor_version: BLOCK_EDITOR_VERSION, project, mjml }
}

/**
 * Reads whatever is stored in `Template.blocks`.
 *
 * Tolerates three shapes: the envelope above, a bare GrapesJS project (which is
 * what an older draft or a hand-written record may hold), and anything else —
 * including a project written by a *different* editor, which is reported so the
 * caller can refuse to overwrite it.
 */
export function readBlockProject(blocks: unknown): {
  project: unknown | null
  mjml: string
  editor?: string
  editorVersion?: string
  /** True when `blocks` came from an editor this component does not own. */
  foreign: boolean
} {
  if (!isRecord(blocks)) return { project: null, mjml: '', foreign: false }

  if (typeof blocks.editor === 'string') {
    return {
      project: blocks.project ?? null,
      mjml: typeof blocks.mjml === 'string' ? blocks.mjml : '',
      editor: blocks.editor,
      editorVersion: typeof blocks.editor_version === 'string' ? blocks.editor_version : undefined,
      foreign: blocks.editor !== BLOCK_EDITOR,
    }
  }

  // A bare GrapesJS project: it always carries `pages`.
  if ('pages' in blocks) return { project: blocks, mjml: '', foreign: false }

  return { project: null, mjml: '', foreign: false }
}

// --- custom blocks ----------------------------------------------------------

/** Marker class the editor uses to recognise its own blocks after a reload. */
export const SP_I18N_CLASS = 'sp-i18n'
export const SP_CONDITION_CLASS = 'sp-condition'
export const SP_UNSUBSCRIBE_CLASS = 'sp-unsubscribe'

export interface BlockDefinition {
  id: string
  label: string
  /** MJML fragment GrapesJS drops into the canvas. */
  content: string
  /** Where the fragment may be dropped; GrapesJS matches it against parents. */
  category: string
  media?: string
}

export interface BlockDefinitionOptions {
  /** Labels, so the palette speaks the console's locale. */
  t: (key: string) => string
  /** Key the "i18n text" block starts with. */
  sampleKey?: string
  /** Condition the "conditional section" block starts with. */
  sampleCondition?: string
}

/**
 * The three sendplane-specific blocks.
 *
 * None of them uses a `data-*` attribute on an MJML tag: MJML's strict
 * validator — which `internal/render.compileMJML` runs — rejects unknown
 * attributes on `mj-*` elements outright. The markers therefore ride on
 * `css-class`, which is a legal MJML attribute, and `data-sp-track` rides on a
 * plain `<a>` inside `mj-text`, whose children MJML passes through unvalidated.
 */
export function blockDefinitions(options: BlockDefinitionOptions): BlockDefinition[] {
  const { t, sampleKey = 'body.text', sampleCondition = 'recipient.vars.pro' } = options
  return [
    {
      id: 'sp-i18n-text',
      label: t('blockEditor.blocks.i18nText'),
      category: 'sendplane',
      content: `<mj-text css-class="${SP_I18N_CLASS}">${i18nTag(sampleKey)}</mj-text>`,
    },
    {
      id: 'sp-conditional-section',
      label: t('blockEditor.blocks.conditional'),
      category: 'sendplane',
      content:
        `<mj-raw>{% if ${sampleCondition} %}</mj-raw>` +
        `<mj-section css-class="${SP_CONDITION_CLASS}"><mj-column>` +
        `<mj-text>${t('blockEditor.blocks.conditionalBody')}</mj-text>` +
        `</mj-column></mj-section>` +
        `<mj-raw>{% endif %}</mj-raw>`,
    },
    {
      id: 'sp-unsubscribe',
      label: t('blockEditor.blocks.unsubscribe'),
      category: 'sendplane',
      content:
        `<mj-text css-class="${SP_UNSUBSCRIBE_CLASS}" align="center" font-size="12px">` +
        `<a href="{{ unsubscribe_url }}" data-sp-track="off">` +
        `${t('blockEditor.blocks.unsubscribeLabel')}</a></mj-text>`,
    },
  ]
}

// Non-global twin of `T_TAG`: `replaceI18nKey` must not inherit `lastIndex`.
const T_TAG_ONCE = /\{%-?\s*t\s+["'][^"']+["'][^%]*%\}/

/**
 * Points an existing i18n block at another key. Anything that is not already a
 * `{% t … %}` tag is replaced outright rather than appended to, because the
 * block owns its whole text.
 */
export function replaceI18nKey(content: string, key: string): string {
  const tag = i18nTag(key)
  return T_TAG_ONCE.test(content) ? content.replace(T_TAG_ONCE, tag) : tag
}
