-- The claim orders by (priority DESC, next_attempt_at, id). A campaign's rows
-- are ingested in 50k-row COPY chunks that share one next_attempt_at, so
-- without id in the index every claim incremental-sorts the whole remaining
-- tie group (up to a chunk) to return a few dozen rows. With id the claim
-- reads its LIMIT straight off the index.
CREATE INDEX IF NOT EXISTS delivery_claim_v2
  ON delivery (tenant_id, lane, priority DESC, next_attempt_at, id) WHERE status IN (0, 1, 3);
DROP INDEX IF EXISTS delivery_claim;
