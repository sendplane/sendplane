package probe

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/dnscheck"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

var baseTime = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

// testDKIMPublic is a valid DKIM p= value, so the DNS layer's DKIM check has
// something real to parse.
var testDKIMPublic = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(der)
})

// fixtureRunID is the placeholder run ID the testdata files carry; mailFor
// swaps in the real one so that the fixtures stay valid samples on their own.
var fixtureRunID = regexp.MustCompile(`0199aaaa-bbbb-7ccc-8ddd-[0-9a-f]{12}`)

var probeHeaderLine = regexp.MustCompile(`(?m)^X-Sendplane-Probe: .*$`)

// testTenantID is the tenant every env runs in. The probe token carries it
// (Runner.Token), so the fixtures have to be stamped with the same one.
const testTenantID = "tenant-1"

type env struct {
	st     store.Store
	runner *Runner
	sender *store.Sender
	boxes  []*store.ProbeMailbox
	now    time.Time
}

func newEnv(t *testing.T, opts Options, mailboxes ...string) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{now: baseTime}

	p := memstore.New(memstore.WithClock(func() time.Time { return e.now }))
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(ctx, testTenantID)
	if err != nil {
		t.Fatal(err)
	}
	e.st = st

	tr := &store.Transport{ID: "tr-1", Name: "relay", Host: "smtp.example.com", Port: 587}
	if err := st.Transports().Create(ctx, tr); err != nil {
		t.Fatal(err)
	}
	dom := &store.SendingDomain{
		ID: "dom-1", Domain: "mail.example.com",
		DKIMSelector: "sp1", ReturnPathDomain: "bounce.example.com",
	}
	if err := st.Domains().Create(ctx, dom); err != nil {
		t.Fatal(err)
	}
	snd := &store.Sender{
		ID: "snd-1", Name: "probe sender",
		FromEmail: "noreply@mail.example.com", TransportID: tr.ID, DomainID: dom.ID,
	}
	if err := st.Senders().Create(ctx, snd); err != nil {
		t.Fatal(err)
	}
	e.sender = snd

	for i, name := range mailboxes {
		box := &store.ProbeMailbox{
			ID:          name,
			Name:        name,
			Address:     name + "@probe.example",
			Host:        "imap." + name + ".example",
			Port:        993,
			InboxFolder: "INBOX",
			SpamFolder:  "[Gmail]/Spam",
			AuthServID:  "mx.google.com",
			Enabled:     true,
		}
		if i == 1 {
			box.AuthServID = "mail.example.net"
		}
		if err := st.ProbeMailboxes().Create(ctx, box); err != nil {
			t.Fatal(err)
		}
		e.boxes = append(e.boxes, box)
	}

	if opts.Clock == nil {
		opts.Clock = func() time.Time { return e.now }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	e.runner = New(opts)
	return e
}

