package sender

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osteele/liquid"

	"github.com/sendplane/sendplane/store"
)

// cache is a small TTL map. A zero TTL never expires, which is what message
// versions use: they are immutable by contract.
//
// It deliberately does not deduplicate concurrent loads. The entries are
// tenant configuration rows read with a few seconds of TTL, so the worst case
// is a handful of extra reads right after an entry expires, and a singleflight
// would add a lock held across a store call on the hot path.
type cache[T any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]cached[T]
}

type cached[T any] struct {
	v  T
	at time.Time
}

func newCache[T any](ttl time.Duration) *cache[T] {
	return &cache[T]{ttl: ttl, m: map[string]cached[T]{}}
}

func (c *cache[T]) get(key string, now time.Time, load func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := c.m[key]
	c.mu.Unlock()
	if ok && (c.ttl <= 0 || now.Sub(e.at) < c.ttl) {
		return e.v, nil
	}
	v, err := load()
	if err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	c.m[key] = cached[T]{v: v, at: now}
	c.mu.Unlock()
	return v, nil
}

// invalidate drops one key, used when a transport row turns out to be stale.
func (c *cache[T]) invalidate(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// tenantState is everything the sender keeps per tenant: the bound store, the
// cached configuration rows, and the in-flight counter that enforces the
// per-tenant concurrency cap of architecture 8.1.
type tenantState struct {
	id string
	st store.Store

	settings   *cache[*store.TenantSettings]
	campaigns  *cache[*store.Campaign]
	senders    *cache[*store.Sender]
	transports *cache[*store.Transport]
	domains    *cache[*store.SendingDomain]
	versions   *cache[*store.MessageVersion]
	// secrets caches decrypted transport passwords and parsed DKIM keys,
	// keyed by row ID and Version so a rotation invalidates them.
	passwords *cache[string]
	dkimKeys  *cache[*dkimKey]
	// unsubTpl caches the parsed tenant unsubscribe URL template, keyed by the
	// settings version.
	unsubTpl *cache[*liquid.Template]

	// running is the running-campaign set of ADR-0002, refreshed on its own
	// interval.
	runMu     sync.Mutex
	running   []string
	runningAt time.Time

	inflight atomic.Int64
	// workers is the number of live sender replicas seen in this tenant.
	workers atomic.Int64

	batcher *batcher
}

func newTenantState(id string, st store.Store, ttl time.Duration) *tenantState {
	return &tenantState{
		id:         id,
		st:         st,
		settings:   newCache[*store.TenantSettings](ttl),
		campaigns:  newCache[*store.Campaign](ttl),
		senders:    newCache[*store.Sender](ttl),
		transports: newCache[*store.Transport](ttl),
		domains:    newCache[*store.SendingDomain](ttl),
		versions:   newCache[*store.MessageVersion](0), // immutable
		passwords:  newCache[string](0),                // keyed by row version
		dkimKeys:   newCache[*dkimKey](0),
		unsubTpl:   newCache[*liquid.Template](0),
	}
}

// tenantSettings returns the tenant's settings, creating the default row on
// first access (store.LoadTenantSettings): the sender must not stall because
// the control plane has not written one, and it must not keep sending against
// defaults that nothing persisted either — an operator who then edits the
// settings would be editing a row the sender invented (ADR-0006).
func (t *tenantState) tenantSettings(ctx context.Context, now time.Time) (*store.TenantSettings, error) {
	return t.settings.get("", now, func() (*store.TenantSettings, error) {
		return store.LoadTenantSettings(ctx, t.st, t.id, now)
	})
}

func (t *tenantState) campaign(ctx context.Context, id string, now time.Time) (*store.Campaign, error) {
	return t.campaigns.get(id, now, func() (*store.Campaign, error) {
		return t.st.Campaigns().Get(ctx, id)
	})
}

func (t *tenantState) version(ctx context.Context, id string, now time.Time) (*store.MessageVersion, error) {
	return t.versions.get(id, now, func() (*store.MessageVersion, error) {
		return t.st.Versions().Get(ctx, id)
	})
}

func (t *tenantState) sender(ctx context.Context, id string, now time.Time) (*store.Sender, error) {
	return t.senders.get(id, now, func() (*store.Sender, error) {
		return t.st.Senders().Get(ctx, id)
	})
}

func (t *tenantState) transport(ctx context.Context, id string, now time.Time) (*store.Transport, error) {
	return t.transports.get(id, now, func() (*store.Transport, error) {
		return t.st.Transports().Get(ctx, id)
	})
}

func (t *tenantState) domain(ctx context.Context, id string, now time.Time) (*store.SendingDomain, error) {
	if id == "" {
		return nil, nil
	}
	return t.domains.get(id, now, func() (*store.SendingDomain, error) {
		return t.st.Domains().Get(ctx, id)
	})
}
