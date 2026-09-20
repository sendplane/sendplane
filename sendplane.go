// Package sendplane is an embeddable mail sending engine: templates,
// campaigns, transactional messages, per-message retries, bounce handling,
// tracking and sending-domain health.
//
// The host application owns accounts, subscribers and the unsubscribe page;
// it injects authentication, authorization, tenant resolution and hooks
// through Options and mounts Handler on its own mux. See docs/architecture.md.
package sendplane

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Sendplane is the engine instance. It is safe for concurrent use.
type Sendplane struct {
	opts Options
}

// SenderConfig configures one sender process (RunSender).
type SenderConfig struct {
	WorkerID    string
	Concurrency int
	Lanes       []string
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
	o.Limits = o.Limits.withDefaults()
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

// RunControl runs the leader loops (scheduler, finalizer, aggregation,
// health checks, retention, lease reclaim, event outbox).
func (s *Sendplane) RunControl(ctx context.Context) error { return ErrNotImplemented }

// RunSender runs the claim/render/SMTP loop.
func (s *Sendplane) RunSender(ctx context.Context, c SenderConfig) error { return ErrNotImplemented }

// RunBounce runs the IMAP/POP3 bounce mailbox poller.
func (s *Sendplane) RunBounce(ctx context.Context) error { return ErrNotImplemented }
