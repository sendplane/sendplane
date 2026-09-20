package sender

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/store"
)

// expectedOutcome replays chaossmtp's decision function to predict where a
// recipient ends up and how many retries it consumes. It is the same pure
// function the server uses, so the test asserts against the failure sequence
// the server actually produced rather than against a recorded snapshot.
func expectedOutcome(seed uint64, r chaossmtp.Rates, rcpt string, maxAttempts int) (store.DeliveryStatus, int) {
	for n := 1; n <= maxAttempts; n++ {
		switch chaossmtp.Decide(seed, r, rcpt, n) {
		case chaossmtp.Accept:
			// A success does not consume a retry, so AttemptCount is the
			// number of failures before it.
			return store.DeliverySent, n - 1
		case chaossmtp.PermFail:
			// permanent does not consume a retry either (ADR-0003).
			return store.DeliveryFailed, n - 1
		default: // TempFail, Drop: transient, consumes one retry
		}
	}
	return store.DeliveryFailed, maxAttempts
}

// TestEndToEndChaos is the brief's acceptance test: 5,000 deliveries through
// two sender replicas and a relay that fails 5% temporarily, 1% permanently
// and drops 0.5% mid-DATA. Every delivery must reach a terminal state, the
// attempt counts must match the deterministic failure sequence, and no message
// may be accepted twice.
func TestEndToEndChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end chaos test")
	}
	const (
		n           = 5000
		seed        = 20260921
		maxAttempts = 6
	)
	rates := chaossmtp.Rates{TempFailRate: 0.05, PermFailRate: 0.01, DropRate: 0.005}

	f := newFixture(t, fixtureOptions{rates: rates, seed: seed, maxConns: 8})
	ids := f.insert(n)
	if len(ids) != n {
		t.Fatalf("inserted %d deliveries, want %d", len(ids), n)
	}

	stop := f.runSenders(2, nil)
	ok := f.waitTerminal(ids, 120*time.Second)
	stop()
	if !ok {
		byStatus, _ := f.st.Deliveries().CountByStatus(context.Background(), f.campaignID)
		t.Fatalf("not every delivery finished: %v", byStatus)
	}

	var wantSent, wantFailed int
	for _, id := range ids {
		d := f.get(id)
		status, attempts := expectedOutcome(seed, rates, d.EmailNorm, maxAttempts)
		if d.Status != status {
			t.Errorf("%s: status = %s, want %s (last error %q)", d.EmailNorm, d.Status, status, d.LastError)
			continue
		}
		if d.AttemptCount != attempts {
			t.Errorf("%s: attempts = %d, want %d", d.EmailNorm, d.AttemptCount, attempts)
		}
		if status == store.DeliverySent {
			wantSent++
			if d.MessageID == "" {
				t.Errorf("%s: no Message-ID recorded", d.EmailNorm)
			}
			if d.SentAt.IsZero() {
				t.Errorf("%s: sent without a SentAt", d.EmailNorm)
			}
		} else {
			wantFailed++
		}
	}
	t.Logf("expected %d sent, %d failed; chaos stats %+v", wantSent, wantFailed, f.chaos.Stats())
	if wantFailed == 0 || wantSent == 0 {
		t.Fatal("the chaos rates should produce both outcomes")
	}

	msgs := f.chaos.Messages()
	if len(msgs) != wantSent {
		t.Errorf("relay accepted %d messages, %d deliveries are sent", len(msgs), wantSent)
	}
	// A message is accepted at most once: a drop happens before the relay
	// records anything, so a retry after one is not a duplicate.
	seen := map[string]string{}
	for _, m := range msgs {
		if m.MessageID == "" {
			t.Fatalf("accepted message without a Message-ID: %+v", m.Headers)
		}
		if prev, dup := seen[m.MessageID]; dup {
			t.Errorf("Message-ID %s accepted twice (recipients %s and %v)", m.MessageID, prev, m.Rcpts)
		}
		seen[m.MessageID] = strings.Join(m.Rcpts, ",")

		// Every accepted message carries the attempt the sender was on, and
		// the correlation headers of architecture 10.
		if m.Headers.Get(HeaderSendplaneID) == "" {
			t.Fatalf("missing %s", HeaderSendplaneID)
		}
		if !strings.HasPrefix(m.From, "bounce+") {
			t.Fatalf("envelope sender %q is not VERP", m.From)
		}
	}
}

