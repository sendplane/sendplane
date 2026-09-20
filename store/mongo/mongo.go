// Package mongo implements store.Provider on MongoDB in shared mode: one
// database holds every tenant and each document carries a tenant_id that
// ForTenant pins into every filter (ADR-0006).
//
// It deliberately uses no multi-document transactions, so it runs on a
// standalone mongod as well as on a replica set (ADR-0007, architecture 5.3):
//
//   - idempotent ingest is InsertMany(ordered:false) against a unique partial
//     index on (campaign_id, email_norm), counting duplicate-key errors;
//   - Claim is candidate find -> conditional UpdateMany stamping a per-call
//     claim token -> find by that token, so two claimers never share a row;
//   - every other transition is a conditional UpdateOne (CAS on status, lease
//     owner or version).
//
// # Storage format
//
// Documents use _id for the aggregate ID; aggregates without one (tenant
// settings, suppressions, locks, workers, recipient chunks) use a composite
// key that starts with the tenant so the collection stays partitionable.
// Enums are stored as int32, []byte and json.RawMessage as BSON binary.
//
// Times are BSON dates. A BSON date holds milliseconds, which is exactly the
// contract's resolution (store/doc.go), so every instant is truncated on the
// way in rather than silently rounded, and range predicates truncate their
// bound the same way. The zero time (the contract's NULL) is stored as null.
// Keeping them as dates is what makes them indexable, comparable in the
// aggregation pipeline and readable from the shell.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/sendplane/sendplane/store"
)

// Collection names. They map 1:1 onto the Postgres tables (architecture 5.3).
const (
	collTenantSettings = "tenant_settings"
	collTransport      = "transport"
	collSender         = "sender"
	collDomain         = "sending_domain"
	collBounceMailbox  = "bounce_mailbox"
	collProbeMailbox   = "probe_mailbox"
	collProbeRun       = "probe_run"
	collLayout         = "layout"
	collTemplate       = "template"
	collVersion        = "message_version"
	collCampaign       = "campaign"
	collChunk          = "recipient_chunk"
	collDelivery       = "delivery"
	collAttempt        = "delivery_attempt"
	collSuppression    = "suppression"
	collBounce         = "bounce_event"
	collTracking       = "tracking_event"
	collOutbox         = "outbox_event"
	collLock           = "lock"
	collWorker         = "worker"
)

// Option configures a Provider.
type Option func(*Provider)

// WithClock replaces time.Now, so tests control the timestamps the store
// stamps on rows. Methods that take an explicit now (Claim,
// ReleaseExpiredLeases, locks, outbox) use that argument instead.
func WithClock(clock func() time.Time) Option {
	return func(p *Provider) { p.clock = clock }
}

// Provider is the MongoDB store.Provider in shared mode.
type Provider struct {
	client *mongo.Client
	db     *mongo.Database
	clock  func() time.Time
	// owned is true when Open created the client, in which case Close
	// disconnects it. A client passed to NewShared belongs to the caller.
	owned bool
}

var _ store.Provider = (*Provider)(nil)

// NewShared wraps an already connected client. Every document written through
// the returned Provider carries a tenant_id and ForTenant returns a Store that
// adds that predicate to every filter.
func NewShared(_ context.Context, client *mongo.Client, dbName string, opts ...Option) (*Provider, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: nil mongo client", store.ErrInvalid)
	}
	if dbName == "" {
		return nil, fmt.Errorf("%w: empty database name", store.ErrInvalid)
	}
	p := &Provider{client: client, db: client.Database(dbName), clock: time.Now}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// Open connects to uri and returns a shared-mode Provider that owns the
// connection: Close disconnects it.
func Open(ctx context.Context, uri, dbName string, opts ...Option) (*Provider, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo: connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo: ping: %w", err)
	}
	p, err := NewShared(ctx, client, dbName, opts...)
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	p.owned = true
	return p, nil
}

// Database exposes the underlying handle for diagnostics and tests.
func (p *Provider) Database() *mongo.Database { return p.db }

// now is the clock stamp this provider writes on documents, already reduced
// to the resolution the contract stores (store/doc.go).
func (p *Provider) now() time.Time { return store.TruncateTime(p.clock()) }

