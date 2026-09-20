import type { RecipientLine } from './types.js'

/**
 * One unit of the recipient stream: an object to serialize, or bytes/text that
 * are already NDJSON (a file read straight through, for instance).
 */
export type RecipientChunk = RecipientLine | Uint8Array | string

/** Anything the ingest wrapper accepts as a source of recipient lines. */
export type RecipientSource =
  string | Iterable<RecipientChunk> | AsyncIterable<RecipientChunk> | ReadableStream<RecipientChunk>

export interface IngestProgress {
  /** Recipient lines handed to the request body so far. */
  lines: number
  /** Bytes handed to the request body so far. */
  bytes: number
}

const LF = 0x0a

/**
 * Whether `fetch` can take a `ReadableStream` request body. Chrome and the
 * WHATWG spec require `duplex: 'half'` and reject a stream body otherwise;
 * Safari and Firefox (as of writing) do not support it at all, so the ingest
 * wrapper buffers instead.
 *
 * The probe is the canonical one: a `Request` built with a stream body only
 * reads the `duplex` getter on an implementation that supports streaming, and
 * an implementation that silently treats the stream as a plain object would
 * have set a `Content-Type`.
 */
export function supportsRequestStreams(): boolean {
  if (typeof ReadableStream === 'undefined' || typeof Request === 'undefined') return false
  let duplexAccessed = false
  let hasContentType = true
  try {
    const request = new Request('https://sendplane.invalid/', {
      body: new ReadableStream(),
      method: 'POST',
      get duplex() {
        duplexAccessed = true
        return 'half'
      },
    } as RequestInit)
    hasContentType = request.headers.has('Content-Type')
  } catch {
    return false
  }
  return duplexAccessed && !hasContentType
}

/** Normalizes any accepted source into an async iterator of chunks. */
async function* iterate(source: RecipientSource): AsyncGenerator<RecipientChunk> {
  // A string is itself `Iterable<string>`, so without this it would be walked
  // one character at a time.
  if (typeof source === 'string') {
    yield source
    return
  }
  if (isReadableStream(source)) {
    const reader = source.getReader()
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) return
        if (value !== undefined) yield value
      }
    } finally {
      reader.releaseLock()
    }
    return
  }
  if (Symbol.asyncIterator in source) {
    yield* source as AsyncIterable<RecipientChunk>
    return
  }
  yield* source as Iterable<RecipientChunk>
}

/**
 * Serializes recipient lines into NDJSON byte chunks. Byte and string chunks
 * pass through (a file is already NDJSON), objects are JSON-encoded with the
 * trailing newline the endpoint's framing needs.
 */
export async function* encodeNdjson(
  source: RecipientSource,
  onProgress?: (progress: IngestProgress) => void,
): AsyncGenerator<Uint8Array> {
  const encoder = new TextEncoder()
  const progress: IngestProgress = { lines: 0, bytes: 0 }
  let reported = 0
  for await (const chunk of iterate(source)) {
    let bytes: Uint8Array
    let lines: number
    if (chunk instanceof Uint8Array) {
      bytes = chunk
      lines = countLines(chunk)
    } else if (typeof chunk === 'string') {
      const text = chunk.endsWith('\n') ? chunk : `${chunk}\n`
      bytes = encoder.encode(text)
      lines = countLines(bytes)
    } else {
      bytes = encoder.encode(`${JSON.stringify(chunk)}\n`)
      lines = 1
    }
    if (bytes.length === 0) continue
    progress.lines += lines
    progress.bytes += bytes.length
    yield bytes
    // Reporting every chunk would be one callback per recipient on an object
    // source; 64 KiB is frequent enough for a progress bar.
    if (onProgress && progress.bytes - reported >= 65536) {
      reported = progress.bytes
      onProgress({ ...progress })
    }
  }
  onProgress?.({ ...progress })
}

/** Wraps an async iterator of byte chunks as a `ReadableStream` request body. */
export function toReadableStream(chunks: AsyncIterable<Uint8Array>): ReadableStream<Uint8Array> {
  const iterator = chunks[Symbol.asyncIterator]()
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const { done, value } = await iterator.next()
      if (done) controller.close()
      else controller.enqueue(value)
    },
    async cancel(reason) {
      await iterator.return?.(reason)
    },
  })
}

/** Buffers the whole stream, for runtimes without streaming request bodies. */
export async function collect(chunks: AsyncIterable<Uint8Array>): Promise<Uint8Array> {
  const parts: Uint8Array[] = []
  let total = 0
  for await (const chunk of chunks) {
    parts.push(chunk)
    total += chunk.length
  }
  const out = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    out.set(part, offset)
    offset += part.length
  }
  return out
}

