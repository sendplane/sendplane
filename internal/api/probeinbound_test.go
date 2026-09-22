package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/probe"
	"github.com/sendplane/sendplane/internal/probe/inbound"
	"github.com/sendplane/sendplane/internal/probe/inbound/sendplanehook"
	"github.com/sendplane/sendplane/store"
)

// The inbound webhook of ADR-0016, end to end over the real `sendplane`
// provider: the signature, the payload shape and the reply codes are the ones
// a real delivery would carry. Only the probe runner is a stub, because a real
// one would want a DNS resolver.
//
// The signatures are made with the *real* clock, not the harness's fixed one:
// the freshness rule is checked against wall time by design, and sigv1's own
// tests pin the timestamp arithmetic.

const inboundSecret = "whsec_api-test"

// inboundPath is the default route for the provider sendplane ships.
const inboundPath = "/probe/inbound/sendplane"

// signed is the request options for one delivery: the sigv1 header over the
// exact bytes being posted, and no bearer token, because this route is public.
func signed(body string) []reqOpt {
	return []reqOpt{
		noAuth(),
		withHeader("Content-Type", "application/json"),
		withHeader(sendplanehook.HeaderSignature,
			sendplanehook.Sign(inboundSecret, time.Now(), []byte(body))),
	}
}

// stubCompleter stands in for internal/probe.Runner. It signs tokens the same
// way, so the handler's verify step is exercised for real.
type stubCompleter struct {
	key       string
	completed []string
	refreshed []string
	fail      error
}

func (c *stubCompleter) token(tenantID, runID string) string {
	return tenantID + "/" + runID + "/" + c.key
}

func (c *stubCompleter) VerifyToken(tenantID, runID, token string) bool {
	return token == c.token(tenantID, runID)
}

func (c *stubCompleter) CompleteRun(
	ctx context.Context, st store.Store, run *store.ProbeRun, ev probe.Evidence, now time.Time,
) error {
	if c.fail != nil {
		return c.fail
	}
	obs := probe.Observe(ev, now)
	status, reason := probe.Verdict(obs)
	run.Pending = false
	run.Delivered = true
	run.Status, run.Reason = status, reason
	run.Folder = obs.Folder
	run.SPF, run.DKIM, run.DMARC = obs.SPF, obs.DKIM, obs.DMARC
	run.RawHeaders = ev.RawHeaders
	run.ReceivedAt = ev.ReceivedAt
	c.completed = append(c.completed, run.ID)
	return st.ProbeRuns().Update(ctx, run)
}

func (c *stubCompleter) RefreshSenderHealth(
	_ context.Context, _ store.Store, senderID string, _ time.Time,
) error {
	c.refreshed = append(c.refreshed, senderID)
	return nil
}

// withInbound configures the default route the way the root wiring does.
func withInbound(c *stubCompleter) func(*Deps) {
	return func(d *Deps) {
		p, ok := inbound.Lookup("sendplane")
		if !ok {
			panic("the sendplane inbound provider is not registered")
		}
		d.ProbeInbound = []ProbeInboundRoute{{
			Path: inbound.DefaultPath("sendplane"), Provider: p,
			Secrets: []string{inboundSecret},
		}}
		d.ProbeCompleter = c
	}
}

// seedWebhookProbe creates a webhook-kind probe mailbox, a sender and a
// pending run for them, and returns the run.
func (e *env) seedWebhookProbe(t *testing.T) (*store.ProbeMailbox, *store.ProbeRun) {
	t.Helper()
	ctx := context.Background()
	box := decodeInto[ProbeMailbox](t, e.do(http.MethodPost, "/api/v1/probe-mailboxes", ProbeMailboxInput{
		Name: "forwarder", Kind: ptr(ProbeMailboxKindWebhook),
		Address: "probe@example.net", AuthservId: ptr("mx.example.net"),
	}), http.StatusCreated)

	snd := e.seedSender()
	run := &store.ProbeRun{
		ID: store.NewID(), SenderID: snd.Id, MailboxID: box.Id,
		GroupID: store.NewID(), Pending: true, Status: store.HealthUnknown,
		StartedAt: e.now, CreatedAt: e.now,
	}
	if err := e.st.ProbeRuns().Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	stored, err := e.st.ProbeMailboxes().Get(ctx, box.Id)
	if err != nil {
		t.Fatalf("get mailbox: %v", err)
	}
	return stored, run
}