// ForTenant returns a store bound to tenantID. Nothing is written: tenants are
// created implicitly by their first row (ADR-0006).
func (p *Provider) ForTenant(_ context.Context, tenantID string) (store.Store, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: empty tenant id", store.ErrInvalid)
	}
	return newTenantStore(p, tenantID), nil
}

// unfinishedCampaigns are the campaign states a control loop still has to act
// on, and the second half of the ActiveTenants predicate.
var unfinishedCampaigns = []store.CampaignStatus{
	store.CampaignScheduled, store.CampaignRunning, store.CampaignPaused,
}

// ActiveTenants lists the tenants with a non-terminal delivery or an unfinished
// campaign (store.Provider). Two distincts, one per collection, because each
// has its own status index and no join would be cheaper.
func (p *Provider) ActiveTenants(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	err := p.distinctTenants(ctx, seen, collDelivery,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: statusInts(openStatuses)}}}})
	if err != nil {
		return nil, err
	}
	err = p.distinctTenants(ctx, seen, collCampaign,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: campaignStatusInts(unfinishedCampaigns)}}}})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// knownTenantCollections are the configuration collections Tenants unions
// over: the small, tenant-keyed ones (store.Provider.Tenants).
var knownTenantCollections = []string{
	collTenantSettings, collTransport, collSender, collDomain,
	collBounceMailbox, collProbeMailbox, collLayout, collTemplate, collCampaign,
}

