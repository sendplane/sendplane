package sender

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// The per-delivery event types the send path emits (architecture 12). They
// are written to the same store as the result they describe, right after
// Complete committed it, which is the outbox pattern the campaign and bounce
// events already use.
//
// Both are off unless the tenant subscribes to them, and delivery.sent is off
// even in the default set: it is one outbox row, one dispatch and one HTTP
// POST per recipient, so a million-recipient campaign would produce a million
// of each (store.DefaultOffEventTypes).
const (
	EventDeliverySent   = "delivery.sent"
	EventDeliveryFailed = "delivery.failed"
)

// completion is one committed result together with the identity of the
// delivery it belongs to. store.DeliveryResult carries the outcome and nothing
// else, and a delivery.* event has to name the recipient and the campaign, so
// the batcher keeps both.
type completion struct {
	res store.DeliveryResult
	d   deliveryRef
}

// deliveryRef is the part of a Delivery an event payload needs. It is a copy
// rather than a pointer: the batch outlives the worker that produced it.
type deliveryRef struct {
	CampaignID string
	SenderID   string
	Email      string
	EmailNorm  string
	Lane       store.Lane
	AttemptNo  int
}

// deliveryEventPayload is the JSON body of a delivery.sent / delivery.failed
// outbox event. It follows the shape internal/bounce uses for
// delivery.bounced: identity, the new status, what caused it, and when.
type deliveryEventPayload struct {
	DeliveryID string               `json:"delivery_id"`
	CampaignID string               `json:"campaign_id,omitempty"`
	SenderID   string               `json:"sender_id,omitempty"`
	Status     store.DeliveryStatus `json:"status"`
	Email      string               `json:"email,omitempty"`
	EmailNorm  string               `json:"email_norm,omitempty"`
	Lane       store.Lane           `json:"lane"`
	OccurredAt time.Time            `json:"occurred_at"`

	MessageID  string           `json:"message_id,omitempty"`
	AttemptNo  int              `json:"attempt_no,omitempty"`
	ErrorClass store.ErrorClass `json:"error_class,omitempty"`
	SMTPCode   int              `json:"smtp_code,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// batcher commits DeliveryResults in batches (architecture 8.1): every
// ResultBatchSize results, or every ResultFlushInterval, whichever comes
// first.
//
// Batching is what keeps a 1M campaign from doing 1M individual commits. The
// cost is the at-least-once window ADR-0002 documents: a crash between the
// SMTP 250 and the commit re-sends the message. MarkSent narrows that window
// to the messages whose 250 arrived in the last flush interval and whose
// MarkSent also failed.
type batcher struct {
	t        *tenantState
	st       store.Store
	size     int
	interval time.Duration
	log      *slog.Logger
	metrics  host.Metrics
	clock    func() time.Time

	mu  sync.Mutex
	buf []completion

	wake   chan struct{}
	done   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newBatcher(t *tenantState, size int, interval time.Duration, log *slog.Logger, m host.Metrics, clock func() time.Time) *batcher {
	b := &batcher{
		t: t, st: t.st, size: size, interval: interval, log: log, metrics: m, clock: clock,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	go b.loop()
	return b
}

// add queues one result. It never blocks on the store.
func (b *batcher) add(r store.DeliveryResult, d *store.Delivery) {
	if r.DeliveryID == "" {
		return
	}
	c := completion{res: r}
	if d != nil {
		c.d = deliveryRef{
			CampaignID: d.CampaignID, SenderID: d.SenderID,
			Email: d.Email, EmailNorm: d.EmailNorm, Lane: d.Lane,
			AttemptNo: d.AttemptCount,
		}
		if r.IncrementAttempt {
			c.d.AttemptNo++
		}
	}
	b.mu.Lock()
	b.buf = append(b.buf, c)
	full := len(b.buf) >= b.size
	b.mu.Unlock()
	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

func (b *batcher) loop() {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.closed:
			b.flush()
			return
		case <-b.wake:
			b.flush()
		case <-t.C:
			b.flush()
		}
	}
}

// flush commits whatever has accumulated. The buffer is swapped under the lock
// and the store call happens outside it, so adding never waits for a commit.
func (b *batcher) flush() {
	b.mu.Lock()
	batch := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	results := make([]store.DeliveryResult, len(batch))
	for i, c := range batch {
		results[i] = c.res
	}
	// A background context: the results describe work that already happened,
	// so they must be committed even while the sender is shutting down.
	ctx := context.Background()
	if err := b.st.Deliveries().Complete(ctx, results); err != nil {
		b.log.Error("sendplane: committing delivery results failed",
			"count", len(batch), "err", err)
		return
	}
	for _, r := range results {
		b.metrics.Count(MetricProcessed, 1, "status", r.NewStatus.String(), "class", r.ErrorClass.String())
	}
	b.enqueueEvents(ctx, batch)
}

// enqueueEvents writes the delivery.sent / delivery.failed rows of the batch
// that this tenant subscribes to (architecture 12). It runs after Complete
// committed, so an event only ever describes a transition that happened.
//
// The contract is one outbox row per delivery: the host gets the same event
// whether the delivery was one transactional message or one of a million, and
// the dispatcher batches them into HTTP POSTs on its own. That is why
// delivery.sent is not in the default subscription.
//
// An enqueue that fails is logged and dropped: the delivery rows are already
// committed, and failing the flush would re-commit them instead.
func (b *batcher) enqueueEvents(ctx context.Context, batch []completion) {
	var candidates []completion
	for _, c := range batch {
		if eventTypeFor(c.res.NewStatus) != "" {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return
	}
	now := store.TruncateTime(b.clock())
	settings, err := b.t.tenantSettings(ctx, now)
	if err != nil {
		b.log.Error("sendplane: cannot read the event subscription",
			"tenant", b.t.id, "err", err)
		return
	}

	evs := make([]store.OutboxEvent, 0, len(candidates))
	for _, c := range candidates {
		typ := eventTypeFor(c.res.NewStatus)
		if !settings.SubscribedTo(typ) {
			continue
		}
		payload, err := json.Marshal(deliveryEventPayload{
			DeliveryID: c.res.DeliveryID,
			CampaignID: c.d.CampaignID,
			SenderID:   c.d.SenderID,
			Status:     c.res.NewStatus,
			Email:      c.d.Email,
			EmailNorm:  c.d.EmailNorm,
			Lane:       c.d.Lane,
			OccurredAt: now,
			MessageID:  c.res.MessageID,
			AttemptNo:  c.d.AttemptNo,
			ErrorClass: c.res.ErrorClass,
			SMTPCode:   c.res.SMTPCode,
			Error:      c.res.Error,
		})
		if err != nil {
			b.log.Error("sendplane: cannot encode a delivery event",
				"delivery", c.res.DeliveryID, "type", typ, "err", err)
			continue
		}
		evs = append(evs, store.OutboxEvent{
			Type:          typ,
			Payload:       payload,
			Status:        store.OutboxPending,
			CreatedAt:     now,
			NextAttemptAt: now,
		})
	}
	if len(evs) == 0 {
		return
	}
	if err := b.st.Outbox().Enqueue(ctx, evs); err != nil {
		b.log.Error("sendplane: cannot enqueue delivery events",
			"tenant", b.t.id, "count", len(evs), "err", err)
	}
}

// eventTypeFor maps a committed status to its event type, or "" for the
// statuses that produce none. Only the two terminal outcomes of the send path
// are events: deferred is not terminal, and suppressed and bounced belong to
// the paths that decide them (internal/bounce).
func eventTypeFor(status store.DeliveryStatus) string {
	switch status {
	case store.DeliverySent:
		return EventDeliverySent
	case store.DeliveryFailed:
		return EventDeliveryFailed
	default:
		return ""
	}
}

// close flushes and stops the goroutine.
func (b *batcher) close() {
	b.once.Do(func() { close(b.closed) })
	<-b.done
}
