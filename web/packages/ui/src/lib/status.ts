import type {
  BounceType,
  CampaignStatus,
  DeliveryStatus,
  ErrorClass,
  HealthStatus,
  OutboxStatus,
  TransportStatus,
} from '@sendplane/api'

import type { components } from '@sendplane/api'

type MailboxStatus = components['schemas']['MailboxStatus']

/** The visual families the badge paints; everything else maps onto these. */
export type Tone = 'neutral' | 'info' | 'ok' | 'warn' | 'danger'

/** Which vocabulary a status value belongs to. */
export type StatusKind =
  | 'delivery'
  | 'campaign'
  | 'transport'
  | 'health'
  | 'outbox'
  | 'bounce'
  | 'errorClass'
  | 'mailbox'

const DELIVERY: Record<DeliveryStatus, Tone> = {
  pending: 'neutral',
  queued: 'info',
  leased: 'info',
  deferred: 'warn',
  sent: 'ok',
  failed: 'danger',
  bounced: 'danger',
  complained: 'danger',
  suppressed: 'warn',
  cancelled: 'neutral',
}

const CAMPAIGN: Record<CampaignStatus, Tone> = {
  draft: 'neutral',
  scheduled: 'info',
  running: 'info',
  paused: 'warn',
  completed: 'ok',
  cancelled: 'neutral',
}

const TRANSPORT: Record<TransportStatus, Tone> = {
  healthy: 'ok',
  cooldown: 'warn',
  unhealthy: 'danger',
}

const HEALTH: Record<HealthStatus, Tone> = {
  unknown: 'neutral',
  green: 'ok',
  yellow: 'warn',
  red: 'danger',
}

const OUTBOX: Record<OutboxStatus, Tone> = {
  pending: 'info',
  delivered: 'ok',
  failed: 'danger',
}

const BOUNCE: Record<BounceType, Tone> = {
  unknown: 'neutral',
  hard: 'danger',
  soft: 'warn',
  complaint: 'danger',
}

const ERROR_CLASS: Record<ErrorClass, Tone> = {
  none: 'neutral',
  transient: 'warn',
  rate_limited: 'warn',
  permanent: 'danger',
  policy: 'danger',
  auth: 'danger',
}

// Deliberately not the green/yellow/red `HEALTH` table above: a mailbox login
// either works or it does not (ADR-0015, `MailboxStatus`'s own description).
const MAILBOX: Record<MailboxStatus, Tone> = {
  unknown: 'neutral',
  ok: 'ok',
  error: 'danger',
}

const TABLES: Record<StatusKind, Record<string, Tone>> = {
  delivery: DELIVERY,
  campaign: CAMPAIGN,
  transport: TRANSPORT,
  health: HEALTH,
  outbox: OUTBOX,
  bounce: BOUNCE,
  errorClass: ERROR_CLASS,
  mailbox: MAILBOX,
}

export function toneFor(kind: StatusKind, value: string | undefined): Tone {
  if (!value) return 'neutral'
  return TABLES[kind][value] ?? 'neutral'
}

/** Message key for the label, so every status reads in the operator's locale. */
export function labelKeyFor(kind: StatusKind, value: string | undefined): string {
  return `status.${kind}.${value ?? 'unknown'}`
}
