import { ref, type Ref } from 'vue'

const STORAGE_KEY = 'sendplane.apiKey'
const THEME_KEY = 'sendplane.theme'
const LOCALE_KEY = 'sendplane.locale'

/**
 * The API key lives in `sessionStorage`, not `localStorage`: it is a bearer
 * credential, and scoping it to the tab means closing the tab logs out. The
 * console never sends a tenant; the host's `TenantResolver` derives it from the
 * principal the key maps to.
 */
export const apiKey: Ref<string> = ref(read(sessionStorage, STORAGE_KEY))

export function setApiKey(value: string): void {
  apiKey.value = value.trim()
  write(sessionStorage, STORAGE_KEY, apiKey.value)
}

export function clearApiKey(): void {
  apiKey.value = ''
  remove(sessionStorage, STORAGE_KEY)
}

/** Empty means same-origin, which is how the Go binary serves the console. */
export const baseUrl: string = import.meta.env.VITE_SENDPLANE_API?.replace(/\/$/, '') ?? ''

export function authHeaders(): Record<string, string> {
  return apiKey.value ? { 'X-API-Key': apiKey.value } : {}
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
