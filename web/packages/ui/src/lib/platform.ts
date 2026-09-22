/**
 * Helpers for the platform-resource layer (ADR-0017): telling a shared entity's
 * errors apart, and approximating what a shared sender's From templates will
 * render to.
 */
import { isSendplaneError, type ApiErrorCode } from '@sendplane/api'

function codeOf(error: unknown): ApiErrorCode | string | undefined {
  return isSendplaneError(error) ? error.code : undefined
}

/** 403: the object is configuration, so the only way to change it is a deploy. */
export function isPlatformReadOnly(error: unknown): boolean {
  return codeOf(error) === 'platform_read_only'
}

/** 403: this sender's `uses` does not cover the kind of send that was tried. */
export function isSenderUseDenied(error: unknown): boolean {
  return codeOf(error) === 'sender_use_denied'
}

/** 422: a shared sender's From template reads a `tenant_vars` key that was not supplied. */
export function isTenantVarsMissing(error: unknown): boolean {
  return codeOf(error) === 'tenant_vars_missing'
}

export function isFromDomainNotOwned(error: unknown): boolean {
  return codeOf(error) === 'from_domain_not_owned'
}

export function isTransportNotAssignable(error: unknown): boolean {
  return codeOf(error) === 'transport_not_assignable'
}

/**
 * The keys a `tenant_vars_missing` names, taken from `details[].field`, which
 * the spec fixes as `tenant_vars.<key>`. A server that reports the failure
 * without details yields an empty list, and callers fall back to the message.
 */
export function missingTenantVarKeys(error: unknown): string[] {
  if (!isSendplaneError(error)) return []
  const keys: string[] = []
  for (const detail of error.details) {
    const field = detail.field ?? ''
    if (field === 'tenant_vars') continue
    if (field.startsWith('tenant_vars.')) keys.push(field.slice('tenant_vars.'.length))
  }
  return [...new Set(keys)]
}

// `{{ tenant.a.b }}`, with optional Liquid whitespace-control dashes. Anything
// beyond a bare variable — a filter, a tag, a condition — is deliberately not
// matched: this is a hint, not a second Liquid implementation.
const TENANT_VAR = /\{\{-?\s*tenant\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\s*-?\}\}/g

export interface TenantTemplateRender {
  text: string
  /** Variables the template reads that the supplied vars do not cover. */
  missing: string[]
  /** False when the template contains Liquid this substitution cannot model. */
  exact: boolean
}

/**
 * Substitutes `{{ tenant.key }}` in a shared sender's `from_name` / `from_email`
 * so the operator sees roughly what will go out.
 *
 * It is explicitly *approximate*: the server renders the real Liquid at send
 * time and answers `422 tenant_vars_missing` if a variable is not supplied, so
 * nothing here is a validation. Filters and tags are left untouched and flagged
 * through `exact: false`.
 */
export function renderTenantTemplate(
  template: string | undefined,
  vars: Record<string, unknown>,
): TenantTemplateRender {
  const source = template ?? ''
  const missing: string[] = []
  const text = source.replace(TENANT_VAR, (whole, path: string) => {
    const value = lookup(vars, path)
    if (value === undefined || value === null || value === '') {
      missing.push(path)
      return whole
    }
    return String(value)
  })
  // Whatever braces survive are Liquid this function does not understand.
  return { text, missing: [...new Set(missing)], exact: !/\{\{|\{%/.test(text) }
}

/** The `tenant.*` variables a template reads, for showing what a sender needs. */
export function extractTenantVarKeys(...templates: (string | undefined)[]): string[] {
  const keys: string[] = []
  for (const template of templates) {
    for (const match of (template ?? '').matchAll(TENANT_VAR)) keys.push(match[1]!)
  }
  return [...new Set(keys)]
}

/** True when the value looks like a Liquid template rather than a literal. */
export function looksTemplated(value: string | undefined): boolean {
  return /\{\{|\{%/.test(value ?? '')
}

function lookup(vars: Record<string, unknown>, path: string): unknown {
  let current: unknown = vars
  for (const part of path.split('.')) {
    if (typeof current !== 'object' || current === null) return undefined
    current = (current as Record<string, unknown>)[part]
  }
  return current
}