// payloadFor is what a forwarder posts for a probe mail that passed everything
// the verdict of architecture 11.4 reads.
func payloadFor(token string, headers map[string][]string) string {
	h := map[string][]string{
		"Authentication-Results": {
			"mx.example.net; spf=pass smtp.mailfrom=bounce.example.com; " +
				"dkim=pass header.d=example.com header.s=sp1; dmarc=pass (p=REJECT)",
		},
		"Received": {
			"from mx.example.net by inbox.example.net; Sat, 1 Mar 2025 12:00:30 +0000",
			"from mail.example.com (mail.example.com [203.0.113.7]) " +
				"by mx.example.net with ESMTPS; Sat, 1 Mar 2025 12:00:10 +0000",
		},
		"Subject":    {"[sendplane probe run]"},
		"Message-Id": {"<probe@mx.example.net>"},
		"Date":       {"Sat, 1 Mar 2025 12:00:30 +0000"},
	}
	if token != "" {
		h["X-Sendplane-Probe"] = []string{token}
	}
	for k, v := range headers {
		h[k] = v
	}
	body, err := json.Marshal(map[string]any{
		"from":    "news@example.com",
		"to":      []string{"probe@example.net"},
		"headers": h,
		"text":    "sendplane loopback health probe.\r\n",
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestProbeInboundCompletesAPendingRun(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	box, run := e.seedWebhookProbe(t)

	body := payloadFor(c.token(e.tenantID, run.ID), nil)
	w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "accepted" {
		t.Errorf("outcome = %q, want accepted", got)
	}

	got, err := e.st.ProbeRuns().Get(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Pending || !got.Delivered {
		t.Fatalf("run is pending=%v delivered=%v, want a finished delivered run", got.Pending, got.Delivered)
	}
	if got.Status != store.HealthGreen {
		t.Errorf("status = %s (%s), want green: spf/dkim/dmarc all pass over TLS",
			got.Status, got.Reason)
	}
	if got.Folder != probe.FolderUnknown {
		t.Errorf("folder = %q, want %q: a webhook cannot see the folder and that must not "+
			"downgrade the verdict", got.Folder, probe.FolderUnknown)
	}
	if got.SPF != "pass" || got.DKIM != "pass" || got.DMARC != "pass" {
		t.Errorf("auth results = %q/%q/%q", got.SPF, got.DKIM, got.DMARC)
	}
	if !strings.Contains(got.RawHeaders, "Authentication-Results") {
		t.Errorf("raw headers were not kept: %q", got.RawHeaders)
	}
	if len(c.refreshed) != 1 || c.refreshed[0] != run.SenderID {
		t.Errorf("sender health refreshed %v, want [%s]", c.refreshed, run.SenderID)
	}

	// The mailbox has no login, so an arriving probe is the only thing that
	// can report it healthy (architecture 11.5).
	after, err := e.st.ProbeMailboxes().Get(context.Background(), box.ID)
	if err != nil {
		t.Fatalf("get mailbox: %v", err)
	}
	if after.Health.Status != store.MailboxOK {
		t.Errorf("mailbox health = %s, want ok", after.Health.Status)
	}
}

func TestProbeInboundIsIdempotent(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)
	body := payloadFor(c.token(e.tenantID, run.ID), nil)

	for i := range 2 {
		w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d; body: %s", i+1, w.Code, w.Body.String())
		}
	}
	if len(c.completed) != 1 {
		t.Errorf("CompleteRun ran %d times for the same body, want 1", len(c.completed))
	}

	// The dedupe memo is keyed on the body, so a redelivery that is *not*
	// byte-identical — a forwarder that reordered the JSON, say — misses it
	// and has to be stopped by ProbeRun.Pending instead. That is the real
	// contract; the memo is only the shortcut in front of it.
	other := payloadFor(c.token(e.tenantID, run.ID), map[string][]string{
		"X-Forwarder-Note": {"same mail, different bytes"},
	})
	w := e.do(http.MethodPost, inboundPath, []byte(other), signed(other)...)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "accepted" {
		t.Fatalf("status = %d body = %q, want 200 accepted", w.Code, w.Body.String())
	}
	if len(c.completed) != 1 {
		t.Errorf("a redelivery completed the run again: %v", c.completed)
	}
}

func TestProbeInboundRejectsABadSignature(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)
	body := payloadFor(c.token(e.tenantID, run.ID), nil)

	w := e.do(http.MethodPost, inboundPath, []byte(body),
		noAuth(),
		withHeader(sendplanehook.HeaderSignature,
			sendplanehook.Sign("not-the-secret", time.Now(), []byte(body))))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", w.Code, w.Body.String())
	}
	if len(c.completed) != 0 {
		t.Error("an unverified request reached the runner")
	}
	got, _ := e.st.ProbeRuns().Get(context.Background(), run.ID)
	if !got.Pending {
		t.Error("an unverified request finished the run")
	}
}

