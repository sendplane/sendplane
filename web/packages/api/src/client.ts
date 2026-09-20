import createOpenapiClient, {
  type Client,
  type FetchResponse,
  type MaybeOptionalInit,
  type Middleware,
} from 'openapi-fetch'
import type {
  HttpMethod,
  MediaType,
  PathsWithMethod,
  RequiredKeysOf,
} from 'openapi-typescript-helpers'

import { SendplaneError } from './errors.js'
import {
  collect,
  encodeNdjson,
  supportsRequestStreams,
  toReadableStream,
  type IngestProgress,
  type RecipientSource,
} from './ndjson.js'
import type { paths } from './schema.js'
import type { DeliveryList, I18nImportResult, RecipientIngestResult } from './types.js'

export type AuthHeaders = Record<string, string>

export interface SendplaneClientOptions {
  /**
   * Origin the host mounted `Sendplane.Handler()` on. Empty means same-origin,
   * which is what the console uses when the Go binary serves it.
   */
  baseUrl?: string
  /**
   * Called before every request. sendplane has no opinion about the credential:
   * the reference binary accepts `X-API-Key` or `Authorization: Bearer`, another
   * host's `Authenticator` may accept something else entirely.
   */
  getAuthHeaders?: () => AuthHeaders | Promise<AuthHeaders>
  /** Injected `fetch`, for tests and for runtimes with a custom implementation. */
  fetch?: typeof globalThis.fetch
  /** Extra headers merged under the auth headers. */
  headers?: Record<string, string>
}

export interface IngestOptions {
  /**
   * Chunk replay key. Repeating a completed chunk returns its stored counts
   * instead of inserting again; sendplane falls back to a hash of the body.
   */
  idempotencyKey?: string
  signal?: AbortSignal
  onProgress?: (progress: IngestProgress) => void
  /**
   * Forces the buffering path. Streaming is used when the runtime supports it;
   * set this to compare, or to work around an intermediary that cannot take a
   * chunked request body.
   */
  buffer?: boolean
}

// openapi-fetch does not export `InitParam`, so it is mirrored here to give the
// unwrapped methods the same "second argument optional unless something in it
// is required" behaviour as `client.GET`.
type InitParam<Init> =
  RequiredKeysOf<Init> extends never
    ? [(Init & { [key: string]: unknown })?]
    : [Init & { [key: string]: unknown }]

type SuccessData<R> = R extends { data: infer D } ? D : never

// `FetchResponse` constrains its operation to `Record<string | number, any>`,
// which `paths[Path][M]` only satisfies once the `undefined` members of the
// generated path object (the verbs a path does not have) are filtered out.
type OperationOf<Path extends keyof paths, M extends HttpMethod> = Extract<
  paths[Path][M],
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  Record<string | number, any>
>

/** `client.GET` without the `{ data, error }` envelope: resolves or throws. */
export type UnwrappedMethod<M extends HttpMethod> = <
  Path extends PathsWithMethod<paths, M>,
  Init extends MaybeOptionalInit<paths[Path], M>,
>(
  url: Path,
  ...init: InitParam<Init>
) => Promise<SuccessData<FetchResponse<OperationOf<Path, M>, Init, MediaType>>>

/**
 * Query of `GET /api/v1/deliveries`, taken straight from the generated
 * operation so that a new filter in the spec shows up here without an edit.
 */
export type DeliveryQuery = NonNullable<
  NonNullable<paths['/api/v1/deliveries']['get']['parameters']>['query']
>

export interface SendplaneClient extends Client<paths> {
  /** The base URL every request is resolved against. */
  readonly baseUrl: string

  /** `GET` returning the payload directly; throws `SendplaneError` on failure. */
  get: UnwrappedMethod<'get'>
  post: UnwrappedMethod<'post'>
  put: UnwrappedMethod<'put'>
  del: UnwrappedMethod<'delete'>

  /**
   * Appends recipients to a campaign as a streamed NDJSON body, so memory stays
   * flat no matter how long the source is. Call it once per chunk; 10k-100k
   * lines per chunk is the size the server is tuned for.
   */
  ingestRecipients(
    campaignId: string,
    lines: RecipientSource,
    options?: IngestOptions,
  ): Promise<RecipientIngestResult>

  /**
   * The tenant-wide delivery listing: every delivery of the tenant, whatever
   * its campaign. It is `client.get('/api/v1/deliveries')` with the query
   * named, which is what a screen searching by address wants; the
   * campaign-scoped `/campaigns/{id}/deliveries` stays available through the
   * generic methods.
   */
  listDeliveries(query?: DeliveryQuery, signal?: AbortSignal): Promise<DeliveryList>

  /** Exports a template's i18n bundle as YAML text (`format=yaml`). */
  getI18nYaml(templateId: string, signal?: AbortSignal): Promise<string>

  /** Replaces a template's i18n bundle from YAML text. */
  putI18nYaml(templateId: string, yaml: string, signal?: AbortSignal): Promise<I18nImportResult>
}

/**
 * Builds a typed client for the sendplane REST API. Framework-agnostic: the
 * only injection point is `getAuthHeaders`, so a host can plug in whatever its
 * `Authenticator` accepts.
 */
