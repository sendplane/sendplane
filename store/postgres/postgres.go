// Package postgres is the PostgreSQL implementation of store.Provider
// (architecture 5.2, ADR-0006). It runs in shared mode: one database, every
// table carrying tenant_id, and ForTenant returning a scoped store.Store that
// adds the tenant predicate to every statement it issues.
//
// SQL is written by hand next to the Go that uses it: no ORM, no query
// builder. The schema lives in migrations/ and is applied by Migrate.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sendplane/sendplane/store"
)

// errClosed is returned by every method after Close.
var errClosed = errors.New("postgres: provider is closed")

// defaultCopyThreshold is the batch size above which InsertBatch switches from
// a multi-row INSERT to COPY into a temporary table (architecture 5.2).
const defaultCopyThreshold = 100

// Option configures a Provider.
type Option func(*Provider)

// WithClock replaces time.Now, so tests control the timestamps the store
// stamps on rows. Methods that take an explicit now (Claim,
// ReleaseExpiredLeases, locks, outbox) use that argument instead.
func WithClock(clock func() time.Time) Option {
	return func(p *Provider) { p.clock = clock }
}

// WithCopyThreshold sets the DeliveryRepo.InsertBatch batch size above which
// rows are streamed with COPY instead of a multi-row INSERT.
func WithCopyThreshold(n int) Option {
	return func(p *Provider) { p.copyThreshold = n }
}

// Provider is the shared-mode PostgreSQL store.Provider. It is safe for
// concurrent use; all state lives in the connection pool.
type Provider struct {
	pool          *pgxpool.Pool
	ownsPool      bool
	clock         func() time.Time
	copyThreshold int
	closed        bool
}

// NewShared wraps an existing pool. The pool stays owned by the caller: Close
// does not close it.
func NewShared(ctx context.Context, pool *pgxpool.Pool, opts ...Option) (*Provider, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil pool", store.ErrInvalid)
	}
	p := &Provider{pool: pool, clock: time.Now, copyThreshold: defaultCopyThreshold}
	for _, o := range opts {
		o(p)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return p, nil
}

// Open creates a pool from dsn and wraps it. The returned Provider owns the
// pool, so Close closes it.
func Open(ctx context.Context, dsn string, opts ...Option) (*Provider, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: dsn: %v", store.ErrInvalid, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	p, err := NewShared(ctx, pool, opts...)
	if err != nil {
		pool.Close()
		return nil, err
	}
	p.ownsPool = true
	return p, nil
}

// Pool exposes the underlying pool for hosts that need their own statements.
func (p *Provider) Pool() *pgxpool.Pool { return p.pool }

// now is the clock stamp this provider writes on rows, already reduced to the
// resolution the contract stores (store/doc.go).
func (p *Provider) now() time.Time { return store.TruncateTime(p.clock()) }

func (p *Provider) check() error {
	if p.closed {
		return errClosed
	}
	return nil
}

// ForTenant returns a store bound to tenantID. It never touches the database:
// sendplane does not manage tenant lifecycle (ADR-0006).
func (p *Provider) ForTenant(_ context.Context, tenantID string) (store.Store, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: empty tenant id", store.ErrInvalid)
	}
	return newTenantStore(p, tenantID), nil
}

// ActiveTenants lists the tenants with a non-terminal delivery or an unfinished
// campaign (store.Provider). The two halves are separate index scans unioned
// afterwards, rather than one OR across two tables: delivery_claim /
// delivery_campaign_status and campaign_by_status each serve their own branch.
func (p *Provider) ActiveTenants(ctx context.Context) ([]string, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	const q = `SELECT tenant_id FROM (
	              SELECT DISTINCT tenant_id FROM delivery WHERE status IN (0, 1, 2, 3)
	              UNION
	              SELECT DISTINCT tenant_id FROM campaign WHERE status IN (1, 2, 3)
	           ) t WHERE tenant_id <> $1 ORDER BY tenant_id`
	rows, err := p.pool.Query(ctx, q, store.SystemTenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, id)
	}
	return out, mapErr(rows.Err())
}

// knownTenantTables are the configuration tables Tenants unions over. They are
// the small, tenant-keyed ones, each with a (tenant_id, ...) index leading
// with the column being distinct-ed, so the scan stays cheap; the
// per-recipient tables are deliberately left out (store.Provider.Tenants).
var knownTenantTables = []string{
	"tenant_settings", "transport", "sender", "sending_domain",
	"bounce_mailbox", "probe_mailbox", "layout", "template", "campaign",
}

// Tenants lists every tenant with a settings row or a configuration row
// (store.Provider).
func (p *Provider) Tenants(ctx context.Context) ([]string, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(knownTenantTables))
	for _, t := range knownTenantTables {
		parts = append(parts, "SELECT DISTINCT tenant_id FROM "+t)
	}
	q := "SELECT tenant_id FROM (" + strings.Join(parts, " UNION ") +
		") t WHERE tenant_id <> $1 ORDER BY tenant_id"
	rows, err := p.pool.Query(ctx, q, store.SystemTenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, id)
	}
	return out, mapErr(rows.Err())
}