func TestSuppressedAddressIsSkipped(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	ctx := context.Background()
	blocked := "blocked@example.org"
	if err := f.st.Suppressions().Upsert(ctx, &store.Suppression{
		EmailNorm: blocked, Reason: store.SuppressionHardBounce,
	}); err != nil {
		t.Fatal(err)
	}
	id := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = blocked, blocked
	})
	other := f.insertOne(nil)

	stop := f.runSenders(1, nil)
	ok := f.waitTerminal([]string{id, other}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("deliveries did not finish")
	}
	if got := f.get(id); got.Status != store.DeliverySuppressed {
		t.Errorf("suppressed address: status = %s", got.Status)
	}
	if got := f.get(other); got.Status != store.DeliverySent {
		t.Errorf("other address: status = %s (%s)", got.Status, got.LastError)
	}
	if n := len(f.chaos.Messages()); n != 1 {
		t.Errorf("relay saw %d messages, want 1", n)
	}

	// With suppression disabled the same address goes out.
	settings, _ := f.st.TenantSettings().Get(ctx)
	settings.SuppressionEnabled = false
	if err := f.st.TenantSettings().Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	id2 := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = "blocked2@example.org", "blocked2@example.org"
	})
	if err := f.st.Suppressions().Upsert(ctx, &store.Suppression{
		EmailNorm: "blocked2@example.org", Reason: store.SuppressionManual,
	}); err != nil {
		t.Fatal(err)
	}
	stop = f.runSenders(1, nil)
	ok = f.waitTerminal([]string{id2}, 20*time.Second)
	stop()
	if !ok || f.get(id2).Status != store.DeliverySent {
		t.Errorf("with suppression off: status = %s", f.get(id2).Status)
	}
}

func TestBeforeSendSkipAndRewrite(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	skipped := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = "skip@example.org", "skip@example.org"
	})
	kept := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = "keep@example.org", "keep@example.org"
	})

	stop := f.runSenders(1, func(c *Config) {
		c.Hooks.BeforeSend = func(_ context.Context, m *host.OutboundMessage) error {
			if m.Recipient.EmailNorm == "skip@example.org" {
				return host.ErrSkip
			}
			m.Subject = "rewritten"
			m.Headers["X-Test"] = "yes"
			return nil
		}
	})
	ok := f.waitTerminal([]string{skipped, kept}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("deliveries did not finish")
	}
	if got := f.get(skipped); got.Status != store.DeliverySuppressed {
		t.Errorf("ErrSkip: status = %s", got.Status)
	}
	if got := f.get(kept); got.Status != store.DeliverySent {
		t.Errorf("kept: status = %s (%s)", got.Status, got.LastError)
	}
	msgs := f.chaos.Messages()
	if len(msgs) != 1 {
		t.Fatalf("relay saw %d messages, want 1", len(msgs))
	}
	if got := msgs[0].Subject; got != "rewritten" {
		t.Errorf("subject = %q, want the hook's value", got)
	}
	if got := msgs[0].Headers.Get("X-Test"); got != "yes" {
		t.Errorf("X-Test = %q", got)
	}
}

func TestBeforeSendErrorIsRetriedThenFails(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	id := f.insertOne(nil)
	calls := make(chan struct{}, 64)

	stop := f.runSenders(1, func(c *Config) {
		c.Hooks.BeforeSend = func(context.Context, *host.OutboundMessage) error {
			select {
			case calls <- struct{}{}:
			default:
			}
			return errors.New("host is down")
		}
	})
	ok := f.waitTerminal([]string{id}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("delivery did not finish")
	}
	d := f.get(id)
	if d.Status != store.DeliveryFailed {
		t.Errorf("status = %s, want failed after the retries ran out", d.Status)
	}
	if d.AttemptCount != 6 {
		t.Errorf("attempts = %d, want 6", d.AttemptCount)
	}
	if d.LastErrorClass != store.ErrorClassTransient {
		t.Errorf("class = %s", d.LastErrorClass)
	}
	if len(calls) < 2 {
		t.Errorf("hook called %d times, expected a retry", len(calls))
	}
	if n := len(f.chaos.Messages()); n != 0 {
		t.Errorf("relay saw %d messages, want none", n)
	}
}

