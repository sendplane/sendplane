package bounce

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// guard wraps a Fake and fails the test if two sessions overlap, which is the
// property the per-mailbox store lock has to provide.
type guard struct {
	*mailbox.Fake
	t      *testing.T
	inside atomic.Int32
	// hold widens the window so that a missing lock actually shows up.
	hold time.Duration
}

func (g *guard) Fetch(ctx context.Context, max int) ([]mailbox.Message, error) {
	if n := g.inside.Add(1); n > 1 {
		g.t.Errorf("two pollers were inside the same mailbox at once (%d)", n)
	}
	defer g.inside.Add(-1)
	time.Sleep(g.hold)
	return g.Fake.Fetch(ctx, max)
}

func newPollEnv(t *testing.T, msgs ...string) (*memstore.Provider, *mailbox.Fake, TenantMailbox) {
	t.Helper()
	ctx := context.Background()
	p := memstore.New()
	st, err := p.ForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	settings := store.DefaultTenantSettings(testTenant, testNow)
	settings.Tracking.SigningKeys = testKeys()
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("create settings: %v", err)
	}
	if _, err := st.Deliveries().InsertBatch(ctx, []store.Delivery{
		sentDelivery(testDelivery, "nosuch@example.org"),
		sentDelivery(testDelivery2, "full@example.org"),
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	fake := mailbox.NewFake()
	for _, name := range msgs {
		fake.Add(loadFixture(t, name))
	}
	return p, fake, TenantMailbox{
		TenantID: testTenant, MailboxID: "mbox-1",
		Config: mailbox.Config{
			Protocol: mailbox.ProtocolIMAP, Host: "imap.example.com",
			Username: "bounce@example.com", AfterProcess: mailbox.ActionDelete,
		},
	}
}

func newRunner(t *testing.T, p store.Provider, owner string, client mailbox.Client, tune ...func(*RunnerConfig)) *Runner {
	t.Helper()
	cfg := RunnerConfig{
		Owner: owner, Provider: p,
		Source:    MailboxSourceFunc(func(context.Context) ([]TenantMailbox, error) { return nil, nil }),
		Processor: NewProcessor(Options{Clock: func() time.Time { return testNow }}),
		Dial: func(context.Context, mailbox.Config, host.SecretCipher) (mailbox.Client, error) {
			return client, nil
		},
		Clock: func() time.Time { return testNow },
	}
	for _, f := range tune {
		f(&cfg)
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func TestPollOnceHandlesAndAcks(t *testing.T) {
	ctx := context.Background()
	p, fake, box := newPollEnv(t,
		"postfix_hard.eml", "soft_mailbox_full.eml", "unrelated.eml")

	r := newRunner(t, p, "worker-1", fake)
	stats, err := r.PollOnce(ctx, box)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if !stats.Locked || stats.Fetched != 3 || stats.Acked != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Processed != 1 || stats.Recorded != 2 {
		t.Fatalf("stats = %+v", stats)
	}

	// AfterProcess delete: everything handled left the mailbox.
	if fake.Remaining() != 0 {
		t.Fatalf("%d messages left in the mailbox", fake.Remaining())
	}
	acks := fake.Acks()
	if len(acks) != 1 || acks[0].Action != mailbox.ActionDelete || len(acks[0].IDs) != 3 {
		t.Fatalf("acks = %+v", acks)
	}

	st, _ := p.ForTenant(ctx, testTenant)
	d, err := st.Deliveries().Get(ctx, testDelivery)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Status != store.DeliveryBounced {
		t.Fatalf("delivery status = %v", d.Status)
	}

	// The lock is handed back at the end of the pass.
	lock, err := st.Locks().Get(ctx, LockName(box.MailboxID))
	if err == nil && lock != nil && lock.Owner == "worker-1" {
		t.Fatalf("lock still held after the pass: %+v", lock)
	}
}

func TestPollOnceKeepPolicy(t *testing.T) {
	ctx := context.Background()
	p, fake, box := newPollEnv(t, "postfix_hard.eml")
	box.Config.AfterProcess = mailbox.ActionKeep

	r := newRunner(t, p, "worker-1", fake)
	if _, err := r.PollOnce(ctx, box); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if fake.Remaining() != 1 {
		t.Fatalf("keep deleted the message")
	}
	acks := fake.Acks()
	if len(acks) != 1 || acks[0].Action != mailbox.ActionKeep {
		t.Fatalf("acks = %+v", acks)
	}
	// A kept message is not handed out again, so the second pass is a no-op.
	stats, err := r.PollOnce(ctx, box)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Fetched != 0 {
		t.Fatalf("second pass fetched %d messages", stats.Fetched)
	}
}

func TestPollOnceLockExclusivity(t *testing.T) {
	ctx := context.Background()
	// Two fixtures that correlate with two different deliveries, so both
	// produce an event and a duplicated pass would be visible.
	p, fake, box := newPollEnv(t, "postfix_hard.eml", "soft_mailbox_full.eml")
	g := &guard{Fake: fake, t: t, hold: 20 * time.Millisecond}

	r1 := newRunner(t, p, "worker-1", g)
	r2 := newRunner(t, p, "worker-2", g)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		locked  int
		fetched int
	)
	for _, r := range []*Runner{r1, r2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats, err := r.PollOnce(ctx, box)
			if err != nil {
				t.Errorf("PollOnce: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if stats.Locked {
				locked++
			}
			fetched += stats.Fetched
		}()
	}
	wg.Wait()

	if locked != 1 {
		t.Fatalf("%d runners took the mailbox lock, want 1", locked)
	}
	if fetched != 2 {
		t.Fatalf("the two runners fetched %d messages in total, want 2", fetched)
	}

	st, _ := p.ForTenant(ctx, testTenant)
	res, err := st.Bounces().List(ctx, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("list bounces: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("recorded %d bounce events, want 2", len(res.Items))
	}
}

func TestPollOnceStoreFailureLeavesMessage(t *testing.T) {
	ctx := context.Background()
	_, fake, box := newPollEnv(t, "postfix_hard.eml")
	// A closed provider fails every repository call.
	p2 := memstore.New()
	if err := p2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := newRunner(t, p2, "worker-1", fake)
	if _, err := r.PollOnce(ctx, box); err == nil {
		t.Fatal("PollOnce succeeded against a closed store")
	}
	if fake.Remaining() != 1 {
		t.Fatal("a message was acked although the store failed")
	}
}

func TestPollOnceDialFailure(t *testing.T) {
	p, _, box := newPollEnv(t, "postfix_hard.eml")
	r := newRunner(t, p, "worker-1", nil, func(c *RunnerConfig) {
		c.Dial = func(context.Context, mailbox.Config, host.SecretCipher) (mailbox.Client, error) {
			return nil, errors.New("connection refused")
		}
	})
	if _, err := r.PollOnce(context.Background(), box); err == nil {
		t.Fatal("PollOnce ignored a dial failure")
	}
	// The lock must be back even though the pass failed.
	st, _ := p.ForTenant(context.Background(), testTenant)
	if lock, err := st.Locks().Get(context.Background(), LockName(box.MailboxID)); err == nil &&
		lock != nil && lock.Owner == "worker-1" {
		t.Fatalf("lock still held after a failed pass: %+v", lock)
	}
}

// TestRunDrainsAndStops drives the whole Runner: it must pick the mailbox up
// from the source, drain it, and shut down when the context ends.
func TestRunDrainsAndStops(t *testing.T) {
	p, fake, box := newPollEnv(t, "postfix_hard.eml", "arf_complaint.eml")
	r := newRunner(t, p, "worker-1", fake, func(c *RunnerConfig) {
		c.Source = MailboxSourceFunc(func(context.Context) ([]TenantMailbox, error) {
			return []TenantMailbox{box}, nil
		})
		c.PollInterval = 5 * time.Millisecond
		c.RefreshInterval = 10 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for fake.Remaining() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("mailbox still holds %d messages", fake.Remaining())
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// TestRunSurvivesAFailingMailbox pins the "never crash the runner" rule: a
// mailbox that cannot be dialed is backed off, and the runner keeps going.
func TestRunSurvivesAFailingMailbox(t *testing.T) {
	p, fake, box := newPollEnv(t, "postfix_hard.eml")
	bad := box
	bad.MailboxID = "mbox-broken"
	bad.Config.Username = "broken"

	var dials atomic.Int32
	r := newRunner(t, p, "worker-1", fake, func(c *RunnerConfig) {
		c.Source = MailboxSourceFunc(func(context.Context) ([]TenantMailbox, error) {
			return []TenantMailbox{box, bad}, nil
		})
		c.PollInterval = 5 * time.Millisecond
		c.RefreshInterval = 10 * time.Millisecond
		c.MaxBackoff = 20 * time.Millisecond
		c.Dial = func(_ context.Context, cfg mailbox.Config, _ host.SecretCipher) (mailbox.Client, error) {
			if dials.Add(1); cfg.Username == "broken" {
				return nil, errors.New("connection refused")
			}
			return fake, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for fake.Remaining() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the healthy mailbox was never drained")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNewRunnerValidates(t *testing.T) {
	src := MailboxSourceFunc(func(context.Context) ([]TenantMailbox, error) { return nil, nil })
	for name, cfg := range map[string]RunnerConfig{
		"no provider": {Owner: "w", Source: src},
		"no source":   {Owner: "w", Provider: memstore.New()},
		"no owner":    {Provider: memstore.New(), Source: src},
	} {
		if _, err := NewRunner(cfg); err == nil {
			t.Errorf("%s: NewRunner accepted it", name)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	interval := time.Second
	max := 4 * time.Second
	got := time.Duration(0)
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		got = nextBackoff(got, interval, max)
		if got != want {
			t.Fatalf("nextBackoff = %v, want %v", got, want)
		}
	}
}
