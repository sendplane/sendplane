package control

import (
	"fmt"
	"os"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Intervals is how often each leader loop ticks. A zero field keeps the
// default; the defaults are the ones docs/architecture.md 7.3 names.
type Intervals struct {
	Scheduler   time.Duration // default 5s
	Finalizer   time.Duration // default 10s
	Canceller   time.Duration // default 2s
	LeaseReaper time.Duration // default 30s
	Retention   time.Duration // default 1h
	Outbox      time.Duration // default 1s
	// OutboxSweep is the slower pass that dispatches the events of tenants
	// that are not active (see loopSpecs).
	OutboxSweep time.Duration // default 10s
}

// Batches bounds how much work one tick of a loop does, so that a leader stays
// responsive on a campaign with a million rows. A zero field keeps the default.
type Batches struct {
	// CampaignPage is the page size used to walk campaigns by status.
	CampaignPage int // default 200
	// CancelChunk is how many deliveries one canceller tick moves per campaign.
	CancelChunk int // default 10_000
	// RetryChunk is how many deliveries one RetryCampaign call requeues at a time.
	RetryChunk int // default 10_000
	// LeaseReapLimit is the ReleaseExpiredLeases limit per tick.
	LeaseReapLimit int // default 5_000
	// RetentionChunk is the DeleteBefore limit per call, RetentionMaxChunks
	// the number of those calls one retention tick makes per campaign.
	RetentionChunk     int // default 5_000
	RetentionMaxChunks int // default 200
	// StatsRefreshScan bounds how many completed campaigns one finalizer tick
	// looks at when refreshing tracking uniques. The walk resumes where the
	// previous tick stopped, so a tenant with more completed campaigns than
	// this is covered over several ticks rather than skipped.
	StatsRefreshScan int // default 2_000
	// OutboxClaim is the ClaimPending limit per tick.
	OutboxClaim int // default 100
}

// Option configures New. Options are shared by the Leader, the loops and the
// tracking buffer.
type Option func(*config)

type config struct {
	owner       string
	leaderTTL   time.Duration
	leaderRetry time.Duration

	intervals Intervals
	batches   Batches

	outboxWorkers     int
	outboxLease       time.Duration
	outboxBackoff     []time.Duration
	outboxMaxAttempts int

	// largeCampaignRows is the delivery count above which a campaign is only
	// recounted every largeCampaignEvery ticks (architecture 7.3: "캠페인
	// 크기에 따라 증가").
	largeCampaignRows  int64
	largeCampaignEvery int

	trackFlushEvery time.Duration
	trackFlushSize  int
	trackMaxBuffer  int

	// trackingRefreshWindow is how long after completion a campaign's cached
	// tracking uniques keep being refreshed; tenantsRefresh is the TTL of the
	// Provider.Tenants listing the allTenants loops share.
	trackingRefreshWindow time.Duration
	tenantsRefresh        time.Duration

	// extraLoops are the leader loops registered from outside this package.
	extraLoops []loopSpec
}

func defaultConfig() config {
	return config{
		owner:       defaultOwner(),
		leaderTTL:   30 * time.Second,
		leaderRetry: 5 * time.Second,
		intervals: Intervals{
			Scheduler:   5 * time.Second,
			Finalizer:   10 * time.Second,
			Canceller:   2 * time.Second,
			LeaseReaper: 30 * time.Second,
			Retention:   time.Hour,
			Outbox:      time.Second,
			OutboxSweep: 10 * time.Second,
		},
		batches: Batches{
			CampaignPage:       200,
			CancelChunk:        10_000,
			RetryChunk:         10_000,
			LeaseReapLimit:     5_000,
			RetentionChunk:     5_000,
			RetentionMaxChunks: 200,
			StatsRefreshScan:   2_000,
			OutboxClaim:        100,
		},
		outboxWorkers:         8,
		outboxLease:           time.Minute,
		outboxBackoff:         []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour},
		outboxMaxAttempts:     10,
		largeCampaignRows:     100_000,
		largeCampaignEvery:    6,
		trackFlushEvery:       time.Second,
		trackFlushSize:        5_000,
		trackMaxBuffer:        100_000,
		trackingRefreshWindow: 14 * 24 * time.Hour,
		tenantsRefresh:        30 * time.Second,
	}
}

// defaultOwner identifies this replica in LockRepo and OutboxRepo leases.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), store.NewID()[:8])
}

// WithOwner replaces the generated lease owner. Two replicas must never share
// one: the leader lock renews for whoever names itself the current owner.
func WithOwner(owner string) Option {
	return func(c *config) {
		if owner != "" {
			c.owner = owner
		}
	}
}

// WithLeaderTTL sets the leader lease duration. The lease is renewed every
// ttl/3, so a crashed leader is replaced within roughly one ttl.
func WithLeaderTTL(ttl time.Duration) Option {
	return func(c *config) {
		if ttl > 0 {
			c.leaderTTL = ttl
		}
	}
}

// WithLeaderRetry sets how long a replica that lost the election waits before
// trying again.
func WithLeaderRetry(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.leaderRetry = d
		}
	}
}

