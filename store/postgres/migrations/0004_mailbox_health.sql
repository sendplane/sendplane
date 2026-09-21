-- Mailbox reachability (store.MailboxHealth, architecture 11.5, ADR-0015).
--
-- Probe and bounce mailboxes get the same five columns: an operator has to be
-- able to see "sendplane can no longer log in to this account" without reading
-- it out of a probe verdict, which is about deliverability and not about
-- credentials.
--
-- The columns are written by UpdateHealth alone, which touches neither
-- `version` nor `updated_at`, so a background check never collides with an
-- operator editing the row.
--
--   health_status: smallint, store.MailboxStatus (0 unknown, 1 ok, 2 error).
--   health_checked_at / health_last_ok_at: NULL for "never", like every other
--   zero time.Time in this schema.

ALTER TABLE probe_mailbox
  ADD COLUMN IF NOT EXISTS health_status        smallint    NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS health_stage         text        NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS health_reason        text        NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS health_checked_at    timestamptz,
  ADD COLUMN IF NOT EXISTS health_last_ok_at    timestamptz,
  ADD COLUMN IF NOT EXISTS health_failures      integer     NOT NULL DEFAULT 0;

ALTER TABLE bounce_mailbox
  ADD COLUMN IF NOT EXISTS health_status        smallint    NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS health_stage         text        NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS health_reason        text        NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS health_checked_at    timestamptz,
  ADD COLUMN IF NOT EXISTS health_last_ok_at    timestamptz,
  ADD COLUMN IF NOT EXISTS health_failures      integer     NOT NULL DEFAULT 0;

-- No index: the mailbox-check loop reads a tenant's whole enabled set (a
-- handful of rows, already covered by *_mailbox_list / bounce_mailbox_enabled)
-- and picks the due ones in Go.
