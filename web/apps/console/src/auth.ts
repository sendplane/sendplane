import { SYSTEM_TENANT_ID, TENANT_HEADER } from '@sendplane/api'
import { ref, type Ref } from 'vue'

const STORAGE_KEY = 'sendplane.apiKey'
const TENANT_KEY = 'sendplane.tenant'
const OWN_TENANT_KEY = 'sendplane.ownTenant'
const THEME_KEY = 'sendplane.theme'
const LOCALE_KEY = 'sendplane.locale'

/**
 * The API key lives in `sessionStorage`, not `localStorage`: it is a bearer
 * credential, and scoping it to the tab means closing the tab logs out.
 */
export const apiKey: Ref<string> = ref(read(sessionStorage, STORAGE_KEY))

export function setApiKey(value: string): void {
  apiKey.value = value.trim()
  write(sessionStorage, STORAGE_KEY, apiKey.value)
}

export function clearApiKey(): void {
  apiKey.value = ''
  remove(sessionStorage, STORAGE_KEY)
  // Signing out must not leave the next sign-in looking at the operator view.
  setTenant('')
}

export { SYSTEM_TENANT_ID }

/**
 * The tenant this tab asks for, or `''` for "whatever the principal maps to".
 *
 * Normally the console sends nothing and the host's `TenantResolver` derives
 * the tenant from the principal. An operator whose resolver honours the header
 * (`Whoami.can_switch_tenant`) can point the whole console at the system tenant
 * instead; that is a property of this tab, so it lives beside the credential
 * rather than in `localStorage`.
 */
export const tenantOverride: Ref<string> = ref(read(sessionStorage, TENANT_KEY))

export function setTenant(value: string): void {
  tenantOverride.value = value
  if (value) write(sessionStorage, TENANT_KEY, value)
  else remove(sessionStorage, TENANT_KEY)
}

/**
 * The principal's own tenant, remembered from the first `whoami` that was not
 * the system tenant: while the console is pointed at `_system`, `whoami` no
 * longer reports it, and the switcher still has to be able to label the way
 * back.
 */
export const ownTenantId: Ref<string> = ref(read(sessionStorage, OWN_TENANT_KEY))

export function rememberOwnTenant(tenantId: string): void {
  if (!tenantId || tenantId === SYSTEM_TENANT_ID) return
  ownTenantId.value = tenantId
  write(sessionStorage, OWN_TENANT_KEY, tenantId)
}

/** Empty means same-origin, which is how the Go binary serves the console. */
export const baseUrl: string = import.meta.env.VITE_SENDPLANE_API?.replace(/\/$/, '') ?? ''

export function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = {}
  if (apiKey.value) headers['X-API-Key'] = apiKey.value
  if (tenantOverride.value) headers[TENANT_HEADER] = tenantOverride.value
  return headers
}

// --- preferences -----------------------------------------------------------

export type ThemeChoice = 'system' | 'light' | 'dark'

export const theme: Ref<ThemeChoice> = ref(
  (read(localStorage, THEME_KEY) as ThemeChoice) || 'system',
)

export function setTheme(choice: ThemeChoice): void {
  theme.value = choice
  write(localStorage, THEME_KEY, choice)
  applyTheme()
}

/** `data-theme` pins the tokens; removing it hands control back to the OS. */
export function applyTheme(): void {
  const root = document.documentElement
  if (theme.value === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', theme.value)
}

export const locale: Ref<string> = ref(read(localStorage, LOCALE_KEY) || detectLocale())

export function setLocale(next: string): void {
  locale.value = next
  write(localStorage, LOCALE_KEY, next)
  document.documentElement.lang = next
}

function detectLocale(): string {
  const preferred = navigator.language?.toLowerCase() ?? 'en'
  return preferred.startsWith('ko') ? 'ko' : 'en'
}

// Storage can throw in a private window or with site data blocked; a console
// that cannot remember a preference is still a usable console.
function read(storage: Storage, key: string): string {
  try {
    return storage.getItem(key) ?? ''
  } catch {
    return ''
  }
}

function write(storage: Storage, key: string, value: string): void {
  try {
    storage.setItem(key, value)
  } catch {
    /* ignore */
  }
}

function remove(storage: Storage, key: string): void {
  try {
    storage.removeItem(key)
  } catch {
    /* ignore */
  }
}
