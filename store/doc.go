// Package store defines the persistence contract for sendplane: plain model
// structs plus one repository interface per aggregate. Postgres, MongoDB and
// custom backends implement Provider; store/storetest is the conformance
// suite every implementation must pass (ADR-0007).
//
// # Tenancy
//
// Provider.ForTenant returns a Store that is already bound to a tenant, so no
// repository method takes a tenant ID. A shared-mode implementation adds the
// tenant predicate itself, which makes a missing tenant filter impossible to
// write (ADR-0006). Reading an object of another tenant returns ErrNotFound.
//
// # Contract rules (ADR-0007)
//
//   - No multi-repository transactions. Implementations may use them, but the
//     contract never requires atomicity across two repositories, so MongoDB
//     standalone and custom key-value backends stay implementable. Atomicity
//     is obtained from idempotent inserts, compare-and-set transitions and the
//     event outbox instead.
//
//   - Idempotent inserts. DeliveryRepo.InsertBatch is unique on
//     (campaign_id, email_norm) for campaign deliveries: re-inserting the same
//     batch inserts nothing and is not an error. Deliveries without a campaign
//     (transactional and probe) are never deduplicated.
//
//   - CAS transitions. Every state transition is conditional on the state the
//     caller observed. Claim moves queued/deferred to leased; Complete and
//     MarkSent only apply while the row is still leased by that exact owner
//     and are silently ignored otherwise (Complete) or report ErrLeaseLost
//     (MarkSent). Delivery processing is therefore at-least-once (ADR-0002).
//
//   - Optimistic concurrency. Mutable aggregates carry a Version that Update
//     matches and bumps; a mismatch is ErrConflict. On success Update writes
//     the new Version (and UpdatedAt) back into the caller's struct.
//
//   - Explicit now. Every method whose result depends on the current time
//     (Claim, ReleaseExpiredLeases, Requeue, Locks, Outbox, IsSuppressed)
//     takes the timestamp as a parameter so that callers and tests control it.
//
//   - Cursor pagination. List methods take a Page and return a Result whose
//     NextCursor is empty on the last page. Cursors are opaque and stable:
//     inserting rows after a page was read never repeats or skips a row that
//     already existed.
//
// # Times and nullability
//
// Every implementation stores times at millisecond resolution and returns
// them in UTC; callers must not depend on finer precision. Implementations
// truncate on write (TruncateTime), so an instant handed to a repository
// comes back truncated, and a caller comparing the two compares through
// TruncateTime.
//
// A zero time.Time means SQL NULL; the conditional updates (SetFirstOpened,
// SetFirstClicked, SetUnsubscribed) act on that, and TruncateTime keeps the
// zero time zero. Free-form recipient data is map[string]any, opaque payloads
// are json.RawMessage. IDs are UUIDv7 strings (NewID).
package store