func TestHeaderInjectionIsRejected(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	// The recipient name is interpolated into the subject, so a CRLF in it is
	// an attempt to add headers (architecture 16).
	bad := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = "inject@example.org", "inject@example.org"
		d.Name = "Bob\r\nBcc: victim@example.org"
	})
	// A hook that adds a header outside the allowlist is rejected too.
	viaHook := f.insertOne(func(d *store.Delivery) {
		d.Email, d.EmailNorm = "hookinject@example.org", "hookinject@example.org"
	})

	stop := f.runSenders(1, func(c *Config) {
		c.Hooks.BeforeSend = func(_ context.Context, m *host.OutboundMessage) error {
			if m.Recipient.EmailNorm == "hookinject@example.org" {
				m.Headers["Bcc"] = "victim@example.org"
			}
			return nil
		}
	})
	ok := f.waitTerminal([]string{bad, viaHook}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("deliveries did not finish")
	}
	for _, id := range []string{bad, viaHook} {
		d := f.get(id)
		if d.Status != store.DeliveryFailed {
			t.Errorf("%s: status = %s, want failed", d.EmailNorm, d.Status)
		}
		if d.LastErrorClass != store.ErrorClassPermanent {
			t.Errorf("%s: class = %s, want permanent", d.EmailNorm, d.LastErrorClass)
		}
		if d.AttemptCount != 0 {
			t.Errorf("%s: a rejected message must not consume retries, got %d", d.EmailNorm, d.AttemptCount)
		}
	}
	if n := len(f.chaos.Messages()); n != 0 {
		t.Errorf("relay saw %d messages, want none", n)
	}
}

