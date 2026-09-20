package control

import (
	"context"
	"log/slog"
	"time"

	"github.com/sendplane/sendplane/store"
)

// SystemTenantID is the tenant the leader lock lives in.
//
// store.Store is tenant-bound on purpose (ADR-0006), but leader election is a
// cluster-wide singleton with no tenant of its own, so it needs a scope that
// belongs to no customer. The name starts with an underscore, which tenant IDs
// handed out by a host never do.
//
// This arguably belongs in the store package next to DefaultTenantID, so that
// a custom store.Provider knows the scope exists and does not, say, iterate it
// as a real tenant. It is declared here because internal/control may not
// change the store contract; see the package README.
const SystemTenantID = "_system"

// LeaderLockName is the LockRepo key the control leader holds.
const LeaderLockName = "control-leader"

// Leader runs the registered loops on exactly one replica at a time. It takes
// a lease from LockRepo (ADR-0002), renews it at ttl/3 while the loops run,
// and stops every loop the moment a renewal fails, so that a replica which
// lost the lease (network partition, GC pause, clock skew) stops writing
// before another replica starts.
type Leader struct {
	provider store.Provider
	log      *slog.Logger
	clock    func() time.Time
	cfg      config
	loops    []loopSpec

	// onState, when set, is called with true when the loops start and false
	// when they stop. Tests use it; production leaves it nil.
	onState func(bool)
}

// newLeader builds a Leader with no loops registered.
func newLeader(provider store.Provider, log *slog.Logger, clock func() time.Time, cfg config) *Leader {
	return &Leader{provider: provider, log: log, clock: clock, cfg: cfg}
}

// register adds a loop. It must be called before Run.
func (l *Leader) register(specs ...loopSpec) {
	l.loops = append(l.loops, specs...)
}

// Run blocks until ctx is done, acquiring the lease and running the loops
// whenever it can. It returns nil on a clean shutdown: losing an election is
// the normal state of a non-leader replica, not an error.
func (l *Leader) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		l.attempt(ctx)
		timer.Reset(l.cfg.leaderRetry)
	}
}

// attempt tries to take the lease once and, if it got it, holds it until it is
// lost or ctx is done.
func (l *Leader) attempt(ctx context.Context) {
	st, err := l.provider.ForTenant(ctx, SystemTenantID)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Error("control: leader cannot reach the store", "err", err)
		}
		return
	}
	ok, err := st.Locks().Acquire(ctx, LeaderLockName, l.cfg.owner, l.cfg.leaderTTL, l.now())
	if err != nil {
		if ctx.Err() == nil {
			l.log.Error("control: leader acquire failed", "err", err)
		}
		return
	}
	if !ok {
		return
	}
	l.hold(ctx, st)
}

// hold runs the loops until the lease is lost or ctx is done.
func (l *Leader) hold(ctx context.Context, st store.Store) {
	l.log.Info("control: leader acquired", "owner", l.cfg.owner, "loops", len(l.loops))
	if l.onState != nil {
		l.onState(true)
	}

	loopCtx, stopLoops := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.runLoops(loopCtx)
	}()

	renew := time.NewTicker(max(l.cfg.leaderTTL/3, time.Millisecond))
	defer renew.Stop()
	reason := "shutdown"
renewal:
	for {
		select {
		case <-loopCtx.Done():
			break renewal
		case <-renew.C:
			ok, err := st.Locks().Renew(loopCtx, LeaderLockName, l.cfg.owner, l.cfg.leaderTTL, l.now())
			if err != nil {
				reason = "renew failed: " + err.Error()
				break renewal
			}
			if !ok {
				reason = "lease taken over"
				break renewal
			}
		}
	}

	stopLoops()
	<-done
	if l.onState != nil {
		l.onState(false)
	}
	l.log.Info("control: leader stopped", "owner", l.cfg.owner, "reason", reason)

	// Release runs outside the (possibly cancelled) ctx so a graceful
	// shutdown hands the lease over immediately instead of leaving the
	// cluster idle for a ttl. It is a no-op when someone else already
	// took over.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := st.Locks().Release(rctx, LeaderLockName, l.cfg.owner); err != nil {
		l.log.Warn("control: leader release failed", "err", err)
	}
}

// runLoops starts one goroutine per registered loop and waits for all of them.
func (l *Leader) runLoops(ctx context.Context) {
	done := make(chan struct{}, len(l.loops))
	for _, spec := range l.loops {
		go func(s loopSpec) {
			defer func() { done <- struct{}{} }()
			l.runLoop(ctx, s)
		}(spec)
	}
	for range l.loops {
		<-done
	}
}

func (l *Leader) now() time.Time { return store.TruncateTime(l.clock()) }
