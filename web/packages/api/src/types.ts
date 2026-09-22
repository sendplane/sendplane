import type { components, operations, paths } from './schema.js'

export type { components, operations, paths }

type S = components['schemas']

/** Every named schema, so callers can write `Schemas['Campaign']`. */
export type Schemas = S

// Enumerations -----------------------------------------------------------
export type DeliveryStatus = S['DeliveryStatus']
export type CampaignStatus = S['CampaignStatus']
export type TransportStatus = S['TransportStatus']
export type HealthStatus = S['HealthStatus']
export type ErrorClass = S['ErrorClass']
export type Lane = S['Lane']
export type BounceType = S['BounceType']
export type BounceSource = S['BounceSource']
export type OutboxStatus = S['OutboxStatus']
export type ContentMode = S['ContentMode']
export type TLSMode = S['TLSMode']
export type UnsubscribeMode = S['UnsubscribeMode']
export type SuppressionReason = S['SuppressionReason']

// Objects ----------------------------------------------------------------
export type TenantSettings = S['TenantSettings']
export type TenantSettingsInput = S['TenantSettingsInput']
export type RetryPolicy = S['RetryPolicy']
export type TrackingConfig = S['TrackingConfig']
export type Transport = S['Transport']
export type TransportInput = S['TransportInput']
export type TransportHealth = S['TransportHealth']
export type Sender = S['Sender']
export type SenderInput = S['SenderInput']
export type SenderHealth = S['SenderHealth']
export type SendingDomain = S['SendingDomain']
export type SendingDomainInput = S['SendingDomainInput']
export type ProbeMailbox = S['ProbeMailbox']
export type ProbeMailboxInput = S['ProbeMailboxInput']
export type ProbeRun = S['ProbeRun']
export type Layout = S['Layout']
export type LayoutInput = S['LayoutInput']
export type Template = S['Template']
export type TemplateInput = S['TemplateInput']
export type MessageVersion = S['MessageVersion']
export type PreviewRequest = S['PreviewRequest']
export type PreviewRecipient = S['PreviewRecipient']
export type PreviewResult = S['PreviewResult']
export type I18nBundle = S['I18nBundle']
export type I18nKeyList = S['I18nKeyList']
export type I18nKeyUsage = S['I18nKeyUsage']
export type I18nKeyIssue = S['I18nKeyIssue']
export type I18nImportResult = S['I18nImportResult']
export type Campaign = S['Campaign']
export type CampaignInput = S['CampaignInput']
export type CampaignStats = S['CampaignStats']
export type RecipientLine = S['RecipientLine']
export type RecipientIngestResult = S['RecipientIngestResult']
export type LinkClick = S['LinkClick']
export type RetryRequest = S['RetryRequest']
export type Delivery = S['Delivery']
export type DeliveryAttempt = S['DeliveryAttempt']
export type BounceEvent = S['BounceEvent']
export type Suppression = S['Suppression']
export type SuppressionInput = S['SuppressionInput']
export type OutboxEvent = S['OutboxEvent']
export type MessageRequest = S['MessageRequest']
export type MessageResult = S['MessageResult']
export type Vars = S['Vars']
export type ServiceHealth = S['ServiceHealth']
export type Whoami = S['Whoami']
export type TenantVars = S['TenantVars']
export type SettingSource = S['SettingSource']
export type MessageRecipient = S['MessageRecipient']
export type MessageResultItem = S['MessageResultItem']
/**
 * An entity ID: either a UUIDv7 sendplane minted, or `sys:<name>` for a
 * platform resource resolved from the operator's configuration (ADR-0017).
 * It is an opaque string; nothing client-side may validate it as a UUID.
 */
export type ResourceId = S['ResourceId']
/** What a shared sender may be used for (`Sender.uses`). */
export type SenderUse = NonNullable<Sender['uses']>[number]

// List envelopes, for callers that hold a page rather than its items.
export type TransportList = S['TransportList']
export type SenderList = S['SenderList']
export type SendingDomainList = S['SendingDomainList']
export type ProbeMailboxList = S['ProbeMailboxList']
export type ProbeRunList = S['ProbeRunList']
export type LayoutList = S['LayoutList']
export type TemplateList = S['TemplateList']
export type MessageVersionList = S['MessageVersionList']
export type CampaignList = S['CampaignList']
export type DeliveryList = S['DeliveryList']
export type DeliveryAttemptList = S['DeliveryAttemptList']
export type BounceEventList = S['BounceEventList']
export type SuppressionList = S['SuppressionList']
export type OutboxEventList = S['OutboxEventList']
export type LinkClickList = S['LinkClickList']
export type RetryResult = S['RetryResult']
export type UnsubscribeNotice = S['UnsubscribeNotice']
export type UnsubscribeResult = S['UnsubscribeResult']
export type ProbeTriggerResult = S['ProbeTriggerResult']
export type SigningKeyInfo = S['SigningKeyInfo']

/** Shared by every list response: `{ items, next_cursor? }`. */
export interface Page<T> {
  items: T[]
  next_cursor?: string
}

// The lane filter of GET /api/v1/deliveries, in the spec's order.
export const LANES = ['bulk', 'transactional', 'probe'] as const satisfies readonly Lane[]

export const DELIVERY_STATUSES = [
  'pending',
  'queued',
  'leased',
  'deferred',
  'sent',
  'failed',
  'bounced',
  'complained',
  'suppressed',
  'cancelled',
] as const satisfies readonly DeliveryStatus[]

export const CAMPAIGN_STATUSES = [
  'draft',
  'scheduled',
  'running',
  'paused',
  'completed',
  'cancelled',
] as const satisfies readonly CampaignStatus[]

export const ERROR_CLASSES = [
  'none',
  'transient',
  'rate_limited',
  'permanent',
  'policy',
  'auth',
] as const satisfies readonly ErrorClass[]

export const HEALTH_STATUSES = [
  'unknown',
  'green',
  'yellow',
  'red',
] as const satisfies readonly HealthStatus[]

export const TRANSPORT_STATUSES = [
  'healthy',
  'cooldown',
  'unhealthy',
] as const satisfies readonly TransportStatus[]

export const SENDER_USES = [
  'campaign',
  'transactional',
  'probe',
] as const satisfies readonly SenderUse[]

/**
 * The operator's own scope. It sees platform transports, domains, mailboxes and
 * their state, and it cannot create campaigns or send messages (ADR-0017).
 */
export const SYSTEM_TENANT_ID = '_system'

/**
 * Header a privileged caller switches tenant with. sendplane itself never
 * reads it: honouring it is the host `TenantResolver`'s decision, which is what
 * `Whoami.can_switch_tenant` reports.
 */
export const TENANT_HEADER = 'X-Sendplane-Tenant'
