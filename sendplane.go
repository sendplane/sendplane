// Package sendplane is an embeddable mail sending engine: templates,
// campaigns, transactional messages, per-message retries, bounce handling,
// tracking and sending-domain health.
//
// The host application owns accounts, subscribers and the unsubscribe page;
// it injects authentication, authorization, tenant resolution and hooks
// through Options and mounts Handler on its own mux. See docs/architecture.md.
//
// The types a host injects or receives (Hooks, Principal, Action, Limits,
// EventSink, ...) live in the leaf package host and are re-exported here as
// aliases, so sendplane.Hooks and host.Hooks are one type. That is what lets
// this package import internal/control and internal/sender without a cycle.
package sendplane

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/control"
	"github.com/sendplane/sendplane/internal/sender"
	"github.com/sendplane/sendplane/store"
)

// Sendplane is the engine instance. It is safe for concurrent use.
type Sendplane struct {
	opts Options
}

// SenderConfig configures one sender process (RunSender). Every field except
// WorkerID has a default; see docs/architecture.md 8.
type SenderConfig struct {
	// WorkerID identifies this replica. It is the lease owner on claimed
	// deliveries and the ID of its heartbeat row, so it must be stable for the
	// life of the process and unique in the cluster. Required.
	WorkerID string

	// Lanes is the worker pool size per lane. Transactional is always offered
	// capacity before bulk. Default: 8 transactional, 32 bulk.
	Lanes map[store.Lane]int
	// ClaimBatch is the maximum number of deliveries claimed in one call.
	ClaimBatch int
	// LeaseFor is how long a claim holds a delivery.
	LeaseFor time.Duration
	// PollInterval is how long the loop sleeps after a pass that claimed
	// nothing.
	PollInterval time.Duration
	// CampaignRefresh is the TTL of the running-campaign set (ADR-0002).
	CampaignRefresh time.Duration
	// TenantConcurrency caps the deliveries one tenant may occupy at once, so
	// one big campaign cannot starve the others.
	TenantConcurrency int
	// DefaultRatePerSecond applies to transports that configure no rate. Zero
	// means unlimited.
	DefaultRatePerSecond float64

	// MaxMsgsPerConn is how many messages one pooled SMTP connection carries
	// before it is recycled. Zero means no limit.
	MaxMsgsPerConn int
	// EHLOName is the name announced in EHLO. Default: the local hostname.
	EHLOName string
	// TLSConfig is used for STARTTLS and implicit TLS connections.
	TLSConfig *tls.Config
}

// New validates options and applies defaults.
func New(o Options) (*Sendplane, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("sendplane: Options.Store is required")
	}
	if o.Auth == nil {
		return nil, fmt.Errorf("sendplane: Options.Auth is required")
	}
	if o.Authz == nil {
		o.Authz = allowAll{}
	}
	if o.Tenants == nil {
		o.Tenants = principalTenant{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Metrics == nil {
		o.Metrics = host.NopMetrics{}
	}
	o.Limits = o.Limits.WithDefaults()
	return &Sendplane{opts: o}, nil
}

// Options returns the effective options, with defaults applied.
func (s *Sendplane) Options() Options { return s.opts }

// Handler returns the /api/v1 router to mount on the host's mux.
func (s *Sendplane) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, ErrNotImplemented.Error(), http.StatusNotImplemented)
	})
}

// RunControl runs the control plane: the leader loops (scheduler, finalizer,
// canceller, lease reaper, retention, event outbox) and the tracking buffer.
// It blocks until ctx is done and returns nil on a clean shutdown.
//
// Every replica may call it. Exactly one of them holds the leader lease at a
// time, so the loops run once cluster-wide (ADR-0002).
func (s *Sendplane) RunControl(ctx context.Context) error {
	c, err := control.New(s.opts.Store, s.opts.Hooks, s.opts.Logger, s.opts.Clock)
	if err != nil {
		return err
	}
	return c.Run(ctx)
}

// RunSender runs the claim/render/SMTP loop. It blocks until ctx is done and
// returns nil on a clean shutdown.
func (s *Sendplane) RunSender(ctx context.Context, c SenderConfig) error {
	snd, err := sender.New(s.opts.Store, sender.Config{
		WorkerID:             c.WorkerID,
		Lanes:                c.Lanes,
		ClaimBatch:           c.ClaimBatch,
		LeaseFor:             c.LeaseFor,
		PollInterval:         c.PollInterval,
		CampaignRefresh:      c.CampaignRefresh,
		TenantConcurrency:    c.TenantConcurrency,
		DefaultRatePerSecond: c.DefaultRatePerSecond,
		MaxMsgsPerConn:       c.MaxMsgsPerConn,
		EHLOName:             c.EHLOName,
		TLSConfig:            c.TLSConfig,

		Hooks:   s.opts.Hooks,
		Secrets: s.opts.Secrets,
		Metrics: s.opts.Metrics,
		Logger:  s.opts.Logger,
		Clock:   s.opts.Clock,
	})
	if err != nil {
		return err
	}
	return snd.Run(ctx)
}

// RunBounce runs the IMAP/POP3 bounce mailbox poller.
func (s *Sendplane) RunBounce(ctx context.Context) error { return ErrNotImplemented }
