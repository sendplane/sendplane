-- Sensitive transactional deliveries (store.Delivery, ADR-0019).
--
-- A delivery sent with secret_vars (an OTP, a one-time link) keeps them here,
-- encrypted, only until the message goes out:
--
--   secret_ct:         bytea, the ciphertext sealed with
--                      store.DeliverySecretContext (bound to the tenant, the
--                      delivery id and a hash of email_norm). NULL whenever
--                      secret_state is not 1.
--   secret_expires_at: when a pending secret stops being sendable. NULL for
--                      "no expiry", like every other zero time.Time here.
--   secret_state:      smallint, store.SecretState (0 none, 1 pending,
--                      2 consumed, 3 discarded, 4 expired). 0 is every
--                      ordinary delivery, so existing rows need no backfill.
--
-- The invariant every statement in store/postgres/delivery.go keeps: when a
-- row leaves secret_state 1, secret_ct becomes NULL in the same UPDATE, and
-- nothing ever sets secret_state back to 1.

ALTER TABLE delivery
  ADD COLUMN IF NOT EXISTS secret_ct         bytea,
  ADD COLUMN IF NOT EXISTS secret_expires_at timestamptz,
  ADD COLUMN IF NOT EXISTS secret_state      smallint NOT NULL DEFAULT 0;

-- ExpireSecrets walks a tenant's pending secrets in expiry order. Pending
-- secrets are few and short-lived, so the partial index stays small however
-- many deliveries the table holds.
CREATE INDEX IF NOT EXISTS delivery_secret_expiry
  ON delivery (tenant_id, secret_expires_at) WHERE secret_state = 1;
