import { describe, expect, it } from 'vitest'

import {
  createClient,
  isSendplaneError,
  supportsRequestStreams,
  type SendplaneError,
  type Campaign,
  type RecipientIngestResult,
  type RecipientLine,
} from '../src/index.js'
import { createFetchMock, type MockResponse, type RecordedRequest } from './fetch-mock.js'

const BASE = 'https://app.example.test'

function client(responder: (request: RecordedRequest) => MockResponse | Promise<MockResponse>) {
  const mock = createFetchMock(responder)
  return {
    mock,
    api: createClient({
      baseUrl: BASE,
      fetch: mock.fetch,
      getAuthHeaders: () => ({ 'X-API-Key': 'k-123' }),
    }),
  }
}

const campaign: Campaign = {
  id: '018f5a2c-0000-7000-8000-000000000001',
  name: 'launch',
  sender_id: '018f5a2c-0000-7000-8000-000000000002',
  status: 'draft',
  version: 1,
}

describe('createClient', () => {
  it('injects auth headers on every request', async () => {
    const { api, mock } = client(() => ({ json: { items: [campaign] } }))
    await api.GET('/api/v1/campaigns')
    expect(mock.calls[0]?.headers.get('X-API-Key')).toBe('k-123')
  })

  it('resolves auth headers asynchronously', async () => {
    const mock = createFetchMock(() => ({ json: { items: [] } }))
    const api = createClient({
      baseUrl: BASE,
      fetch: mock.fetch,
      getAuthHeaders: async () => ({ Authorization: 'Bearer jwt' }),
    })
    await api.GET('/api/v1/campaigns')
    expect(mock.calls[0]?.headers.get('Authorization')).toBe('Bearer jwt')
  })

  it('serializes repeated filters as explode=true, matching the spec', async () => {
    const { api, mock } = client(() => ({ json: { items: [] } }))
    await api.GET('/api/v1/campaigns/{campaignId}/deliveries', {
      params: {
        path: { campaignId: campaign.id! },
        query: { status: ['failed', 'bounced'], limit: 25 },
      },
    })
    const url = new URL(mock.calls[0]!.url)
    expect(url.pathname).toBe(`/api/v1/campaigns/${campaign.id}/deliveries`)
    expect(url.searchParams.getAll('status')).toEqual(['failed', 'bounced'])
    expect(url.searchParams.get('limit')).toBe('25')
  })

  it('unwraps the success payload with client.get', async () => {
    const { api } = client(() => ({ json: campaign }))
    const got = await api.get('/api/v1/campaigns/{campaignId}', {
      params: { path: { campaignId: campaign.id! } },
    })
    expect(got.name).toBe('launch')
  })

  it('maps the Error schema onto SendplaneError', async () => {
    const { api } = client(() => ({
      status: 422,
      json: {
        code: 'missing_i18n_keys',
        message: 'welcome.title is untranslated in ko',
        details: [{ field: 'i18n.ko', message: 'missing', value: 'welcome.title' }],
      },
    }))

    const error = await api
      .post('/api/v1/templates/{templateId}/publish', {
        params: { path: { templateId: 't1' } },
      })
      .then(
        () => null,
        (e: unknown) => e,
      )

    expect(isSendplaneError(error)).toBe(true)
    const sp = error as SendplaneError
    expect(sp.code).toBe('missing_i18n_keys')
    expect(sp.status).toBe(422)
    expect(sp.details[0]?.field).toBe('i18n.ko')
    expect(sp.isVersionConflict).toBe(false)
  })

  it('falls back to a status-derived code when the body is not an Error', async () => {
    const { api } = client(() => ({
      status: 502,
      text: '<html>bad gateway</html>',
      contentType: 'text/html',
    }))
    const error: SendplaneError = await api.get('/api/v1/settings').then(
      () => Promise.reject(new Error('expected a failure')),
      (e: SendplaneError) => e,
    )
    expect(error.code).toBe('internal')
    expect(error.status).toBe(502)
    expect(error.isRetryable).toBe(true)
  })

  it('reports a failed fetch as network_error', async () => {
    const api = createClient({
      baseUrl: BASE,
      fetch: () => Promise.reject(new TypeError('Failed to fetch')),
    })
    const error: SendplaneError = await api.getI18nYaml('t1').then(
      () => Promise.reject(new Error('expected a failure')),
      (e: SendplaneError) => e,
    )
    expect(error.code).toBe('network_error')
    expect(error.status).toBe(0)
    expect(error.isRetryable).toBe(true)
  })
})

const ingestResult: RecipientIngestResult = {
  accepted: 2,
  duplicates: 0,
  invalid: 0,
  total: 2,
}

const lines: RecipientLine[] = [
  { email: 'a@x.com', name: 'A', locale: 'ko', vars: { plan: 'pro' } },
  { email: 'b@x.com', name: 'B' },
]

