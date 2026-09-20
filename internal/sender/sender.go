// Package sender is the claim → render → SMTP loop of architecture 8.
//
// One Sender is one replica. It polls the delivery queue per tenant and lane
// (ADR-0002), renders each delivery for its recipient (internal/render),
// applies the tracking and unsubscribe transforms of architecture 9.2, builds
// the MIME message of architecture 10, and hands it to a pooled SMTP
// connection. SMTP results are normalized into the five error classes of
// architecture 4.2 and committed in batches, with an immediate MarkSent right
// after 250 so that a crash before the batch commits costs a duplicate at
// most, never a lost send.
//
// The package does not import the root sendplane package: the root package is
// what calls Run. The types the host injects (Hooks, OutboundMessage,
// SecretCipher, Metrics) come from the leaf package host instead, which the
// root re-exports as aliases, so Config is filled straight from Options with
// no adapter. See README.
package sender

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// Config configures one sender replica.
type Config struct {
	// WorkerID identifies this replica. It is the lease owner on claimed
	// deliveries and the ID of its Workers heartbeat row, so it must be stable
	// for the life of the process and unique in the cluster.
	WorkerID string

	// Lanes is the worker pool size per lane. Transactional is always served
	// before bulk. Default: 8 transactional, 32 bulk.
	Lanes map[store.Lane]int
	// ClaimBatch is the maximum number of deliveries claimed in one call.
	ClaimBatch int
	// LeaseFor is how long a claim holds a delivery (architecture 8.1).
	LeaseFor time.Duration
	// PollInterval is how long the loop sleeps after a pass that claimed
	// nothing.
	PollInterval time.Duration
	// CampaignRefresh is the TTL of the running-campaign set.
	CampaignRefresh time.Duration
	// TenantConcurrency caps the deliveries one tenant may occupy at once, so
	// one big campaign cannot starve the others.
	TenantConcurrency int
	// TenantCacheTTL is the TTL of cached tenant configuration rows.
	TenantCacheTTL time.Duration

	// HeartbeatInterval is how often the Workers row is refreshed, WorkerTTL
	// how far back ListActive looks. The active replica count divides the
	// cluster-wide transport rate (architecture 8.2).
	HeartbeatInterval time.Duration
	WorkerTTL         time.Duration
	// DefaultRatePerSecond applies to transports that configure no rate. Zero
	// means unlimited.
	DefaultRatePerSecond float64

	// ResultBatchSize and ResultFlushInterval drive the batched Complete.
	ResultBatchSize     int
	ResultFlushInterval time.Duration

	// Connection pool.
	MaxMsgsPerConn  int
	ConnIdleTimeout time.Duration
	DialTimeout     time.Duration
	SendTimeout     time.Duration
	EHLOName        string
	TLSConfig       *tls.Config
	Dialer          DialFunc

	// RenderTimeout bounds one recipient's Liquid render.
	RenderTimeout time.Duration

	// TransportFailThreshold is how many consecutive auth/TLS/connect failures
	// mark a transport unhealthy; TransportProbeInterval how often an unhealthy
	// one is probed (architecture 8.3).
	TransportFailThreshold int
	TransportProbeInterval time.Duration

	// RateLimitCap and AuthRetryAfter are the policy knobs of policy.go.
	RateLimitCap   time.Duration
	AuthRetryAfter time.Duration

	Hooks    host.Hooks
	Secrets  host.SecretCipher
	Renderer *render.Renderer
	Metrics  host.Metrics
	Logger   *slog.Logger
	Clock    func() time.Time
	// Rand is the jitter source; nil uses math/rand/v2.
	Rand func() float64
}

// Default configuration values (architecture 8).
const (
	DefaultClaimBatch             = 64
	DefaultLeaseFor               = 2 * time.Minute
	DefaultPollInterval           = 200 * time.Millisecond
	DefaultCampaignRefresh        = 3 * time.Second
	DefaultTenantConcurrency      = 64
	DefaultTenantCacheTTL         = 5 * time.Second
	DefaultHeartbeatInterval      = 10 * time.Second
	DefaultWorkerTTL              = 30 * time.Second
	DefaultResultBatchSize        = 100
	DefaultResultFlushInterval    = time.Second
	DefaultRenderTimeout          = 10 * time.Second
	DefaultTransportFailThreshold = 3
	DefaultTransportProbeInterval = time.Minute
)

