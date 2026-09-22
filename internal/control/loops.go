package control

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/sendplane/sendplane/store"
)

// TickLoop is one loop's unit of work for one tenant. Implementations are
// built per tenant and keep whatever per-tenant state they need between ticks
// (the finalizer's adaptive counter, for instance).
//
// It is exported so that a loop living outside this package - the loopback
// probe, which cannot be imported here without a cycle - can be registered
// with WithLoop and get the same leader-only, per-tenant treatment as the
// built-in ones.
type TickLoop interface {
	Tick(ctx context.Context, now time.Time) error
}

// tickLoop is the internal spelling of the same thing.
type tickLoop = TickLoop

// loopSpec registers a loop with the Leader: a name for logs, the interval it
// ticks on, and how to build its per-tenant instance.
type loopSpec struct {
	name     string
	interval time.Duration
	// newTenant builds the loop for one tenant. The Store it is handed stays
	// valid for the life of the instance.
	newTenant func(st store.Store, tenantID string) tickLoop
	// linger is how many extra rounds a tenant keeps being ticked after it
	// left Provider.ActiveTenants. Zero for every loop but the outbox; see
	// tenantSet.
	linger int
	// allTenants ticks every tenant Provider.Tenants knows of, not only the
	// active ones. It is for the loops whose work arrives *after* the tenant
	// went quiet: the loopback probe is judged a minute after the probe mail
	// went terminal, retention deletes rows of campaigns that finished weeks
	// ago, and a recipient opens a newsletter long after the campaign
	// completed. Those loops would otherwise only ever run for a tenant that
	// happens to be busy with something else (the bounce poller iterates
	// Provider.Tenants for exactly this reason; see bounce.go).
	//
	// The active set is still ticked, and immediately: the known-tenant list
	// is refreshed on its own interval (Leader.knownTenants) because a
	// Provider may compute it expensively, and a loop must not wait for that
	// refresh to reach a tenant that has work right now.
	allTenants bool
	// includeSystem also ticks store.SystemTenantID, which no tenant listing
	// returns. It is how the platform loops reach the shared senders and
	// mailboxes, which the overlay only makes visible there (ADR-0017).
	includeSystem bool
}

// tenantSet is one loop's state between ticks: the per-tenant loop instances,
// plus how many more rounds a tenant that left the active set still gets.
//
// Five of the six loops have no grace period. Provider.ActiveTenants reports a
// tenant while it has a scheduled, running or paused campaign, not only while
// it has claimable deliveries, so the tick that matters most — the one right
// after a campaign's last delivery went terminal — is reached by the store
// contract itself. This used to be a "keep ticking a few more rounds"
// heuristic that a leader restart defeated, leaving campaigns stuck in
// running.
//
// The outbox dispatcher still needs one, and for a different reason: the tick
// that completes a campaign is also the tick that enqueues
// campaign.completed, and it is precisely that transition which drops the
// tenant out of the active set. Without a grace window the event would sit
// pending until the tenant had work again. This is a window, not a guarantee:
// an event whose dispatch fails is retried on the outbox backoff, and a tenant
// that stays idle that long gets it on its next campaign. Draining a fully
// idle tenant's outbox needs a queue the store can enumerate, which the
// contract does not have.
type tenantSet struct {
	loops  map[string]tickLoop
	linger map[string]int
}

func newTenantSet() *tenantSet {
	return &tenantSet{loops: map[string]tickLoop{}, linger: map[string]int{}}
}