func (e *env) runs(t *testing.T) []store.ProbeRun {
	t.Helper()
	got, err := e.runner.runsOf(context.Background(), e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env) deliveries(t *testing.T) []store.Delivery {
	t.Helper()
	res, err := e.st.Deliveries().ListByCampaign(context.Background(), "", store.DeliveryFilter{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Items
}

func (e *env) events(t *testing.T) []store.OutboxEvent {
	t.Helper()
	res, err := e.st.Outbox().List(context.Background(), store.OutboxPending, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Items
}

// mailFor builds a probe mail for runID out of a testdata fixture.
func mailFor(t *testing.T, r *Runner, file, runID, folder string) RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	s := fixtureRunID.ReplaceAllString(string(raw), runID)
	s = probeHeaderLine.ReplaceAllString(s, "X-Sendplane-Probe: "+r.Token(testTenantID, runID))
	// The mailbox holds the mail 30s after the fixture's own Date, whatever
	// that date is, so latency is the same assertion for every fixture.
	sent, err := parseDate(ParseHeaders([]byte(s)).Get("Date"))
	if err != nil {
		t.Fatal(err)
	}
	return RawMessage{
		ID: "uid-" + runID, Folder: folder, Raw: []byte(s),
		ReceivedAt: sent.Add(30 * time.Second),
	}
}

// fakeFetcher answers a header search from a fixed set of messages.
type fakeFetcher struct {
	mu      sync.Mutex
	msgs    []RawMessage
	deleted []string
	// searches records every (name, value) pair asked for.
	searches [][2]string
}

func (f *fakeFetcher) FetchByHeader(_ context.Context, name, value string) ([]RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, [2]string{name, value})
	var out []RawMessage
	for _, m := range f.msgs {
		h := ParseHeaders(m.Raw)
		if got := h.Get(name); got != "" && got == value {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeFetcher) Delete(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ids...)
	var kept []RawMessage
	for _, m := range f.msgs {
		if !contains(ids, m.ID) {
			kept = append(kept, m)
		}
	}
	f.msgs = kept
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// perMailbox hands each mailbox its own fetcher.
type perMailbox map[string]*fakeFetcher

func (p perMailbox) Open(_ context.Context, m *store.ProbeMailbox) (MailboxFetcher, error) {
	f, ok := p[m.ID]
	if !ok {
		return &fakeFetcher{}, nil
	}
	return f, nil
}

func TestTriggerCreatesRunAndDelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{}, "gmail")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}

	runs := e.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.ID != runID {
		t.Errorf("Trigger returned %q, run is %q", runID, run.ID)
	}
	if run.Status != store.HealthUnknown || !run.ReceivedAt.IsZero() {
		t.Errorf("run is not pending: %+v", run)
	}
	if run.MailboxID != "gmail" || run.SenderID != e.sender.ID {
		t.Errorf("run = %+v", run)
	}

	ds := e.deliveries(t)
	if len(ds) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(ds))
	}
	d := ds[0]
	if d.Lane != store.LaneProbe {
		t.Errorf("lane = %s, want probe", d.Lane)
	}
	if d.Status != store.DeliveryQueued {
		t.Errorf("status = %s, want queued", d.Status)
	}
	if d.Email != "gmail@probe.example" {
		t.Errorf("email = %q", d.Email)
	}
	if d.ID != run.DeliveryID {
		t.Errorf("run.DeliveryID = %q, delivery is %q", run.DeliveryID, d.ID)
	}
	if got := d.Vars["run_id"]; got != runID {
		t.Errorf("vars.run_id = %v, want %q", got, runID)
	}
	if d.CampaignID != "" {
		t.Errorf("probe delivery must not belong to a campaign: %q", d.CampaignID)
	}

	v, err := e.st.Versions().Get(ctx, d.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if v.TemplateID != ProbeTemplateID || v.SubjectTpl != probeSubjectTpl {
		t.Errorf("version = %+v", v)
	}

	// A second trigger reuses the built-in version instead of piling up rows.
	if _, err := e.runner.Trigger(ctx, e.st, e.sender.ID); err != nil {
		t.Fatal(err)
	}
	res, err := e.st.Versions().ListByTemplate(ctx, ProbeTemplateID, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("probe versions = %d, want 1", len(res.Items))
	}
}

func TestCollectVerdicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		file   string
		folder string
		want   store.HealthStatus
	}{
		{"pass", "gmail_pass.eml", "INBOX", store.HealthGreen},
		{"dkim fail", "gmail_spam_dkimfail.eml", "[Gmail]/Spam", store.HealthRed},
		{"delivered to spam", "gmail_pass.eml", "[Gmail]/Spam", store.HealthYellow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			e := newEnv(t, Options{}, "gmail")

			runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeFetcher{msgs: []RawMessage{mailFor(t, e.runner, tc.file, runID, tc.folder)}}

			e.now = baseTime.Add(time.Minute)
			if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
				t.Fatal(err)
			}

			run, err := e.st.ProbeRuns().Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", run.Status, run.Reason, tc.want)
			}
			if !run.Delivered {
				t.Error("run is not marked delivered")
			}
			if run.ObservedIP != "203.0.113.10" {
				t.Errorf("observed ip = %q", run.ObservedIP)
			}
			if !run.TLS {
				t.Error("TLS not detected")
			}
			if run.Latency != 30*time.Second {
				t.Errorf("latency = %s, want 30s", run.Latency)
			}
			if run.RawHeaders == "" {
				t.Error("raw headers were not kept for diagnosis")
			}

			snd, err := e.st.Senders().Get(ctx, e.sender.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snd.Health != tc.want {
				t.Errorf("sender health = %s, want %s", snd.Health, tc.want)
			}
			if snd.HealthCheckedAt.IsZero() {
				t.Error("HealthCheckedAt was not set")
			}

			// The probe mail is removed once its verdict is stored.
			if len(f.deleted) != 1 || f.deleted[0] != "uid-"+runID {
				t.Errorf("deleted = %v", f.deleted)
			}
		})
	}
}

func TestCollectSearchesHeaderThenSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{HMACKey: []byte("k")}, "gmail")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The sender does not set X-Sendplane-Probe yet, so strip it: the subject
	// fallback is the path that actually runs today.
	msg := mailFor(t, e.runner, "gmail_pass.eml", runID, "INBOX")
	msg.Raw = []byte(probeHeaderLine.ReplaceAllString(string(msg.Raw), "X-Sendplane-Unused: x"))
	f := &fakeFetcher{msgs: []RawMessage{msg}}

	e.now = baseTime.Add(time.Minute)
	if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
		t.Fatal(err)
	}
	if len(f.searches) != 2 {
		t.Fatalf("searches = %v, want the header then the subject", f.searches)
	}
	if f.searches[0][0] != HeaderProbe || f.searches[1][0] != "Subject" {
		t.Fatalf("searches = %v", f.searches)
	}
	if f.searches[1][1] != Subject(runID) {
		t.Fatalf("subject search = %q, want %q", f.searches[1][1], Subject(runID))
	}
	run, err := e.st.ProbeRuns().Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.HealthGreen {
		t.Fatalf("status = %s (%s)", run.Status, run.Reason)
	}
}

func TestCollectTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Timeout: 15 * time.Minute}, "gmail")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{}

	// Inside the window nothing is decided: the mail may still be in flight.
	e.now = baseTime.Add(10 * time.Minute)
	if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.ProbeRuns().Get(ctx, runID)
	if run.Status != store.HealthUnknown {
		t.Fatalf("status = %s before the timeout, want still pending", run.Status)
	}

	// Past it, the first failure is yellow: greylisting produces exactly one
	// (ADR-0012), and ConsecutiveFailuresForRed defaults to two.
	e.now = baseTime.Add(16 * time.Minute)
	if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
		t.Fatal(err)
	}
	run, _ = e.st.ProbeRuns().Get(ctx, runID)
	if run.Status != store.HealthYellow {
		t.Fatalf("status = %s (%s), want yellow on the first miss", run.Status, run.Reason)
	}
	if run.Delivered {
		t.Error("run is marked delivered")
	}

	// A second miss in a row is red.
	e.now = baseTime.Add(20 * time.Minute)
	runID2, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.now = baseTime.Add(40 * time.Minute)
	if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
		t.Fatal(err)
	}
	run2, _ := e.st.ProbeRuns().Get(ctx, runID2)
	if run2.Status != store.HealthRed {
		t.Fatalf("status = %s (%s), want red on the second miss", run2.Status, run2.Reason)
	}
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.Health != store.HealthRed {
		t.Fatalf("sender health = %s, want red", snd.Health)
	}
}

