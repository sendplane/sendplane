package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

func newDispatcher(t *testing.T, st store.Store, sink sendplane.EventSink, c *Control, clk *fakeClock) *outboxDispatcher {
	t.Helper()
	return &outboxDispatcher{st: st, sink: sink, log: discardLogger(), cfg: &c.cfg, clock: clk.Now}
}

func enqueue(t *testing.T, st store.Store, typ string, n int) {
	t.Helper()
	evs := make([]store.OutboxEvent, 0, n)
	for i := 0; i < n; i++ {
		evs = append(evs, store.OutboxEvent{
			Type:      typ,
			Payload:   json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
			CreatedAt: baseTime,
		})
	}
	if err := st.Outbox().Enqueue(context.Background(), evs); err != nil {
		t.Fatalf("Outbox.Enqueue: %v", err)
	}
}

func TestOutboxDispatcherDeliversAndMarks(t *testing.T) {
	sink := &recordingSink{}
	_, st, clk, c := newFixture(t, sendplane.Hooks{Events: sink})
	ctx := context.Background()

	enqueue(t, st, EventCampaignCompleted, 5)
	d := newDispatcher(t, st, sink, c, clk)
	if err := d.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := len(sink.got()); got != 5 {
		t.Fatalf("sink got %d events, want 5", got)
	}
	for _, e := range sink.got() {
		if e.TenantID != testTenant {
			t.Errorf("event tenant %q, want %q", e.TenantID, testTenant)
		}
		if e.Type != EventCampaignCompleted {
			t.Errorf("event type %q", e.Type)
		}
		if !e.OccurredAt.Equal(baseTime) {
			t.Errorf("OccurredAt %v, want the enqueue time %v", e.OccurredAt, baseTime)
		}
		if e.ID == "" || len(e.Payload) == 0 {
			t.Errorf("event %+v lost its ID or payload", e)
		}
	}
	if n := len(listOutbox(t, st, store.OutboxDelivered)); n != 5 {
		t.Fatalf("%d delivered rows, want 5", n)
	}
	if n := len(listOutbox(t, st, store.OutboxPending)); n != 0 {
		t.Fatalf("%d still pending", n)
	}
}

func TestOutboxDispatcherBackoffAndDeadLetter(t *testing.T) {
	sink := &recordingSink{}
	sink.setErr(errors.New("webhook 503"))
	_, st, clk, c := newFixture(t, sendplane.Hooks{Events: sink})
	ctx := context.Background()

	enqueue(t, st, EventCampaignCompleted, 1)
	d := newDispatcher(t, st, sink, c, clk)

	// 1m, 5m, 30m, 2h, 12h, then 12h for every later attempt.
	want := []time.Duration{
		time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour,
		12 * time.Hour, 12 * time.Hour, 12 * time.Hour, 12 * time.Hour, 12 * time.Hour,
	}
	now := baseTime
	for i, backoff := range want {
		if err := d.Tick(ctx, now); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
		pending := listOutbox(t, st, store.OutboxPending)
		if len(pending) != 1 {
			t.Fatalf("after failure %d: %d pending, want 1", i+1, len(pending))
		}
		ev := pending[0]
		if ev.Attempts != i+1 {
			t.Fatalf("after failure %d: Attempts %d", i+1, ev.Attempts)
		}
		if got, wantAt := ev.NextAttemptAt, now.Add(backoff); !got.Equal(wantAt) {
			t.Fatalf("after failure %d: NextAttemptAt %v, want %v (backoff %s)", i+1, got, wantAt, backoff)
		}
		if ev.LastError != "webhook 503" {
			t.Errorf("LastError %q", ev.LastError)
		}
		// Jump past the backoff so the next tick can claim it again.
		now = ev.NextAttemptAt
	}

	// The tenth failure is the dead letter.
	if err := d.Tick(ctx, now); err != nil {
		t.Fatalf("Tick 10: %v", err)
	}
	failed := listOutbox(t, st, store.OutboxFailed)
	if len(failed) != 1 {
		t.Fatalf("%d dead-lettered events, want 1", len(failed))
	}
	if failed[0].Attempts != 10 {
		t.Errorf("Attempts %d, want 10", failed[0].Attempts)
	}
	if n := len(listOutbox(t, st, store.OutboxPending)); n != 0 {
		t.Errorf("%d events still pending after the dead letter", n)
	}

	// A dead-lettered event is never claimed again.
	if err := d.Tick(ctx, now.Add(365*24*time.Hour)); err != nil {
		t.Fatalf("Tick after dead letter: %v", err)
	}
	if got := listOutbox(t, st, store.OutboxFailed); len(got) != 1 || got[0].Attempts != 10 {
		t.Errorf("dead letter changed: %+v", got)
	}
}

func TestOutboxDispatcherSkipsNotYetDue(t *testing.T) {
	sink := &recordingSink{}
	_, st, clk, c := newFixture(t, sendplane.Hooks{Events: sink})
	ctx := context.Background()

	if err := st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type: EventCampaignCompleted, CreatedAt: baseTime,
		NextAttemptAt: baseTime.Add(time.Hour),
	}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	d := newDispatcher(t, st, sink, c, clk)
	if err := d.Tick(ctx, baseTime); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := len(sink.got()); n != 0 {
		t.Fatalf("dispatched %d events before they were due", n)
	}
}

func TestOutboxLoopNotRegisteredWithoutSink(t *testing.T) {
	_, _, _, c := newFixture(t, sendplane.Hooks{})
	for _, s := range c.loopSpecs() {
		if s.name == "outbox" {
			t.Fatal("the outbox dispatcher was registered without an EventSink")
		}
	}
	_, _, _, withSink := newFixture(t, sendplane.Hooks{Events: &recordingSink{}})
	found := false
	for _, s := range withSink.loopSpecs() {
		if s.name == "outbox" {
			found = true
		}
	}
	if !found {
		t.Fatal("the outbox dispatcher was not registered with an EventSink")
	}
}

func TestBackoffFor(t *testing.T) {
	schedule := []time.Duration{time.Minute, 5 * time.Minute}
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{0, time.Minute}, {1, time.Minute}, {2, 5 * time.Minute}, {9, 5 * time.Minute},
	} {
		if got := backoffFor(schedule, tc.n); got != tc.want {
			t.Errorf("backoffFor(n=%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
	if got := backoffFor(nil, 3); got != time.Minute {
		t.Errorf("backoffFor(nil) = %s, want 1m", got)
	}
}