func TestProbeInboundIgnoresMailThatIsNotAProbe(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	e.seedWebhookProbe(t)

	body := payloadFor("", nil) // no X-Sendplane-Probe header
	w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
	// 200, not 404: a non-2xx makes a sender defer and retry a message
	// sendplane will never want.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "ignored" {
		t.Errorf("outcome = %q, want ignored", got)
	}
	if len(c.completed) != 0 {
		t.Error("a mail with no probe token completed a run")
	}
}

func TestProbeInboundIgnoresAForgedToken(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)

	// The right tenant and run, a MAC somebody made up.
	body := payloadFor(e.tenantID+"/"+run.ID+"/deadbeef", nil)
	w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ignored" {
		t.Fatalf("status = %d body = %q, want 200 ignored", w.Code, w.Body.String())
	}
	got, _ := e.st.ProbeRuns().Get(context.Background(), run.ID)
	if !got.Pending {
		t.Error("a forged probe token completed the run")
	}
}

func TestProbeInboundDefersWhenTheRunCannotBeWritten(t *testing.T) {
	c := &stubCompleter{key: "mac", fail: fmt.Errorf("store is down")}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)

	body := payloadFor(c.token(e.tenantID, run.ID), nil)
	w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the sender holds the mail", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "deferred" {
		t.Errorf("outcome = %q, want deferred", got)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a deferral carries no Retry-After")
	}
}

// A deferral must not be remembered: the sender redelivers the same bytes, and
// a memo written before the run was actually completed would turn that retry
// into a no-op and lose the probe for good.
func TestProbeInboundRetriesAfterADeferral(t *testing.T) {
	c := &stubCompleter{key: "mac", fail: fmt.Errorf("store is down")}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)
	body := payloadFor(c.token(e.tenantID, run.ID), nil)

	post := func() *httptest.ResponseRecorder {
		return e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
	}
	if w := post(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first delivery: status = %d, want 503", w.Code)
	}

	// The store comes back; the sender redelivers the same bytes.
	c.fail = nil
	if w := post(); w.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	got, _ := e.st.ProbeRuns().Get(context.Background(), run.ID)
	if got.Pending {
		t.Fatal("the retry after a deferral was swallowed by the idempotency memo")
	}
}

func TestProbeInboundUnknownProviderIs404(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	w := e.do(http.MethodPost, "/probe/inbound/postmark", []byte(`{}`), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestProbeInboundIsNotMountedWithoutConfiguration(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodPost, inboundPath, []byte(`{}`), noAuth())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no inbound webhook is configured", w.Code)
	}
}