describe('ingestRecipients', () => {
  it('streams NDJSON with a ReadableStream body when the runtime supports it', async () => {
    const { api, mock } = client(() => ({ json: ingestResult }))
    const result = await api.ingestRecipients(campaign.id!, lines, { idempotencyKey: 'chunk-0007' })

    const call = mock.calls[0]!
    expect(call.method).toBe('POST')
    expect(call.url).toBe(`${BASE}/api/v1/campaigns/${campaign.id}/recipients`)
    expect(call.headers.get('Content-Type')).toBe('application/x-ndjson')
    expect(call.headers.get('Idempotency-Key')).toBe('chunk-0007')
    expect(call.headers.get('X-API-Key')).toBe('k-123')
    expect(call.streamed).toBe(supportsRequestStreams())
    expect(call.body).toBe(
      '{"email":"a@x.com","name":"A","locale":"ko","vars":{"plan":"pro"}}\n{"email":"b@x.com","name":"B"}\n',
    )
    expect(result).toEqual(ingestResult)
  })

  it('buffers the body when asked to, producing an identical request', async () => {
    const { api, mock } = client(() => ({ json: ingestResult }))
    await api.ingestRecipients(campaign.id!, lines, { buffer: true })
    expect(mock.calls[0]!.streamed).toBe(false)
    expect(mock.calls[0]!.body.split('\n').filter(Boolean)).toHaveLength(2)
  })

  it('accepts an async iterable and reports progress', async () => {
    const { api, mock } = client(() => ({ json: ingestResult }))
    async function* source(): AsyncGenerator<RecipientLine> {
      for (const line of lines) yield line
    }
    const progress: number[] = []
    await api.ingestRecipients(campaign.id!, source(), {
      onProgress: (p) => progress.push(p.lines),
    })
    expect(progress.at(-1)).toBe(2)
    expect(mock.calls[0]!.body.split('\n').filter(Boolean)).toHaveLength(2)
  })

  it('throws SendplaneError when the chunk breaks a limit', async () => {
    const { api } = client(() => ({
      status: 422,
      json: { code: 'limit_exceeded', message: 'campaign recipient limit reached' },
    }))
    await expect(api.ingestRecipients(campaign.id!, lines)).rejects.toMatchObject({
      name: 'SendplaneError',
      code: 'limit_exceeded',
      status: 422,
    })
  })
})

describe('listDeliveries', () => {
  const delivery = {
    id: '018f5a2c-0000-7000-8000-000000000003',
    version_id: '018f5a2c-0000-7000-8000-000000000004',
    sender_id: '018f5a2c-0000-7000-8000-000000000002',
    lane: 'transactional' as const,
    status: 'sent' as const,
    email: 'a+b@example.com',
  }

  it('hits the tenant-wide route with the repeated filters exploded', async () => {
    const { api, mock } = client(() => ({ json: { items: [delivery] } }))
    const page = await api.listDeliveries({
      email: 'a+b@example.com',
      lane: ['transactional', 'probe'],
      status: ['sent'],
      since: '2025-03-01T00:00:00Z',
      limit: 25,
    })
    const url = new URL(mock.calls[0]!.url)
    expect(url.pathname).toBe('/api/v1/deliveries')
    expect(url.searchParams.getAll('lane')).toEqual(['transactional', 'probe'])
    expect(url.searchParams.getAll('status')).toEqual(['sent'])
    // `+` has to survive as part of the address, not decode to a space.
    expect(url.searchParams.get('email')).toBe('a+b@example.com')
    expect(url.searchParams.get('since')).toBe('2025-03-01T00:00:00Z')
    expect(url.searchParams.get('limit')).toBe('25')
    expect(page.items[0]?.email).toBe('a+b@example.com')
  })

  it('takes no query at all', async () => {
    const { api, mock } = client(() => ({ json: { items: [] } }))
    await api.listDeliveries()
    expect(new URL(mock.calls[0]!.url).search).toBe('')
  })

  it('throws SendplaneError like the generic methods', async () => {
    const { api } = client(() => ({
      status: 403,
      json: { code: 'forbidden', message: 'role may not delivery.read' },
    }))
    await expect(api.listDeliveries({ email: 'a@example.com' })).rejects.toMatchObject({
      code: 'forbidden',
      status: 403,
    })
  })
})

describe('i18n YAML wrappers', () => {
  const yaml = 'default_locale: en\nlocales:\n  en:\n    welcome.title: Welcome\n'

  it('asks for YAML explicitly through format and Accept', async () => {
    const { api, mock } = client(() => ({ text: yaml, contentType: 'application/x-yaml' }))
    const got = await api.getI18nYaml('t1')
    const url = new URL(mock.calls[0]!.url)
    expect(url.pathname).toBe('/api/v1/templates/t1/i18n')
    expect(url.searchParams.get('format')).toBe('yaml')
    expect(mock.calls[0]!.headers.get('Accept')).toBe('application/x-yaml')
    expect(got).toBe(yaml)
  })

  it('imports YAML and returns the JSON summary', async () => {
    const { api, mock } = client(() => ({
      json: { locales: 2, keys: 7, missing_keys: [{ key: 'cta.label', locale: 'ko' }] },
    }))
    const result = await api.putI18nYaml('t1', yaml)
    expect(mock.calls[0]!.method).toBe('PUT')
    expect(mock.calls[0]!.headers.get('Content-Type')).toBe('application/x-yaml')
    expect(mock.calls[0]!.body).toBe(yaml)
    expect(result.missing_keys?.[0]?.key).toBe('cta.label')
  })
})
