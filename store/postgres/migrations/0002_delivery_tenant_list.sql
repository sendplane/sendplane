-- Indexes for the tenant-wide delivery listing (GET /api/v1/deliveries).
--
-- delivery_list is (tenant_id, campaign_id, created_at, id): with no campaign
-- in the predicate its leading column is the only usable one, so a tenant-wide
-- page would sort the tenant's whole delivery table. These two cover the
-- queries that listing actually issues.

-- Keyset pagination over every delivery of the tenant: the ORDER BY
-- (created_at, id) is read straight off the index, and the status, lane and
-- error_class filters are checked on the rows it returns.
CREATE INDEX IF NOT EXISTS delivery_tenant_list
  ON delivery (tenant_id, created_at, id);

-- "Find this address across campaigns", the reason the endpoint exists. The
-- address is exact (email_norm), so created_at both narrows a since/until
-- window and gives the sort order inside one address.
CREATE INDEX IF NOT EXISTS delivery_tenant_email
  ON delivery (tenant_id, email_norm, created_at);
