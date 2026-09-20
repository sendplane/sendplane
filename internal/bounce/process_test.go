package bounce

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

var testNow = time.Date(2025, 9, 19, 20, 0, 0, 0, time.UTC)

// fixtureEnv is one tenant's store seeded with the two deliveries the corpus
// correlates with.
type fixtureEnv struct {
	provider *memstore.Provider
	st       store.Store
	proc     *Processor
}

func newEnv(t *testing.T, suppression bool, opts ...func(*Options)) *fixtureEnv {
	t.Helper()
	ctx := context.Background()
	p := memstore.New()
	st, err := p.ForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	settings := store.DefaultTenantSettings(testTenant, testNow)
	settings.SuppressionEnabled = suppression
	settings.Tracking.SigningKeys = testKeys()
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("create settings: %v", err)
	}

	o := Options{Clock: func() time.Time { return testNow }}
	for _, f := range opts {
		f(&o)
	}
	return &fixtureEnv{provider: p, st: st, proc: NewProcessor(o)}
}

func (e *fixtureEnv) seed(t *testing.T, d store.Delivery) {
	t.Helper()
	if _, err := e.st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
}

func sentDelivery(id, email string) store.Delivery {
	return store.Delivery{
		ID: id, CampaignID: "campaign-1", Lane: store.LaneBulk,
		Status: store.DeliverySent, Email: email, EmailNorm: email,
		MessageID: id + "@example.com", SentAt: testNow.Add(-time.Hour),
		CreatedAt: testNow.Add(-2 * time.Hour),
	}
}

func (e *fixtureEnv) handle(t *testing.T, file string) Outcome {
	t.Helper()
	out, err := e.proc.Handle(context.Background(), e.st, testTenant, mailbox.Message{
		ID: file, Raw: loadFixture(t, file), Received: testNow.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Handle(%s): %v", file, err)
	}
	return out
}

func (e *fixtureEnv) delivery(t *testing.T, id string) *store.Delivery {
	t.Helper()
	d, err := e.st.Deliveries().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get delivery: %v", err)
	}
	return d
}

func (e *fixtureEnv) events(t *testing.T) []store.BounceEvent {
	t.Helper()
	res, err := e.st.Bounces().List(context.Background(), store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("list bounces: %v", err)
	}
	return res.Items
}

func (e *fixtureEnv) outbox(t *testing.T) []store.OutboxEvent {
	t.Helper()
	res, err := e.st.Outbox().List(context.Background(), store.OutboxPending, store.Page{Limit: 100})
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	return res.Items
}

