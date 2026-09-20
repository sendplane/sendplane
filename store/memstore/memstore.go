// Package memstore is the in-memory reference implementation of
// store.Provider. It passes the whole store/storetest conformance suite and
// exists so domain logic can be unit tested without a database.
//
// It is not a persistence layer: everything lives in mutex-protected maps and
// is lost when the process exits.
package memstore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/sendplane/sendplane/store"
)

// errClosed is returned by every method after Close.
var errClosed = errors.New("memstore: provider is closed")

// Option configures New.
type Option func(*Provider)

// WithClock replaces time.Now, so tests control the timestamps the store
// stamps on rows. Methods that take an explicit now (Claim,
// ReleaseExpiredLeases, locks, outbox) use that argument instead.
func WithClock(clock func() time.Time) Option {
	return func(p *Provider) { p.clock = clock }
}

// Provider is the in-memory store.Provider. It is safe for concurrent use.
type Provider struct {
	mu     sync.Mutex
	clock  func() time.Time
	closed bool
	// tenants holds shared-mode data: every row also carries its TenantID, and
	// a Store only ever reaches its own tenant's maps.
	tenants map[string]*tenantData
}

// New returns an empty provider.
func New(opts ...Option) *Provider {
	p := &Provider{clock: time.Now, tenants: map[string]*tenantData{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *Provider) now() time.Time { return p.clock().UTC() }

func (p *Provider) check() error {
	if p.closed {
		return errClosed
	}
	return nil
}

type tenantData struct {
	settings *store.TenantSettings

	transports     map[string]*store.Transport
	senders        map[string]*store.Sender
	domains        map[string]*store.SendingDomain
	probeMailboxes map[string]*store.ProbeMailbox
	probeRuns      map[string]*store.ProbeRun
	layouts        map[string]*store.Layout
	templates      map[string]*store.Template
	versions       map[string]*store.MessageVersion
	campaigns      map[string]*store.Campaign
	deliveries     map[string]*store.Delivery
	bounces        map[string]*store.BounceEvent
	outbox         map[string]*store.OutboxEvent

	// campaignEmails enforces the (campaign_id, email_norm) unique key.
	campaignEmails map[string]string
	chunks         map[string]*store.RecipientChunk
	attempts       map[string]*store.DeliveryAttempt
	suppressions   map[string]*store.Suppression
	tracking       map[string]*store.TrackingEvent
	locks          map[string]*store.Lock
	workers        map[string]*store.Worker
}

func newTenantData() *tenantData {
	return &tenantData{
		transports:     map[string]*store.Transport{},
		senders:        map[string]*store.Sender{},
		domains:        map[string]*store.SendingDomain{},
		probeMailboxes: map[string]*store.ProbeMailbox{},
		probeRuns:      map[string]*store.ProbeRun{},
		layouts:        map[string]*store.Layout{},
		templates:      map[string]*store.Template{},
		versions:       map[string]*store.MessageVersion{},
		campaigns:      map[string]*store.Campaign{},
		deliveries:     map[string]*store.Delivery{},
		bounces:        map[string]*store.BounceEvent{},
		outbox:         map[string]*store.OutboxEvent{},
		campaignEmails: map[string]string{},
		chunks:         map[string]*store.RecipientChunk{},
		attempts:       map[string]*store.DeliveryAttempt{},
		suppressions:   map[string]*store.Suppression{},
		tracking:       map[string]*store.TrackingEvent{},
		locks:          map[string]*store.Lock{},
		workers:        map[string]*store.Worker{},
	}
}

// ForTenant returns a store bound to tenantID, creating its (empty) data on
// first use.
func (p *Provider) ForTenant(_ context.Context, tenantID string) (store.Store, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return nil, err
	}
	d, ok := p.tenants[tenantID]
	if !ok {
		d = newTenantData()
		p.tenants[tenantID] = d
	}
	return newTenantStore(p, tenantID, d), nil
}

// ActiveTenants lists tenants that have at least one delivery which is not in
// a terminal state, i.e. the tenants a sender still has work for.
func (p *Provider) ActiveTenants(_ context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.check(); err != nil {
		return nil, err
	}
	var out []string
	for id, d := range p.tenants {
		for _, dl := range d.deliveries {
			if !dl.Status.Terminal() {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Migrate is a no-op: there is no schema.
func (p *Provider) Migrate(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.check()
}

// Close drops all data.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.tenants = nil
	return nil
}

type tenantStore struct {
	p      *Provider
	tenant string
	d      *tenantData

	transports     *table[store.Transport]
	senders        *table[store.Sender]
	domains        *table[store.SendingDomain]
	probeMailboxes *table[store.ProbeMailbox]
	layouts        *table[store.Layout]
	templates      *table[store.Template]

	probeRuns *probeRunRepo
	versions  *versionRepo
	campaigns *campaignRepo

	settings     *settingsRepo
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

func newTenantStore(p *Provider, tenant string, d *tenantData) *tenantStore {
	s := &tenantStore{p: p, tenant: tenant, d: d}
	s.transports = &table[store.Transport]{p: p, tenant: tenant, rows: d.transports, m: meta[store.Transport]{
		id:      func(v *store.Transport) *string { return &v.ID },
		tenant:  func(v *store.Transport) *string { return &v.TenantID },
		version: func(v *store.Transport) *int64 { return &v.Version },
		created: func(v *store.Transport) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Transport) *time.Time { return &v.UpdatedAt },
	}}
	s.senders = &table[store.Sender]{p: p, tenant: tenant, rows: d.senders, m: meta[store.Sender]{
		id:      func(v *store.Sender) *string { return &v.ID },
		tenant:  func(v *store.Sender) *string { return &v.TenantID },
		version: func(v *store.Sender) *int64 { return &v.Version },
		created: func(v *store.Sender) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Sender) *time.Time { return &v.UpdatedAt },
	}}
	s.domains = &table[store.SendingDomain]{p: p, tenant: tenant, rows: d.domains, m: meta[store.SendingDomain]{
		id:      func(v *store.SendingDomain) *string { return &v.ID },
		tenant:  func(v *store.SendingDomain) *string { return &v.TenantID },
		version: func(v *store.SendingDomain) *int64 { return &v.Version },
		created: func(v *store.SendingDomain) *time.Time { return &v.CreatedAt },
		updated: func(v *store.SendingDomain) *time.Time { return &v.UpdatedAt },
	}}
	s.probeMailboxes = &table[store.ProbeMailbox]{p: p, tenant: tenant, rows: d.probeMailboxes, m: meta[store.ProbeMailbox]{
		id:      func(v *store.ProbeMailbox) *string { return &v.ID },
		tenant:  func(v *store.ProbeMailbox) *string { return &v.TenantID },
		version: func(v *store.ProbeMailbox) *int64 { return &v.Version },
		created: func(v *store.ProbeMailbox) *time.Time { return &v.CreatedAt },
		updated: func(v *store.ProbeMailbox) *time.Time { return &v.UpdatedAt },
	}}
	s.layouts = &table[store.Layout]{p: p, tenant: tenant, rows: d.layouts, m: meta[store.Layout]{
		id:      func(v *store.Layout) *string { return &v.ID },
		tenant:  func(v *store.Layout) *string { return &v.TenantID },
		version: func(v *store.Layout) *int64 { return &v.Version },
		created: func(v *store.Layout) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Layout) *time.Time { return &v.UpdatedAt },
	}}
	s.templates = &table[store.Template]{p: p, tenant: tenant, rows: d.templates, m: meta[store.Template]{
		id:      func(v *store.Template) *string { return &v.ID },
		tenant:  func(v *store.Template) *string { return &v.TenantID },
		version: func(v *store.Template) *int64 { return &v.Version },
		created: func(v *store.Template) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Template) *time.Time { return &v.UpdatedAt },
	}}
	s.probeRuns = &probeRunRepo{table[store.ProbeRun]{p: p, tenant: tenant, rows: d.probeRuns, m: meta[store.ProbeRun]{
		id:      func(v *store.ProbeRun) *string { return &v.ID },
		tenant:  func(v *store.ProbeRun) *string { return &v.TenantID },
		created: func(v *store.ProbeRun) *time.Time { return &v.CreatedAt },
	}}}
	s.versions = &versionRepo{table[store.MessageVersion]{p: p, tenant: tenant, rows: d.versions, m: meta[store.MessageVersion]{
		id:      func(v *store.MessageVersion) *string { return &v.ID },
		tenant:  func(v *store.MessageVersion) *string { return &v.TenantID },
		created: func(v *store.MessageVersion) *time.Time { return &v.CreatedAt },
	}}}
	s.campaigns = &campaignRepo{table[store.Campaign]{p: p, tenant: tenant, rows: d.campaigns, m: meta[store.Campaign]{
		id:      func(v *store.Campaign) *string { return &v.ID },
		tenant:  func(v *store.Campaign) *string { return &v.TenantID },
		version: func(v *store.Campaign) *int64 { return &v.Version },
		created: func(v *store.Campaign) *time.Time { return &v.CreatedAt },
		updated: func(v *store.Campaign) *time.Time { return &v.UpdatedAt },
	}}}
	s.bounces = &bounceRepo{table[store.BounceEvent]{p: p, tenant: tenant, rows: d.bounces, m: meta[store.BounceEvent]{
		id:      func(v *store.BounceEvent) *string { return &v.ID },
		tenant:  func(v *store.BounceEvent) *string { return &v.TenantID },
		created: func(v *store.BounceEvent) *time.Time { return &v.CreatedAt },
	}}}
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

func (s *tenantStore) TenantSettings() store.TenantSettingsRepo  { return s.settings }
func (s *tenantStore) Transports() store.TransportRepo           { return s.transports }
func (s *tenantStore) Senders() store.SenderRepo                 { return s.senders }
func (s *tenantStore) Domains() store.DomainRepo                 { return s.domains }
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

var _ store.Provider = (*Provider)(nil)
var _ store.Store = (*tenantStore)(nil)
