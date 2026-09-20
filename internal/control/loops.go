package control

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/sendplane/sendplane/store"
)

// tickLoop is one loop's unit of work for one tenant. Implementations are
// built per tenant and keep whatever per-tenant state they need between ticks
// (the finalizer's adaptive counter, for instance).
type tickLoop interface {
	Tick(ctx context.Context, now time.Time) error
}

// loopSpec registers a loop with the Leader: a name for logs, the interval it
// ticks on, and how to build its per-tenant instance.
type loopSpec struct {
	name     string
	interval time.Duration
	// newTenant builds the loop for one tenant. The Store it is handed stays
	// valid for the life of the instance.
	newTenant func(st store.Store, tenantID string) tickLoop
}

// lingerTicks is how many more rounds a tenant keeps being ticked after it
// left the active set.
//
// Provider.ActiveTenants reports the tenants that still have non-terminal
// deliveries, which is what a sender needs. Control needs slightly more: the
// tick that matters most for a campaign is the one right after its last
// delivery went terminal, and by then the tenant is no longer "active". So a
// tenant that drops out is ticked a few more times before it is forgotten.
// See the package README: the durable fix belongs in the store contract.
const lingerTicks = 3

// tenantSet is one loop's state between ticks: the per-tenant instances and
// how many more rounds a tenant that left the active set still gets.
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
		set.linger[tenantID] = lingerTicks
	}

	ids := make([]string, 0, len(set.linger))
	for tenantID := range set.linger {
		ids = append(ids, tenantID)
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

	// Age out the tenants that were not active this round, so their
	// per-tenant state does not grow without bound.
	for tenantID, left := range set.linger {
		if live[tenantID] {
			continue
		}
		if left <= 1 {
			delete(set.linger, tenantID)
			delete(set.loops, tenantID)
			continue
		}
		set.linger[tenantID] = left - 1
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
