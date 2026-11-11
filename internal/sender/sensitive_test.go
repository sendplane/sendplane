package sender

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/store"
)

// otpCode is the secret every test here sends. It must never show up outside
// the message itself.
const otpCode = "OTP-482913"

// secretHTML reads the secret and has a trackable link, so the "no tracking"
// assertion has something that would otherwise be rewritten.
const secretHTML = `<html><body><p>Your code is {{ secret.code }}</p>` +
	`<a href="https://example.com/one">one</a>` +
	`<a href="{{ unsubscribe_url }}" data-sp-track="off">unsubscribe</a></body></html>`

// sensitiveFixture is the sender fixture plus a version that reads
// `secret.code`, a cipher, and a log the tests can search.
type sensitiveFixture struct {
	*fixture
	cipher *aeadCipher
	logs   *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newSensitiveFixture(t *testing.T, opt fixtureOptions) *sensitiveFixture {
	t.Helper()
	opt.skipCampaign = true
	return &sensitiveFixture{fixture: newFixture(t, opt), cipher: newAEADCipher(t), logs: &lockedBuffer{}}
}

// version publishes a version with the given HTML (secretHTML when empty).
func (f *sensitiveFixture) version(html string) string {
	f.t.Helper()
	if html == "" {
		html = secretHTML
	}
	v := &store.MessageVersion{
		SubjectTpl: "Your code", HTMLTpl: html, TextTpl: "Code: {{ secret.code }}",
		DefaultLocale: "en", Links: []string{"https://example.com/one"},
	}
	if err := f.st.Versions().Create(context.Background(), v); err != nil {
		f.t.Fatalf("version: %v", err)
	}
	return v.ID
}

// insertSensitive adds a queued transactional delivery whose secret_vars are
// {"code": otpCode}, sealed for its own row the way the API seals them.
// mutate runs after sealing, so a test can break the secret.
func (f *sensitiveFixture) insertSensitive(versionID string, mutate func(*store.Delivery)) string {
	f.t.Helper()
	return f.insertSensitiveWith(versionID, func(_ *sensitiveFixture, d *store.Delivery) {
		if mutate != nil {
			mutate(d)
		}
	})
}

func (f *sensitiveFixture) insertSensitiveWith(versionID string, mutate func(*sensitiveFixture, *store.Delivery)) string {
	f.t.Helper()
	email := fmt.Sprintf("otp%d@example.org", time.Now().UnixNano())
	d := store.Delivery{
		ID: store.NewID(), TenantID: testTenant,
		VersionID: versionID, SenderID: f.senderID,
		Lane: store.LaneTransactional, Status: store.DeliveryQueued,
		Email: email, EmailNorm: email, Name: "Otp", Locale: "en",
		Vars:            map[string]any{"plan": "pro"},
		SecretState:     store.SecretPending,
		SecretExpiresAt: time.Now().Add(10 * time.Minute),
	}
	d.SecretCiphertext = f.cipher.seal(f.t, `{"code":"`+otpCode+`"}`, store.DeliverySecretContext(&d))
	mutate(f, &d)
	if _, err := f.st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
		f.t.Fatalf("InsertBatch: %v", err)
	}
	return d.ID
}

// run runs one sender over the transactional lane until every id is
// terminal.
func (f *sensitiveFixture) run(ids []string, mutate func(*Config)) {
	f.t.Helper()
	f.runWith(ids, mutate, nil)
}

// runWith is run with a setup step on the Sender (runSendersWith).
func (f *sensitiveFixture) runWith(ids []string, mutate func(*Config), setup func(*Sender)) {
	f.t.Helper()
	stop := f.runSendersWith(1, func(c *Config) {
		c.Lanes = map[store.Lane]int{store.LaneTransactional: 2}
		c.Secrets = f.cipher
		c.Logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		if mutate != nil {
			mutate(c)
		}
	}, setup)
	ok := f.waitTerminal(ids, 20*time.Second)
	stop()
	if !ok {
		for _, id := range ids {
			d := f.get(id)
			f.t.Logf("%s: %s %q", id, d.Status, d.LastError)
		}
		f.t.Fatal("deliveries did not finish")
	}
}