func (c Config) withDefaults() Config {
	if len(c.Lanes) == 0 {
		c.Lanes = map[store.Lane]int{store.LaneTransactional: 8, store.LaneBulk: 32}
	}
	if c.ClaimBatch <= 0 {
		c.ClaimBatch = DefaultClaimBatch
	}
	if c.LeaseFor <= 0 {
		c.LeaseFor = DefaultLeaseFor
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.CampaignRefresh <= 0 {
		c.CampaignRefresh = DefaultCampaignRefresh
	}
	if c.TenantConcurrency <= 0 {
		c.TenantConcurrency = DefaultTenantConcurrency
	}
	if c.TenantCacheTTL <= 0 {
		c.TenantCacheTTL = DefaultTenantCacheTTL
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.WorkerTTL <= 0 {
		c.WorkerTTL = DefaultWorkerTTL
	}
	if c.ResultBatchSize <= 0 {
		c.ResultBatchSize = DefaultResultBatchSize
	}
	if c.ResultFlushInterval <= 0 {
		c.ResultFlushInterval = DefaultResultFlushInterval
	}
	if c.RenderTimeout <= 0 {
		c.RenderTimeout = DefaultRenderTimeout
	}
	if c.TransportFailThreshold <= 0 {
		c.TransportFailThreshold = DefaultTransportFailThreshold
	}
	if c.TransportProbeInterval <= 0 {
		c.TransportProbeInterval = DefaultTransportProbeInterval
	}
	if c.Metrics == nil {
		c.Metrics = host.NopMetrics{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Renderer == nil {
		c.Renderer = render.NewRenderer()
	}
	return c
}

// Sender is one replica of the send loop.
type Sender struct {
	cfg      Config
	provider store.Provider
	pool     *Pool
	limiter  *Limiter
	signer   tracking.Signer
	health   *healthTracker

	lanes []*lane

	mu      sync.Mutex
	tenants map[string]*tenantState
	// order is the round-robin order of the last refresh, and cursor is where
	// the next pass starts, so a tenant at the end of the list is not starved
	// by the ones in front of it.
	order  []string
	cursor int

	workers atomic.Int64
	wg      sync.WaitGroup
}

// lane is one lane's worker pool.
type lane struct {
	kind store.Lane
	size int
	jobs chan job
	// inflight counts queued plus running jobs, which is what decides how many
	// deliveries the next claim may take.
	inflight atomic.Int64
	wg       sync.WaitGroup
}

type job struct {
	t *tenantState
	d store.Delivery
}

// New returns a Sender. It does not touch the store.
func New(p store.Provider, cfg Config) (*Sender, error) {
	if p == nil {
		return nil, errors.New("sender: store provider is required")
	}
	if cfg.WorkerID == "" {
		return nil, errors.New("sender: WorkerID is required")
	}
	cfg = cfg.withDefaults()

	s := &Sender{
		cfg:      cfg,
		provider: p,
		tenants:  map[string]*tenantState{},
		health:   newHealthTracker(cfg.TransportFailThreshold, cfg.TransportProbeInterval),
	}
	s.pool = NewPool(PoolConfig{
		MaxMsgsPerConn: cfg.MaxMsgsPerConn,
		IdleTimeout:    cfg.ConnIdleTimeout,
		DialTimeout:    cfg.DialTimeout,
		SendTimeout:    cfg.SendTimeout,
		EHLOName:       cfg.EHLOName,
		TLSConfig:      cfg.TLSConfig,
		Dialer:         cfg.Dialer,
		Now:            cfg.Clock,
		Metrics:        cfg.Metrics,
	})
	s.workers.Store(1)
	s.limiter = NewLimiter(func() int { return int(s.workers.Load()) }, cfg.Clock)

	// Lanes are served in a fixed order so that transactional mail is always
	// offered capacity before bulk (architecture 8.1).
	for _, kind := range []store.Lane{store.LaneTransactional, store.LaneBulk, store.LaneProbe} {
		n, ok := cfg.Lanes[kind]
		if !ok || n <= 0 {
			continue
		}
		s.lanes = append(s.lanes, &lane{kind: kind, size: n, jobs: make(chan job, n)})
	}
	if len(s.lanes) == 0 {
		return nil, errors.New("sender: no lane has a worker pool")
	}
	return s, nil
}

// Run claims and sends until ctx is done. It returns nil on a clean shutdown.
func (s *Sender) Run(ctx context.Context) error {
	for _, l := range s.lanes {
		for i := 0; i < l.size; i++ {
			l.wg.Add(1)
			go s.worker(ctx, l)
		}
	}
	s.wg.Add(1)
	go s.heartbeatLoop(ctx)

	err := s.loop(ctx)

	for _, l := range s.lanes {
		close(l.jobs)
		l.wg.Wait()
	}
	s.wg.Wait()

	s.mu.Lock()
	tenants := make([]*tenantState, 0, len(s.tenants))
	for _, t := range s.tenants {
		tenants = append(tenants, t)
	}
	s.mu.Unlock()
	for _, t := range tenants {
		if t.batcher != nil {
			t.batcher.close()
		}
	}
	s.pool.Close()
	return err
}

func (s *Sender) loop(ctx context.Context) error {
	refresh := time.NewTicker(s.cfg.CampaignRefresh)
	defer refresh.Stop()
	if err := s.refreshTenants(ctx); err != nil {
		s.cfg.Logger.Warn("sendplane: listing active tenants failed", "err", err)
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-refresh.C:
			if err := s.refreshTenants(ctx); err != nil {
				s.cfg.Logger.Warn("sendplane: listing active tenants failed", "err", err)
			}
			s.probeTransports(ctx)
		default:
		}

		claimed := s.pass(ctx)
		if claimed == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(s.cfg.PollInterval):
			}
		}
	}
}

// pass walks the tenants once in round-robin order and claims what the lane
// and tenant capacity allow.
func (s *Sender) pass(ctx context.Context) int {
	s.mu.Lock()
	order := make([]string, len(s.order))
	copy(order, s.order)
	start := s.cursor
	if len(order) > 0 {
		s.cursor = (s.cursor + 1) % len(order)
	}
	s.mu.Unlock()

	total := 0
	for i := range order {
		id := order[(start+i)%len(order)]
		t := s.tenant(ctx, id)
		if t == nil {
			continue
		}
		for _, l := range s.lanes {
			total += s.claimInto(ctx, t, l)
		}
	}
	return total
}

// claimInto claims for one tenant and lane and dispatches the result.
func (s *Sender) claimInto(ctx context.Context, t *tenantState, l *lane) int {
	laneFree := l.size - int(l.inflight.Load())
	tenantFree := s.cfg.TenantConcurrency - int(t.inflight.Load())
	limit := min(min(laneFree, tenantFree), s.cfg.ClaimBatch)
	if limit <= 0 {
		return 0
	}

	campaigns, err := s.runningCampaigns(ctx, t)
	if err != nil {
		s.cfg.Logger.Warn("sendplane: listing running campaigns failed",
			"tenant", t.id, "err", err)
		return 0
	}

	// Capacity is reserved before the claim and the unused part is given back
	// straight after, so two passes can never oversubscribe a pool.
	l.inflight.Add(int64(limit))
	t.inflight.Add(int64(limit))

	now := s.cfg.Clock()
	ds, err := t.st.Deliveries().Claim(ctx, store.ClaimRequest{
		Lane:        l.kind,
		CampaignIDs: campaigns,
		Limit:       limit,
		LeaseFor:    s.cfg.LeaseFor,
		WorkerID:    s.cfg.WorkerID,
		Now:         now,
	})
	if err != nil {
		l.inflight.Add(-int64(limit))
		t.inflight.Add(-int64(limit))
		if ctx.Err() == nil {
			s.cfg.Logger.Warn("sendplane: claim failed", "tenant", t.id, "lane", l.kind.String(), "err", err)
		}
		return 0
	}
	if unused := limit - len(ds); unused > 0 {
		l.inflight.Add(-int64(unused))
		t.inflight.Add(-int64(unused))
	}
	if len(ds) == 0 {
		return 0
	}
	s.cfg.Metrics.Count(MetricClaimed, int64(len(ds)), "lane", l.kind.String(), "tenant", t.id)

	for _, d := range ds {
		select {
		case l.jobs <- job{t: t, d: d}:
		case <-ctx.Done():
			// Shutting down: give the capacity back and let the lease expire,
			// which ReleaseExpiredLeases turns back into queued.
			l.inflight.Add(-1)
			t.inflight.Add(-1)
		}
	}
	return len(ds)
}

func (s *Sender) worker(ctx context.Context, l *lane) {
	defer l.wg.Done()
	for j := range l.jobs {
		s.handle(ctx, j)
		l.inflight.Add(-1)
		j.t.inflight.Add(-1)
	}
}

func (s *Sender) handle(ctx context.Context, j job) {
	// A cancelled context still has to finish the message that is already
	// claimed; the work loop stops claiming new ones instead.
	res := s.process(ctx, j.t, j.d)
	if res.DeliveryID == "" {
		return
	}
	j.t.batcher.add(res)
}

// refreshTenants reloads the active tenant list (ADR-0006).
func (s *Sender) refreshTenants(ctx context.Context) error {
	ids, err := s.provider.ActiveTenants(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.order = ids
	if len(ids) > 0 {
		s.cursor %= len(ids)
	} else {
		s.cursor = 0
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.tenant(ctx, id)
	}
	return nil
}

// tenant returns the cached per-tenant state, creating it on first use.
func (s *Sender) tenant(ctx context.Context, id string) *tenantState {
	s.mu.Lock()
	t, ok := s.tenants[id]
	s.mu.Unlock()
	if ok {
		return t
	}
	st, err := s.provider.ForTenant(ctx, id)
	if err != nil {
		s.cfg.Logger.Warn("sendplane: opening tenant store failed", "tenant", id, "err", err)
		return nil
	}
	t = newTenantState(id, st, s.cfg.TenantCacheTTL)
	t.batcher = newBatcher(st, s.cfg.ResultBatchSize, s.cfg.ResultFlushInterval, s.cfg.Logger, s.cfg.Metrics)

	s.mu.Lock()
	if existing, ok := s.tenants[id]; ok {
		s.mu.Unlock()
		t.batcher.close()
		return existing
	}
	s.tenants[id] = t
	s.mu.Unlock()
	return t
}

// runningCampaigns is the claim filter of ADR-0002: the sender keeps a short
// cache of the running campaign set instead of joining the campaign table on
// every claim.
func (s *Sender) runningCampaigns(ctx context.Context, t *tenantState) ([]string, error) {
	now := s.cfg.Clock()
	t.runMu.Lock()
	if !t.runningAt.IsZero() && now.Sub(t.runningAt) < s.cfg.CampaignRefresh {
		ids := t.running
		t.runMu.Unlock()
		return ids, nil
	}
	t.runMu.Unlock()

	// A non-nil empty slice means "only deliveries without a campaign", which
	// is the right filter when no campaign is running.
	ids := []string{}
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := t.st.Campaigns().ListByStatus(ctx, []store.CampaignStatus{store.CampaignRunning}, page)
		if err != nil {
			return nil, err
		}
		for _, c := range res.Items {
			ids = append(ids, c.ID)
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}
	t.runMu.Lock()
	t.running, t.runningAt = ids, now
	t.runMu.Unlock()
	return ids, nil
}

// heartbeatLoop refreshes this replica's Workers row and recomputes the active
// replica count the rate limiter divides by (architecture 8.2).
func (s *Sender) heartbeatLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.HeartbeatInterval)
	defer t.Stop()
	s.heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.heartbeat(ctx)
		}
	}
}

