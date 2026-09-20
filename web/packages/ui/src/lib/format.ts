/** Presentation helpers shared by the pages. All are pure and locale-aware. */

export function formatDateTime(value: string | undefined | null, locale = 'en'): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'medium' }).format(date)
}

export function formatRelative(value: string | undefined | null, locale = 'en'): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  const seconds = Math.round((date.getTime() - Date.now()) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['second', 60],
    ['minute', 60],
    ['hour', 24],
    ['day', 7],
    ['week', 4.348],
    ['month', 12],
    ['year', Number.POSITIVE_INFINITY],
  ]
  let amount = seconds
  for (const [unit, step] of units) {
    if (Math.abs(amount) < step || unit === 'year') {
      return new Intl.RelativeTimeFormat(locale, { numeric: 'auto' }).format(
        Math.round(amount),
        unit,
      )
    }
    amount /= step
  }
  return formatDateTime(value, locale)
}

export function formatNumber(value: number | undefined | null, locale = 'en'): string {
  if (value === undefined || value === null) return '—'
  return new Intl.NumberFormat(locale).format(value)
}

/**
 * Ratio against the campaign's `sent` count, which the spec fixes as the
 * denominator for every engagement metric.
 */
export function formatRate(
  numerator: number | undefined,
  denominator: number | undefined,
  locale = 'en',
): string {
  if (!denominator || numerator === undefined) return '—'
  return new Intl.NumberFormat(locale, {
    style: 'percent',
    maximumFractionDigits: 2,
  }).format(numerator / denominator)
}

export function formatBytes(bytes: number, locale = 'en'): string {
  const units = ['B', 'KiB', 'MiB', 'GiB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  const formatted = new Intl.NumberFormat(locale, {
    maximumFractionDigits: unit === 0 ? 0 : 1,
  }).format(value)
  return `${formatted} ${units[unit]}`
}

/** Shortens a UUID for a dense table without losing recognisability. */
export function shortId(id: string | undefined | null): string {
  if (!id) return '—'
  return id.length <= 12 ? id : `${id.slice(0, 8)}…${id.slice(-4)}`
}

export function safeJson(value: unknown, indent = 2): string {
  try {
    return JSON.stringify(value, null, indent) ?? ''
  } catch {
    return String(value)
  }
}

/** Parses operator-entered JSON, returning the error message instead of throwing. */
export function tryParseJson<T>(text: string, fallback: T): { value: T; error?: string } {
  const trimmed = text.trim()
  if (!trimmed) return { value: fallback }
  try {
    return { value: JSON.parse(trimmed) as T }
  } catch (error) {
    return { value: fallback, error: error instanceof Error ? error.message : String(error) }
  }
}

/** `a=1` lines to a record, used by the per-domain rate cap field. */
export function parseKeyValueLines(text: string): Record<string, number> {
  const out: Record<string, number> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const at = trimmed.indexOf('=')
    if (at < 0) continue
    const key = trimmed.slice(0, at).trim()
    const value = Number(trimmed.slice(at + 1).trim())
    if (key && Number.isFinite(value)) out[key] = value
  }
  return out
}

export function formatKeyValueLines(record: Record<string, number> | undefined): string {
  if (!record) return ''
  return Object.entries(record)
    .map(([key, value]) => `${key}=${value}`)
    .join('\n')
}

export function splitLines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
}

/** Triggers a browser download without needing a server round-trip. */
export function downloadText(filename: string, text: string, mime = 'text/plain'): void {
  if (typeof document === 'undefined') return
  const url = URL.createObjectURL(new Blob([text], { type: mime }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  URL.revokeObjectURL(url)
}
