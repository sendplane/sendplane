import { describe, expect, it } from 'vitest'

import {
  collect,
  encodeNdjson,
  parseCsv,
  recipientsFromCsvBlob,
  recipientsFromFile,
  recipientsFromNdjsonBlob,
  type RecipientLine,
} from '../src/index.js'

const decode = (bytes: Uint8Array) => new TextDecoder().decode(bytes)

async function drain<T>(source: AsyncIterable<T>): Promise<T[]> {
  const out: T[] = []
  for await (const item of source) out.push(item)
  return out
}

describe('encodeNdjson', () => {
  it('appends the line terminator objects need', async () => {
    const bytes = await collect(encodeNdjson([{ email: 'a@x.com' }, { email: 'b@x.com' }]))
    expect(decode(bytes)).toBe('{"email":"a@x.com"}\n{"email":"b@x.com"}\n')
  })

  it('passes pre-encoded NDJSON text through and counts its lines', async () => {
    const seen: number[] = []
    const bytes = await collect(
      encodeNdjson(['{"email":"a@x.com"}\n{"email":"b@x.com"}\n'], (p) => seen.push(p.lines)),
    )
    expect(decode(bytes)).toBe('{"email":"a@x.com"}\n{"email":"b@x.com"}\n')
    expect(seen.at(-1)).toBe(2)
  })

  it('adds a missing terminator to a string chunk', async () => {
    const bytes = await collect(encodeNdjson(['{"email":"a@x.com"}']))
    expect(decode(bytes)).toBe('{"email":"a@x.com"}\n')
  })
})

describe('parseCsv', () => {
  it('handles quoted fields, doubled quotes and CRLF', () => {
    const rows = parseCsv('email,name\r\n"a@x.com","Smith, ""Al"""\r\nb@x.com,B\r\n')
    expect(rows).toEqual([
      ['email', 'name'],
      ['a@x.com', 'Smith, "Al"'],
      ['b@x.com', 'B'],
    ])
  })
})

describe('recipient readers', () => {
  it('reads NDJSON out of a blob', async () => {
    const blob = new Blob(['{"email":"a@x.com"}\n{"email":"b@x.com","locale":"ko"}\n'])
    const got = await drain(recipientsFromNdjsonBlob(blob))
    expect(got).toEqual([{ email: 'a@x.com' }, { email: 'b@x.com', locale: 'ko' }])
  })

  it('reads a final line without a trailing newline', async () => {
    const got = await drain(recipientsFromNdjsonBlob(new Blob(['{"email":"a@x.com"}'])))
    expect(got).toEqual([{ email: 'a@x.com' }])
  })

  it('maps CSV columns onto recipient fields and nests vars', async () => {
    const csv = 'email,name,locale,vars.plan,vars.order.total\na@x.com,A,ko,pro,42\n'
    const got = await drain(recipientsFromCsvBlob(new Blob([csv])))
    expect(got).toEqual<RecipientLine[]>([
      { email: 'a@x.com', name: 'A', locale: 'ko', vars: { plan: 'pro', order: { total: '42' } } },
    ])
  })

  it('rejects a CSV without an email column', async () => {
    await expect(drain(recipientsFromCsvBlob(new Blob(['name\nA\n'])))).rejects.toThrow(/email/)
  })

  it('picks the reader from the file extension', async () => {
    const file = new File(['email\na@x.com\n'], 'people.csv', { type: 'text/csv' })
    expect(await drain(recipientsFromFile(file))).toEqual([{ email: 'a@x.com' }])
  })
})
