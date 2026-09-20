package control

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// countingLoop records how many times it ticked and for which tenants.
type countingLoop struct {
	ticks   *atomic.Int64
	tenants *sync.Map
	tenant  string
}

func (l *countingLoop) Tick(context.Context, time.Time) error {
	l.ticks.Add(1)
	l.tenants.Store(l.tenant, true)
	return nil
}

// leaderTests use real time: the lease TTL is what the Leader sleeps on, and
// a fake clock would not make the ticker fire. The durations are short but
// generous enough not to be flaky.
const (
	testTTL   = 300 * time.Millisecond
	testRetry = 10 * time.Millisecond
)

// newTestLeader builds a Leader with one counting loop registered.
func newTestLeader(p *memstore.Provider, owner string, ticks *atomic.Int64, tenants *sync.Map) *Leader {
	cfg := defaultConfig()
	cfg.owner = owner
	cfg.leaderTTL = testTTL
	cfg.leaderRetry = testRetry
	l := newLeader(p, discardLogger(), time.Now, cfg)
	l.register(loopSpec{
		name:     "counting",
		interval: 5 * time.Millisecond,
		newTenant: func(_ store.Store, tenantID string) tickLoop {
			return &countingLoop{ticks: ticks, tenants: tenants, tenant: tenantID}
		},
	})
	return l
}

// seedActiveTenant gives the provider a tenant with non-terminal work, which
// is what memstore's ActiveTenants reports.
func seedActiveTenant(t *testing.T, p *memstore.Provider, tenantID string) {
	t.Helper()
	st, err := p.ForTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	cam := seedCampaign(t, st, store.CampaignRunning)
	seedDeliveries(t, st, cam.ID, store.DeliveryQueued, 1)
}

func TestLeaderRunsLoopsOnOneReplicaOnly(t *testing.T) {
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	seedActiveTenant(t, p, testTenant)

	var ticksA, ticksB atomic.Int64
	var tenants sync.Map
	a := newTestLeader(p, "replica-a", &ticksA, &tenants)
	b := newTestLeader(p, "replica-b", &ticksB, &tenants)

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = a.Run(ctxA) }()
	go func() { defer wg.Done(); _ = b.Run(ctxB) }()

	// One of them must be ticking.
	waitFor(t, time.Second, func() bool { return ticksA.Load() > 2 || ticksB.Load() > 2 })

	// ... and only one. Whichever lost must have ticked exactly zero times.
	aN, bN := ticksA.Load(), ticksB.Load()
	if aN > 0 && bN > 0 {
		t.Fatalf("both replicas ran the loops: a=%d b=%d", aN, bN)
	}
	leaderIsA := aN > 0
	loser := &ticksB
	if !leaderIsA {
		loser = &ticksA
	}
	if v, ok := tenants.Load(testTenant); !ok || v != true {
		t.Error("the loop never ran for the active tenant")
	}

	// Stop the holder; the other must take over within a TTL.
	stopLeader := stopA
	if !leaderIsA {
		stopLeader = stopB
	}
	before := loser.Load()
	stopLeader()
	waitFor(t, 2*testTTL, func() bool { return loser.Load() > before+2 })

	stopA()
	stopB()
	wg.Wait()
}

func TestLeaderOnlyOneHolderInStore(t *testing.T) {
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	ctx := context.Background()

	sys, err := p.ForTenant(ctx, SystemTenantID)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	now := time.Now()
	ok, err := sys.Locks().Acquire(ctx, LeaderLockName, "a", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("first Acquire = %v, %v; want true, nil", ok, err)
	}
	ok, err = sys.Locks().Acquire(ctx, LeaderLockName, "b", time.Minute, now)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if ok {
		t.Fatal("a second owner took a live leader lease")
	}
	// Once the lease has expired, the next replica takes over.
	ok, err = sys.Locks().Acquire(ctx, LeaderLockName, "b", time.Minute, now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("takeover after expiry = %v, %v; want true, nil", ok, err)
	}
}

func TestLeaderStopsLoopsWhenLeaseIsLost(t *testing.T) {
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	seedActiveTenant(t, p, testTenant)

	var ticks atomic.Int64
	var tenants sync.Map
	l := newTestLeader(p, "replica-a", &ticks, &tenants)

	var states []bool
	var mu sync.Mutex
	l.onState = func(running bool) {
		mu.Lock()
		states = append(states, running)
		mu.Unlock()
	}

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Run(ctx) }()
	waitFor(t, time.Second, func() bool { return ticks.Load() > 2 })

	// Steal the lease: renewal must fail and the loops must stop.
	sys, err := p.ForTenant(ctx, SystemTenantID)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	if err := sys.Locks().Release(ctx, LeaderLockName, "replica-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	ok, err := sys.Locks().Acquire(ctx, LeaderLockName, "thief", time.Hour, time.Now())
	if err != nil || !ok {
		t.Fatalf("steal = %v, %v", ok, err)
	}

	waitFor(t, 2*testTTL, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(states) >= 2 && states[len(states)-1] == false
	})
	stopped := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if grew := ticks.Load() - stopped; grew > 1 {
		t.Errorf("loops kept running after the lease was lost: %d more ticks", grew)
	}

	stop()
	<-done
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}