// TestMarkSentBeforeComplete pins the two-phase commit of architecture 8.1:
// the delivery is already sent as soon as the relay answered 250, before the
// batched Complete writes the attempt row.
func TestMarkSentBeforeComplete(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	id := f.insertOne(nil)

	stop := f.runSenders(1, func(c *Config) {
		// Long enough that the batch cannot have flushed while we look.
		c.ResultFlushInterval = 10 * time.Second
		c.ResultBatchSize = 1000
	})
	defer stop()

	deadline := time.Now().Add(20 * time.Second)
	var d *store.Delivery
	for time.Now().Before(deadline) {
		if d = f.get(id); d.Status == store.DeliverySent {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if d.Status != store.DeliverySent {
		t.Fatalf("status = %s, want sent from the MarkSent fast path", d.Status)
	}
	if d.MessageID == "" || d.SentAt.IsZero() {
		t.Errorf("MarkSent did not record the message id / time: %+v", d)
	}
	// The lease is still held, so the batched Complete can still attach the
	// attempt row (store/delivery.go).
	if d.LeaseOwner == "" {
		t.Error("MarkSent released the lease; Complete would then be dropped")
	}
	attempts, err := f.st.Attempts().ListByDelivery(context.Background(), id, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Items) != 0 {
		t.Errorf("the attempt row was written before the batch flushed: %+v", attempts.Items)
	}

	// Now let the batch flush and check the attempt landed.
	stop()
	attempts, err = f.st.Attempts().ListByDelivery(context.Background(), id, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts.Items) != 1 {
		t.Fatalf("after shutdown: %d attempts, want 1", len(attempts.Items))
	}
	a := attempts.Items[0]
	if a.ErrorClass != store.ErrorClassNone || a.TransportID != f.transportID {
		t.Errorf("attempt = %+v", a)
	}
	if got := f.get(id); got.Status != store.DeliverySent || got.LeaseOwner != "" {
		t.Errorf("after Complete: status %s, lease %q", got.Status, got.LeaseOwner)
	}
}

func TestRateLimitedResponseSlowsTheTransport(t *testing.T) {
	// The relay accepts two messages per connection and then answers 421.
	f := newFixture(t, fixtureOptions{
		maxConns: 1,
		chaos:    chaossmtp.Options{RateLimitAfter: 2},
	})
	ids := f.insert(6)

	stop := f.runSenders(1, func(c *Config) {
		c.DefaultRatePerSecond = 100
	})
	ok := f.waitTerminal(ids, 30*time.Second)
	stop()
	if !ok {
		t.Fatal("deliveries did not finish")
	}
	for _, id := range ids {
		if got := f.get(id); got.Status != store.DeliverySent {
			t.Errorf("%s: status = %s (%s)", got.EmailNorm, got.Status, got.LastError)
		}
	}
	if f.chaos.Stats().RateLimited == 0 {
		t.Fatal("the relay never rate limited, the test proves nothing")
	}
	// The transport was put into cooldown by the first 421 and released again
	// once deliveries started succeeding.
	tr, err := f.st.Transports().Get(context.Background(), f.transportID)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status == store.TransportUnhealthy {
		t.Errorf("a rate limit must not mark the transport unhealthy: %s", tr.StatusReason)
	}
}

func TestAuthFailureKeepsDeliveryQueuedAndMarksTransport(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		chaos: chaossmtp.Options{Username: "user", Password: "right", RequireAuth: true},
	})
	// The stored password is wrong, so every connection fails to authenticate.
	ctx := context.Background()
	tr, err := f.st.Transports().Get(ctx, f.transportID)
	if err != nil {
		t.Fatal(err)
	}
	tr.Password = []byte("wrong")
	if err := f.st.Transports().Update(ctx, tr); err != nil {
		t.Fatal(err)
	}
	id := f.insertOne(nil)

	stop := f.runSenders(1, func(c *Config) {
		c.TransportFailThreshold = 2
		c.TransportProbeInterval = time.Hour // no recovery during the test
	})
	deadline := time.Now().Add(20 * time.Second)
	var marked bool
	for time.Now().Before(deadline) {
		cur, err := f.st.Transports().Get(ctx, f.transportID)
		if err == nil && cur.Status == store.TransportUnhealthy {
			marked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if !marked {
		t.Fatal("the transport was never marked unhealthy")
	}
	// StatusUntil is what lets a replica that never saw this failure re-probe
	// the transport instead of skipping it forever.
	cur, err := f.st.Transports().Get(ctx, f.transportID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.StatusUntil.IsZero() {
		t.Error("StatusUntil is zero: unhealthy would never expire for another replica")
	} else if !cur.StatusUntil.After(cur.StatusChangedAt) {
		t.Errorf("StatusUntil %v is not after StatusChangedAt %v", cur.StatusUntil, cur.StatusChangedAt)
	}
	d := f.get(id)
	if d.Status != store.DeliveryQueued && d.Status != store.DeliveryLeased {
		t.Errorf("status = %s, want queued: an auth failure is not the delivery's fault", d.Status)
	}
	if d.AttemptCount != 0 {
		t.Errorf("attempts = %d, want 0: a transport fault must not exhaust a delivery", d.AttemptCount)
	}
	if f.chaos.Stats().AuthFailed == 0 {
		t.Error("the relay never saw a failed AUTH")
	}
}

func TestTrackingRewritesLinksAndInsertsPixel(t *testing.T) {
	f := newFixture(t, fixtureOptions{chaos: chaossmtp.Options{KeepBodies: true}})
	id := f.insertOne(nil)

	stop := f.runSenders(1, nil)
	ok := f.waitTerminal([]string{id}, 20*time.Second)
	stop()
	if !ok {
		t.Fatal("delivery did not finish")
	}
	msgs := f.chaos.Messages()
	if len(msgs) != 1 {
		t.Fatalf("relay saw %d messages", len(msgs))
	}
	body := decodeBody(t, string(msgs[0].Body))

	if strings.Contains(body, `href="https://example.com/one"`) {
		t.Error("a trackable link was not rewritten")
	}
	if n := strings.Count(body, "https://"+trackDomain+"/t/c/"); n != 2 {
		t.Errorf("%d click links, want 2", n)
	}
	if !strings.Contains(body, "https://"+trackDomain+"/t/o/") {
		t.Error("no open pixel")
	}
	if !strings.Contains(body, "https://"+trackDomain+"/t/u/") {
		t.Error("no sendplane unsubscribe URL in the body")
	}
	// The unsubscribe anchor is opted out of click tracking, so it must not
	// have become a /t/c/ link.
	if strings.Contains(body, "/t/c/") && strings.Count(body, "/t/c/") > 2 {
		t.Error("the unsubscribe link was click-tracked")
	}

	lu := msgs[0].Headers.Get("List-Unsubscribe")
	if !strings.Contains(lu, "/t/u/") {
		t.Errorf("List-Unsubscribe = %q", lu)
	}
	if got := msgs[0].Headers.Get("List-Unsubscribe-Post"); got != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q", got)
	}
}

// decodeBody undoes quoted-printable soft line breaks so that URLs split
// across lines can be searched for.
func decodeBody(t *testing.T, raw string) string {
	t.Helper()
	out := strings.ReplaceAll(raw, "=\r\n", "")
	out = strings.ReplaceAll(out, "=3D", "=")
	return out
}
