package bounce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
)

// The delivery event types the bounce path emits (architecture 12). The
// payload mirrors internal/control's campaign events: the transition is
// committed first, the event goes into the same store right after, and the
// control outbox dispatcher is what hands it to the host.
const (
	EventDeliveryBounced    = "delivery.bounced"
	EventDeliveryComplained = "delivery.complained"
)

// Skip reasons. They are the vocabulary of Outcome.Skipped, and the poller
// counts them by name, so they are part of this package's surface.
const (
	SkipAutoReply       = "auto_reply"
	SkipNotABounce      = "not_a_bounce"
	SkipNoCorrelation   = "no_correlation"
	SkipWrongTenant     = "wrong_tenant"
	SkipUnknownDelivery = "unknown_delivery"
	SkipUnverified      = "unverified"
	SkipDeliveryNotSent = "delivery_not_sent"
	SkipAlreadyTerminal = "already_terminal"
	SkipSoftBounce      = "soft_bounce"
	SkipNotTransitioned = "not_transitioned"
)

// Outcome is what Handle did with one message.
type Outcome struct {
	Parsed Parsed
	// Recorded is true when a BounceEvent was written.
	Recorded bool
	EventID  string
	// Processed is true when the delivery actually changed status. It is what
	// tells a caller a hard bounce or complaint landed.
	Processed bool
	NewStatus store.DeliveryStatus
	// Suppressed is true when the address was added to the suppression list.
	Suppressed bool
	// Unverified is true for a VERP address whose HMAC did not match: the
	// event is recorded, the delivery is not touched (ADR-0008).
	Unverified bool
	// Skipped names why nothing (or nothing more) happened. It is empty only
	// when the delivery transitioned.
	Skipped string
}

// Options configures a Processor.
type Options struct {
	// RetainRaw keeps the whole message on the BounceEvent. Architecture 10
	// says raw retention is a tenant setting, but store.TenantSettings has no
	// field for it, so it is a processor-wide option and defaults to off. See
	// README, "store 계약에 없어서 못 한 것".
	RetainRaw bool
	// MaxRawBytes caps a retained raw message. Zero uses DefaultMaxRawBytes.
	MaxRawBytes int
	// Clock is the time source. Nil uses time.Now.
	Clock func() time.Time
	// Logger is used for the paths that swallow an error. Nil discards.
	Logger  *slog.Logger
	Metrics host.Metrics
}

// DefaultMaxRawBytes bounds a retained raw message: a returned original can be
// megabytes, and the point of keeping it is diagnosis, not archival.
const DefaultMaxRawBytes = 256 << 10

// Metric names emitted by the processor and the poller.
const (
	MetricHandled   = "sendplane_bounce_handled_total"
	MetricSkipped   = "sendplane_bounce_skipped_total"
	MetricPollError = "sendplane_bounce_poll_errors_total"
)

// Processor turns parsed bounces into store writes. It holds no per-tenant
// state, so one instance serves every mailbox the Runner polls.
type Processor struct {
	opts    Options
	clock   func() time.Time
	log     *slog.Logger
	metrics host.Metrics
}

