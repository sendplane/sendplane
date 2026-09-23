-- Shared templates and layouts with tenant overrides (ADR-0018,
-- architecture 6).
--
--   key
--     An optional, per-tenant unique name for a template or layout
--     ("welcome", "receipt.v2"). It is what identifies "the same" template
--     across tenants: a tenant's template whose key equals a shared system
--     template's key *overrides* it. The empty string means "no key" and is
--     exempt from the uniqueness, which is why the index is partial. No
--     backfill: existing rows simply have no key.
--
--   shared
--     Only meaningful in the `_system` tenant: the template (or layout) is
--     readable by every tenant, read-through, never copied. The platform
--     overlay (store.WithPlatform) serves it. No index: the system tenant
--     holds the operator's handful of templates, and the overlay reads them
--     through the ordinary tenant listing.
--
--   uses
--     What a shared template may be used for (campaign, transactional); NULL
--     means everything. Enforced by the template policy hook, not here.
--
--   overridden_from_version
--     On a tenant's override: the shared template's published version id
--     when the copy was made, so a console can say "the shared original has
--     changed since you overrode it".

ALTER TABLE template ADD COLUMN IF NOT EXISTS key                     text    NOT NULL DEFAULT '';
ALTER TABLE template ADD COLUMN IF NOT EXISTS shared                  boolean NOT NULL DEFAULT false;
ALTER TABLE template ADD COLUMN IF NOT EXISTS uses                    text[];
ALTER TABLE template ADD COLUMN IF NOT EXISTS overridden_from_version text    NOT NULL DEFAULT '';

ALTER TABLE layout ADD COLUMN IF NOT EXISTS key    text    NOT NULL DEFAULT '';
ALTER TABLE layout ADD COLUMN IF NOT EXISTS shared boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX IF NOT EXISTS template_key ON template (tenant_id, key) WHERE key <> '';
CREATE UNIQUE INDEX IF NOT EXISTS layout_key   ON layout   (tenant_id, key) WHERE key <> '';
