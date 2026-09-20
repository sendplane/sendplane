import { vi } from 'vitest'

export interface RecordedRequest {
  method: string
  url: string
  headers: Headers
  /** True when the body handed to `fetch` was a `ReadableStream`. */
  streamed: boolean
  body: string
}

export interface MockResponse {
  status?: number
  json?: unknown
  text?: string
  contentType?: string
}

/**
 * A `fetch` stand-in that records what the client sent. It accepts both call
 * shapes the client uses: openapi-fetch hands over a `Request`, the hand-written
 * wrappers hand over `(url, init)`.
 */
export function createFetchMock(
  responder: (request: RecordedRequest) => MockResponse | Promise<MockResponse>,
) {
  const calls: RecordedRequest[] = []

  const impl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    let method: string
    let url: string
    let headers: Headers
    let rawBody: BodyInit | null | undefined

    if (typeof Request !== 'undefined' && input instanceof Request) {
      method = input.method
      url = input.url
      headers = new Headers(input.headers)
      rawBody = input.body ? ((await input.text()) as BodyInit) : undefined
    } else {
      method = init?.method ?? 'GET'
      url = String(input)
      headers = new Headers(init?.headers)
      rawBody = init?.body ?? undefined
    }

    const streamed = typeof ReadableStream !== 'undefined' && rawBody instanceof ReadableStream
    let body = ''
    if (typeof rawBody === 'string') body = rawBody
    else if (rawBody !== undefined && rawBody !== null) body = await new Response(rawBody).text()

    const recorded: RecordedRequest = { method, url, headers, streamed, body }
    calls.push(recorded)

    const spec = await responder(recorded)
    const status = spec.status ?? 200
    const contentType =
      spec.contentType ?? (spec.json !== undefined ? 'application/json' : 'text/plain')
    const payload = spec.json !== undefined ? JSON.stringify(spec.json) : (spec.text ?? '')
    return new Response(status === 204 ? null : payload, {
      status,
      headers: { 'Content-Type': contentType },
    })
  }

  return { calls, fetch: vi.fn(impl) as unknown as typeof globalThis.fetch }
}
