-- The tenant's outbox subscription filter (docs/architecture.md 12).
--
-- jsonb rather than text[] for the same reason retry and tracking are jsonb:
-- the repository marshals the Go value as a whole, so a list needs no array
-- type mapping. NULL and 'null' both read back as an empty slice, which is
-- what "the default set" is spelled as (store.TenantSettings.EventTypes), so
-- existing rows keep behaving exactly as they did before this column existed.
ALTER TABLE tenant_settings
  ADD COLUMN IF NOT EXISTS event_types jsonb;
