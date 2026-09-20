package bounce

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// TenantMailbox is one mailbox to poll, already resolved to a tenant. The root
// package builds these from whatever it keeps bounce mailboxes in; this
// package never reads configuration itself.
type TenantMailbox struct {
	TenantID string
	// MailboxID must be unique across tenants for the lock name to be, which
	// a store ID already is.
	MailboxID string
	Config    mailbox.Config
	// PollInterval overrides the runner default for this mailbox. Zero uses
	// the default.
	PollInterval time.Duration
}

func (m TenantMailbox) key() string { return m.TenantID + "/" + m.MailboxID }

// MailboxSource lists the mailboxes to poll. It is re-read periodically, so a
// mailbox added or removed at runtime is picked up without a restart.
//
// Provider.ActiveTenants is deliberately not used: bounces arrive hours or
// days after a campaign finished, long after the tenant stopped being active.
type MailboxSource interface {
	ListMailboxes(ctx context.Context) ([]TenantMailbox, error)
}

// MailboxSourceFunc adapts a function to MailboxSource.
type MailboxSourceFunc func(ctx context.Context) ([]TenantMailbox, error)

func (f MailboxSourceFunc) ListMailboxes(ctx context.Context) ([]TenantMailbox, error) {
	return f(ctx)
}

// LockName is the LockRepo key one mailbox's poller holds, in that tenant's
// store. One poller per mailbox is what keeps replicas from processing the
// same mail twice (architecture 10).
func LockName(mailboxID string) string { return "bounce:" + mailboxID }

// DialFunc opens a mailbox. It is a field on RunnerConfig so that tests can
// hand out a mailbox.Fake.
type DialFunc func(ctx context.Context, cfg mailbox.Config, cipher host.SecretCipher) (mailbox.Client, error)

// RunnerConfig configures the poller.
type RunnerConfig struct {
	// Owner identifies this replica as the lock holder. It must be unique in
	// the cluster and stable for the life of the process.
	Owner    string
	Provider store.Provider
	Source   MailboxSource
	// Processor is shared by every mailbox. Nil builds a default one.
	Processor *Processor
	Secrets   host.SecretCipher

	// PollInterval is how long a mailbox waits between passes. FetchBatch is
	// how many messages one pass takes at a time.
	PollInterval time.Duration
	FetchBatch   int
	// RefreshInterval is how often the mailbox list is re-read.
	RefreshInterval time.Duration
	// LockTTL is the mailbox lease. It is renewed after every batch, so it
	// only has to outlive one batch, not one pass.
	LockTTL time.Duration
	// SessionTimeout bounds one whole pass (dial, fetch, handle, ack).
	SessionTimeout time.Duration
	// MaxBackoff caps the per-mailbox backoff after a failure.
	MaxBackoff time.Duration
	// UseIdle lets a pass wait in IMAP IDLE instead of sleeping, when the
	// server supports it.
	UseIdle bool

	Logger  *slog.Logger
	Metrics host.Metrics
	Clock   func() time.Time
	// Dial is the mailbox dialer. Nil uses mailbox.Dial.
	Dial DialFunc
}

// Defaults (architecture 10: a bounce mailbox is polled, not streamed).
const (
	DefaultPollInterval    = time.Minute
	DefaultFetchBatch      = 50
	DefaultRefreshInterval = 5 * time.Minute
	DefaultLockTTL         = 2 * time.Minute
	DefaultSessionTimeout  = 10 * time.Minute
	DefaultMaxBackoff      = 15 * time.Minute
)

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.FetchBatch <= 0 {
		c.FetchBatch = DefaultFetchBatch
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = DefaultRefreshInterval
	}
	if c.LockTTL <= 0 {
		c.LockTTL = DefaultLockTTL
	}
	if c.SessionTimeout <= 0 {
		c.SessionTimeout = DefaultSessionTimeout
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Metrics == nil {
		c.Metrics = host.NopMetrics{}
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Dial == nil {
		c.Dial = mailbox.Dial
	}
	if c.Processor == nil {
		c.Processor = NewProcessor(Options{
			Clock: c.Clock, Logger: c.Logger, Metrics: c.Metrics,
		})
	}
	return c
}

// Runner polls every configured mailbox, one goroutine each.
type Runner struct {
	cfg RunnerConfig

	mu      sync.Mutex
	workers map[string]context.CancelFunc
}

// NewRunner builds a Runner. It fails only on a configuration that cannot
// work at all.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Provider == nil {
		return nil, errors.New("bounce: RunnerConfig.Provider is required")
	}
	if cfg.Source == nil {
		return nil, errors.New("bounce: RunnerConfig.Source is required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("bounce: RunnerConfig.Owner is required")
	}
	return &Runner{cfg: cfg.withDefaults(), workers: map[string]context.CancelFunc{}}, nil
}