export function createClient(options: SendplaneClientOptions = {}): SendplaneClient {
  const baseUrl = stripTrailingSlash(options.baseUrl ?? '')
  const doFetch =
    options.fetch ?? ((...args: Parameters<typeof fetch>) => globalThis.fetch(...args))

  const auth: Middleware = {
    async onRequest({ request }) {
      if (options.headers) {
        for (const [name, value] of Object.entries(options.headers))
          request.headers.set(name, value)
      }
      const headers = await options.getAuthHeaders?.()
      if (headers) {
        for (const [name, value] of Object.entries(headers)) request.headers.set(name, value)
      }
      return request
    },
  }

  const base = createOpenapiClient<paths>({
    baseUrl: baseUrl || '/',
    fetch: (request) => doFetch(request),
    // `?status=a&status=b` — the spec marks every repeated filter `explode: true`.
    querySerializer: { array: { style: 'form', explode: true } },
  })
  base.use(auth)

  const unwrap = <M extends HttpMethod>(
    method: (...args: never[]) => Promise<unknown>,
  ): UnwrappedMethod<M> =>
    // The cast is the price of re-deriving openapi-fetch's overload shape; the
    // runtime behaviour is "return data, throw on error" for every method.
    (async (...args: never[]) => {
      const result = (await method(...args)) as {
        data?: unknown
        error?: unknown
        response: Response
      }
      if (result.error !== undefined) throw SendplaneError.fromBody(result.response, result.error)
      if (!result.response.ok) throw await SendplaneError.fromResponse(result.response)
      return result.data
    }) as UnwrappedMethod<M>

  async function authorizedFetch(
    path: string,
    init: RequestInit & { duplex?: 'half' },
  ): Promise<Response> {
    const headers = new Headers(init.headers)
    if (options.headers) {
      for (const [name, value] of Object.entries(options.headers)) headers.set(name, value)
    }
    const authHeaders = await options.getAuthHeaders?.()
    if (authHeaders) {
      for (const [name, value] of Object.entries(authHeaders)) headers.set(name, value)
    }
    try {
      return await doFetch(`${baseUrl}${path}`, { ...init, headers } as RequestInit)
    } catch (cause) {
      throw SendplaneError.fromNetwork(cause)
    }
  }

  const get = unwrap<'get'>(base.GET as never)

  const client: SendplaneClient = Object.assign(base, {
    baseUrl,
    get,
    post: unwrap<'post'>(base.POST as never),
    put: unwrap<'put'>(base.PUT as never),
    del: unwrap<'delete'>(base.DELETE as never),

    async ingestRecipients(
      campaignId: string,
      lines: RecipientSource,
      ingest: IngestOptions = {},
    ): Promise<RecipientIngestResult> {
      const headers: Record<string, string> = { 'Content-Type': 'application/x-ndjson' }
      if (ingest.idempotencyKey) headers['Idempotency-Key'] = ingest.idempotencyKey

      const chunks = encodeNdjson(lines, ingest.onProgress)
      const streaming = !ingest.buffer && supportsRequestStreams()
      const body: BodyInit = streaming
        ? (toReadableStream(chunks) as unknown as BodyInit)
        : ((await collect(chunks)) as unknown as BodyInit)

      const init: RequestInit & { duplex?: 'half' } = { method: 'POST', headers, body }
      if (ingest.signal) init.signal = ingest.signal
      // Required by the Fetch standard for a stream body; harmless otherwise.
      if (streaming) init.duplex = 'half'

      const response = await authorizedFetch(
        `/api/v1/campaigns/${encodeURIComponent(campaignId)}/recipients`,
        init,
      )
      if (!response.ok) throw await SendplaneError.fromResponse(response)
      return (await response.json()) as RecipientIngestResult
    },

    async listDeliveries(query: DeliveryQuery = {}, signal?: AbortSignal): Promise<DeliveryList> {
      const init: { params: { query: DeliveryQuery }; signal?: AbortSignal } = {
        params: { query },
      }
      if (signal) init.signal = signal
      return (await get('/api/v1/deliveries', init)) as DeliveryList
    },

    async getI18nYaml(templateId: string, signal?: AbortSignal): Promise<string> {
      const init: RequestInit = { method: 'GET', headers: { Accept: 'application/x-yaml' } }
      if (signal) init.signal = signal
      const response = await authorizedFetch(
        `/api/v1/templates/${encodeURIComponent(templateId)}/i18n?format=yaml`,
        init,
      )
      if (!response.ok) throw await SendplaneError.fromResponse(response)
      return await response.text()
    },

    async putI18nYaml(
      templateId: string,
      yaml: string,
      signal?: AbortSignal,
    ): Promise<I18nImportResult> {
      const init: RequestInit = {
        method: 'PUT',
        headers: { 'Content-Type': 'application/x-yaml', Accept: 'application/json' },
        body: yaml,
      }
      if (signal) init.signal = signal
      const response = await authorizedFetch(
        `/api/v1/templates/${encodeURIComponent(templateId)}/i18n`,
        init,
      )
      if (!response.ok) throw await SendplaneError.fromResponse(response)
      return (await response.json()) as I18nImportResult
    },
  })

  return client
}

function stripTrailingSlash(url: string): string {
  return url.endsWith('/') ? url.slice(0, -1) : url
}
