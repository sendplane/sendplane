-- Probe mailbox inbound channel (store.ProbeMailboxKind, ADR-0016).
--
-- A probe mail comes back either over IMAP, which sendplane polls, or over a
-- webhook the receiving provider posts to sendplane's global inbound endpoint.
-- The two need the same row - address, authserv-id, enabled, health - and
-- differ only in whether the IMAP block is filled in.
--
--   kind: '' (the pre-0005 rows) and 'imap' are the same thing; 'webhook'
--   leaves host/port/tls/username/password/inbox_folder/spam_folder empty,
--   which the API refuses to let a caller set.
--
-- No backfill: ProbeMailboxKind.Normalized() reads '' as imap, so a rewrite of
-- every existing row would only be churn.

ALTER TABLE probe_mailbox
  ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT '';

-- No index: the probe trigger and the collector read a tenant's whole mailbox
-- list (a handful of rows, covered by probe_mailbox_list) and filter in Go.