func TestCollectWorstOfAcrossMailboxes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{}, "gmail", "postfix")

	if _, err := e.runner.Trigger(ctx, e.st, e.sender.ID); err != nil {
		t.Fatal(err)
	}
	runs := e.runs(t)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want one per mailbox", len(runs))
	}
	if !runs[0].StartedAt.Equal(runs[1].StartedAt) {
		t.Error("runs of one trigger must share StartedAt: that is what groups them")
	}

	boxes := perMailbox{}
	for _, run := range runs {
		switch run.MailboxID {
		case "gmail":
			boxes["gmail"] = &fakeFetcher{msgs: []RawMessage{
				mailFor(t, e.runner, "gmail_pass.eml", run.ID, "INBOX"),
			}}
		case "postfix":
			// No DMARC record and no TLS on this hop: yellow.
			boxes["postfix"] = &fakeFetcher{msgs: []RawMessage{
				mailFor(t, e.runner, "postfix_opendmarc.eml", run.ID, "INBOX"),
			}}
		}
	}

	e.now = baseTime.Add(time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, boxes, e.now); err != nil {
		t.Fatal(err)
	}

	byMailbox := map[string]store.HealthStatus{}
	for _, run := range e.runs(t) {
		byMailbox[run.MailboxID] = run.Status
	}
	if byMailbox["gmail"] != store.HealthGreen {
		t.Errorf("gmail = %s, want green", byMailbox["gmail"])
	}
	if byMailbox["postfix"] != store.HealthYellow {
		t.Errorf("postfix = %s, want yellow", byMailbox["postfix"])
	}

	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.Health != store.HealthYellow {
		t.Fatalf("sender health = %s, want the worst of the two", snd.Health)
	}
}

// recordingResolver answers nothing and remembers what it was asked. The
// point of the test it serves is that the DNS layer is driven by the IP the
// probe observed, not by configuration.
type recordingResolver struct {
	mu      sync.Mutex
	queried []string
	zone    map[string][]string
}

func (r *recordingResolver) record(kind, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queried = append(r.queried, kind+" "+name)
}

func (r *recordingResolver) asked(q string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return contains(r.queried, q)
}

func (r *recordingResolver) TXT(_ context.Context, name string) ([]string, error) {
	r.record("TXT", name)
	if v, ok := r.zone[name]; ok {
		return v, nil
	}
	return nil, dnscheck.ErrNoRecord
}

func (r *recordingResolver) A(_ context.Context, name string) ([]net.IP, error) {
	r.record("A", name)
	if name == "smtp-out1.example.com" {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	return nil, dnscheck.ErrNoRecord
}

func (r *recordingResolver) AAAA(_ context.Context, name string) ([]net.IP, error) {
	r.record("AAAA", name)
	return nil, dnscheck.ErrNoRecord
}

func (r *recordingResolver) MX(_ context.Context, name string) ([]dnscheck.MX, error) {
	r.record("MX", name)
	return []dnscheck.MX{{Host: "mx." + name, Pref: 10}}, nil
}

func (r *recordingResolver) PTR(_ context.Context, name string) ([]string, error) {
	r.record("PTR", name)
	if name == "10.113.0.203.in-addr.arpa" {
		return []string{"smtp-out1.example.com"}, nil
	}
	return nil, dnscheck.ErrNoRecord
}

func TestCollectRunsDNSWithObservedIP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	res := &recordingResolver{zone: map[string][]string{
		"mail.example.com":                {"v=spf1 ip4:203.0.113.0/24 -all"},
		"_dmarc.mail.example.com":         {"v=DMARC1; p=reject; rua=mailto:d@mail.example.com"},
		"sp1._domainkey.mail.example.com": {"v=DKIM1; k=rsa; p=" + testDKIMPublic()},
	}}
	e := newEnv(t, Options{DNS: dnscheck.New(res)}, "gmail")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{msgs: []RawMessage{mailFor(t, e.runner, "gmail_pass.eml", runID, "INBOX")}}

	e.now = baseTime.Add(time.Minute)
	if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
		t.Fatal(err)
	}

	// The reverse name of the IP the Received chain recorded — nothing
	// configured it (architecture 11.3).
	if !res.asked("PTR 10.113.0.203.in-addr.arpa") {
		t.Fatalf("DNS layer did not use the observed IP: %v", res.queried)
	}
	if !res.asked("TXT sp1._domainkey.mail.example.com") {
		t.Fatalf("DKIM selector from the sending domain was not checked: %v", res.queried)
	}

	run, _ := e.st.ProbeRuns().Get(ctx, runID)
	if len(run.DNS) == 0 {
		t.Fatal("DNS report was not stored on the run")
	}
	var rep dnscheck.Report
	if err := json.Unmarshal(run.DNS, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.SPF == nil || rep.SPF.Details["result"] != "pass" {
		t.Errorf("spf = %+v", rep.SPF)
	}
	if rep.PTR == nil || rep.PTR.Status != store.HealthGreen {
		t.Errorf("ptr = %+v", rep.PTR)
	}
	if !run.PTRMatch {
		t.Error("PTRMatch was not carried onto the run")
	}
	if run.Status != store.HealthGreen {
		t.Errorf("status = %s (%s)", run.Status, run.Reason)
	}
}

func TestHealthChangedEventOnlyOnChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{}, "gmail")

	probeOnce := func(file string) {
		t.Helper()
		runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
		if err != nil {
			t.Fatal(err)
		}
		f := &fakeFetcher{msgs: []RawMessage{mailFor(t, e.runner, file, runID, "INBOX")}}
		e.now = e.now.Add(time.Minute)
		if err := e.runner.Collect(ctx, e.st, f, e.now); err != nil {
			t.Fatal(err)
		}
	}

	probeOnce("gmail_pass.eml")
	evs := e.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1 (unknown → green)", len(evs))
	}
	if evs[0].Type != EventSenderHealthChanged {
		t.Fatalf("type = %q", evs[0].Type)
	}
	var payload healthChangedPayload
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.From != store.HealthUnknown || payload.To != store.HealthGreen {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.SenderID != e.sender.ID {
		t.Fatalf("payload sender = %q", payload.SenderID)
	}

	// Still green: no event. An event per probe would be six an hour per
	// sender, all saying nothing.
	probeOnce("gmail_pass.eml")
	if n := len(e.events(t)); n != 1 {
		t.Fatalf("events = %d after an unchanged verdict, want 1", n)
	}

	probeOnce("gmail_spam_dkimfail.eml")
	evs = e.events(t)
	if len(evs) != 2 {
		t.Fatalf("events = %d after green → red, want 2", len(evs))
	}
	if err := json.Unmarshal(evs[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.From != store.HealthGreen || payload.To != store.HealthRed {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestTickRespectsInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Interval: 6 * time.Hour}, "gmail")

	// Never checked: due immediately.
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 1 {
		t.Fatalf("deliveries = %d, want 1 for a sender that was never checked", n)
	}

	// A run is pending and still inside its timeout: no second mail.
	e.now = baseTime.Add(5 * time.Minute)
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 1 {
		t.Fatalf("deliveries = %d, want the pending run to suppress a retrigger", n)
	}

	// Close the run out so the sender has a HealthCheckedAt.
	e.now = baseTime.Add(20 * time.Minute)
	if err := e.runner.Collect(ctx, e.st, &fakeFetcher{}, e.now); err != nil {
		t.Fatal(err)
	}
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.HealthCheckedAt.IsZero() {
		t.Fatal("HealthCheckedAt was not set by the timeout path")
	}

	// Well inside the interval: nothing.
	e.now = baseTime.Add(2 * time.Hour)
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 1 {
		t.Fatalf("deliveries = %d, want no probe inside the interval", n)
	}

	// Past it: one more.
	e.now = baseTime.Add(7 * time.Hour)
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 2 {
		t.Fatalf("deliveries = %d, want a probe once the interval elapsed", n)
	}
}

func TestTickReprobesAfterADomainChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Interval: 6 * time.Hour}, "gmail")

	// Pretend the sender was checked a minute ago.
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	snd.Health = store.HealthGreen
	snd.HealthCheckedAt = e.now
	if err := e.st.Senders().Update(ctx, snd); err != nil {
		t.Fatal(err)
	}

	e.now = baseTime.Add(time.Minute)
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 0 {
		t.Fatalf("deliveries = %d, want none", n)
	}

	// Rotating the DKIM selector invalidates the verdict.
	e.now = baseTime.Add(2 * time.Minute)
	dom, _ := e.st.Domains().Get(ctx, "dom-1")
	dom.DKIMSelector = "sp2"
	if err := e.st.Domains().Update(ctx, dom); err != nil {
		t.Fatal(err)
	}

	e.now = baseTime.Add(3 * time.Minute)
	if err := e.runner.Tick(ctx, e.st, e.now); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t)); n != 1 {
		t.Fatalf("deliveries = %d, want a re-probe after the domain changed", n)
	}
}

