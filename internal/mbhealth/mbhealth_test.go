package mbhealth

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	return st
}

func newBounceMailbox(t *testing.T, st store.Store) *store.BounceMailbox {
	t.Helper()
	m := &store.BounceMailbox{Name: "bounces", Host: "imap.example.com", Port: 993, Enabled: true}
	if err := st.BounceMailboxes().Create(context.Background(), m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return m
}

func eventTypes(t *testing.T, st store.Store) []string {
	t.Helper()
	res, err := st.Outbox().List(context.Background(), "", store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("Outbox List: %v", err)
	}
	out := make([]string, 0, len(res.Items))
	for _, ev := range res.Items {
		out = append(out, ev.Type)
	}
	return out
}

// The streak drives both the badge and the notification, and they differ on
// purpose: the badge flips at once, the event waits for FailuresForEvent.
func TestRecordStreakAndEvents(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	row := newBounceMailbox(t, st)
	now := time.Now().UTC()

	m := Mailbox{ID: row.ID, Name: row.Name}
	h, err := Record(ctx, st, KindBounce, m, Fail(store.MailboxStageAuth, "login rejected"), now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if h.Status != store.MailboxError || h.ConsecutiveFailures != 1 {
		t.Fatalf("first failure = %+v", h)
	}
	if got := eventTypes(t, st); len(got) != 0 {
		t.Fatalf("a single failure emitted %v; it is a blip, not news", got)
	}

	// Second failure in a row: now somebody has to be told.
	m.Health = h
	h, err = Record(ctx, st, KindBounce, m, Fail(store.MailboxStageAuth, "login rejected"), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if h.ConsecutiveFailures != 2 {
		t.Fatalf("second failure = %+v", h)
	}
	if got := eventTypes(t, st); len(got) != 1 || got[0] != EventMailboxUnhealthy {
		t.Fatalf("events = %v, want one %s", got, EventMailboxUnhealthy)
	}

	// Still broken: one event per outage, not one per poll.
	m.Health = h
	h, err = Record(ctx, st, KindBounce, m, Fail(store.MailboxStageAuth, "login rejected"), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := eventTypes(t, st); len(got) != 1 {
		t.Fatalf("events = %v, want no repeat", got)
	}

	// Recovery clears the streak and announces itself.
	m.Health = h
	ok := now.Add(3 * time.Minute)
	h, err = Record(ctx, st, KindBounce, m, OK(), ok)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if h.Status != store.MailboxOK || h.ConsecutiveFailures != 0 || h.Reason != "" {
		t.Fatalf("recovery = %+v", h)
	}
	if !h.LastOKAt.Equal(store.TruncateTime(ok)) {
		t.Errorf("LastOKAt = %v, want %v", h.LastOKAt, ok)
	}
	if got := eventTypes(t, st); len(got) != 2 || got[1] != EventMailboxRecovered {
		t.Fatalf("events = %v, want a trailing %s", got, EventMailboxRecovered)
	}

	// The row itself carries the health, and UpdateHealth left the version
	// alone (store.MailboxHealth).
	stored, err := st.BounceMailboxes().Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Health.Status != store.MailboxOK {
		t.Errorf("stored health = %+v", stored.Health)
	}
	if stored.Version != row.Version {
		t.Errorf("UpdateHealth bumped the version to %d", stored.Version)
	}
}

// A blip that healed before the threshold produces no events at all, in
// either direction.
func TestRecordBlipIsSilent(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	row := newBounceMailbox(t, st)
	now := time.Now().UTC()

	m := Mailbox{ID: row.ID, Name: row.Name}
	h, err := Record(ctx, st, KindBounce, m, Fail(store.MailboxStageDial, "connection reset"), now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	m.Health = h
	if _, err := Record(ctx, st, KindBounce, m, OK(), now.Add(time.Minute)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := eventTypes(t, st); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestRecordProbeMailbox(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	row := &store.ProbeMailbox{Name: "gmail", Address: "p@gmail.com", Host: "imap.gmail.com", Port: 993}
	if err := st.ProbeMailboxes().Create(ctx, row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := Record(ctx, st, KindProbe, Mailbox{ID: row.ID, Name: row.Name}, OK(), now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	stored, err := st.ProbeMailboxes().Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Health.Status != store.MailboxOK || stored.Health.Stage != store.MailboxStageOK {
		t.Fatalf("health = %+v", stored.Health)
	}
}

func TestRecordUnknownKind(t *testing.T) {
	st := newStore(t)
	if _, err := Record(context.Background(), st, Kind("smtp"), Mailbox{ID: "x"}, OK(), time.Now()); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}
