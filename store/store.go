package store

import "context"

// Provider owns the connections and hands out tenant-bound stores.
type Provider interface {
	// ForTenant returns a Store scoped to tenantID. It never returns
	// ErrNotFound: sendplane does not manage tenant lifecycle.
	ForTenant(ctx context.Context, tenantID string) (Store, error)
	// ActiveTenants lists the tenants a sender should poll. A shared
	// implementation derives it from the tenants that have work; a routed one
	// from its configured mapping (ADR-0006).
	ActiveTenants(ctx context.Context) ([]string, error)
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