// LookupDeliveryTenant answers with one primary-key lookup: shared mode keeps
// every tenant's deliveries in one table, and the ID is the primary key
// (store.Provider).
func (p *Provider) LookupDeliveryTenant(ctx context.Context, deliveryID string) (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	if deliveryID == "" {
		return "", fmt.Errorf("%w: empty delivery id", store.ErrInvalid)
	}
	var tenantID string
	err := p.pool.QueryRow(ctx,
		`SELECT tenant_id FROM delivery WHERE id = $1`, deliveryID).Scan(&tenantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: delivery %s", store.ErrNotFound, deliveryID)
		}
		return "", mapErr(err)
	}
	return tenantID, nil
}

// Close marks the provider closed and, when it created the pool itself,
// closes the pool.
func (p *Provider) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.ownsPool {
		p.pool.Close()
	}
	return nil
}

// tenantStore is a store.Store bound to one tenant. Every repository below
// carries the tenant and puts it into each statement's WHERE clause, which is
// what makes a missing tenant filter impossible to write (ADR-0006).
type tenantStore struct {
	p      *Provider
	tenant string

	settings     *settingsRepo
	transports   *crud[store.Transport]
	senders      *crud[store.Sender]
	domains      *crud[store.SendingDomain]
	bounceBoxes  *bounceMailboxRepo
	mailboxes    *probeMailboxRepo
	probeRuns    *probeRunRepo
	layouts      *layoutRepo
	templates    *templateRepo
	versions     *versionRepo
	campaigns    *campaignRepo
	chunks       *chunkRepo
	deliveries   *deliveryRepo
	attempts     *attemptRepo
	suppressions *suppressionRepo
	bounces      *bounceRepo
	tracking     *trackingRepo
	outbox       *outboxRepo
	locks        *lockRepo
	workers      *workerRepo
}

func newTenantStore(p *Provider, tenant string) *tenantStore {
	s := &tenantStore{p: p, tenant: tenant}
	s.settings = &settingsRepo{p: p, tenant: tenant}
	s.transports = newCrud(p, tenant, transportSpec)
	s.senders = newCrud(p, tenant, senderSpec)
	s.domains = newCrud(p, tenant, domainSpec)
	s.bounceBoxes = &bounceMailboxRepo{newCrud(p, tenant, bounceMailboxSpec)}
	s.mailboxes = &probeMailboxRepo{newCrud(p, tenant, mailboxSpec)}
	s.probeRuns = &probeRunRepo{newCrud(p, tenant, probeRunSpec)}
	s.layouts = &layoutRepo{newCrud(p, tenant, layoutSpec)}
	s.templates = &templateRepo{newCrud(p, tenant, templateSpec)}
	s.versions = &versionRepo{newCrud(p, tenant, messageVersionSpec)}
	s.campaigns = &campaignRepo{newCrud(p, tenant, campaignSpec)}
	s.chunks = &chunkRepo{p: p, tenant: tenant}
	s.deliveries = &deliveryRepo{p: p, tenant: tenant}
	s.attempts = &attemptRepo{p: p, tenant: tenant}
	s.suppressions = &suppressionRepo{p: p, tenant: tenant}
	s.bounces = &bounceRepo{newCrud(p, tenant, bounceSpec)}
	s.tracking = &trackingRepo{p: p, tenant: tenant}
	s.outbox = &outboxRepo{p: p, tenant: tenant}
	s.locks = &lockRepo{p: p, tenant: tenant}
	s.workers = &workerRepo{p: p, tenant: tenant}
	return s
}

func (s *tenantStore) TenantSettings() store.TenantSettingsRepo  { return s.settings }
func (s *tenantStore) Transports() store.TransportRepo           { return s.transports }
func (s *tenantStore) Senders() store.SenderRepo                 { return s.senders }
func (s *tenantStore) Domains() store.DomainRepo                 { return s.domains }
func (s *tenantStore) BounceMailboxes() store.BounceMailboxRepo  { return s.bounceBoxes }
func (s *tenantStore) ProbeMailboxes() store.ProbeMailboxRepo    { return s.mailboxes }
func (s *tenantStore) ProbeRuns() store.ProbeRunRepo             { return s.probeRuns }
func (s *tenantStore) Layouts() store.LayoutRepo                 { return s.layouts }
func (s *tenantStore) Templates() store.TemplateRepo             { return s.templates }
func (s *tenantStore) Versions() store.MessageVersionRepo        { return s.versions }
func (s *tenantStore) Campaigns() store.CampaignRepo             { return s.campaigns }
func (s *tenantStore) RecipientChunks() store.RecipientChunkRepo { return s.chunks }
func (s *tenantStore) Deliveries() store.DeliveryRepo            { return s.deliveries }
func (s *tenantStore) Attempts() store.AttemptRepo               { return s.attempts }
func (s *tenantStore) Suppressions() store.SuppressionRepo       { return s.suppressions }
func (s *tenantStore) Bounces() store.BounceRepo                 { return s.bounces }
func (s *tenantStore) Tracking() store.TrackingRepo              { return s.tracking }
func (s *tenantStore) Outbox() store.OutboxRepo                  { return s.outbox }
func (s *tenantStore) Locks() store.LockRepo                     { return s.locks }
func (s *tenantStore) Workers() store.WorkerRepo                 { return s.workers }

var (
	_ store.Provider = (*Provider)(nil)
	_ store.Store    = (*tenantStore)(nil)
)