func (f *sensitiveFixture) attempts(id string) []store.DeliveryAttempt {
	f.t.Helper()
	res, err := f.st.Attempts().ListByDelivery(context.Background(), id, store.Page{Limit: store.MaxPageLimit})
	if err != nil {
		f.t.Fatalf("attempts: %v", err)
	}
	return res.Items
}

// The happy path: the secret is rendered into the message, the message has
// no open pixel and no rewritten link (the unsubscribe header stays), and the
// stored row ends consumed with its ciphertext gone.
func TestSensitiveDeliverySends(t *testing.T) {
	f := newSensitiveFixture(t, fixtureOptions{chaos: chaossmtp.Options{KeepBodies: true}})
	id := f.insertSensitive(f.version(""), nil)
	f.run([]string{id}, nil)

	d := f.get(id)
	if d.Status != store.DeliverySent {
		t.Fatalf("status = %s (%s), want sent", d.Status, d.LastError)
	}
	if d.SecretState != store.SecretConsumed || d.SecretCiphertext != nil {
		t.Fatalf("secret state %s, ciphertext kept %v; want consumed and dropped", d.SecretState, d.SecretCiphertext != nil)
	}
	if _, ok := d.Vars["code"]; ok {
		t.Fatalf("the secret ended up in vars: %v", d.Vars)
	}

	msgs := f.chaos.Messages()
	if len(msgs) != 1 {
		t.Fatalf("relay saw %d messages", len(msgs))
	}
	body := decodeBody(t, string(msgs[0].Body))
	if !strings.Contains(body, "Your code is "+otpCode) || !strings.Contains(body, "Code: "+otpCode) {
		t.Fatalf("the secret was not rendered into both parts:\n%s", body)
	}
	if strings.Contains(body, "/t/o/") {
		t.Error("a sensitive message got an open pixel")
	}
	if strings.Contains(body, "/t/c/") || !strings.Contains(body, `href="https://example.com/one"`) {
		t.Error("a sensitive message had its links rewritten")
	}
	if lu := msgs[0].Headers.Get("List-Unsubscribe"); !strings.Contains(lu, "/t/u/") {
		t.Errorf("List-Unsubscribe = %q, want it kept", lu)
	}
	if strings.Contains(f.logs.String(), otpCode) {
		t.Errorf("the logs contain the secret:\n%s", f.logs.String())
	}
}

// Every way a secret can be unusable fails the delivery before anything is
// rendered or sent, with a fixed text and no fallback.
func TestSensitiveDeliveryRefusedWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*sensitiveFixture, *store.Delivery)
		noCipher  bool
		wantError string
		wantState store.SecretState
	}{
		{
			name:      "already consumed",
			mutate:    func(_ *sensitiveFixture, d *store.Delivery) { d.SecretState = store.SecretConsumed },
			wantError: secretUnavailableError, wantState: store.SecretConsumed,
		},
		{
			name:      "pending without ciphertext",
			mutate:    func(_ *sensitiveFixture, d *store.Delivery) { d.SecretCiphertext = nil },
			wantError: secretUnavailableError, wantState: store.SecretDiscarded,
		},
		{
			name:      "expired",
			mutate:    func(_ *sensitiveFixture, d *store.Delivery) { d.SecretExpiresAt = time.Now().Add(-time.Second) },
			wantError: store.SecretExpiredError, wantState: store.SecretExpired,
		},
		{
			// A ciphertext sealed, with the right key, for another delivery
			// and copied onto this one.
			name: "bound to another row",
			mutate: func(f *sensitiveFixture, d *store.Delivery) {
				other := *d
				other.ID = store.NewID()
				d.SecretCiphertext = f.cipher.seal(t, `{"code":"`+otpCode+`"}`, store.DeliverySecretContext(&other))
			},
			wantError: secretDecryptFailedError, wantState: store.SecretDiscarded,
		},
		{
			// The row's own ID, but sealed for another address: rewriting a
			// row's email columns does not redirect its secret.
			name: "sealed for another recipient",
			mutate: func(f *sensitiveFixture, d *store.Delivery) {
				other := *d
				other.Email, other.EmailNorm = "attacker@example.net", "attacker@example.net"
				d.SecretCiphertext = f.cipher.seal(t, `{"code":"`+otpCode+`"}`, store.DeliverySecretContext(&other))
			},
			wantError: secretDecryptFailedError, wantState: store.SecretDiscarded,
		},
		{
			name: "no cipher", noCipher: true,
			wantError: secretDecryptFailedError, wantState: store.SecretDiscarded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSensitiveFixture(t, fixtureOptions{})
			mutate := tc.mutate
			if mutate == nil {
				mutate = func(*sensitiveFixture, *store.Delivery) {}
			}
			id := f.insertSensitiveWith(f.version(""), mutate)
			f.run([]string{id}, func(c *Config) {
				if tc.noCipher {
					c.Secrets = nil
				}
			})
			d := f.get(id)
			if d.Status != store.DeliveryFailed || d.LastErrorClass != store.ErrorClassPermanent || d.LastError != tc.wantError {
				t.Fatalf("delivery = %s %s %q, want failed permanent %q", d.Status, d.LastErrorClass, d.LastError, tc.wantError)
			}
			if d.SecretState != tc.wantState || d.SecretCiphertext != nil {
				t.Fatalf("secret state %s ciphertext kept %v, want %s and dropped", d.SecretState, d.SecretCiphertext != nil, tc.wantState)
			}
			if n := len(f.chaos.Messages()); n != 0 {
				t.Fatalf("relay saw %d messages", n)
			}
		})
	}
}

