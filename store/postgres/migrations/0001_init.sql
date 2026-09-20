-- sendplane shared-mode schema (ADR-0006, architecture 5.2).
--
-- Every table carries tenant_id and every query written against this schema
-- filters on it, which is what makes a cross-tenant read return ErrNotFound.
--
-- Conventions:
--   * IDs are UUIDv7 strings, stored as text.
--   * Enums are smallint; their numeric values are the on-disk format
--     (store/enums.go).
--   * A zero time.Time is SQL NULL, which is what the conditional updates
--     (set_first_*) and the "never expires" suppression rely on.
--   * timestamptz holds microseconds, but the contract's resolution is
--     milliseconds (store/doc.go): every instant is truncated on the way in,
--     so a stored value and a comparison bound built from the same time.Time
--     match exactly and every backend returns the same thing.
--   * Optional strings are NOT NULL DEFAULT '': Go has no NULL string. The one
--     exception is delivery.campaign_id, which must be NULL for "no campaign"
--     so that the partial unique index only covers campaign deliveries.
--   * Structured values (i18n bundles, retry policy, stats, vars) are jsonb.

CREATE TABLE IF NOT EXISTS tenant_settings (
  tenant_id                text PRIMARY KEY,
  retry                    jsonb,
  retention_days           integer     NOT NULL DEFAULT 0,
  suppression_enabled      boolean     NOT NULL DEFAULT false,
  bounce_retain_raw        boolean     NOT NULL DEFAULT false,
  unsubscribe_mode         text        NOT NULL DEFAULT '',
  unsubscribe_url_template text        NOT NULL DEFAULT '',
  unsubscribe_one_click    boolean     NOT NULL DEFAULT false,
  default_locale           text        NOT NULL DEFAULT '',
  tracking                 jsonb,
  version                  bigint      NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL,
  updated_at               timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS transport (
  id                     text PRIMARY KEY,
  tenant_id              text        NOT NULL,
  name                   text        NOT NULL DEFAULT '',
  host                   text        NOT NULL DEFAULT '',
  port                   integer     NOT NULL DEFAULT 0,
  tls                    text        NOT NULL DEFAULT '',
  username               text        NOT NULL DEFAULT '',
  password               bytea,
  max_conns              integer     NOT NULL DEFAULT 0,
  rate_per_second        double precision NOT NULL DEFAULT 0,
  domain_rate_per_second jsonb,
  status                 smallint    NOT NULL DEFAULT 0,
  status_reason          text        NOT NULL DEFAULT '',
  status_changed_at      timestamptz,
  status_until           timestamptz,
  version                bigint      NOT NULL DEFAULT 1,
  created_at             timestamptz NOT NULL,
  updated_at             timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS transport_list ON transport (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS sender (
  id                text PRIMARY KEY,
  tenant_id         text        NOT NULL,
  name              text        NOT NULL DEFAULT '',
  from_name         text        NOT NULL DEFAULT '',
  from_email        text        NOT NULL DEFAULT '',
  reply_to          text        NOT NULL DEFAULT '',
  transport_id      text        NOT NULL DEFAULT '',
  domain_id         text        NOT NULL DEFAULT '',
  health            smallint    NOT NULL DEFAULT 0,
  health_reason     text        NOT NULL DEFAULT '',
  health_checked_at timestamptz,
  version           bigint      NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL,
  updated_at        timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sender_list ON sender (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS sending_domain (
  id                 text PRIMARY KEY,
  tenant_id          text        NOT NULL,
  domain             text        NOT NULL DEFAULT '',
  dkim_selector      text        NOT NULL DEFAULT '',
  dkim_private_key   bytea,
  return_path_domain text        NOT NULL DEFAULT '',
  expected_spf       text        NOT NULL DEFAULT '',
  outbound_ips       text[],
  health             smallint    NOT NULL DEFAULT 0,
  health_reason      text        NOT NULL DEFAULT '',
  health_checked_at  timestamptz,
  version            bigint      NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL,
  updated_at         timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sending_domain_list ON sending_domain (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS bounce_mailbox (
  id            text PRIMARY KEY,
  tenant_id     text        NOT NULL,
  name          text        NOT NULL DEFAULT '',
  address       text        NOT NULL DEFAULT '',
  protocol      text        NOT NULL DEFAULT '',
  host          text        NOT NULL DEFAULT '',
  port          integer     NOT NULL DEFAULT 0,
  tls           text        NOT NULL DEFAULT '',
  username      text        NOT NULL DEFAULT '',
  password      bytea,
  folder        text        NOT NULL DEFAULT '',
  after_process text        NOT NULL DEFAULT '',
  enabled       boolean     NOT NULL DEFAULT false,
  version       bigint      NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL,
  updated_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS bounce_mailbox_list ON bounce_mailbox (tenant_id, created_at, id);
-- The poller reads the enabled set of one tenant on every refresh.
CREATE INDEX IF NOT EXISTS bounce_mailbox_enabled
  ON bounce_mailbox (tenant_id, created_at, id) WHERE enabled;

CREATE TABLE IF NOT EXISTS probe_mailbox (
  id           text PRIMARY KEY,
  tenant_id    text        NOT NULL,
  name         text        NOT NULL DEFAULT '',
  address      text        NOT NULL DEFAULT '',
  host         text        NOT NULL DEFAULT '',
  port         integer     NOT NULL DEFAULT 0,
  tls          text        NOT NULL DEFAULT '',
  username     text        NOT NULL DEFAULT '',
  password     bytea,
  inbox_folder text        NOT NULL DEFAULT '',
  spam_folder  text        NOT NULL DEFAULT '',
  authserv_id  text        NOT NULL DEFAULT '',
  enabled      boolean     NOT NULL DEFAULT false,
  version      bigint      NOT NULL DEFAULT 1,
  created_at   timestamptz NOT NULL,
  updated_at   timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS probe_mailbox_list ON probe_mailbox (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS probe_run (
  id            text PRIMARY KEY,
  tenant_id     text        NOT NULL,
  sender_id     text        NOT NULL DEFAULT '',
  mailbox_id    text        NOT NULL DEFAULT '',
  delivery_id   text        NOT NULL DEFAULT '',
  group_id      text        NOT NULL DEFAULT '',
  pending       boolean     NOT NULL DEFAULT false,
  status        smallint    NOT NULL DEFAULT 0,
  reason        text        NOT NULL DEFAULT '',
  delivered     boolean     NOT NULL DEFAULT false,
  folder        text        NOT NULL DEFAULT '',
  latency       bigint      NOT NULL DEFAULT 0,
  spf           text        NOT NULL DEFAULT '',
  dkim          text        NOT NULL DEFAULT '',
  dmarc         text        NOT NULL DEFAULT '',
  dkim_domain   text        NOT NULL DEFAULT '',
  dkim_selector text        NOT NULL DEFAULT '',
  dmarc_policy  text        NOT NULL DEFAULT '',
  tls           boolean     NOT NULL DEFAULT false,
  observed_ip   text        NOT NULL DEFAULT '',
  ptr           text        NOT NULL DEFAULT '',
  ptr_match     boolean     NOT NULL DEFAULT false,
  dns           jsonb,
  raw_headers   text        NOT NULL DEFAULT '',
  started_at    timestamptz,
  received_at   timestamptz,
  created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS probe_run_by_sender ON probe_run (tenant_id, sender_id, created_at, id);
-- The collector asks for the runs still waiting, not for history.
CREATE INDEX IF NOT EXISTS probe_run_pending
  ON probe_run (tenant_id, created_at, id) WHERE pending;

CREATE TABLE IF NOT EXISTS layout (
  id         text PRIMARY KEY,
  tenant_id  text        NOT NULL,
  name       text        NOT NULL DEFAULT '',
  mode       text        NOT NULL DEFAULT '',
  body       text        NOT NULL DEFAULT '',
  i18n       jsonb,
  version    bigint      NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS layout_list ON layout (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS template (
  id                   text PRIMARY KEY,
  tenant_id            text        NOT NULL,
  name                 text        NOT NULL DEFAULT '',
  layout_id            text        NOT NULL DEFAULT '',
  subject              text        NOT NULL DEFAULT '',
  preheader            text        NOT NULL DEFAULT '',
  mode                 text        NOT NULL DEFAULT '',
  body                 text        NOT NULL DEFAULT '',
  blocks               jsonb,
  text_body            text        NOT NULL DEFAULT '',
  i18n                 jsonb,
  default_locale       text        NOT NULL DEFAULT '',
  published_version_id text        NOT NULL DEFAULT '',
  version              bigint      NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL,
  updated_at           timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS template_list ON template (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS message_version (
  id             text PRIMARY KEY,
  tenant_id      text        NOT NULL,
  template_id    text        NOT NULL DEFAULT '',
  layout_id      text        NOT NULL DEFAULT '',
  subject_tpl    text        NOT NULL DEFAULT '',
  html_tpl       text        NOT NULL DEFAULT '',
  text_tpl       text        NOT NULL DEFAULT '',
  i18n           jsonb,
  default_locale text        NOT NULL DEFAULT '',
  links          text[],
  checksum       text        NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS message_version_by_template ON message_version (tenant_id, template_id, created_at, id);

CREATE TABLE IF NOT EXISTS campaign (
  id             text PRIMARY KEY,
  tenant_id      text        NOT NULL,
  name           text        NOT NULL DEFAULT '',
  template_id    text        NOT NULL DEFAULT '',
  version_id     text        NOT NULL DEFAULT '',
  sender_id      text        NOT NULL DEFAULT '',
  default_locale text        NOT NULL DEFAULT '',
  vars           jsonb,
  status         smallint    NOT NULL DEFAULT 0,
  schedule_at    timestamptz,
  started_at     timestamptz,
  completed_at   timestamptz,
  stats          jsonb,
  version        bigint      NOT NULL DEFAULT 1,
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS campaign_list ON campaign (tenant_id, created_at, id);
CREATE INDEX IF NOT EXISTS campaign_by_status ON campaign (tenant_id, status, created_at, id);

CREATE TABLE IF NOT EXISTS recipient_chunk (
  tenant_id   text        NOT NULL,
  campaign_id text        NOT NULL,
  key         text        NOT NULL,
  state       text        NOT NULL DEFAULT '',
  accepted    integer     NOT NULL DEFAULT 0,
  duplicates  integer     NOT NULL DEFAULT 0,
  invalid     integer     NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL,
  updated_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, campaign_id, key)
);

CREATE TABLE IF NOT EXISTS delivery (
  id               text PRIMARY KEY,
  tenant_id        text        NOT NULL,
  campaign_id      text,                     -- NULL = transactional/probe
  version_id       text        NOT NULL DEFAULT '',
  sender_id        text        NOT NULL DEFAULT '',
  lane             smallint    NOT NULL DEFAULT 0,
  priority         integer     NOT NULL DEFAULT 0,
  status           smallint    NOT NULL DEFAULT 0,
  email            text        NOT NULL DEFAULT '',
  email_norm       text        NOT NULL,
  name             text        NOT NULL DEFAULT '',
  locale           text        NOT NULL DEFAULT '',
  vars             jsonb,
  unsubscribe_url  text        NOT NULL DEFAULT '',
  headers          jsonb,
  attempt_count    integer     NOT NULL DEFAULT 0,
  retry_gen        integer     NOT NULL DEFAULT 0,
  next_attempt_at  timestamptz NOT NULL,
  lease_owner      text        NOT NULL DEFAULT '',
  lease_until      timestamptz,
  last_error_class smallint    NOT NULL DEFAULT 0,
  last_smtp_code   integer     NOT NULL DEFAULT 0,
  last_error       text        NOT NULL DEFAULT '',
  message_id       text        NOT NULL DEFAULT '',
  sent_at          timestamptz,
  finished_at      timestamptz,
  first_opened_at  timestamptz,
  first_clicked_at timestamptz,
  unsubscribed_at  timestamptz,
  created_at       timestamptz NOT NULL,
  updated_at       timestamptz NOT NULL
);

-- Idempotent ingest: re-sending the same chunk inserts nothing (ADR-0007).
-- Deliveries without a campaign are outside the index and never deduplicated.
CREATE UNIQUE INDEX IF NOT EXISTS delivery_campaign_email
  ON delivery (tenant_id, campaign_id, email_norm) WHERE campaign_id IS NOT NULL;
-- The claim query's index: only the claimable statuses are in it. Pending (0)
-- is in the set because a campaign's rows are ingested pending and become
-- claimable when the campaign joins the claimer's running set, without a bulk
-- update at start (store.DeliveryRepo.Claim, ADR-0002).
CREATE INDEX IF NOT EXISTS delivery_claim
  ON delivery (tenant_id, lane, priority DESC, next_attempt_at) WHERE status IN (0, 1, 3);
-- Lease recovery walks only leased rows.
CREATE INDEX IF NOT EXISTS delivery_lease
  ON delivery (lease_until) WHERE status = 2;
CREATE INDEX IF NOT EXISTS delivery_campaign_status
  ON delivery (tenant_id, campaign_id, status);
-- Keyset pagination and retention deletes.
CREATE INDEX IF NOT EXISTS delivery_list
  ON delivery (tenant_id, campaign_id, created_at, id);

CREATE TABLE IF NOT EXISTS delivery_attempt (
  id            text PRIMARY KEY,
  tenant_id     text        NOT NULL,
  delivery_id   text        NOT NULL,
  attempt_no    integer     NOT NULL DEFAULT 0,
  retry_gen     integer     NOT NULL DEFAULT 0,
  transport_id  text        NOT NULL DEFAULT '',
  started_at    timestamptz,
  finished_at   timestamptz,
  smtp_code     integer     NOT NULL DEFAULT 0,
  enhanced_code text        NOT NULL DEFAULT '',
  error_class   smallint    NOT NULL DEFAULT 0,
  error_text    text        NOT NULL DEFAULT '',
  created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_attempt_by_delivery
  ON delivery_attempt (tenant_id, delivery_id, created_at, id);

CREATE TABLE IF NOT EXISTS bounce_event (
  id              text PRIMARY KEY,
  tenant_id       text        NOT NULL,
  delivery_id     text        NOT NULL DEFAULT '',
  type            smallint    NOT NULL DEFAULT 0,
  source          text        NOT NULL DEFAULT '',
  verified        boolean     NOT NULL DEFAULT false,
  recipient       text        NOT NULL DEFAULT '',
  email_norm      text        NOT NULL DEFAULT '',
  smtp_status     text        NOT NULL DEFAULT '',
  diagnostic_code text        NOT NULL DEFAULT '',
  message_id      text        NOT NULL DEFAULT '',
  raw             jsonb,
  received_at     timestamptz,
  created_at      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS bounce_event_list ON bounce_event (tenant_id, created_at, id);
CREATE INDEX IF NOT EXISTS bounce_event_by_delivery
  ON bounce_event (tenant_id, delivery_id, created_at, id);

CREATE TABLE IF NOT EXISTS suppression (
  tenant_id          text        NOT NULL,
  email_norm         text        NOT NULL,
  reason             text        NOT NULL DEFAULT '',
  source_delivery_id text        NOT NULL DEFAULT '',
  created_at         timestamptz NOT NULL,
  expires_at         timestamptz,              -- NULL = never expires
  PRIMARY KEY (tenant_id, email_norm)
);
CREATE INDEX IF NOT EXISTS suppression_list ON suppression (tenant_id, created_at, email_norm);
-- Retention walks the entries that actually expire; "never expires" is NULL
-- and outside this index.
CREATE INDEX IF NOT EXISTS suppression_expiry
  ON suppression (tenant_id, expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS tracking_event (
  id            text PRIMARY KEY,
  tenant_id     text        NOT NULL,
  delivery_id   text        NOT NULL DEFAULT '',
  campaign_id   text        NOT NULL DEFAULT '',
  kind          smallint    NOT NULL DEFAULT 0,
  url           text        NOT NULL DEFAULT '',
  link_no       integer     NOT NULL DEFAULT -1,
  user_agent    text        NOT NULL DEFAULT '',
  ip_hash       text        NOT NULL DEFAULT '',
  suspected_bot boolean     NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS tracking_event_by_campaign
  ON tracking_event (tenant_id, campaign_id, kind) WHERE NOT suspected_bot;
-- Retention deletes walk (tenant_id, created_at).
CREATE INDEX IF NOT EXISTS tracking_event_retention
  ON tracking_event (tenant_id, created_at, id);

CREATE TABLE IF NOT EXISTS outbox_event (
  id              text PRIMARY KEY,
  tenant_id       text        NOT NULL,
  type            text        NOT NULL DEFAULT '',
  payload         jsonb,
  status          text        NOT NULL DEFAULT 'pending',
  attempts        integer     NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL,
  lease_owner     text        NOT NULL DEFAULT '',
  lease_until     timestamptz,
  last_error      text        NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL,
  delivered_at    timestamptz
);
CREATE INDEX IF NOT EXISTS outbox_event_pending
  ON outbox_event (tenant_id, status, next_attempt_at, id);
CREATE INDEX IF NOT EXISTS outbox_event_list
  ON outbox_event (tenant_id, status, created_at, id);

CREATE TABLE IF NOT EXISTS job_lock (
  tenant_id   text        NOT NULL,
  name        text        NOT NULL,
  owner       text        NOT NULL DEFAULT '',
  acquired_at timestamptz NOT NULL,
  expires_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS worker (
  tenant_id    text        NOT NULL,
  id           text        NOT NULL,
  role         text        NOT NULL DEFAULT '',
  lanes        smallint[],
  concurrency  integer     NOT NULL DEFAULT 0,
  started_at   timestamptz NOT NULL,
  last_seen_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS worker_active ON worker (tenant_id, last_seen_at);
