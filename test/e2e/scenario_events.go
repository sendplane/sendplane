package main

import (
	"context"
	"fmt"
	"time"
)

// Scenario 7: the outbox really reaches the host (architecture 12). The
// harness is the host — it listens on the port the control container's
// events.webhook.url points at through host.docker.internal — so this checks
// the whole path: transition -> outbox row -> dispatcher -> signed HTTP POST.

// wantEvents are the event types every run must deliver. delivery.failed is
// in the list because the bulk campaign of scenario 2 produces permanent failures and the
// default subscription of architecture 12 includes it; delivery.sent is
// deliberately absent and asserted *not* to arrive below, because one event
// per recipient is what the default subscription exists to keep out.
var wantEvents = []string{
	"campaign.started",
	"campaign.completed",
	"delivery.bounced",
	"delivery.complained",
	"delivery.failed",
	"recipient.unsubscribed",
	// Only a finished probe run can change a sender's health, so this one is
	// owed exactly because scenario 6 has to reach a verdict.
	"sender.health_changed",
}

func (r *runner) scenarioEvents(ctx context.Context) error {
	want := wantEvents
	if !r.probeCollected {
		// Scenario 6 already failed on this; do not report the same cause
		// twice under a different name.
		want = want[:len(want)-1]
		r.logf("  skipping sender.health_changed: scenario 6's probe run was never collected")
	}
	for _, typ := range want {
		evs, err := r.sink.waitFor(ctx, typ, 60*time.Second)
		if err != nil {
			r.fail("%v", err)
			continue
		}
		unsigned := 0
		for _, e := range evs {
			if !e.Signed {
				unsigned++
			}
		}
		if unsigned > 0 {
			r.fail("%d of the %d %s events arrived with an X-Sendplane-Signature that did not verify",
				unsigned, len(evs), typ)
		} else {
			r.logf("  %s: %d event(s), all signed", typ, len(evs))
		}
	}

	r.assertDeliveryEventSubscription()

	total, batches, unsigned := r.sink.counts()
	if total == 0 {
		r.fail("the event receiver got nothing at all; check events.webhook.url and extra_hosts in the compose file")
	}
	if unsigned > 0 {
		r.fail("%d of %d webhook batches had a bad signature", unsigned, batches)
	}

	// Nothing may have been given up on: a dead-letter entry means the
	// dispatcher exhausted its retries against a receiver that was up.
	var dead outboxEventList
	if err := r.api.getJSON(ctx, "/api/v1/events/dead-letter?limit=50", &dead); err != nil {
		return fmt.Errorf("GET /events/dead-letter: %w", err)
	}
	if len(dead.Items) != 0 {
		for _, e := range dead.Items {
			r.fail("dead-lettered event %s (%s) after %d attempts: %s",
				short(e.ID), e.Type, e.Attempts, e.LastError)
		}
	}

	// And nothing is stuck pending either, which a silently failing sink would
	// look like from the API side.
	var failed outboxEventList
	if err := r.api.getJSON(ctx, "/api/v1/events?status=failed&limit=50", &failed); err != nil {
		return fmt.Errorf("GET /events?status=failed: %w", err)
	}
	for _, e := range failed.Items {
		r.fail("event %s (%s) is in status failed: %s", short(e.ID), e.Type, e.LastError)
	}
	r.logf("  %d event(s) in %d batch(es); dead-letter empty", total, batches)
	return nil
}

// assertDeliveryEventSubscription pins both halves of the tenant subscription
// filter (architecture 12, GAP-2 in README.md) on the one campaign big enough
// to show the difference: every permanent failure of the 10,000-recipient bulk
// campaign has to arrive as delivery.failed, and none of the ~9,900 successes
// may arrive as delivery.sent, because this tenant never asked for it.
func (r *runner) assertDeliveryEventSubscription() {
	if !r.selected("2. bulk campaign") {
		return
	}
	failed := uniqueEvents(r.sink.byType("delivery.failed"))
	if int64(len(failed)) != r.exp.Failed {
		r.fail("%d delivery.failed events arrived, want %d (the failures of the bulk campaign)",
			len(failed), r.exp.Failed)
	} else {
		r.logf("  delivery.failed: %d event(s), exactly the bulk campaign's failures", len(failed))
	}
	if sent := r.sink.byType("delivery.sent"); len(sent) > 0 {
		r.fail("%d delivery.sent events arrived; the default subscription must not include it "+
			"(one event per recipient is what event_types exists to keep out)", len(sent))
	}
}

// uniqueEvents drops duplicates by event id. Dispatch is at-least-once: an
// event whose MarkDelivered did not commit is sent again, and that must not
// turn a correct count into a failure.
func uniqueEvents(evs []receivedEvent) []receivedEvent {
	seen := map[string]bool{}
	out := evs[:0]
	for _, e := range evs {
		if e.ID != "" && seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out
}