func TestTriggerWithoutMailboxFallsBackToDNS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	res := &recordingResolver{zone: map[string][]string{
		"mail.example.com":        {"v=spf1 ip4:203.0.113.0/24 -all"},
		"_dmarc.mail.example.com": {"v=DMARC1; p=reject; rua=mailto:d@mail.example.com"},
	}}
	e := newEnv(t, Options{DNS: dnscheck.New(res)})

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.st.ProbeRuns().Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	// The DKIM record is missing, so the DNS layer is red, and without a
	// loopback there is no way to know better.
	if run.Status != store.HealthRed {
		t.Fatalf("status = %s (%s)", run.Status, run.Reason)
	}
	if run.MailboxID != "" {
		t.Errorf("mailbox = %q, want none", run.MailboxID)
	}
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.Health != store.HealthRed || snd.HealthCheckedAt.IsZero() {
		t.Fatalf("sender = %+v", snd)
	}
	if n := len(e.deliveries(t)); n != 0 {
		t.Fatalf("deliveries = %d, want none without a mailbox", n)
	}
}

func TestTriggerWithoutMailboxOrDNS(t *testing.T) {
	t.Parallel()
	e := newEnv(t, Options{})
	if _, err := e.runner.Trigger(context.Background(), e.st, e.sender.ID); err == nil {
		t.Fatal("want ErrNoMailbox")
	}
}

// failingOpener is a MailboxOpener that cannot reach the mailbox, carrying the
// stage the way internal/mailbox.DialError does.
type failingOpener struct{ stage string }

func (o failingOpener) Open(context.Context, *store.ProbeMailbox) (MailboxFetcher, error) {
	return nil, stagedErr(o)
}

type stagedErr struct{ stage string }

func (e stagedErr) Error() string        { return "imap login rejected" }
func (e stagedErr) MailboxStage() string { return e.stage }

// A mailbox sendplane cannot log in to says nothing about the sender. Closing
// its runs out as red "not delivered" would send an operator hunting through
// DNS for a problem that is a rotated IMAP password (ADR-0015).
func TestCollectMailboxUnreachable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Timeout: 15 * time.Minute}, "gmail")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	opener := failingOpener{stage: store.MailboxStageAuth}

	// Before the timeout the run stays pending; the mailbox health is already
	// recorded, because the collector is what noticed.
	e.now = baseTime.Add(time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, opener, e.now); err == nil {
		t.Fatal("CollectWith hid the open failure")
	}
	box, err := e.st.ProbeMailboxes().Get(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if box.Health.Status != store.MailboxError || box.Health.Stage != store.MailboxStageAuth {
		t.Fatalf("mailbox health = %+v, want an auth error", box.Health)
	}
	run, _ := e.st.ProbeRuns().Get(ctx, runID)
	if !run.Pending {
		t.Fatal("the run was closed out before its timeout")
	}

	// Past the timeout it is closed out as unknown, naming the stage.
	e.now = baseTime.Add(16 * time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, opener, e.now); err == nil {
		t.Fatal("CollectWith hid the open failure")
	}
	run, _ = e.st.ProbeRuns().Get(ctx, runID)
	if run.Pending {
		t.Fatal("the run is still pending past its timeout")
	}
	if run.Status != store.HealthUnknown {
		t.Fatalf("status = %s (%s), want unknown", run.Status, run.Reason)
	}
	if !strings.Contains(run.Reason, "probe mailbox unreachable") ||
		!strings.Contains(run.Reason, store.MailboxStageAuth) {
		t.Fatalf("reason = %q, want it to name the mailbox and the stage", run.Reason)
	}

	// The sender's summary carries that reason rather than a delivery verdict,
	// so nobody reads a broken mailbox as a broken sender.
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.Health != store.HealthUnknown {
		t.Fatalf("sender health = %s, want unknown", snd.Health)
	}
	if !strings.Contains(snd.HealthReason, "probe mailbox unreachable") {
		t.Fatalf("sender reason = %q, want the mailbox to be named", snd.HealthReason)
	}

	// Two failed opens in a row is what tells the host.
	evs, err := e.st.Outbox().List(ctx, "", store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	unhealthy := 0
	for _, ev := range evs.Items {
		if ev.Type == "mailbox.unhealthy" {
			unhealthy++
		}
	}
	if unhealthy != 1 {
		t.Fatalf("%d mailbox.unhealthy events, want exactly 1", unhealthy)
	}
}

// A mailbox that comes back is recorded as healthy again on the next collect.
func TestCollectRecordsMailboxRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Timeout: 15 * time.Minute}, "gmail")

	if _, err := e.runner.Trigger(ctx, e.st, e.sender.ID); err != nil {
		t.Fatal(err)
	}
	e.now = baseTime.Add(time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, perMailbox{}, e.now); err != nil {
		t.Fatal(err)
	}
	box, err := e.st.ProbeMailboxes().Get(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if box.Health.Status != store.MailboxOK {
		t.Fatalf("mailbox health = %+v, want ok after a successful open", box.Health)
	}
	if box.Health.LastOKAt.IsZero() {
		t.Error("LastOKAt was not stamped")
	}
}

