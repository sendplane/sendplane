package control

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// baseTime is the fixed instant every test starts from.
var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is the test clock. Every loop takes `now` as an argument, so most
// tests only need it for the timestamps memstore stamps on rows.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: baseTime} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const testTenant = "t1"

// newFixture builds a memstore provider on a fake clock plus a Control bound
// to it, and returns the tenant store the tests work on.
func newFixture(t *testing.T, hooks host.Hooks, opts ...Option) (*memstore.Provider, store.Store, *fakeClock, *Control) {
	t.Helper()
	clk := newClock()
	p := memstore.New(memstore.WithClock(clk.Now))
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	c, err := New(p, hooks, discardLogger(), clk.Now, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, st, clk, c
}

// seedSender creates a sender so StartCampaign's precondition is satisfiable.
func seedSender(t *testing.T, st store.Store) *store.Sender {
	t.Helper()
	s := &store.Sender{Name: "s", FromEmail: "a@example.com"}
	if err := st.Senders().Create(context.Background(), s); err != nil {
		t.Fatalf("Senders.Create: %v", err)
	}
	return s
}

// seedCampaign creates a campaign in the given status. mods runs before the
// insert so a test can set ScheduleAt, CompletedAt and so on.
func seedCampaign(t *testing.T, st store.Store, status store.CampaignStatus, mods ...func(*store.Campaign)) *store.Campaign {
	t.Helper()
	c := &store.Campaign{
		Name:      "c",
		VersionID: "v1",
		SenderID:  "sender-1",
		Status:    status,
	}
	for _, m := range mods {
		m(c)
	}
	if err := st.Campaigns().Create(context.Background(), c); err != nil {
		t.Fatalf("Campaigns.Create: %v", err)
	}
	return c
}

// seedDeliveries inserts n deliveries of one status into a campaign and
// returns their IDs.
func seedDeliveries(t *testing.T, st store.Store, campaignID string, status store.DeliveryStatus, n int, mods ...func(*store.Delivery)) []string {
	t.Helper()
	ds := make([]store.Delivery, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		d := store.Delivery{
			ID:         store.NewID(),
			CampaignID: campaignID,
			VersionID:  "v1",
			Status:     status,
			Email:      fmt.Sprintf("r%d@example.com", i),
			EmailNorm:  fmt.Sprintf("r%d@example.com", i),
		}
		for _, m := range mods {
			m(&d)
		}
		ids = append(ids, d.ID)
		ds = append(ds, d)
	}
	inserted, err := st.Deliveries().InsertBatch(context.Background(), ds)
	if err != nil {
		t.Fatalf("Deliveries.InsertBatch: %v", err)
	}
	if inserted != n {
		t.Fatalf("InsertBatch inserted %d, want %d", inserted, n)
	}
	return ids
}

// getCampaign re-reads a campaign or fails the test.
func getCampaign(t *testing.T, st store.Store, id string) *store.Campaign {
	t.Helper()
	c, err := st.Campaigns().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Campaigns.Get(%s): %v", id, err)
	}
	return c
}

// listOutbox returns every outbox event in a status, oldest first.
func listOutbox(t *testing.T, st store.Store, status store.OutboxStatus) []store.OutboxEvent {
	t.Helper()
	res, err := st.Outbox().List(context.Background(), status, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("Outbox.List: %v", err)
	}
	return res.Items
}

// recordingSink is an EventSink that remembers what it was handed and can be
// told to fail.
type recordingSink struct {
	mu     sync.Mutex
	events []host.Event
	err    error
}

func (s *recordingSink) Emit(_ context.Context, evs []host.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, evs...)
	return nil
}

func (s *recordingSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingSink) got() []host.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]host.Event(nil), s.events...)
}
