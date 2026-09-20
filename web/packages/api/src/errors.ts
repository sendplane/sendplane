import type { components } from './schema.js'

/** The single error shape every non-2xx sendplane response uses. */
export type ApiError = components['schemas']['Error']
export type ApiErrorCode = ApiError['code']
export type ApiErrorDetail = components['schemas']['ErrorDetail']

/**
 * Codes the transport can produce that the spec's `Error.code` enum does not
 * cover: the request never reached sendplane, or the body was not the documented
 * shape (a proxy error page, say).
 */
export type TransportErrorCode = 'network_error' | 'unknown'

export interface SendplaneErrorInit {
  message: string
  code?: ApiErrorCode | TransportErrorCode
  status?: number
  details?: ApiErrorDetail[]
  response?: Response
  body?: unknown
  cause?: unknown
}

/**
 * Every failure the client surfaces. `code` is the stable switch: the spec is
 * explicit that clients branch on it and never on `message`.
 */
export class SendplaneError extends Error {
  override readonly name = 'SendplaneError'
  /** HTTP status, or 0 when the request never got a response. */
  readonly status: number
  readonly code: ApiErrorCode | TransportErrorCode
  readonly details: ApiErrorDetail[]
  /** Present whenever a response was received. */
  readonly response: Response | undefined
  /** The parsed response body, whatever shape it had. */
  readonly body: unknown

  constructor(init: SendplaneErrorInit) {
    super(init.message, init.cause === undefined ? undefined : { cause: init.cause })
    this.status = init.status ?? 0
    this.code = init.code ?? 'unknown'
    this.details = init.details ?? []
    this.response = init.response
    this.body = init.body
  }

  /** 409 `version_conflict`: the object moved on since it was read. */
  get isVersionConflict(): boolean {
    return this.code === 'version_conflict'
  }

  get isNotFound(): boolean {
    return this.code === 'not_found' || this.status === 404
  }

  get isUnauthenticated(): boolean {
    return this.code === 'unauthenticated' || this.status === 401
  }

  get isForbidden(): boolean {
    return this.code === 'forbidden' || this.status === 403
  }

  /** Retrying the exact same request may work. */
  get isRetryable(): boolean {
    return this.code === 'rate_limited' || this.code === 'network_error' || this.status >= 500
  }

  /**
   * Builds an error from a response plus whatever body was already read. Pass
   * `body` when the caller has consumed it (openapi-fetch hands it over as
   * `error`); otherwise the response body is read here.
   */
  static async fromResponse(response: Response, body?: unknown): Promise<SendplaneError> {
    let parsed = body
    if (parsed === undefined && !response.bodyUsed) {
      const text = await response.text().catch(() => '')
      if (text) {
        try {
          parsed = JSON.parse(text)
        } catch {
          parsed = text
        }
      }
    }
    return SendplaneError.fromBody(response, parsed)
  }

  /** Synchronous form for callers that already hold the parsed body. */
  static fromBody(response: Response, body: unknown): SendplaneError {
    const api = isApiError(body) ? body : undefined
    return new SendplaneError({
      message: api?.message ?? `${response.status} ${response.statusText || 'request failed'}`,
      code: api?.code ?? statusToCode(response.status),
      status: response.status,
      details: api?.details ?? [],
      response,
      body,
    })
  }

  /** The request never produced a response (DNS, CORS, offline, abort). */
  static fromNetwork(cause: unknown): SendplaneError {
    const message = cause instanceof Error ? cause.message : String(cause)
    return new SendplaneError({ message, code: 'network_error', status: 0, cause })
  }
}

export function isSendplaneError(value: unknown): value is SendplaneError {
  return value instanceof SendplaneError
}

function isApiError(value: unknown): value is ApiError {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as ApiError).code === 'string' &&
    typeof (value as ApiError).message === 'string'
  )
}

/** Best-effort mapping when the body was not the documented `Error` shape. */
function statusToCode(status: number): ApiErrorCode | TransportErrorCode {
  switch (status) {
    case 400:
      return 'invalid_request'
    case 401:
      return 'unauthenticated'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'version_conflict'
    case 413:
      return 'payload_too_large'
    case 422:
      return 'validation_failed'
    case 429:
      return 'rate_limited'
    default:
      return status >= 500 ? 'internal' : 'unknown'
  }
}