// Run blocks until ctx is done. It never returns an error for a mailbox that
// is failing: a broken IMAP account must not take the process down, so every
// failure is logged, counted and backed off.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer func() {
		r.stopAll()
		wg.Wait()
	}()

	t := time.NewTicker(r.cfg.RefreshInterval)
	defer t.Stop()
	for {
		r.refresh(ctx, &wg)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// refresh starts a worker for every new mailbox and stops the ones that left
// the list.
func (r *Runner) refresh(ctx context.Context, wg *sync.WaitGroup) {
	boxes, err := r.cfg.Source.ListMailboxes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			r.cfg.Logger.Error("sendplane: listing bounce mailboxes failed", "err", err)
			r.cfg.Metrics.Count(MetricPollError, 1, "phase", "list")
		}
		return
	}

	want := make(map[string]TenantMailbox, len(boxes))
	for _, b := range boxes {
		if b.TenantID == "" || b.MailboxID == "" {
			r.cfg.Logger.Error("sendplane: bounce mailbox without a tenant or id, skipped",
				"tenant", b.TenantID, "mailbox", b.MailboxID)
			continue
		}
		want[b.key()] = b
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for key, stop := range r.workers {
		if _, ok := want[key]; !ok {
			stop()
			delete(r.workers, key)
		}
	}
	for key, box := range want {
		if _, ok := r.workers[key]; ok {
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		r.workers[key] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.pollLoop(workerCtx, box)
		}()
	}
}

func (r *Runner) stopAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, stop := range r.workers {
		stop()
		delete(r.workers, key)
	}
}