func TestHandleHardBounce(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "nosuch@example.org"))

	out := e.handle(t, "postfix_hard.eml")
	if !out.Processed || out.NewStatus != store.DeliveryBounced {
		t.Fatalf("Outcome = %+v", out)
	}
	if !out.Suppressed || !out.Recorded || out.Skipped != "" {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliveryBounced {
		t.Fatalf("delivery status = %v", got)
	}

	evs := e.events(t)
	if len(evs) != 1 {
		t.Fatalf("recorded %d bounce events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Type != store.BounceHard || ev.Source != store.BounceSourceVERP || !ev.Verified {
		t.Fatalf("event = %+v", ev)
	}
	if ev.EmailNorm != "nosuch@example.org" || ev.SMTPStatus != "5.1.1" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ev.Raw) != 0 {
		t.Fatalf("raw retained although the option is off: %d bytes", len(ev.Raw))
	}

	sup, s, err := e.st.Suppressions().IsSuppressed(context.Background(), "nosuch@example.org", testNow)
	if err != nil || !sup {
		t.Fatalf("IsSuppressed = %v, %v", sup, err)
	}
	if s.Reason != store.SuppressionHardBounce || s.SourceDeliveryID != testDelivery {
		t.Fatalf("suppression = %+v", s)
	}

	evsOut := e.outbox(t)
	if len(evsOut) != 1 || evsOut[0].Type != EventDeliveryBounced {
		t.Fatalf("outbox = %+v", evsOut)
	}
	var payload deliveryEventPayload
	if err := json.Unmarshal(evsOut[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.DeliveryID != testDelivery || payload.Status != store.DeliveryBounced ||
		payload.BounceType != store.BounceHard || !payload.Suppressed {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestHandleSuppressionDisabled(t *testing.T) {
	e := newEnv(t, false)
	e.seed(t, sentDelivery(testDelivery, "nosuch@example.org"))

	out := e.handle(t, "postfix_hard.eml")
	if !out.Processed || out.Suppressed {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliveryBounced {
		t.Fatalf("delivery status = %v", got)
	}
	sup, _, err := e.st.Suppressions().IsSuppressed(context.Background(), "nosuch@example.org", testNow)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if sup {
		t.Fatal("address suppressed although the tenant turned suppression off")
	}
	// The event still goes to the host: the host DB can be the source of
	// truth when suppression is off (ADR-0008).
	if evs := e.outbox(t); len(evs) != 1 {
		t.Fatalf("outbox has %d events, want 1", len(evs))
	}
}

func TestHandleComplaint(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "reader@example.org"))

	out := e.handle(t, "arf_complaint.eml")
	if !out.Processed || out.NewStatus != store.DeliveryComplained || !out.Suppressed {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliveryComplained {
		t.Fatalf("delivery status = %v", got)
	}
	_, s, err := e.st.Suppressions().IsSuppressed(context.Background(), "reader@example.org", testNow)
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if s == nil || s.Reason != store.SuppressionComplaint {
		t.Fatalf("suppression = %+v", s)
	}
	if evs := e.outbox(t); len(evs) != 1 || evs[0].Type != EventDeliveryComplained {
		t.Fatalf("outbox = %+v", evs)
	}
}

func TestHandleSoftBounceKeepsStatus(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery2, "full@example.org"))

	out := e.handle(t, "soft_mailbox_full.eml")
	if out.Processed {
		t.Fatalf("a soft bounce transitioned the delivery: %+v", out)
	}
	if !out.Recorded || out.Skipped != SkipSoftBounce {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery2).Status; got != store.DeliverySent {
		t.Fatalf("delivery status = %v, want sent", got)
	}
	if evs := e.events(t); len(evs) != 1 || evs[0].Type != store.BounceSoft {
		t.Fatalf("events = %+v", evs)
	}
	sup, _, _ := e.st.Suppressions().IsSuppressed(context.Background(), "full@example.org", testNow)
	if sup {
		t.Fatal("a soft bounce suppressed the address")
	}
	if evs := e.outbox(t); len(evs) != 0 {
		t.Fatalf("a soft bounce emitted %d host events", len(evs))
	}
}

func TestHandleUnverifiedVERP(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "victim@example.org"))

	out := e.handle(t, "forged_verp.eml")
	if out.Processed || !out.Unverified || out.Skipped != SkipUnverified {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliverySent {
		t.Fatalf("a forged bounce changed the delivery to %v", got)
	}
	evs := e.events(t)
	if len(evs) != 1 || evs[0].Verified {
		t.Fatalf("events = %+v", evs)
	}
	sup, _, _ := e.st.Suppressions().IsSuppressed(context.Background(), "victim@example.org", testNow)
	if sup {
		t.Fatal("a forged bounce suppressed the address")
	}
	if evs := e.outbox(t); len(evs) != 0 {
		t.Fatalf("a forged bounce emitted %d host events", len(evs))
	}
}

func TestHandleWrongTenant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "gone@example.org"))

	// exim_hard.eml correlates through X-Sendplane-ID: acme/<delivery>. A
	// store for another tenant must reject it before it reads anything.
	other, err := e.provider.ForTenant(ctx, "other-tenant")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	out, err := e.proc.Handle(ctx, other, "other-tenant", mailbox.Message{
		ID: "1", Raw: loadFixture(t, "exim_hard.eml"), Received: testNow,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.Processed || out.Recorded || out.Skipped != SkipWrongTenant {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliverySent {
		t.Fatalf("another tenant's bounce changed the delivery to %v", got)
	}
}

func TestHandleNonSentDelivery(t *testing.T) {
	e := newEnv(t, true)
	d := sentDelivery(testDelivery, "nosuch@example.org")
	d.Status = store.DeliveryQueued
	e.seed(t, d)

	out := e.handle(t, "postfix_hard.eml")
	if out.Processed || out.Recorded || out.Skipped != SkipDeliveryNotSent {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliveryQueued {
		t.Fatalf("delivery status = %v", got)
	}
}

func TestHandleDuplicateDSNIsIdempotent(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "nosuch@example.org"))

	first := e.handle(t, "postfix_hard.eml")
	second := e.handle(t, "postfix_hard.eml")
	if !first.Processed {
		t.Fatalf("first Outcome = %+v", first)
	}
	if second.Processed || second.Recorded || second.Skipped != SkipAlreadyTerminal {
		t.Fatalf("second Outcome = %+v", second)
	}
	if evs := e.events(t); len(evs) != 1 {
		t.Fatalf("a redelivered DSN recorded %d events, want 1", len(evs))
	}
	if evs := e.outbox(t); len(evs) != 1 {
		t.Fatalf("a redelivered DSN emitted %d host events, want 1", len(evs))
	}
}

func TestHandleLateSoftBounceAfterHard(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "nosuch@example.org"))
	e.handle(t, "postfix_hard.eml")

	// delayed.eml correlates with the same delivery. It is kept as history
	// and changes nothing.
	out := e.handle(t, "delayed.eml")
	if out.Processed || !out.Recorded || out.Skipped != SkipSoftBounce {
		t.Fatalf("Outcome = %+v", out)
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliveryBounced {
		t.Fatalf("delivery status = %v", got)
	}
	if evs := e.events(t); len(evs) != 2 {
		t.Fatalf("recorded %d events, want 2", len(evs))
	}
}

