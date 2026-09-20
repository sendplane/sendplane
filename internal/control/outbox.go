package control

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

// maxOutboxErrLen caps what goes into OutboxEvent.LastError. A transport error
// can carry a whole SMTP conversation or an HTML error page; the dead letter
// listing only needs enough to tell failures apart.
const maxOutboxErrLen = 1024

// outboxDispatcher hands queued events to the host's EventSink (architecture
// 12). It runs on the leader only: the events are already durable, so dispatch
// throughput is never the bottleneck, and a single dispatcher keeps the
// at-least-once duplicates down to lease expiry.
type outboxDispatcher struct {
	st    store.Store
	sink  sendplane.EventSink
	log   *slog.Logger
	cfg   *config
	clock func() time.Time
}

func (d *outboxDispatcher) Tick(ctx context.Context, now time.Time) error {
	events, err := d.st.Outbox().ClaimPending(ctx, d.cfg.batches.OutboxClaim, d.cfg.outboxLease, d.cfg.owner, now)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	workers := d.cfg.outboxWorkers
	if workers > len(events) {
		workers = len(events)
	}
	queue := make(chan store.OutboxEvent)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for ev := range queue {
				d.dispatch(ctx, ev, now)
			}
		}()
	}
	for _, ev := range events {
		select {
		case queue <- ev:
		case <-ctx.Done():
			// Stop feeding; the leases expire and the next leader retries.
			close(queue)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(queue)
	wg.Wait()
	return nil
}

// dispatch delivers one event and records the outcome. Every failure path ends
// in MarkFailed so the lease is dropped immediately instead of waiting out
// cfg.outboxLease.
func (d *outboxDispatcher) dispatch(ctx context.Context, ev store.OutboxEvent, now time.Time) {
	err := d.sink.Emit(ctx, []sendplane.Event{eventFromOutbox(ev)})
	if err == nil {
		if err := d.st.Outbox().MarkDelivered(ctx, ev.ID, store.TruncateTime(d.clock())); err != nil {
			d.log.Error("control: cannot mark outbox event delivered", "event", ev.ID, "err", err)
		}
		return
	}

	// ev.Attempts is the count before this failure, so this is the nth.
	attempt := ev.Attempts + 1
	var next time.Time
	if attempt < d.cfg.outboxMaxAttempts {
		next = now.Add(backoffFor(d.cfg.outboxBackoff, attempt))
	}
	if err := d.st.Outbox().MarkFailed(ctx, ev.ID, next, truncateErr(err)); err != nil {
		d.log.Error("control: cannot mark outbox event failed", "event", ev.ID, "err", err)
		return
	}
	if next.IsZero() {
		d.log.Error("control: outbox event dead-lettered",
			"event", ev.ID, "type", ev.Type, "attempts", attempt, "err", err)
		return
	}
	d.log.Warn("control: outbox dispatch failed, will retry",
		"event", ev.ID, "type", ev.Type, "attempt", attempt, "next_attempt_at", next, "err", err)
}

// backoffFor returns the wait after the nth failure, n starting at 1. The last
// entry of the schedule is reused for every later attempt.
func backoffFor(schedule []time.Duration, n int) time.Duration {
	if len(schedule) == 0 {
		return time.Minute
	}
	if n < 1 {
		n = 1
	}
	if n > len(schedule) {
		n = len(schedule)
	}
	return schedule[n-1]
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > maxOutboxErrLen {
		return s[:maxOutboxErrLen] + "…"
	}
	return s
}