// --- webhook-kind mailboxes (ADR-0016) ----------------------------------

// webhookBox registers a kind=webhook probe mailbox on an env.
func webhookBox(t *testing.T, e *env, id string) *store.ProbeMailbox {
	t.Helper()
	box := &store.ProbeMailbox{
		ID: id, Name: id, Kind: store.ProbeMailboxWebhook,
		Address: id + "@probe.example", AuthServID: "mx.example.net", Enabled: true,
	}
	if err := e.st.ProbeMailboxes().Create(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	e.boxes = append(e.boxes, box)
	return box
}

// A webhook mailbox takes part in a trigger like any other one: the mail is
// sent to its address and the provider forwards it. Only the collecting half
// differs.
func TestTriggerIncludesWebhookMailboxes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{}, "gmail")
	box := webhookBox(t, e, "hook")

	if _, err := e.runner.Trigger(ctx, e.st, e.sender.ID); err != nil {
		t.Fatal(err)
	}
	runs := e.runs(t)
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want one per enabled mailbox", len(runs))
	}
	found := false
	for i := range runs {
		if runs[i].MailboxID == box.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no run for the webhook mailbox: %+v", runs)
	}
	ds := e.deliveries(t)
	for _, d := range ds {
		if d.Email == box.Address {
			return
		}
	}
	t.Fatalf("no probe delivery addressed to %s: %+v", box.Address, ds)
}

// CollectWith never opens a webhook mailbox - there is nothing to log in to -
// but it still has to close out a run whose probe never arrived, because a
// forward somebody switched off is otherwise invisible forever.
func TestCollectWebhookMailboxTimesOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Timeout: 15 * time.Minute})
	box := webhookBox(t, e, "hook")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	opener := perMailbox{}

	e.now = baseTime.Add(10 * time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, opener, e.now); err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.ProbeRuns().Get(ctx, runID)
	if !run.Pending {
		t.Fatalf("run finished inside the timeout window: %s (%s)", run.Status, run.Reason)
	}

	e.now = baseTime.Add(16 * time.Minute)
	if err := e.runner.CollectWith(ctx, e.st, opener, e.now); err != nil {
		t.Fatal(err)
	}
	run, _ = e.st.ProbeRuns().Get(ctx, runID)
	if run.Pending || run.Status != store.HealthYellow {
		t.Fatalf("status = %s (%s) pending=%v, want yellow on the first miss",
			run.Status, run.Reason, run.Pending)
	}
	if !strings.Contains(run.Reason, "웹훅") {
		t.Errorf("reason = %q, want it to name the webhook rather than a mailbox nobody polls",
			run.Reason)
	}

	// The mailbox itself is marked broken, so a dead forward shows up next to
	// a rotated IMAP password in the console and not only inside a verdict.
	got, err := e.st.ProbeMailboxes().Get(ctx, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Health.Status != store.MailboxError || got.Health.Stage != store.MailboxStageWebhook {
		t.Fatalf("mailbox health = %s/%s (%s), want error/webhook",
			got.Health.Status, got.Health.Stage, got.Health.Reason)
	}
	if got.Health.Reason != "no probe received within timeout" {
		t.Errorf("health reason = %q", got.Health.Reason)
	}
}