func TestHandleAutoReplyAndUnrelated(t *testing.T) {
	e := newEnv(t, true)
	e.seed(t, sentDelivery(testDelivery, "jane@example.org"))

	for file, want := range map[string]string{
		"auto_reply.eml":       SkipAutoReply,
		"autoreply_korean.eml": SkipAutoReply,
		"unrelated.eml":        SkipNotABounce,
	} {
		out := e.handle(t, file)
		if out.Skipped != want || out.Recorded || out.Processed {
			t.Errorf("%s: Outcome = %+v, want skip %q", file, out, want)
		}
	}
	if got := e.delivery(t, testDelivery).Status; got != store.DeliverySent {
		t.Fatalf("delivery status = %v", got)
	}
	if evs := e.events(t); len(evs) != 0 {
		t.Fatalf("recorded %d events for mail that is not a bounce", len(evs))
	}
}

func TestHandleUnknownDeliveryIsRecorded(t *testing.T) {
	e := newEnv(t, true)
	// Nothing seeded: retention already removed the delivery.
	out := e.handle(t, "postfix_hard.eml")
	if out.Processed || out.Skipped != SkipUnknownDelivery || !out.Recorded {
		t.Fatalf("Outcome = %+v", out)
	}
	evs := e.events(t)
	if len(evs) != 1 || evs[0].DeliveryID != testDelivery {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].EmailNorm != "nosuch@example.org" {
		t.Fatalf("event email_norm = %q", evs[0].EmailNorm)
	}
}

func TestHandleRetainRaw(t *testing.T) {
	e := newEnv(t, true, func(o *Options) { o.RetainRaw = true })
	e.seed(t, sentDelivery(testDelivery, "nosuch@example.org"))
	e.handle(t, "postfix_hard.eml")

	evs := e.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	var raw string
	if err := json.Unmarshal(evs[0].Raw, &raw); err != nil {
		t.Fatalf("raw is not a JSON string: %v", err)
	}
	if want := string(loadFixture(t, "postfix_hard.eml")); raw != want {
		t.Fatalf("raw is %d bytes, want %d", len(raw), len(want))
	}
}

func TestHandleProbeLaneIsNotSuppressed(t *testing.T) {
	e := newEnv(t, true)
	d := sentDelivery(testDelivery, "nosuch@example.org")
	d.Lane = store.LaneProbe
	d.CampaignID = ""
	e.seed(t, d)

	out := e.handle(t, "postfix_hard.eml")
	if !out.Processed || out.Suppressed {
		t.Fatalf("Outcome = %+v", out)
	}
	sup, _, _ := e.st.Suppressions().IsSuppressed(context.Background(), "nosuch@example.org", testNow)
	if sup {
		t.Fatal("a probe bounce suppressed the address (ADR-0012)")
	}
}
