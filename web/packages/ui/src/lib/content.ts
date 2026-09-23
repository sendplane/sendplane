/**
 * Helpers for templates and layouts the system tenant shares with every tenant
 * (ADR-0018): the content key, telling a read-only shared row from an editable
 * one, and the template `uses` restriction.
 */
import type { Template } from '@sendplane/api'

/** One use a template may be restricted to (`TemplateUses` in the spec). */
export type TemplateUse = NonNullable<Template['uses']>[number]

export const TEMPLATE_USES: readonly TemplateUse[] = ['campaign', 'transactional']

/** The spec's `ContentKey` pattern, without the empty alternative. */
export const CONTENT_KEY_PATTERN = /^[a-z0-9][a-z0-9._-]{0,63}$/

/** True for an empty key (no key) or one the server will accept. */
export function isValidContentKey(key: string): boolean {
  return key === '' || CONTENT_KEY_PATTERN.test(key)
}

/** The flags a list row or an editor needs; both `Template` and `Layout` fit. */
export interface SharedContent {
  key?: string
  shared?: boolean
  overridden?: boolean
  shared_updated_since_override?: boolean
}

/**
 * A shared template or layout seen from a tenant: readable, previewable, and
 * refused on every write (`403 platform_read_only`). In the system tenant
 * `shared` is only the flag its author set, and the row is its own.
 */
export function isSharedReadOnly(item: SharedContent | undefined, systemTenant: boolean): boolean {
  return item?.shared === true && !systemTenant
}

/**
 * True when the default template-use policy would refuse this template for
 * `use` (`403 template_use_denied`). It only covers *shared* templates: an
 * override carries a copy of `uses` that only a host hook may enforce, so the
 * console does not second-guess it. The server stays the authority.
 */
export function templateUseBlocked(template: Template, use: TemplateUse): boolean {
  const uses = template.uses ?? []
  return template.shared === true && uses.length > 0 && !uses.includes(use)
}

/** What a template picker needs from the translator. */
type Translate = (key: string, named?: Record<string, unknown>) => string

/**
 * One `<option>` of a template picker on a send form: own and shared
 * templates side by side, the shared ones marked, and disabled when the
 * template cannot be used for `use` — or, when the form names the template by
 * key, when it has no key to name it by.
 */
export function templateOption(
  template: Template,
  options: { use: TemplateUse; byKey: boolean; t: Translate; detail?: string },
): { value: string; label: string; disabled: boolean } {
  const { use, byKey, t, detail } = options
  const blocked = templateUseBlocked(template, use)
  const parts = [template.name]
  if (template.key) parts.push(`[${template.key}]`)
  if (detail) parts.push(`— ${detail}`)
  if (template.shared) parts.push(`— ${t('shared.badge')}`)
  if (blocked) parts.push(`(${t('content.onlyFor', { uses: (template.uses ?? []).join(', ') })})`)
  return {
    value: template.id ?? '',
    label: parts.join(' '),
    disabled: blocked || (byKey && !template.key),
  }
}

/**
 * How a send names its template: by key when the form asks for it and the
 * template has one (resolved server-side, the tenant's override first), by ID
 * otherwise. Never both — the server refuses that with a 422.
 */
export function templateReference(
  template: Template | undefined,
  templateId: string,
  byKey: boolean,
): { template_key: string } | { template_id: string } {
  if (byKey && template?.key) return { template_key: template.key }
  return { template_id: templateId }
}