// runLoop ticks one loop until ctx is done. It ticks once immediately so that
// a freshly elected leader does not wait a full interval before catching up.
func (l *Leader) runLoop(ctx context.Context, spec loopSpec) {
	set := newTenantSet()
	t := time.NewTicker(spec.interval)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		l.tickTenants(ctx, spec, set)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tickTenants runs one tick of spec for every active tenant, plus the ones
// still lingering. One tenant's failure is logged and never stops the others:
// a loop that dies takes the whole control plane down with it until the lease
// expires.
func (l *Leader) tickTenants(ctx context.Context, spec loopSpec, set *tenantSet) {
	tenants, err := l.provider.ActiveTenants(ctx)
	if err != nil {
		if ctx.Err() == nil {
			l.log.Error("control: cannot list active tenants", "loop", spec.name, "err", err)
		}
		return
	}
	live := make(map[string]bool, len(tenants))
	for _, tenantID := range tenants {
		live[tenantID] = true
		if spec.linger > 0 {
			set.linger[tenantID] = spec.linger
		}
	}
	if spec.includeSystem {
		// No tenant listing returns the system tenant (store.Provider), so a
		// loop whose work is the platform's has to be told to include it.
		live[store.SystemTenantID] = true
	}
	if spec.allTenants {
		known, err := l.knownTenants(ctx)
		if err != nil {
			// The active half is still worth ticking: a failed listing
			// delays the idle tenants by one interval, it does not stop the
			// loop.
			if ctx.Err() == nil {
				l.log.Error("control: cannot list tenants", "loop", spec.name, "err", err)
			}
		}
		for _, tenantID := range known {
			if tenantID == store.SystemTenantID {
				// The contract says Tenants never returns it; a custom
				// Provider that does anyway must not get a loop ticking a
				// tenant that does not exist.
				continue
			}
			live[tenantID] = true
		}
	}

	// live is what this loop ticks now (the active tenants, plus every known
	// tenant for an allTenants loop); a loop with a grace window also ticks
	// the tenants still counting down.
	ids := make([]string, 0, len(tenants)+len(set.linger))
	seen := make(map[string]bool, len(ids))
	for tenantID := range live {
		ids, seen[tenantID] = append(ids, tenantID), true
	}
	for tenantID := range set.linger {
		if !seen[tenantID] {
			ids = append(ids, tenantID)
		}
	}
	sort.Strings(ids)

	now := l.now()
	for _, tenantID := range ids {
		if ctx.Err() != nil {
			return
		}
		in, ok := set.loops[tenantID]
		if !ok {
			st, err := l.provider.ForTenant(ctx, tenantID)
			if err != nil {
				l.log.Error("control: cannot open tenant store",
					"loop", spec.name, "tenant", tenantID, "err", err)
				continue
			}
			in = spec.newTenant(st, tenantID)
			set.loops[tenantID] = in
		}
		if err := in.Tick(ctx, now); err != nil && ctx.Err() == nil {
			l.log.Error("control: loop tick failed",
				"loop", spec.name, "tenant", tenantID, "err", err)
		}
	}

	// Drop the tenants that went quiet and used up their grace window, so the
	// per-tenant state (and the stores it holds open) does not grow without
	// bound.
	for tenantID := range set.loops {
		if live[tenantID] {
			continue
		}
		if left := set.linger[tenantID]; left > 1 {
			set.linger[tenantID] = left - 1
			continue
		}
		delete(set.linger, tenantID)
		delete(set.loops, tenantID)
	}
}

// eachCampaign walks every campaign in one of the given statuses, page by
// page, calling fn on each. Mutating the status inside fn is safe: the cursor
// is the ID, which is monotonic (UUIDv7), so a row that drops out of the
// filter never makes the walk skip another one.
func eachCampaign(ctx context.Context, st store.Store, statuses []store.CampaignStatus, pageSize int, fn func(store.Campaign) error) error {
	page := store.Page{Limit: pageSize}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := st.Campaigns().ListByStatus(ctx, statuses, page)
		if err != nil {
			return err
		}
		for _, c := range res.Items {
			if err := fn(c); err != nil {
				return err
			}
		}
		if res.NextCursor == "" {
			return nil
		}
		page.Cursor = res.NextCursor
	}
}

// raced reports whether an error means "someone else changed this row first",
// which every loop treats as "try again next tick" rather than as a failure.
func raced(err error) bool {
	return errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)
}

// inflight are the delivery statuses that keep a campaign from completing
// (architecture 7.3).
var inflightStatuses = []store.DeliveryStatus{
	store.DeliveryPending, store.DeliveryQueued, store.DeliveryLeased, store.DeliveryDeferred,
}

// splitCounts totals a CountByStatus result and the part of it that is still
// in flight.
func splitCounts(counts map[store.DeliveryStatus]int64) (total, inflight int64) {
	for status, n := range counts {
		total += n
		for _, s := range inflightStatuses {
			if status == s {
				inflight += n
				break
			}
		}
	}
	return total, inflight
}