// WithIntervals overrides the loop intervals. Zero fields keep their default.
func WithIntervals(iv Intervals) Option {
	return func(c *config) {
		setDur(&c.intervals.Scheduler, iv.Scheduler)
		setDur(&c.intervals.Finalizer, iv.Finalizer)
		setDur(&c.intervals.Canceller, iv.Canceller)
		setDur(&c.intervals.LeaseReaper, iv.LeaseReaper)
		setDur(&c.intervals.Retention, iv.Retention)
		setDur(&c.intervals.Outbox, iv.Outbox)
		setDur(&c.intervals.OutboxSweep, iv.OutboxSweep)
	}
}

// WithBatches overrides the per-tick work limits. Zero fields keep their default.
func WithBatches(b Batches) Option {
	return func(c *config) {
		setInt(&c.batches.CampaignPage, b.CampaignPage)
		setInt(&c.batches.CancelChunk, b.CancelChunk)
		setInt(&c.batches.RetryChunk, b.RetryChunk)
		setInt(&c.batches.LeaseReapLimit, b.LeaseReapLimit)
		setInt(&c.batches.RetentionChunk, b.RetentionChunk)
		setInt(&c.batches.RetentionMaxChunks, b.RetentionMaxChunks)
		setInt(&c.batches.StatsRefreshScan, b.StatsRefreshScan)
		setInt(&c.batches.OutboxClaim, b.OutboxClaim)
	}
}

// Loop describes an extra leader loop registered with WithLoop.
type Loop struct {
	// Name identifies the loop in logs.
	Name string
	// Interval is how often it ticks.
	Interval time.Duration
	// NewTenant builds the loop for one tenant; the Store stays valid for the
	// life of the instance.
	NewTenant func(st store.Store, tenantID string) TickLoop
	// AllTenants ticks every tenant Provider.Tenants knows of instead of only
	// the active ones. Set it when the loop's work arrives after the tenant
	// has gone quiet - the loopback probe is collected a minute after the
	// probe delivery went terminal, by which time the tenant is idle unless
	// it happens to be busy with something else.
	AllTenants bool
}

// WithLoop registers an extra leader loop. It gets exactly the treatment the
// built-in loops get: it runs on the replica holding the leader lease and
// nowhere else, it is built once per ticked tenant and handed that tenant's
// Store, and a tick that fails is logged without stopping the other tenants
// or the other loops.
//
// It exists because the loopback probe (internal/probe) must run on the leader
// alone - two replicas would send twice the probe mail and both try to delete
// the same message - but this package cannot import it without a cycle. The
// root package registers it instead.
//
// A loop with an empty name, a non-positive interval or a nil constructor is
// ignored, so a caller can register one conditionally without branching.
func WithLoop(l Loop) Option {
	return func(c *config) {
		if l.Name == "" || l.Interval <= 0 || l.NewTenant == nil {
			return
		}
		c.extraLoops = append(c.extraLoops, loopSpec{
			name: l.Name, interval: l.Interval, newTenant: l.NewTenant,
			allTenants: l.AllTenants,
		})
	}
}

// WithOutboxWorkers bounds how many events one outbox tick hands to the
// EventSink concurrently.
func WithOutboxWorkers(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.outboxWorkers = n
		}
	}
}

// WithOutboxLease sets how long a claimed outbox event stays leased.
func WithOutboxLease(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.outboxLease = d
		}
	}
}

// WithOutboxRetry replaces the dispatch backoff schedule and the attempt count
// after which an event stays in the dead letter state. backoff[n-1] is the
// wait after the nth failure; the last entry is reused.
func WithOutboxRetry(backoff []time.Duration, maxAttempts int) Option {
	return func(c *config) {
		if len(backoff) > 0 {
			c.outboxBackoff = append([]time.Duration(nil), backoff...)
		}
		if maxAttempts > 0 {
			c.outboxMaxAttempts = maxAttempts
		}
	}
}

// WithLargeCampaign tunes the finalizer's adaptive interval: a campaign with
// more than rows deliveries is recounted only every `every` ticks.
func WithLargeCampaign(rows int64, every int) Option {
	return func(c *config) {
		if rows > 0 {
			c.largeCampaignRows = rows
		}
		if every > 0 {
			c.largeCampaignEvery = every
		}
	}
}

// WithTrackingRefresh sets how long after a campaign completed its cached
// tracking uniques keep being refreshed by the finalizer (default 14 days).
// Opens, clicks and unsubscribes almost always arrive after the campaign is
// over, so without this the cached unique counts of architecture 9.3 would
// freeze at whatever they were the moment the last delivery went terminal.
func WithTrackingRefresh(window time.Duration) Option {
	return func(c *config) { setDur(&c.trackingRefreshWindow, window) }
}

// WithTenantsRefresh sets how often the loops that tick every tenant
// (Loop.AllTenants) refresh the Provider.Tenants listing. Default 30s.
func WithTenantsRefresh(d time.Duration) Option {
	return func(c *config) { setDur(&c.tenantsRefresh, d) }
}

// WithTracking tunes the tracking buffer: flush interval, the buffered count
// that forces an early flush, and the cap above which the oldest records are
// dropped.
func WithTracking(flushEvery time.Duration, flushSize, maxBuffer int) Option {
	return func(c *config) {
		setDur(&c.trackFlushEvery, flushEvery)
		setInt(&c.trackFlushSize, flushSize)
		setInt(&c.trackMaxBuffer, maxBuffer)
	}
}

func setDur(dst *time.Duration, v time.Duration) {
	if v > 0 {
		*dst = v
	}
}

func setInt(dst *int, v int) {
	if v > 0 {
		*dst = v
	}
}
