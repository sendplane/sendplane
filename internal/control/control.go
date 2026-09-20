// Package control runs sendplane's control plane: the background loops that
// keep campaigns and deliveries moving, and the synchronous campaign
// transitions the HTTP layer calls.
//
// Two things run here, and they have different cardinality:
//
//   - The leader loops (scheduler, finalizer, canceller, lease reaper,
//     retention, outbox dispatcher) run on exactly one replica, kept singleton
//     by a LockRepo lease (ADR-0002). They are periodic, idempotent and
//     chunked: every one of them must be able to give up half way through a
//     million rows and pick up where it left off on the next tick.
//   - The tracking buffer runs on every replica, because it belongs to the
//     public pixel and redirect handlers that received the events
//     (architecture 9.3).
//
// Nothing in here talks to another process. Control and sender communicate
// only through the store, which is what lets start, pause and cancel be
// single-row writes on a campaign with a million recipients.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Control is the control plane. Build it once per process and call Run.
type Control struct {
	provider store.Provider
	hooks    host.Hooks
	log      *slog.Logger
	clock    func() time.Time
	cfg      config

	leader   *Leader
	tracking *TrackingBuffer
}

// New builds the control plane. provider is required; logger and clock fall
// back to slog.Default and time.Now.
func New(provider store.Provider, hooks host.Hooks, logger *slog.Logger, clock func() time.Time, opts ...Option) (*Control, error) {
	if provider == nil {
		return nil, fmt.Errorf("control: store provider is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	c := &Control{provider: provider, hooks: hooks, log: logger, clock: clock, cfg: cfg}
	c.tracking = newTrackingBuffer(provider, logger, clock, cfg)
	c.leader = newLeader(provider, logger, clock, cfg)
	c.leader.register(c.loopSpecs()...)
	return c, nil
}

// Tracking returns the shared tracking buffer. The public tracking handlers
// record on it; it only flushes while Run is active.
func (c *Control) Tracking() *TrackingBuffer { return c.tracking }

// Run starts the tracking buffer and the leader, and blocks until ctx is done.
// It returns nil on a clean shutdown.
func (c *Control) Run(ctx context.Context) error {
	c.tracking.start(ctx)
	defer func() {
		if err := c.tracking.Close(); err != nil {
			c.log.Error("control: tracking buffer close failed", "err", err)
		}
	}()
	c.log.Info("control: starting", "owner", c.cfg.owner)
	return c.leader.Run(ctx)
}

// outboxLingerTicks is how many rounds the outbox dispatcher keeps ticking a
// tenant that just went idle, so the events that idling produced are still
// dispatched promptly.
const outboxLingerTicks = 3

// loopSpecs is the registered loop list. The outbox dispatcher is only
// registered when the host actually wants events: claiming rows nobody
// consumes would burn their attempts for nothing.
func (c *Control) loopSpecs() []loopSpec {
	specs := []loopSpec{
		{
			name:     "scheduler",
			interval: c.cfg.intervals.Scheduler,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &scheduler{st: st, log: c.tenantLog("scheduler", tenantID), page: c.cfg.batches.CampaignPage}
			},
		},
		{
			name:     "finalizer",
			interval: c.cfg.intervals.Finalizer,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &finalizer{st: st, log: c.tenantLog("finalizer", tenantID), cfg: &c.cfg, skips: map[string]int{}}
			},
		},
		{
			name:     "canceller",
			interval: c.cfg.intervals.Canceller,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &canceller{st: st, log: c.tenantLog("canceller", tenantID),
					page: c.cfg.batches.CampaignPage, chunk: c.cfg.batches.CancelChunk}
			},
		},
		{
			name:     "lease-reaper",
			interval: c.cfg.intervals.LeaseReaper,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &leaseReaper{st: st, log: c.tenantLog("lease-reaper", tenantID), limit: c.cfg.batches.LeaseReapLimit}
			},
		},
		{
			name:     "retention",
			interval: c.cfg.intervals.Retention,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &retention{st: st, tenant: tenantID, log: c.tenantLog("retention", tenantID), cfg: &c.cfg}
			},
		},
	}
	if c.hooks.Events != nil {
		specs = append(specs, loopSpec{
			name:     "outbox",
			interval: c.cfg.intervals.Outbox,
			// The only loop with a grace window: the transition that enqueues
			// campaign.completed is the same one that takes the tenant out of
			// ActiveTenants (see tenantSet).
			linger: outboxLingerTicks,
			newTenant: func(st store.Store, tenantID string) tickLoop {
				return &outboxDispatcher{st: st, sink: c.hooks.Events,
					log: c.tenantLog("outbox", tenantID), cfg: &c.cfg, clock: c.clock}
			},
		})
	} else {
		c.log.Warn("control: no EventSink configured, the event outbox will not be dispatched")
	}
	return append(specs, c.cfg.extraLoops...)
}

func (c *Control) tenantLog(loop, tenantID string) *slog.Logger {
	return c.log.With("loop", loop, "tenant", tenantID)
}

func (c *Control) now() time.Time { return store.TruncateTime(c.clock()) }