func (s *Sender) heartbeat(ctx context.Context) {
	s.mu.Lock()
	tenants := make([]*tenantState, 0, len(s.tenants))
	for _, t := range s.tenants {
		tenants = append(tenants, t)
	}
	s.mu.Unlock()

	now := s.cfg.Clock()
	lanes := make([]store.Lane, 0, len(s.lanes))
	concurrency := 0
	for _, l := range s.lanes {
		lanes = append(lanes, l.kind)
		concurrency += l.size
	}

	maxWorkers := int64(1)
	for _, t := range tenants {
		w := store.Worker{
			ID: s.cfg.WorkerID, Role: store.WorkerRoleSender, Lanes: lanes,
			Concurrency: concurrency, LastSeenAt: now,
		}
		if err := t.st.Workers().Heartbeat(ctx, w); err != nil {
			s.cfg.Logger.Warn("sendplane: worker heartbeat failed", "tenant", t.id, "err", err)
			continue
		}
		active, err := t.st.Workers().ListActive(ctx, now.Add(-s.cfg.WorkerTTL))
		if err != nil {
			s.cfg.Logger.Warn("sendplane: listing active workers failed", "tenant", t.id, "err", err)
			continue
		}
		n := int64(0)
		for _, w := range active {
			if w.Role == store.WorkerRoleSender {
				n++
			}
		}
		if n < 1 {
			n = 1
		}
		t.workers.Store(n)
		if n > maxWorkers {
			maxWorkers = n
		}
	}
	// One replica serves every tenant it sees, so the largest per-tenant count
	// is the replica count. Using the maximum keeps the divisor right for a
	// tenant whose own heartbeat row has not been written yet.
	s.workers.Store(maxWorkers)
}

// probeTransports re-tests the transports that were marked unhealthy
// (architecture 8.3). A probe is a real connection: dial, EHLO, AUTH.
func (s *Sender) probeTransports(ctx context.Context) {
	now := s.cfg.Clock()
	for _, p := range s.health.due(now) {
		t := s.tenant(ctx, p.tenantID)
		if t == nil {
			continue
		}
		tr, err := t.transport(ctx, p.transportID, now)
		if err != nil {
			continue
		}
		password, err := s.password(ctx, t, tr)
		if err != nil {
			continue
		}
		conn, err := s.pool.Get(ctx, tr, password)
		if err != nil {
			s.cfg.Logger.Info("sendplane: transport probe failed",
				"tenant", t.id, "transport", tr.ID, "err", err)
			continue
		}
		s.pool.Put(conn, true)
		s.health.recover(ctx, t, tr, s)
	}
}

// Pool exposes the connection pool, for tests and for a future admin route.
func (s *Sender) Pool() *Pool { return s.pool }

// Limiter exposes the rate limiter, for tests and metrics.
func (s *Sender) Limiter() *Limiter { return s.limiter }

// isNotFound is store.ErrNotFound, wrapped or not.
func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