// pollLoop is one mailbox's goroutine: poll, sleep, repeat, backing off after
// failures and never returning until ctx is done.
func (r *Runner) pollLoop(ctx context.Context, box TenantMailbox) {
	interval := box.PollInterval
	if interval <= 0 {
		interval = r.cfg.PollInterval
	}
	backoff := time.Duration(0)

	for ctx.Err() == nil {
		stats, err := r.PollOnce(ctx, box)
		switch {
		case err != nil && ctx.Err() == nil:
			backoff = nextBackoff(backoff, interval, r.cfg.MaxBackoff)
			r.cfg.Metrics.Count(MetricPollError, 1, "phase", "poll")
			r.cfg.Logger.Error("sendplane: bounce poll failed",
				"tenant", box.TenantID, "mailbox", box.MailboxID,
				"retry_in", backoff, "err", err)
		default:
			backoff = 0
		}

		wait := interval
		if backoff > 0 {
			wait = backoff
		} else if stats.Idled {
			// IDLE already waited; go straight back for the new mail.
			wait = 0
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// Stats is what one pass did.
type Stats struct {
	// Locked is false when another replica holds the mailbox, which is the
	// normal state of every replica but one.
	Locked    bool
	Fetched   int
	Processed int
	Recorded  int
	Skipped   int
	Acked     int
	// Idled is true when the pass ended in an IMAP IDLE instead of returning
	// immediately.
	Idled bool
}

// PollOnce runs one pass over one mailbox: take the lock, dial, drain in
// batches, acknowledge, release. It is exported so that a host (and the
// tests) can drive a single deterministic pass.
func (r *Runner) PollOnce(ctx context.Context, box TenantMailbox) (Stats, error) {
	var stats Stats

	st, err := r.cfg.Provider.ForTenant(ctx, box.TenantID)
	if err != nil {
		return stats, fmt.Errorf("bounce: store for tenant %s: %w", box.TenantID, err)
	}
	name := LockName(box.MailboxID)
	ok, err := st.Locks().Acquire(ctx, name, r.cfg.Owner, r.cfg.LockTTL, r.cfg.Clock().UTC())
	if err != nil {
		return stats, fmt.Errorf("bounce: acquire %s: %w", name, err)
	}
	if !ok {
		return stats, nil
	}
	stats.Locked = true
	defer func() {
		// The release runs on its own context: a cancelled Run must still
		// hand the mailbox back instead of leaving it locked until the TTL.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := st.Locks().Release(releaseCtx, name, r.cfg.Owner); err != nil {
			r.cfg.Logger.Warn("sendplane: releasing bounce mailbox lock failed",
				"mailbox", box.MailboxID, "err", err)
		}
	}()

	sessionCtx, cancel := context.WithTimeout(ctx, r.cfg.SessionTimeout)
	defer cancel()

	client, err := r.cfg.Dial(sessionCtx, box.Config, r.cfg.Secrets)
	if err != nil {
		return stats, err
	}
	defer func() {
		if err := client.Close(); err != nil {
			r.cfg.Logger.Warn("sendplane: closing bounce mailbox failed",
				"mailbox", box.MailboxID, "err", err)
		}
	}()

	action := box.Config.AfterProcess
	if action == "" {
		action = mailbox.ActionKeep
	}
	for {
		msgs, err := client.Fetch(sessionCtx, r.cfg.FetchBatch)
		if err != nil {
			return stats, fmt.Errorf("bounce: fetch %s: %w", box.MailboxID, err)
		}
		if len(msgs) == 0 {
			break
		}
		stats.Fetched += len(msgs)

		handled := make([]string, 0, len(msgs))
		for _, msg := range msgs {
			out, err := r.cfg.Processor.Handle(sessionCtx, st, box.TenantID, msg)
			if err != nil {
				// The store failed. Acknowledge what did work and stop: the
				// rest comes back on the next pass.
				if ackErr := r.ack(sessionCtx, client, handled, action); ackErr != nil {
					r.cfg.Logger.Error("sendplane: acking bounce mail failed",
						"mailbox", box.MailboxID, "err", ackErr)
				}
				stats.Acked += len(handled)
				return stats, fmt.Errorf("bounce: handle %s: %w", msg.ID, err)
			}
			handled = append(handled, msg.ID)
			if out.Processed {
				stats.Processed++
			}
			if out.Recorded {
				stats.Recorded++
			}
			if out.Skipped != "" {
				stats.Skipped++
			}
		}
		if err := r.ack(sessionCtx, client, handled, action); err != nil {
			return stats, fmt.Errorf("bounce: ack %s: %w", box.MailboxID, err)
		}
		stats.Acked += len(handled)

		// Renewing after each batch is what lets a big backlog be drained in
		// one pass without the lease expiring under it.
		renewed, err := st.Locks().Renew(sessionCtx, name, r.cfg.Owner, r.cfg.LockTTL, r.cfg.Clock().UTC())
		if err != nil {
			return stats, fmt.Errorf("bounce: renew %s: %w", name, err)
		}
		if !renewed {
			// Somebody took the mailbox over: stop writing at once.
			r.cfg.Logger.Warn("sendplane: bounce mailbox lease lost",
				"mailbox", box.MailboxID, "owner", r.cfg.Owner)
			return stats, nil
		}
		if len(msgs) < r.cfg.FetchBatch {
			break
		}
	}

	if r.cfg.UseIdle {
		if idler, ok := client.(mailbox.Idler); ok {
			interval := box.PollInterval
			if interval <= 0 {
				interval = r.cfg.PollInterval
			}
			err := idler.Idle(sessionCtx, interval)
			switch {
			case err == nil:
				stats.Idled = true
			case errors.Is(err, mailbox.ErrNoIdle):
				// The server does not do IDLE; the caller sleeps instead.
			default:
				return stats, fmt.Errorf("bounce: idle %s: %w", box.MailboxID, err)
			}
		}
	}
	return stats, nil
}

func (r *Runner) ack(ctx context.Context, client mailbox.Client, ids []string, action mailbox.Action) error {
	if len(ids) == 0 {
		return nil
	}
	return client.Ack(ctx, ids, action)
}

// nextBackoff doubles from one interval up to max.
func nextBackoff(current, interval, max time.Duration) time.Duration {
	if current <= 0 {
		current = interval
	} else {
		current *= 2
	}
	if current > max {
		current = max
	}
	return current
}

// sleep waits, and reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