// Tenants lists every tenant with a settings row or a configuration row
// (store.Provider).
func (p *Provider) Tenants(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	for _, coll := range knownTenantCollections {
		if err := p.distinctTenants(ctx, seen, coll, bson.D{}); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// distinctTenants adds the tenants matching filter in one collection to seen,
// skipping the system scope: it holds the leader lock, never work.
func (p *Provider) distinctTenants(ctx context.Context, seen map[string]bool, coll string, filter bson.D) error {
	res := p.db.Collection(coll).Distinct(ctx, "tenant_id", filter)
	if err := res.Err(); err != nil {
		return fmt.Errorf("mongo: active tenants: %w", err)
	}
	var ids []string
	if err := res.Decode(&ids); err != nil {
		return fmt.Errorf("mongo: active tenants: %w", err)
	}
	for _, id := range ids {
		if id != store.SystemTenantID {
			seen[id] = true
		}
	}
	return nil
}

// Close releases the connection when this Provider owns it.
func (p *Provider) Close() error {
	if !p.owned {
		return nil
	}
	return p.client.Disconnect(context.Background())
}

// index is one index this schema needs. Names are explicit so that re-running
// Migrate is a no-op instead of a rename or a duplicate.
type index struct {
	coll string
	name string
	keys bson.D
	opt  func(*options.IndexOptionsBuilder) *options.IndexOptionsBuilder
}

// listIndex is the (tenant_id, created_at, _id) index every cursor-paginated
// listing walks.
func listIndex(coll string) index {
	return index{coll: coll, name: "tenant_created", keys: bson.D{
		{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
	}}
}

func indexes() []index {
	out := []index{
		listIndex(collTransport),
		listIndex(collSender),
		listIndex(collDomain),
		listIndex(collBounceMailbox),
		listIndex(collProbeMailbox),
		listIndex(collLayout),
		listIndex(collTemplate),
		listIndex(collVersion),
		listIndex(collProbeRun),
		listIndex(collCampaign),
		listIndex(collChunk),
		listIndex(collSuppression),
		listIndex(collBounce),
		listIndex(collAttempt),
		listIndex(collOutbox),
		listIndex(collTracking),
		listIndex(collDelivery),
		{coll: collVersion, name: "tenant_template", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "template_id", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		{
			coll: collBounceMailbox, name: "tenant_enabled",
			keys: bson.D{
				{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: 1},
				{Key: "_id", Value: 1},
			},
			opt: func(b *options.IndexOptionsBuilder) *options.IndexOptionsBuilder {
				return b.SetPartialFilterExpression(bson.D{
					{Key: "enabled", Value: true}})
			},
		},
		{
			coll: collProbeRun, name: "tenant_pending",
			keys: bson.D{
				{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: 1},
				{Key: "_id", Value: 1},
			},
			opt: func(b *options.IndexOptionsBuilder) *options.IndexOptionsBuilder {
				return b.SetPartialFilterExpression(bson.D{
					{Key: "pending", Value: true}})
			},
		},
		{
			coll: collSuppression, name: "tenant_expiry",
			keys: bson.D{
				{Key: "tenant_id", Value: 1}, {Key: "expires_at", Value: 1},
			},
			opt: func(b *options.IndexOptionsBuilder) *options.IndexOptionsBuilder {
				// "never expires" is stored as null and is outside this index.
				return b.SetPartialFilterExpression(bson.D{
					{Key: "expires_at", Value: bson.D{{Key: "$type", Value: "date"}}}})
			},
		},
		{coll: collProbeRun, name: "tenant_sender", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "sender_id", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		{coll: collCampaign, name: "tenant_status", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "status", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		{coll: collBounce, name: "tenant_delivery", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "delivery_id", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		{coll: collAttempt, name: "tenant_delivery", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "delivery_id", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		{coll: collOutbox, name: "tenant_status_due", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "status", Value: 1},
			{Key: "next_attempt_at", Value: 1},
		}},
		{coll: collTracking, name: "tenant_campaign_kind", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "campaign_id", Value: 1},
			{Key: "kind", Value: 1},
		}},
		{coll: collWorker, name: "tenant_last_seen", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "last_seen_at", Value: 1},
		}},

		// Deliveries: the queue indexes (architecture 5.3).
		{
			coll: collDelivery, name: "campaign_email_unique",
			// The tenant leads the key, like the Postgres index
			// delivery_campaign_email: shared mode puts every tenant in one
			// collection, so the uniqueness of a campaign address is a
			// per-tenant fact and the index stays partitionable.
			keys: bson.D{
				{Key: "tenant_id", Value: 1},
				{Key: "campaign_id", Value: 1},
				{Key: "email_norm", Value: 1},
			},
			opt: func(b *options.IndexOptionsBuilder) *options.IndexOptionsBuilder {
				// Deliveries without a campaign omit the field entirely, so
				// they take no part in the unique key (transactional and probe
				// mail is never deduplicated).
				return b.SetUnique(true).SetPartialFilterExpression(bson.D{
					{Key: "campaign_id", Value: bson.D{
						{Key: "$exists", Value: true}, {Key: "$type", Value: "string"},
					}},
				})
			},
		},
		{coll: collDelivery, name: "claim", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "lane", Value: 1},
			{Key: "status", Value: 1}, {Key: "next_attempt_at", Value: 1},
		}},
		{coll: collDelivery, name: "lease_expiry", keys: bson.D{
			{Key: "status", Value: 1}, {Key: "lease_until", Value: 1},
		}},
		{coll: collDelivery, name: "campaign_status", keys: bson.D{
			{Key: "campaign_id", Value: 1}, {Key: "status", Value: 1},
		}},
		{coll: collDelivery, name: "tenant_campaign_created", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "campaign_id", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
		// "Find this address across campaigns" (GET /api/v1/deliveries?email=).
		// The tenant-wide listing without an address walks listIndex(collDelivery),
		// which is already (tenant_id, created_at, _id).
		{coll: collDelivery, name: "tenant_email_created", keys: bson.D{
			{Key: "tenant_id", Value: 1}, {Key: "email_norm", Value: 1},
			{Key: "created_at", Value: 1}, {Key: "_id", Value: 1},
		}},
	}
	return out
}

// Migrate creates the collections and indexes. It is idempotent: every index
// is named, and CreateMany on an index that already exists with the same
// specification does nothing.
func (p *Provider) Migrate(ctx context.Context) error {
	byColl := map[string][]mongo.IndexModel{}
	order := make([]string, 0, 8)
	for _, ix := range indexes() {
		if _, ok := byColl[ix.coll]; !ok {
			order = append(order, ix.coll)
		}
		opt := options.Index().SetName(ix.name)
		if ix.opt != nil {
			opt = ix.opt(opt)
		}
		byColl[ix.coll] = append(byColl[ix.coll], mongo.IndexModel{Keys: ix.keys, Options: opt})
	}
	// Collections that only ever need their _id index still have to exist.
	for _, name := range []string{collTenantSettings, collLock, collWorker} {
		if _, ok := byColl[name]; !ok {
			byColl[name] = nil
			order = append(order, name)
		}
	}
	for _, coll := range order {
		if models := byColl[coll]; len(models) > 0 {
			if _, err := p.db.Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
				return fmt.Errorf("mongo: create indexes on %s: %w", coll, err)
			}
			continue
		}
		if err := p.db.CreateCollection(ctx, coll); err != nil && !isNamespaceExists(err) {
			return fmt.Errorf("mongo: create collection %s: %w", coll, err)
		}
	}
	return nil
}

