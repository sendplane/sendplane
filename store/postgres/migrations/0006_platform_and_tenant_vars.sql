-- Multi-tenant SaaS support: platform state shadow rows and tenant variables
-- (ADR-0017, architecture 5.4).
--
-- Two unrelated-looking additions that come from the same decision: sendplane
-- stores neither the operator's shared *configuration* nor its tenants'
-- *attributes*.
--
--   shared
--     Marks a row that is not a tenant's own configuration. In practice that
--     is only ever a state *shadow* row in the `_system` tenant: the platform
--     overlay (store.WithPlatform) resolves a shared transport, domain,
--     sender or mailbox from the config file on every read, and persists just
--     its runtime state — circuit status, probe verdict, mailbox
--     reachability — under the virtual id ('sys:<name>').
--
--     A shadow row therefore has every configuration column empty by
--     construction: no host, no port, no username, no password, no DKIM key.
--     That is the invariant, not an accident of the current writer, and it is
--     what a dump of this database can be checked against:
--
--       SELECT id, name, host, port, username, password FROM transport
--        WHERE shared;   -- name/host/username '' , port 0, password NULL
--
--     No index: the overlay reads a shadow row by primary key (the virtual
--     id) and never scans for one.
--
--   tenant_vars
--     The tenant attributes one send was requested with (name, slug, plan,
--     whatever the host's TenantVars hook admits), bound as `tenant` in every
--     template and in a platform sender's From templates. They are per
--     request, not per tenant: there is no tenant table, on purpose
--     (ADR-0017), so nothing here is a registry that could go stale against
--     the host's own.
--
--     campaign.tenant_vars carries a bulk send's set. delivery.tenant_vars
--     carries only a delivery that has no campaign to inherit from — a
--     transactional send or a probe — so starting a campaign stays a one-row
--     write and a million recipients do not each store a copy of the same
--     object (ADR-0002).

ALTER TABLE transport      ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;
ALTER TABLE sender         ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;
ALTER TABLE sending_domain ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;
ALTER TABLE probe_mailbox  ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;
ALTER TABLE bounce_mailbox ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;

ALTER TABLE campaign ADD COLUMN IF NOT EXISTS tenant_vars jsonb;
ALTER TABLE delivery ADD COLUMN IF NOT EXISTS tenant_vars jsonb;