/**
 * Reads a `.ndjson` / `.jsonl` file as recipient lines without loading it all
 * into memory, so a 1M-row file streams straight through to the ingest body.
 */
export async function* recipientsFromNdjsonBlob(blob: Blob): AsyncGenerator<RecipientLine> {
  const decoder = new TextDecoder()
  let buffer = ''
  for await (const chunk of iterate(blob.stream() as ReadableStream<Uint8Array>)) {
    buffer += decoder.decode(chunk as Uint8Array, { stream: true })
    let newline = buffer.indexOf('\n')
    while (newline >= 0) {
      const line = buffer.slice(0, newline).trim()
      buffer = buffer.slice(newline + 1)
      if (line) yield JSON.parse(line) as RecipientLine
      newline = buffer.indexOf('\n')
    }
  }
  buffer += decoder.decode()
  const rest = buffer.trim()
  if (rest) yield JSON.parse(rest) as RecipientLine
}

/**
 * Reads a CSV file as recipient lines. The header names the fields; `email` is
 * required, `name` / `locale` / `unsubscribe_url` are recognised, and any
 * `vars.<path>` column lands in `vars` (dots nest).
 */
export async function* recipientsFromCsvBlob(blob: Blob): AsyncGenerator<RecipientLine> {
  const rows = parseCsv(await blob.text())
  const header = rows.shift()
  if (!header) return
  const columns = header.map((name) => name.trim())
  const emailAt = columns.findIndex((name) => name.toLowerCase() === 'email')
  if (emailAt < 0) throw new Error('CSV has no `email` column')
  for (const row of rows) {
    if (row.length === 0 || (row.length === 1 && !row[0])) continue
    const email = (row[emailAt] ?? '').trim()
    if (!email) continue
    const line: RecipientLine = { email }
    const vars: Record<string, unknown> = {}
    for (let i = 0; i < columns.length; i++) {
      const column = columns[i]!
      const raw = row[i]
      if (i === emailAt || raw === undefined || raw === '') continue
      const key = column.toLowerCase()
      if (key === 'name') line.name = raw
      else if (key === 'locale') line.locale = raw
      else if (key === 'unsubscribe_url') line.unsubscribe_url = raw
      else if (key.startsWith('vars.')) assignPath(vars, column.slice('vars.'.length), raw)
      else assignPath(vars, column, raw)
    }
    if (Object.keys(vars).length > 0) line.vars = vars
    yield line
  }
}

/** Picks the reader by file extension, defaulting to NDJSON. */
export function recipientsFromFile(file: File | Blob, name?: string): AsyncIterable<RecipientLine> {
  const filename = (name ?? (file as File).name ?? '').toLowerCase()
  return filename.endsWith('.csv') ? recipientsFromCsvBlob(file) : recipientsFromNdjsonBlob(file)
}

/** Minimal RFC 4180 reader: quoted fields, doubled quotes, CRLF or LF. */
export function parseCsv(text: string): string[][] {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let quoted = false
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (quoted) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          field += '"'
          i++
        } else quoted = false
      } else field += ch
      continue
    }
    if (ch === '"') quoted = true
    else if (ch === ',') {
      row.push(field)
      field = ''
    } else if (ch === '\n' || ch === '\r') {
      if (ch === '\r' && text[i + 1] === '\n') i++
      row.push(field)
      rows.push(row)
      row = []
      field = ''
    } else field += ch
  }
  if (field !== '' || row.length > 0) {
    row.push(field)
    rows.push(row)
  }
  return rows
}

function assignPath(target: Record<string, unknown>, path: string, value: string): void {
  const parts = path.split('.').filter(Boolean)
  if (parts.length === 0) return
  let node = target
  for (let i = 0; i < parts.length - 1; i++) {
    const part = parts[i]!
    const next = node[part]
    if (typeof next !== 'object' || next === null) node[part] = {}
    node = node[part] as Record<string, unknown>
  }
  node[parts[parts.length - 1]!] = value
}

function countLines(bytes: Uint8Array): number {
  let n = 0
  for (const byte of bytes) if (byte === LF) n++
  return n
}

function isReadableStream(value: unknown): value is ReadableStream<unknown> {
  return (
    typeof ReadableStream !== 'undefined' &&
    typeof value === 'object' &&
    value !== null &&
    typeof (value as ReadableStream).getReader === 'function'
  )
}