// isNamespaceExists reports the "collection already exists" server error (48),
// which makes a repeated Migrate a no-op.
func isNamespaceExists(err error) bool {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 48
	}
	return false
}

// tenantStore is a Store bound to one tenant: every filter it builds starts
// with that tenant_id, so reaching another tenant's row is not expressible.
type tenantStore struct {
	p      *Provider
	tenant string

	transports     *table[store.Transport, transportDoc, *transportDoc]
	senders        *table[store.Sender, senderDoc, *senderDoc]
	domains        *table[store.SendingDomain, domainDoc, *domainDoc]
	probeMailboxes *table[store.ProbeMailbox, probeMailboxDoc, *probeMailboxDoc]

	bounceMailboxes *bounceMailboxRepo
	layouts         *table[store.Layout, layoutDoc, *layoutDoc]
	templates       *table[store.Template, templateDoc, *templateDoc]

	versions  *versionRepo
	probeRuns *probeRunRepo
	campaigns *campaignRepo
	bounces   *bounceRepo

	settings     *settingsRepo
	chunks       *chunkRepo
	deliveries   *deliveryRepo
	attempts     *attemptRepo
	suppressions *suppressionRepo
	tracking     *trackingRepo
	outbox       *outboxRepo
	locks        *lockRepo
	workers      *workerRepo
}

var _ store.Store = (*tenantStore)(nil)

func newTenantStore(p *Provider, tenant string) *tenantStore {
	s := &tenantStore{p: p, tenant: tenant}
	s.transports = newTable(s, collTransport, transportMeta())
	s.senders = newTable(s, collSender, senderMeta())
	s.domains = newTable(s, collDomain, domainMeta())
	s.probeMailboxes = newTable(s, collProbeMailbox, probeMailboxMeta())
	s.bounceMailboxes = &bounceMailboxRepo{newTable(s, collBounceMailbox, bounceMailboxMeta())}
	s.layouts = newTable(s, collLayout, layoutMeta())
	s.templates = newTable(s, collTemplate, templateMeta())

	s.versions = &versionRepo{newTable(s, collVersion, versionMeta())}
	s.probeRuns = &probeRunRepo{newTable(s, collProbeRun, probeRunMeta())}
	s.campaigns = &campaignRepo{newTable(s, collCampaign, campaignMeta())}
	s.bounces = &bounceRepo{newTable(s, collBounce, bounceMeta())}

	s.settings = &settingsRepo{s}
	s.chunks = &chunkRepo{s}
	s.deliveries = &deliveryRepo{s}
	s.attempts = &attemptRepo{s}
	s.suppressions = &suppressionRepo{s}
	s.tracking = &trackingRepo{s}
	s.outbox = &outboxRepo{s}
	s.locks = &lockRepo{s}
	s.workers = &workerRepo{s}
	return s
}

func (s *tenantStore) coll(name string) *mongo.Collection { return s.p.db.Collection(name) }

// scope is the base filter of every query: the tenant predicate.
func (s *tenantStore) scope(extra ...bson.E) bson.D {
	d := make(bson.D, 0, len(extra)+1)
	d = append(d, bson.E{Key: "tenant_id", Value: s.tenant})
	return append(d, extra...)
}

func (s *tenantStore) TenantSettings() store.TenantSettingsRepo  { return s.settings }
func (s *tenantStore) Transports() store.TransportRepo           { return s.transports }
func (s *tenantStore) Senders() store.SenderRepo                 { return s.senders }
func (s *tenantStore) Domains() store.DomainRepo                 { return s.domains }
func (s *tenantStore) BounceMailboxes() store.BounceMailboxRepo  { return s.bounceMailboxes }
func (s *tenantStore) ProbeMailboxes() store.ProbeMailboxRepo    { return s.probeMailboxes }
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
