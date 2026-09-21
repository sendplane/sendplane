// Package mbhealth is the one place a mailbox reachability observation turns
// into a stored store.MailboxHealth and, when the verdict flips, into an
// outbox event.
//
// It exists because four callers make the same observation and must agree on
// what it means: the bounce poller (every poll), the loopback probe collector
// (every collect that cannot open a mailbox), the mailbox-check leader loop
// (the one that catches a password changed between probes) and the manual test
// endpoints. Spelling the streak and the transition rule out four times is how
// two of them end up disagreeing about when an operator gets told.
//
// It is a leaf: it imports store and nothing else of sendplane, so both
// internal/mailbox's consumers and internal/probe - which deliberately does
// not import internal/mailbox - can use it.
package mbhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sendplane/sendplane/store"
)

// The outbox event types a transition produces (docs/architecture.md 12).
// Neither is in store.DefaultOffEventTypes: there is one per mailbox per
// outage, not one per recipient, and an operator who is not told that
// sendplane can no longer read the bounce mailbox has no way of finding out.
const (
	EventMailboxUnhealthy = "mailbox.unhealthy"
	EventMailboxRecovered = "mailbox.recovered"
)

// Kind names which mailbox aggregate a health record belongs to. It is in the
// event payload, because "mailbox 3f2a is unreachable" is useless to a host
// that has two kinds of them.
type Kind string

const (
	KindProbe  Kind = "probe"
	KindBounce Kind = "bounce"
)

// FailuresForEvent is how many consecutive failures a mailbox needs before
// mailbox.unhealthy is emitted.
//
// One failure is a blip: an IMAP server restarting, a connection reset, a
// provider briefly rate-limiting logins. Two in a row, a poll or a check
// interval apart, is a configuration problem somebody has to fix. The stored
// Status still flips on the first failure - the badge must not lie while the
// streak builds - only the notification waits.
const FailuresForEvent = 2

// Mailbox is the part of a probe or bounce mailbox row this package needs.
type Mailbox struct {
	ID   string
	Name string
	// Health is the row's current health, i.e. the one being replaced.
	Health store.MailboxHealth
}

// Outcome is one observation. Stage and Reason are ignored when OK.
type Outcome struct {
	OK     bool
	Stage  string
	Reason string
}

// OK is the outcome of a check that worked.
func OK() Outcome { return Outcome{OK: true} }

// Fail is the outcome of a check that did not.
func Fail(stage, reason string) Outcome {
	return Outcome{Stage: stage, Reason: reason}
}

// Next computes the health that replaces m.Health, without writing anything.
// Record is what callers normally use; this is exported so a caller that
// already knows it is not going to write (a dry run, a test) can still see the
// rule.
func Next(prev store.MailboxHealth, o Outcome, now time.Time) store.MailboxHealth {
	now = store.TruncateTime(now)
	if o.OK {
		return store.MailboxHealth{
			Status:    store.MailboxOK,
			Stage:     store.MailboxStageOK,
			CheckedAt: now,
			LastOKAt:  now,
		}
	}
	return store.MailboxHealth{
		Status:              store.MailboxError,
		Stage:               o.Stage,
		Reason:              o.Reason,
		CheckedAt:           now,
		LastOKAt:            prev.LastOKAt,
		ConsecutiveFailures: prev.ConsecutiveFailures + 1,
	}
}

// Record writes the new health through the right repository and enqueues
// mailbox.unhealthy or mailbox.recovered when the verdict flipped.
//
// Only transitions produce an event, and only ones that crossed
// FailuresForEvent: a mailbox that is down for a day produces one
// mailbox.unhealthy, not one per poll, and a blip that healed produces none at
// all. It returns the health it wrote so a caller can log or report it.
func Record(
	ctx context.Context, st store.Store, kind Kind, m Mailbox,
	o Outcome, now time.Time,
) (store.MailboxHealth, error) {
	next := Next(m.Health, o, now)

	var err error
	switch kind {
	case KindProbe:
		err = st.ProbeMailboxes().UpdateHealth(ctx, m.ID, next)
	case KindBounce:
		err = st.BounceMailboxes().UpdateHealth(ctx, m.ID, next)
	default:
		return next, fmt.Errorf("mbhealth: unknown mailbox kind %q", kind)
	}
	if err != nil {
		return next, err
	}

	typ := transition(m.Health, next)
	if typ == "" {
		return next, nil
	}
	payload, err := json.Marshal(eventPayload{
		Kind:       kind,
		MailboxID:  m.ID,
		Name:       m.Name,
		Status:     next.Status,
		Stage:      next.Stage,
		Reason:     next.Reason,
		Failures:   next.ConsecutiveFailures,
		CheckedAt:  next.CheckedAt,
		LastOKAt:   next.LastOKAt,
		OccurredAt: store.TruncateTime(now),
	})
	if err != nil {
		return next, err
	}
	return next, st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type:          typ,
		Payload:       payload,
		Status:        store.OutboxPending,
		CreatedAt:     store.TruncateTime(now),
		NextAttemptAt: store.TruncateTime(now),
	}})
}

// transition returns the event type this step produces, or "" for none.
//
// Recovery is only announced to somebody who was told about the outage, which
// is why it is conditional on the previous streak having reached the threshold
// too: a single failed poll followed by a good one is not news.
func transition(prev, next store.MailboxHealth) string {
	switch {
	case next.Status == store.MailboxError && next.ConsecutiveFailures == FailuresForEvent:
		return EventMailboxUnhealthy
	case next.Status == store.MailboxOK && prev.Status == store.MailboxError &&
		prev.ConsecutiveFailures >= FailuresForEvent:
		return EventMailboxRecovered
	}
	return ""
}

// eventPayload is the JSON body of mailbox.unhealthy / mailbox.recovered.
type eventPayload struct {
	Kind      Kind                `json:"kind"`
	MailboxID string              `json:"mailbox_id"`
	Name      string              `json:"name,omitempty"`
	Status    store.MailboxStatus `json:"status"`
	Stage     string              `json:"stage,omitempty"`
	Reason    string              `json:"reason,omitempty"`
	Failures  int                 `json:"consecutive_failures"`

	CheckedAt  time.Time `json:"checked_at"`
	LastOKAt   time.Time `json:"last_ok_at,omitzero"`
	OccurredAt time.Time `json:"occurred_at"`
}