// NewProcessor builds a Processor.
func NewProcessor(opts Options) *Processor {
	p := &Processor{opts: opts, clock: opts.Clock, log: opts.Logger, metrics: opts.Metrics}
	if p.clock == nil {
		p.clock = time.Now
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.metrics == nil {
		p.metrics = host.NopMetrics{}
	}
	if p.opts.MaxRawBytes <= 0 {
		p.opts.MaxRawBytes = DefaultMaxRawBytes
	}
	return p
}

// Handle runs one message through the whole path of architecture 10: parse,
// correlate, load the delivery, record the event, transition, suppress, emit.
//
// st must be the store of tenantID: the tenant is not derivable from a
// store.Store, and the correlation has to be checked against it (an
// X-Sendplane-ID naming another tenant is a bounce for somebody else's mail).
//
// A returned error means the store failed and the message was not handled, so
// the caller must not acknowledge it. Everything else - an unparseable
// message, an unknown delivery, a forged MAC - is a non-error Outcome with
// Skipped set, because retrying it would only fetch it again forever.
func (p *Processor) Handle(ctx context.Context, st store.Store, tenantID string, msg mailbox.Message) (Outcome, error) {
	now := p.clock().UTC()

	settings, err := store.LoadTenantSettings(ctx, st, tenantID, now)
	if err != nil {
		return Outcome{}, fmt.Errorf("bounce: tenant settings: %w", err)
	}

	parsed, err := ParseWithKeys(msg.Raw, settings.Tracking.SigningKeys)
	if err != nil {
		// A message this package cannot even read is dropped, not retried.
		p.log.Warn("sendplane: bounce mail is not a parseable message",
			"tenant", tenantID, "id", msg.ID, "err", err)
		return p.skip(Outcome{Skipped: SkipNotABounce}), nil
	}
	out := Outcome{Parsed: parsed}

	switch {
	case parsed.AutoReply:
		out.Skipped = SkipAutoReply
		return p.skip(out), nil
	case !parsed.IsBounce():
		out.Skipped = SkipNotABounce
		return p.skip(out), nil
	}

	c := parsed.Correlation
	if c.DeliveryID == "" {
		// A real bounce nothing could be correlated with is still worth
		// keeping: it is the evidence that a mailbox is receiving bounces
		// sendplane cannot attribute.
		out.Skipped = SkipNoCorrelation
		if err := p.record(ctx, st, &out, msg, nil, now); err != nil {
			return out, err
		}
		return p.skip(out), nil
	}
	if c.TenantID != "" && c.TenantID != tenantID {
		// Somebody else's mail in this mailbox. Recording it here would put a
		// foreign delivery ID in this tenant's data.
		out.Skipped = SkipWrongTenant
		return p.skip(out), nil
	}

	// The store is tenant-scoped, so a delivery from another tenant is simply
	// not found.
	d, err := st.Deliveries().Get(ctx, c.DeliveryID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		out.Skipped = SkipUnknownDelivery
		if err := p.record(ctx, st, &out, msg, nil, now); err != nil {
			return out, err
		}
		return p.skip(out), nil
	case err != nil:
		return out, fmt.Errorf("bounce: load delivery %s: %w", c.DeliveryID, err)
	}

	if !c.Verified {
		// A forged VERP: recorded, flagged, and the delivery is left alone
		// (ADR-0008).
		out.Unverified = true
		out.Skipped = SkipUnverified
		if err := p.record(ctx, st, &out, msg, d, now); err != nil {
			return out, err
		}
		return p.skip(out), nil
	}

	switch {
	case d.Status == store.DeliverySent:
		// The normal path.
	case (d.Status == store.DeliveryBounced || d.Status == store.DeliveryComplained) &&
		parsed.Type == store.BounceSoft:
		// A late delay notice for a delivery that has already bounced is
		// history worth keeping, but it changes nothing.
		out.Skipped = SkipSoftBounce
		if err := p.record(ctx, st, &out, msg, d, now); err != nil {
			return out, err
		}
		return p.skip(out), nil
	case d.Status == store.DeliveryBounced || d.Status == store.DeliveryComplained:
		// A redelivered DSN. The first one did the work.
		out.Skipped = SkipAlreadyTerminal
		return p.skip(out), nil
	default:
		// queued, failed, suppressed, cancelled: a DSN for a delivery that
		// never reached the network, or one whose row was reused by a retry.
		out.Skipped = SkipDeliveryNotSent
		return p.skip(out), nil
	}

	if err := p.record(ctx, st, &out, msg, d, now); err != nil {
		return out, err
	}

	if parsed.Type == store.BounceSoft {
		// Soft bounces are recorded and leave the status alone
		// (architecture 4.1).
		out.Skipped = SkipSoftBounce
		return p.skip(out), nil
	}

	status := store.DeliveryBounced
	reason := store.SuppressionHardBounce
	eventType := EventDeliveryBounced
	mark := st.Deliveries().MarkBounced
	if parsed.Type == store.BounceComplaint {
		status, reason, eventType = store.DeliveryComplained, store.SuppressionComplaint, EventDeliveryComplained
		mark = st.Deliveries().MarkComplained
	}

	changed, err := mark(ctx, d.ID, now)
	if err != nil {
		return out, fmt.Errorf("bounce: mark %s %s: %w", status, d.ID, err)
	}
	if !changed {
		// Somebody else won the race between the Get and the CAS: their run
		// suppressed and emitted.
		out.Skipped = SkipNotTransitioned
		return p.skip(out), nil
	}
	out.Processed = true
	out.NewStatus = status

	if settings.SuppressionEnabled && d.Lane != store.LaneProbe {
		// Probe deliveries are excluded from suppression (ADR-0012): a
		// loopback address that bounces is a health problem, not a recipient
		// to stop mailing.
		if err := st.Suppressions().Upsert(ctx, &store.Suppression{
			EmailNorm:        d.EmailNorm,
			Reason:           reason,
			SourceDeliveryID: d.ID,
			CreatedAt:        now,
		}); err != nil {
			return out, fmt.Errorf("bounce: suppress %s: %w", d.EmailNorm, err)
		}
		out.Suppressed = true
	}

	if err := p.enqueueEvent(ctx, st, d, parsed, eventType, status, out.Suppressed, now); err != nil {
		return out, fmt.Errorf("bounce: enqueue %s: %w", eventType, err)
	}
	p.metrics.Count(MetricHandled, 1, "type", parsed.Type.String(), "status", status.String())
	return out, nil
}

func (p *Processor) skip(out Outcome) Outcome {
	if out.Skipped != "" {
		p.metrics.Count(MetricSkipped, 1, "reason", out.Skipped)
	}
	return out
}

// record writes the BounceEvent. d may be nil when nothing was correlated.
func (p *Processor) record(ctx context.Context, st store.Store, out *Outcome, msg mailbox.Message, d *store.Delivery, now time.Time) error {
	parsed := out.Parsed
	ev := &store.BounceEvent{
		ID:             store.NewID(),
		DeliveryID:     parsed.Correlation.DeliveryID,
		Type:           parsed.Type,
		Source:         parsed.Correlation.Source,
		Verified:       parsed.Correlation.Verified,
		Recipient:      parsed.FinalRecipient,
		SMTPStatus:     parsed.Status,
		DiagnosticCode: parsed.DiagnosticCode,
		MessageID:      parsed.MessageID,
		ReceivedAt:     receivedAt(msg, parsed, now),
		CreatedAt:      now,
	}
	if parsed.Correlation.Source == BounceSourceNone {
		// The model has no "nothing matched" source; a heuristic match is the
		// honest label for an event that came out of the pattern tables.
		ev.Source = store.BounceSourceHeuristic
	}
	if d != nil {
		// The delivery is authoritative about the address: the DSN's final
		// recipient may be a forwarding destination we never sent to.
		ev.EmailNorm = d.EmailNorm
		if ev.Recipient == "" {
			ev.Recipient = d.Email
		}
	} else if parsed.FinalRecipient != "" {
		if norm, err := store.NormalizeEmail(parsed.FinalRecipient); err == nil {
			ev.EmailNorm = norm
		}
	}
	if p.opts.RetainRaw {
		ev.Raw = rawJSON(msg.Raw, p.opts.MaxRawBytes)
	}
	if err := st.Bounces().Create(ctx, ev); err != nil {
		return fmt.Errorf("bounce: record event: %w", err)
	}
	out.Recorded = true
	out.EventID = ev.ID
	return nil
}

// receivedAt is when the bounce arrived: the server's own timestamp if the
// mailbox had one, then the report's Date, then now.
func receivedAt(msg mailbox.Message, parsed Parsed, now time.Time) time.Time {
	switch {
	case !msg.Received.IsZero():
		return msg.Received.UTC()
	case !parsed.Date.IsZero():
		return parsed.Date.UTC()
	default:
		return now
	}
}

// rawJSON wraps the raw message as a JSON string so that it round-trips
// through BounceEvent.Raw, which is json.RawMessage in every backend.
func rawJSON(raw []byte, max int) json.RawMessage {
	if max > 0 && len(raw) > max {
		raw = raw[:max]
	}
	b, err := json.Marshal(string(raw))
	if err != nil {
		return nil
	}
	return b
}

// deliveryEventPayload is the JSON body of a delivery.bounced /
// delivery.complained outbox event. It follows the shape of
// internal/control's campaign events: identity, the new status, what caused
// it, and when it happened.
type deliveryEventPayload struct {
	DeliveryID string               `json:"delivery_id"`
	CampaignID string               `json:"campaign_id,omitempty"`
	Status     store.DeliveryStatus `json:"status"`
	Email      string               `json:"email,omitempty"`
	EmailNorm  string               `json:"email_norm,omitempty"`
	Lane       store.Lane           `json:"lane"`
	OccurredAt time.Time            `json:"occurred_at"`

	BounceType     store.BounceType   `json:"bounce_type"`
	BounceSource   store.BounceSource `json:"bounce_source,omitempty"`
	SMTPStatus     string             `json:"smtp_status,omitempty"`
	DiagnosticCode string             `json:"diagnostic_code,omitempty"`
	Confidence     Confidence         `json:"confidence,omitempty"`
	Suppressed     bool               `json:"suppressed"`
}

func (p *Processor) enqueueEvent(ctx context.Context, st store.Store, d *store.Delivery, parsed Parsed, typ string, status store.DeliveryStatus, suppressed bool, now time.Time) error {
	payload, err := json.Marshal(deliveryEventPayload{
		DeliveryID:     d.ID,
		CampaignID:     d.CampaignID,
		Status:         status,
		Email:          d.Email,
		EmailNorm:      d.EmailNorm,
		Lane:           d.Lane,
		OccurredAt:     now,
		BounceType:     parsed.Type,
		BounceSource:   parsed.Correlation.Source,
		SMTPStatus:     parsed.Status,
		DiagnosticCode: parsed.DiagnosticCode,
		Confidence:     parsed.Confidence,
		Suppressed:     suppressed,
	})
	if err != nil {
		return err
	}
	return st.Outbox().Enqueue(ctx, []store.OutboxEvent{{
		Type:          typ,
		Payload:       payload,
		Status:        store.OutboxPending,
		CreatedAt:     now,
		NextAttemptAt: now,
	}})
}