// The inbound webhook completes the run; the collect loop that runs afterwards
// must leave it alone and must not report the mailbox broken.
func TestCollectLeavesACompletedWebhookRunAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, Options{Timeout: 15 * time.Minute})
	box := webhookBox(t, e, "hook")

	runID, err := e.runner.Trigger(ctx, e.st, e.sender.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.ProbeRuns().Get(ctx, runID)

	raw, err := os.ReadFile(filepath.Join("testdata", "gmail_pass.eml"))
	if err != nil {
		t.Fatal(err)
	}
	h := ParseHeaders(raw)
	byName := map[string][]string{}
	for _, f := range h {
		byName[f.Name] = append(byName[f.Name], f.Value)
	}
	// The fixture's authserv-id is Gmail's; the mailbox is configured with
	// the forwarder's, so the header would not be trusted. Line them up.
	box.AuthServID = "mx.google.com"

	e.now = baseTime.Add(time.Minute)
	ev := Evidence{Mailbox: box, Headers: HeadersFromMap(byName)}
	if err := e.runner.CompleteRun(ctx, e.st, run, ev, e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.runner.RefreshSenderHealth(ctx, e.st, run.SenderID, e.now); err != nil {
		t.Fatal(err)
	}
	if run.Folder != FolderUnknown {
		t.Fatalf("folder = %q, want %q", run.Folder, FolderUnknown)
	}
	if run.Status != store.HealthGreen {
		t.Fatalf("status = %s (%s), want green: an unknown folder must not downgrade a verdict "+
			"every other signal passed", run.Status, run.Reason)
	}
	snd, _ := e.st.Senders().Get(ctx, e.sender.ID)
	if snd.Health != store.HealthGreen {
		t.Fatalf("sender health = %s, want green", snd.Health)
	}

	// Well past the timeout: the run is finished, so nothing happens.
	e.now = baseTime.Add(time.Hour)
	if err := e.runner.CollectWith(ctx, e.st, perMailbox{}, e.now); err != nil {
		t.Fatal(err)
	}
	after, _ := e.st.ProbeRuns().Get(ctx, runID)
	if after.Status != store.HealthGreen || after.Pending {
		t.Fatalf("the collect loop reopened a finished webhook run: %s pending=%v",
			after.Status, after.Pending)
	}
	gotBox, _ := e.st.ProbeMailboxes().Get(ctx, box.ID)
	if gotBox.Health.Status == store.MailboxError {
		t.Fatalf("the collect loop marked a healthy webhook mailbox broken: %+v", gotBox.Health)
	}
}

func TestTokenCarriesTheTenant(t *testing.T) {
	t.Parallel()
	r := New(Options{HMACKey: []byte("k")})
	tok := r.Token("acme", "run-1")

	tenantID, runID, ok := ParseToken(tok)
	if !ok || tenantID != "acme" || runID != "run-1" {
		t.Fatalf("ParseToken(%q) = %q, %q, %v", tok, tenantID, runID, ok)
	}
	if !r.VerifyToken("acme", "run-1", tok) {
		t.Fatal("a token this runner made did not verify")
	}
	// The MAC covers both halves: a token for one tenant may not be replayed
	// as another tenant's, which is the whole reason the endpoint can be
	// global (ADR-0016).
	if r.VerifyToken("other", "run-1", tok) || r.VerifyToken("acme", "run-2", tok) {
		t.Fatal("the MAC does not cover the tenant and the run")
	}
	if r.VerifyToken("acme", "run-1", "acme/run-1/deadbeefdeadbeef") {
		t.Fatal("a made-up MAC verified")
	}

	// Unsigned deployments still parse.
	plain := New(Options{}).Token("acme", "run-1")
	tenantID, runID, ok = ParseToken(plain)
	if !ok || tenantID != "acme" || runID != "run-1" {
		t.Fatalf("ParseToken(%q) = %q, %q, %v", plain, tenantID, runID, ok)
	}
	if _, _, ok := ParseToken("no-slash"); ok {
		t.Fatal("a token with no tenant parsed")
	}
}