// A body the format does not describe is a 400 and not a deferral: the
// signature verified, so this really is our sender, and retrying the same
// bytes cannot make them parse.
func TestProbeInboundRejectsAMalformedPayload(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	e.seedWebhookProbe(t)

	for name, body := range map[string]string{
		"not json": `{"from":`,
		"no from":  `{"to":["probe@example.net"],"headers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := e.do(http.MethodPost, inboundPath, []byte(body), signed(body)...)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}
	if len(c.completed) != 0 {
		t.Error("a malformed payload reached the runner")
	}
}

// A signature that was valid an hour ago is a replay, and the timestamp inside
// the signed string is what makes that detectable with no stored state.
func TestProbeInboundRejectsAnExpiredSignature(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, withInbound(c))
	_, run := e.seedWebhookProbe(t)
	body := payloadFor(c.token(e.tenantID, run.ID), nil)

	w := e.do(http.MethodPost, inboundPath, []byte(body),
		noAuth(),
		withHeader(sendplanehook.HeaderSignature,
			sendplanehook.Sign(inboundSecret, time.Now().Add(-time.Hour), []byte(body))))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", w.Code, w.Body.String())
	}
	got, _ := e.st.ProbeRuns().Get(context.Background(), run.ID)
	if !got.Pending {
		t.Error("a replayed request finished the run")
	}
}

// Rotation: while both the outgoing and the incoming secret are configured, a
// delivery signed with either is accepted.
func TestProbeInboundAcceptsARotatedSecret(t *testing.T) {
	c := &stubCompleter{key: "mac"}
	e := newEnv(t, func(d *Deps) {
		withInbound(c)(d)
		d.ProbeInbound[0].Secrets = []string{"whsec_new", inboundSecret}
	})
	_, run := e.seedWebhookProbe(t)
	body := payloadFor(c.token(e.tenantID, run.ID), nil)

	w := e.do(http.MethodPost, inboundPath, []byte(body),
		noAuth(),
		withHeader(sendplanehook.HeaderSignature,
			sendplanehook.Sign(inboundSecret, time.Now(), []byte(body))))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "accepted" {
		t.Fatalf("status = %d body = %q, want 200 accepted", w.Code, w.Body.String())
	}
	if len(c.completed) != 1 {
		t.Errorf("CompleteRun ran %d times, want 1", len(c.completed))
	}
}

// --- probe mailbox kind ------------------------------------------------

func TestWebhookProbeMailboxRejectsIMAPFields(t *testing.T) {
	e := newEnv(t)
	decodeError(t, e.do(http.MethodPost, "/api/v1/probe-mailboxes", ProbeMailboxInput{
		Name: "forwarder", Kind: ptr(ProbeMailboxKindWebhook),
		Address: "probe@example.net",
		Host:    ptr("imap.example.com"), Port: ptr(int32(993)),
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func TestIMAPProbeMailboxStillNeedsHostAndPort(t *testing.T) {
	e := newEnv(t)
	decodeError(t, e.do(http.MethodPost, "/api/v1/probe-mailboxes", ProbeMailboxInput{
		Name: "gmail", Address: "probe@example.com",
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func TestProbeMailboxKindDefaultsToIMAP(t *testing.T) {
	e := newEnv(t)
	got := decodeInto[ProbeMailbox](t, e.do(http.MethodPost, "/api/v1/probe-mailboxes", ProbeMailboxInput{
		Name: "gmail", Address: "probe@example.com",
		Host: ptr("imap.example.com"), Port: ptr(int32(993)),
	}), http.StatusCreated)
	if got.Kind == nil || *got.Kind != ProbeMailboxKindImap {
		t.Fatalf("kind = %v, want imap", got.Kind)
	}
}

func TestTestWebhookProbeMailbox(t *testing.T) {
	t.Run("no inbound webhook configured", func(t *testing.T) {
		e := newEnv(t)
		box, _ := e.seedWebhookProbe(t)
		got := decodeInto[MailboxTestResult](t,
			e.do(http.MethodPost, "/api/v1/probe-mailboxes/"+box.ID+"/test", nil), http.StatusOK)
		if got.Ok || got.Stage != MailboxStageConfig {
			t.Fatalf("result = %+v, want ok=false stage=config", got)
		}
		if got.Error == nil || !strings.Contains(*got.Error, "not by login") {
			t.Errorf("error = %v", got.Error)
		}
	})

	t.Run("inbound webhook configured", func(t *testing.T) {
		e := newEnv(t, withInbound(&stubCompleter{key: "mac"}))
		box, _ := e.seedWebhookProbe(t)
		got := decodeInto[MailboxTestResult](t,
			e.do(http.MethodPost, "/api/v1/probe-mailboxes/"+box.ID+"/test", nil), http.StatusOK)
		if !got.Ok || got.Stage != MailboxStageOk {
			t.Fatalf("result = %+v, want ok=true stage=ok", got)
		}
		if got.Server == nil || *got.Server != "webhook:sendplane" {
			t.Errorf("server = %v, want webhook:sendplane", got.Server)
		}
	})
}
