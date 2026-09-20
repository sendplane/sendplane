package store

import "context"

// SystemTenantID is the scope for cluster-wide state that belongs to no
// customer: the control plane's leader lock (LockRepo) lives in it.
//
// Store is tenant-bound on purpose (ADR-0006), so a singleton with no tenant
// still needs a tenant ID. The name starts with an underscore, which a tenant
// ID handed out by a host never does. A custom Provider must serve it like any
// other tenant, and ActiveTenants never returns it: nothing is ever sent for
// it, and a control loop that ticked it would be ticking a tenant that does
// not exist.
const SystemTenantID = "_system"

// Provider owns the connections and hands out tenant-bound stores.
type Provider interface {
	// ForTenant returns a Store scoped to tenantID. It never returns
	// ErrNotFound: sendplane does not manage tenant lifecycle.
	ForTenant(ctx context.Context, tenantID string) (Store, error)
	// ActiveTenants lists the tenants that have work in progress: the ones a
	// sender should poll and the ones a control loop should tick. A shared
	// implementation derives it from the data; a routed one may return its
	// configured mapping instead (ADR-0006).
	//
	// A tenant is active when it has either
	//
	//   - a delivery in a non-terminal status (pending, queued, leased or
	//     deferred), which is what a sender claims from, or
	//   - a campaign in status scheduled, running or paused, which is what the
	//     scheduler, finalizer and canceller act on.
	//
	// The campaign half is what makes the last tick of a campaign reachable:
	// the finalizer has to see the tenant exactly when its last delivery has
	// gone terminal, and by then the delivery half no longer reports it.
	//
	// SystemTenantID is never returned.
	ActiveTenants(ctx context.Context) ([]string, error)
	// Tenants lists every tenant this provider knows of, active or not. It is
	// what the bounce poller iterates: a DSN arrives hours or days after a
	// campaign finished, long after the tenant left ActiveTenants, so a bounce
	// mailbox belonging to an idle tenant would never be polled if the poller
	// only saw active ones.
	//
	// A tenant is known when it has a settings row or a row in one of the
	// configuration aggregates: transports, senders, sending domains, bounce
	// mailboxes, probe mailboxes, layouts, templates or campaigns. Those are
	// the small, tenant-keyed tables, so the scan stays cheap; the per-
	// recipient tables (deliveries, attempts, tracking) are deliberately not
	// scanned, and a tenant that somehow only has those is reached through
	// ActiveTenants instead.
	//
	// It is not a lifecycle API: sendplane does not create or delete tenants
	// (ADR-0006), and a routed Provider may return its configured mapping
	// instead. Implementations may be expensive, so callers refresh it on an
	// interval rather than per operation.
	//
	// SystemTenantID is never returned, and the result is sorted.
	Tenants(ctx context.Context) ([]string, error)
	// Migrate brings the schema (or indexes) up to date.
	Migrate(ctx context.Context) error
	Close() error
}

// Store is bound to one tenant: no repository method takes a tenant ID.
type Store interface {
	TenantSettings() TenantSettingsRepo
	Transports() TransportRepo
	Senders() SenderRepo
	Domains() DomainRepo
	BounceMailboxes() BounceMailboxRepo
	ProbeMailboxes() ProbeMailboxRepo
	ProbeRuns() ProbeRunRepo
	Layouts() LayoutRepo
	Templates() TemplateRepo
	Versions() MessageVersionRepo
	Campaigns() CampaignRepo
	RecipientChunks() RecipientChunkRepo
	Deliveries() DeliveryRepo
	Attempts() AttemptRepo
	Suppressions() SuppressionRepo
	Bounces() BounceRepo
	Tracking() TrackingRepo
	Outbox() OutboxRepo
	Locks() LockRepo
	Workers() WorkerRepo
}
