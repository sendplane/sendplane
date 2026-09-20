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
//
// Only the events sendplane actually enqueues are asserted. `delivery.sent`
// and `delivery.failed` are documented in api/openapi.yaml's OutboxEvent but
// nothing writes them today (see the known-issue table in README.md), so
// asserting on them here would be asserting on the spec rather than on the
// product.
var wantEvents = []string{
	"campaign.started",
	"campaign.completed",
	"delivery.bounced",
	"delivery.complained",
	"recipient.unsubscribed",
}

func (r *runner) scenarioEvents(ctx context.Context) error {
	want := wantEvents
	if r.probeCollected {
		// Only a finished probe run can change a sender's health, so this one
		// is only owed when scenario 6 got that far (BUG-2).
		want = append(append([]string{}, want...), "sender.health_changed")
	} else {
		r.knownFail("BUG-2",
			"no sender.health_changed event is expected: scenario 6's probe run was never collected")
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