// A temporary failure whose retry would only be due after the secret has
// expired fails the delivery there and then. The attempt row keeps the 4xx.
func TestSensitiveDeliveryDeferredPastExpiryFails(t *testing.T) {
	f := newSensitiveFixture(t, fixtureOptions{
		rates: chaossmtp.Rates{TempFailRate: 1},
		settings: func(s *store.TenantSettings) {
			s.Retry = store.RetryPolicy{Backoff: []time.Duration{time.Hour}, MaxAttempts: 6}
		},
	})
	id := f.insertSensitive(f.version(""), nil)
	f.run([]string{id}, nil)

	d := f.get(id)
	if d.Status != store.DeliveryFailed || d.LastError != store.SecretExpiredError || d.LastErrorClass != store.ErrorClassPermanent {
		t.Fatalf("delivery = %s %s %q, want failed permanent %q", d.Status, d.LastErrorClass, d.LastError, store.SecretExpiredError)
	}
	if d.SecretState == store.SecretPending || d.SecretCiphertext != nil {
		t.Fatalf("secret state %s, ciphertext kept %v", d.SecretState, d.SecretCiphertext != nil)
	}
	atts := f.attempts(id)
	if len(atts) != 1 || atts[0].SMTPCode != 451 || atts[0].ErrorClass != store.ErrorClassTransient {
		t.Fatalf("attempts = %+v, want the one 451", atts)
	}

	// Control: the same deferral inside the TTL is an ordinary deferral.
	g := newSensitiveFixture(t, fixtureOptions{
		rates: chaossmtp.Rates{TempFailRate: 1},
		settings: func(s *store.TenantSettings) {
			s.Retry = store.RetryPolicy{Backoff: []time.Duration{time.Hour}, MaxAttempts: 6}
		},
	})
	id2 := g.insertSensitive(g.version(""), func(d *store.Delivery) { d.SecretExpiresAt = time.Now().Add(2 * time.Hour) })
	stop := g.runSenders(1, func(c *Config) {
		c.Lanes = map[store.Lane]int{store.LaneTransactional: 2}
		c.Secrets = g.cipher
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && g.get(id2).Status != store.DeliveryDeferred {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if d := g.get(id2); d.Status != store.DeliveryDeferred || d.SecretState != store.SecretPending || d.SecretCiphertext == nil {
		t.Fatalf("in-TTL deferral = %s, secret %s, ciphertext kept %v; want deferred and still pending", d.Status, d.SecretState, d.SecretCiphertext != nil)
	}
}

// echoSMTP is a relay that quotes the message back in its end-of-DATA reply,
// the way some content filters do. code is the reply code (451 or 554).
func echoSMTP(t *testing.T, code int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveEcho(conn, code)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func serveEcho(conn net.Conn, code int) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	reply := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	reply("220 echo.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			reply("250 echo.test")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"), strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			reply("250 OK")
		case cmd == "DATA":
			reply("354 go ahead")
			var quoted string
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				if quoted == "" && strings.Contains(l, otpCode) {
					quoted = strings.TrimSpace(l)
				}
			}
			enh := "4.7.0"
			if code >= 500 {
				enh = "5.7.1"
			}
			reply(fmt.Sprintf("%d %s content held: %s", code, enh, quoted))
		case cmd == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 unknown")
		}
	}
}

