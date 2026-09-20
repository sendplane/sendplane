export {
  createClient,
  type AuthHeaders,
  type IngestOptions,
  type SendplaneClient,
  type SendplaneClientOptions,
  type UnwrappedMethod,
} from './client.js'

export {
  SendplaneError,
  isSendplaneError,
  type ApiError,
  type ApiErrorCode,
  type ApiErrorDetail,
  type SendplaneErrorInit,
  type TransportErrorCode,
} from './errors.js'

export {
  collect,
  encodeNdjson,
  parseCsv,
  recipientsFromCsvBlob,
  recipientsFromFile,
  recipientsFromNdjsonBlob,
  supportsRequestStreams,
  toReadableStream,
  type IngestProgress,
  type RecipientChunk,
  type RecipientSource,
} from './ndjson.js'

export * from './types.js'