// The secret never reaches a stored error, an attempt row, an outbox payload
// or the log, whichever failure quotes it: a template error (Liquid quotes
// the value it could not convert), a 4xx or 5xx reply that echoes the
// message, a BeforeSend error that includes the body, a render warning.
func TestSensitiveDeliveryNeverLeaksSecret(t *testing.T) {
	for _, tc := range []struct {
		name  string
		html  string
		echo  int
		hook  func(context.Context, *host.OutboundMessage) error
		quote bool // the raw failure text quoted the secret, so [secret] must be there
	}{
		{name: "render error", html: `<p>{{ secret.code | divided_by: 0 }}</p>`, quote: true},
		{name: "smtp 4xx", echo: 451, quote: true},
		{name: "smtp 5xx", echo: 554, quote: true},
		{
			name: "BeforeSend error",
			hook: func(_ context.Context, m *host.OutboundMessage) error {
				return errors.New("rejected: " + m.Text)
			},
			quote: true,
		},
		// Sent fine, but the missing-key warning names the key, which here
		// is the secret itself; it is logged at debug level.
		{name: "render warning", html: `<p>{{ secret.code | t }}</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSensitiveFixture(t, fixtureOptions{settings: func(s *store.TenantSettings) {
				s.Retry = store.RetryPolicy{Backoff: []time.Duration{time.Millisecond}, MaxAttempts: 2}
				// Opt in to every delivery event, so their payloads are
				// checked too.
				s.EventTypes = []string{EventDeliverySent, EventDeliveryFailed}
			}})
			if tc.echo != 0 {
				tr, err := f.st.Transports().Get(context.Background(), f.transportID)
				if err != nil {
					t.Fatal(err)
				}
				tr.Port = echoSMTP(t, tc.echo)
				if err := f.st.Transports().Update(context.Background(), tr); err != nil {
					t.Fatal(err)
				}
			}
			id := f.insertSensitive(f.version(tc.html), nil)
			f.run([]string{id}, func(c *Config) { c.Hooks.BeforeSend = tc.hook })

			d := f.get(id)
			var stored []string
			stored = append(stored, "delivery: "+d.LastError)
			for _, a := range f.attempts(id) {
				stored = append(stored, "attempt: "+a.Error)
			}
			for _, ev := range listOutbox(t, f.fixture) {
				stored = append(stored, "outbox "+ev.Type+": "+string(ev.Payload))
			}
			row, _ := json.Marshal(d)
			stored = append(stored, "row: "+string(row))
			all := strings.Join(stored, "\n")
			if strings.Contains(all, otpCode) {
				t.Fatalf("the secret was stored:\n%s", all)
			}
			if logs := f.logs.String(); strings.Contains(logs, otpCode) {
				t.Fatalf("the secret was logged:\n%s", logs)
			}
			if tc.quote && !strings.Contains(d.LastError, redactedSecret) {
				t.Fatalf("last error %q: expected the quoted secret to be redacted, so this mode did not exercise the leak", d.LastError)
			}
			if !tc.quote && !strings.Contains(f.logs.String(), redactedSecret) {
				t.Fatalf("no redacted warning was logged, so this mode did not exercise the leak:\n%s", f.logs.String())
			}
		})
	}
}

func TestSecretNeedles(t *testing.T) {
	sec := &secretVars{needles: secretNeedles(map[string]any{
		"code": "a<b>&c", "n": float64(123456), "short": "ab",
		"nested": map[string]any{"list": []any{"deep-value"}},
	})}
	in := "x a<b>&c a&lt;b&gt;&amp;c a%3Cb%3E%26c 123456 deep-value ab"
	if got, want := sec.redact(in), "x [secret] [secret] [secret] [secret] [secret] ab"; got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
	var none *secretVars
	if none.redact("abc") != "abc" || none.redactErr(nil) != nil {
		t.Fatal("a nil secretVars must pass text through")
	}
}

// A delivery without secret_vars on a version that reads secret.* must fail
// permanently before anything is sent: rendering it would mail an empty code.
// A sensitive delivery on the same version still sends.
func TestNonSensitiveDeliveryOnSecretVersionFails(t *testing.T) {
	f := newSensitiveFixture(t, fixtureOptions{chaos: chaossmtp.Options{KeepBodies: true}})
	versionID := f.version("")

	var plain []string
	for i := 0; i < 2; i++ { // the second one is served from the per-version cache
		email := fmt.Sprintf("plain%d@example.org", i)
		d := store.Delivery{
			ID: store.NewID(), TenantID: testTenant,
			VersionID: versionID, SenderID: f.senderID,
			Lane: store.LaneTransactional, Status: store.DeliveryQueued,
			Email: email, EmailNorm: email, Locale: "en",
		}
		if _, err := f.st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
		plain = append(plain, d.ID)
	}
	sensitive := f.insertSensitive(versionID, nil)
	f.run(append(append([]string{}, plain...), sensitive), nil)

	for _, id := range plain {
		d := f.get(id)
		if d.Status != store.DeliveryFailed || d.LastError != templateRequiresSecretError {
			t.Fatalf("plain delivery: status %s error %q, want failed %q", d.Status, d.LastError, templateRequiresSecretError)
		}
		if as := f.attempts(id); len(as) != 1 || as[0].ErrorClass != store.ErrorClassPermanent {
			t.Fatalf("attempts = %+v, want one permanent", as)
		}
	}
	if d := f.get(sensitive); d.Status != store.DeliverySent {
		t.Fatalf("sensitive delivery: %s (%s), want sent", d.Status, d.LastError)
	}
	msgs := f.chaos.Messages()
	if len(msgs) != 1 || !strings.Contains(decodeBody(t, string(msgs[0].Body)), otpCode) {
		t.Fatalf("relay saw %d messages, want only the sensitive one", len(msgs))
	}
}

func TestVersionReadsSecretIsCached(t *testing.T) {
	ts := newTenantState("t", nil, 0)
	v := &store.MessageVersion{ID: "v1", HTMLTpl: "{{ secret.code }}"}
	now := time.Now()
	if !ts.versionReadsSecret(v, now) {
		t.Fatal("a version reading secret.code reports false")
	}
	// Versions are immutable: the cached answer wins over a changed body.
	v.HTMLTpl = "plain"
	if !ts.versionReadsSecret(v, now) {
		t.Fatal("the answer was not cached per version ID")
	}
	if ts.versionReadsSecret(&store.MessageVersion{ID: "v2", HTMLTpl: "plain"}, now) {
		t.Fatal("a version without secret.* reports true")
	}
}

// Somebody who can write to the database but holds no key rewrites the
// recipient of a pending sensitive delivery. The RCPT TO and the To header
// come from the email column, so the secret is bound to that exact string:
// the rewrite must make the secret undecryptable and nothing may reach the
// relay, whether or not email_norm is rewritten along with it.
func TestSensitiveDeliveryRecipientRewriteRefused(t *testing.T) {
	const attacker = "attacker@example.net"
	for _, tc := range []struct {
		name   string
		mutate func(*store.Delivery)
	}{
		{name: "email only", mutate: func(d *store.Delivery) { d.Email = attacker }},
		{name: "email and email_norm", mutate: func(d *store.Delivery) { d.Email, d.EmailNorm = attacker, attacker }},
		// Same address after normalization, different bytes on the wire.
		{name: "email case only", mutate: func(d *store.Delivery) { d.Email = strings.ToUpper(d.Email) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSensitiveFixture(t, fixtureOptions{chaos: chaossmtp.Options{KeepBodies: true}})
			id := f.insertSensitive(f.version(""), tc.mutate)
			f.run([]string{id}, nil)

			d := f.get(id)
			if d.Status != store.DeliveryFailed || d.LastError != secretDecryptFailedError {
				t.Fatalf("delivery = %s %q, want failed %q", d.Status, d.LastError, secretDecryptFailedError)
			}
			if d.SecretCiphertext != nil || d.SecretState == store.SecretPending {
				t.Fatalf("secret state %s, ciphertext kept %v", d.SecretState, d.SecretCiphertext != nil)
			}
			if msgs := f.chaos.Messages(); len(msgs) != 0 {
				t.Fatalf("relay saw %d messages, the first to %v", len(msgs), msgs[0].Rcpts)
			}
		})
	}
}

// A relay reply that quotes the message is also what the transport circuit
// records: a rate_limited (451 4.7.0) or auth (535) reply that flips the
// transport's status is persisted as its StatusReason and logged. The reply
// must be scrubbed before the circuit sees it, not only once the delivery's
// own result is settled.
func TestSensitiveReplyNeverReachesTransportStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want store.TransportStatus
	}{
		{name: "rate limited 451", code: 451, want: store.TransportCooldown},
		{name: "auth 535", code: 535, want: store.TransportUnhealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSensitiveFixture(t, fixtureOptions{})
			ctx := context.Background()
			tr, err := f.st.Transports().Get(ctx, f.transportID)
			if err != nil {
				t.Fatal(err)
			}
			tr.Port = echoSMTP(t, tc.code)
			if err := f.st.Transports().Update(ctx, tr); err != nil {
				t.Fatal(err)
			}
			versionID := f.version("")
			// Enough sensitive deliveries for the circuit's threshold (three
			// rate_limited replies in the window, or three auth failures).
			for i := 0; i < 4; i++ {
				f.insertSensitive(versionID, nil)
			}
			stop := f.runSenders(1, func(c *Config) {
				c.Lanes = map[store.Lane]int{store.LaneTransactional: 2}
				c.Secrets = f.cipher
				c.Logger = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			var got *store.Transport
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if got, err = f.st.Transports().Get(ctx, f.transportID); err != nil {
					t.Fatal(err)
				}
				if got.Status == tc.want {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			stop()
			if got.Status != tc.want {
				t.Fatalf("transport status = %s, want %s: the circuit never flipped", got.Status, tc.want)
			}
			if strings.Contains(got.StatusReason, otpCode) {
				t.Fatalf("the transport status reason carries the secret: %q", got.StatusReason)
			}
			if !strings.Contains(got.StatusReason, redactedSecret) {
				t.Fatalf("status reason %q: expected the quoted reply, redacted, so this did not exercise the leak", got.StatusReason)
			}
			if logs := f.logs.String(); strings.Contains(logs, otpCode) {
				t.Fatalf("the secret was logged:\n%s", logs)
			}
		})
	}
}

// jumpClock is real time plus an offset a test moves forward, so a sender
// can be made to live through ten minutes without waiting for them, while
// everything that reads real time (the store, the relay) keeps working.
type jumpClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *jumpClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *jumpClock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off += d
}

// sleep stands in for the limiter's sleep. Like sleepCtx it ends early at the
// context's deadline, with the deadline's error; it jumps the clock by the
// time it would have waited instead of waiting.
func (c *jumpClock) sleep(ctx context.Context, d time.Duration) error {
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < d {
			c.jump(max(rem, 0))
			return context.DeadlineExceeded
		}
	}
	c.jump(d)
	return ctx.Err()
}

// A secret that expires while its delivery is on the way out must not go
// out. openSecret checks the expiry once, at the start; the rate limiter, a
// BeforeSend hook and the connect can each take long enough to cross it.
func TestSensitiveDeliveryExpiringInFlightIsNotSent(t *testing.T) {
	const ttl = 10 * time.Minute
	for _, tc := range []struct {
		name string
		// rate is the transport's rate; the bucket is drained first, so the
		// delivery waits 1/rate seconds for its token.
		rate float64
		// hook and dial jump the clock past the expiry from inside the
		// send path.
		hook, dial bool
		wantSent   bool
	}{
		{name: "limiter wait past expiry", rate: 0.001},                   // waits 1000s, TTL 600s
		{name: "limiter wait inside the TTL", rate: 0.01, wantSent: true}, // waits 100s
		{name: "BeforeSend crosses the expiry", hook: true},
		{name: "connect crosses the expiry", dial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSensitiveFixture(t, fixtureOptions{})
			ctx := context.Background()
			tr, err := f.st.Transports().Get(ctx, f.transportID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rate > 0 {
				tr.RatePerSecond = tc.rate
				if err := f.st.Transports().Update(ctx, tr); err != nil {
					t.Fatal(err)
				}
			}
			clock := &jumpClock{}
			id := f.insertSensitive(f.version(""), func(d *store.Delivery) { d.SecretExpiresAt = time.Now().Add(ttl) })
			f.runWith([]string{id}, func(c *Config) {
				c.Clock = clock.now
				if tc.hook {
					c.Hooks.BeforeSend = func(context.Context, *host.OutboundMessage) error {
						clock.jump(ttl + time.Minute)
						return nil
					}
				}
				if tc.dial {
					var nd net.Dialer
					c.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
						clock.jump(ttl + time.Minute)
						return nd.DialContext(ctx, network, addr)
					}
				}
			}, func(s *Sender) {
				s.limiter.Sleep = clock.sleep
				if tc.rate > 0 {
					// Take the one token a fresh bucket starts with.
					if _, err := s.limiter.Wait(ctx, Key{Name: sharedTransportKey(testTenant, tr), Rate: tc.rate}); err != nil {
						t.Fatal(err)
					}
				}
			})

			d := f.get(id)
			msgs := f.chaos.Messages()
			if tc.wantSent {
				if d.Status != store.DeliverySent || len(msgs) != 1 {
					t.Fatalf("delivery = %s %q, relay saw %d; want sent", d.Status, d.LastError, len(msgs))
				}
				return
			}
			if len(msgs) != 0 {
				t.Fatalf("relay saw %d messages after the secret expired", len(msgs))
			}
			if d.Status != store.DeliveryFailed || d.LastErrorClass != store.ErrorClassPermanent || d.LastError != store.SecretExpiredError {
				t.Fatalf("delivery = %s %s %q, want failed permanent %q", d.Status, d.LastErrorClass, d.LastError, store.SecretExpiredError)
			}
			if d.SecretState == store.SecretPending || d.SecretCiphertext != nil {
				t.Fatalf("secret state %s, ciphertext kept %v; want it wiped", d.SecretState, d.SecretCiphertext != nil)
			}
		})
	}
}

// An ordinary delivery on a version that reads a property called secret of
// another object (`vars.secret.code`) is not a secret template and is sent.
func TestOrdinaryDeliveryOnVarsSecretSends(t *testing.T) {
	f := newSensitiveFixture(t, fixtureOptions{chaos: chaossmtp.Options{KeepBodies: true}})
	v := &store.MessageVersion{
		SubjectTpl: "Hello", HTMLTpl: `<p>{{ vars.secret.code }}</p>`, TextTpl: `{{ vars.secret.code }}`,
		DefaultLocale: "en",
	}
	if err := f.st.Versions().Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	d := store.Delivery{
		ID: store.NewID(), TenantID: testTenant,
		VersionID: v.ID, SenderID: f.senderID,
		Lane: store.LaneTransactional, Status: store.DeliveryQueued,
		Email: "plain@example.org", EmailNorm: "plain@example.org", Locale: "en",
		Vars: map[string]any{"secret": map[string]any{"code": "ordinary-value"}},
	}
	if _, err := f.st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
		t.Fatal(err)
	}
	f.run([]string{d.ID}, nil)
	if got := f.get(d.ID); got.Status != store.DeliverySent {
		t.Fatalf("delivery = %s %q, want sent", got.Status, got.LastError)
	}
	msgs := f.chaos.Messages()
	if len(msgs) != 1 || !strings.Contains(decodeBody(t, string(msgs[0].Body)), "ordinary-value") {
		t.Fatalf("relay saw %d messages, want the rendered one", len(msgs))
	}
}
