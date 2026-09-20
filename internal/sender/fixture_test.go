package sender

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

const (
	testTenant    = "tenant-1"
	testSigningID = "k1"
	trackDomain   = "t.example.test"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// htmlTemplate has two trackable links, one opted out, and a body close tag,
// so link rewriting, the pixel and the unsubscribe exclusion are all exercised.
const htmlTemplate = `<html><body>` +
	`<p>Hello {{ recipient.name }}</p>` +
	`<a href="https://example.com/one">one</a>` +
	`<a href="https://example.com/two">two</a>` +
	`<a href="{{ unsubscribe_url }}" data-sp-track="off">unsubscribe</a>` +
	`</body></html>`

// fixture is a memstore plus a chaos SMTP server wired into one tenant.
type fixture struct {
	t     *testing.T
	p     *memstore.Provider
	st    store.Store
	chaos *chaossmtp.Server

	settings    *store.TenantSettings
	versionID   string
	senderID    string
	transportID string
	domainID    string
	campaignID  string
}

type fixtureOptions struct {
	rates        chaossmtp.Rates
	seed         uint64
	maxConns     int
	chaos        chaossmtp.Options
	settings     func(*store.TenantSettings)
	skipCampaign bool
}

func newFixture(t *testing.T, opt fixtureOptions) *fixture {
	t.Helper()
	ctx := context.Background()

	copts := opt.chaos
	copts.Rates = opt.rates
	copts.Seed = opt.seed
	chaos, err := chaossmtp.Start(copts)
	if err != nil {
		t.Fatalf("chaossmtp: %v", err)
	}
	t.Cleanup(func() { _ = chaos.Close() })

	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	st, err := p.ForTenant(ctx, testTenant)
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}

	settings := store.DefaultTenantSettings(testTenant, time.Now())
	settings.Retry = store.RetryPolicy{
		Backoff:     []time.Duration{time.Millisecond},
		MaxAttempts: 6,
	}
	settings.Tracking = store.TrackingConfig{
		Domain: trackDomain, Opens: true, Clicks: true,
		SigningKeys: []store.SigningKey{{
			KID: testSigningID, Secret: testSecret, CreatedAt: time.Now(),
		}},
	}
	settings.UnsubscribeURLTemplate = "https://host.example/u/{{ recipient.email }}"
	if opt.settings != nil {
		opt.settings(settings)
	}
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		t.Fatalf("settings: %v", err)
	}

	maxConns := opt.maxConns
	if maxConns == 0 {
		maxConns = 8
	}
	transport := &store.Transport{
		Name: "chaos", Host: "127.0.0.1", Port: chaos.Port(),
		TLS: store.TLSNone, MaxConns: maxConns,
	}
	if copts.Username != "" {
		transport.Username = copts.Username
		transport.Password = []byte(copts.Password)
	}
	if err := st.Transports().Create(ctx, transport); err != nil {
		t.Fatalf("transport: %v", err)
	}

	domain := &store.SendingDomain{
		Domain: "example.com", ReturnPathDomain: "bounce.example.com",
	}
	if err := st.Domains().Create(ctx, domain); err != nil {
		t.Fatalf("domain: %v", err)
	}

	snd := &store.Sender{
		Name: "news", FromName: "Example News", FromEmail: "news@example.com",
		ReplyTo: "reply@example.com", TransportID: transport.ID, DomainID: domain.ID,
	}
	if err := st.Senders().Create(ctx, snd); err != nil {
		t.Fatalf("sender: %v", err)
	}

	version := &store.MessageVersion{
		SubjectTpl:    "Hello {{ recipient.name }}",
		HTMLTpl:       htmlTemplate,
		TextTpl:       "Hello {{ recipient.name }}\n{{ unsubscribe_url }}",
		DefaultLocale: "en",
		Links:         []string{"https://example.com/one", "https://example.com/two"},
	}
	if err := st.Versions().Create(ctx, version); err != nil {
		t.Fatalf("version: %v", err)
	}

	f := &fixture{
		t: t, p: p, st: st, chaos: chaos, settings: settings,
		versionID: version.ID, senderID: snd.ID,
		transportID: transport.ID, domainID: domain.ID,
	}
	if !opt.skipCampaign {
		campaign := &store.Campaign{
			Name: "campaign", VersionID: version.ID, SenderID: snd.ID,
			DefaultLocale: "en", Status: store.CampaignRunning,
			Vars: map[string]any{"product": "sendplane"},
		}
		if err := st.Campaigns().Create(ctx, campaign); err != nil {
			t.Fatalf("campaign: %v", err)
		}
		f.campaignID = campaign.ID
	}
	return f
}

// insert adds n queued bulk deliveries named u0@…, u1@… and returns their IDs
// in recipient order.
func (f *fixture) insert(n int) []string {
	f.t.Helper()
	ds := make([]store.Delivery, n)
	for i := range ds {
		email := fmt.Sprintf("u%d@example.org", i)
		ds[i] = store.Delivery{
			CampaignID: f.campaignID,
			VersionID:  f.versionID,
			SenderID:   f.senderID,
			Lane:       store.LaneBulk,
			Status:     store.DeliveryQueued,
			Email:      email,
			EmailNorm:  email,
			Name:       fmt.Sprintf("User %d", i),
			Locale:     "en",
			Vars:       map[string]any{"n": i},
		}
	}
	if _, err := f.st.Deliveries().InsertBatch(context.Background(), ds); err != nil {
		f.t.Fatalf("InsertBatch: %v", err)
	}
	return f.deliveryIDs()
}

// insertOne adds a single delivery, letting the caller adjust it first.
func (f *fixture) insertOne(mutate func(*store.Delivery)) string {
	f.t.Helper()
	email := fmt.Sprintf("single%d@example.org", time.Now().UnixNano())
	d := store.Delivery{
		CampaignID: f.campaignID,
		VersionID:  f.versionID,
		SenderID:   f.senderID,
		Lane:       store.LaneBulk,
		Status:     store.DeliveryQueued,
		Email:      email,
		EmailNorm:  email,
		Name:       "Single",
		Locale:     "en",
	}
	if mutate != nil {
		mutate(&d)
	}
	if _, err := f.st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
		f.t.Fatalf("InsertBatch: %v", err)
	}
	for _, id := range f.deliveryIDs() {
		got, err := f.st.Deliveries().Get(context.Background(), id)
		if err == nil && got.Email == d.Email {
			return id
		}
	}
	f.t.Fatal("inserted delivery not found")
	return ""
}

func (f *fixture) deliveryIDs() []string {
	f.t.Helper()
	var ids []string
	page := store.Page{Limit: store.MaxPageLimit}
	for {
		res, err := f.st.Deliveries().ListByCampaign(context.Background(), f.campaignID, store.DeliveryFilter{}, page)
		if err != nil {
			f.t.Fatalf("ListByCampaign: %v", err)
		}
		for _, d := range res.Items {
			ids = append(ids, d.ID)
		}
		if res.NextCursor == "" {
			return ids
		}
		page.Cursor = res.NextCursor
	}
}

func (f *fixture) get(id string) *store.Delivery {
	f.t.Helper()
	d, err := f.st.Deliveries().Get(context.Background(), id)
	if err != nil {
		f.t.Fatalf("Get %s: %v", id, err)
	}
	return d
}

// testConfig is a Config tuned for tests: short intervals, no real waiting.
func (f *fixture) testConfig(workerID string) Config {
	return Config{
		WorkerID:            workerID,
		Lanes:               map[store.Lane]int{store.LaneBulk: 8},
		ClaimBatch:          32,
		LeaseFor:            2 * time.Minute,
		PollInterval:        2 * time.Millisecond,
		CampaignRefresh:     50 * time.Millisecond,
		TenantConcurrency:   64,
		TenantCacheTTL:      time.Second,
		HeartbeatInterval:   50 * time.Millisecond,
		WorkerTTL:           5 * time.Second,
		ResultBatchSize:     100,
		ResultFlushInterval: 20 * time.Millisecond,
		MaxMsgsPerConn:      50,
		ConnIdleTimeout:     5 * time.Second,
		DialTimeout:         5 * time.Second,
		SendTimeout:         10 * time.Second,
		EHLOName:            "sender.test",
		AuthRetryAfter:      50 * time.Millisecond,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// waitTerminal polls until every delivery is in a terminal status.
func (f *fixture) waitTerminal(ids []string, timeout time.Duration) bool {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		for _, id := range ids {
			if !f.get(id).Status.Terminal() {
				done = false
				break
			}
		}
		if done {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// runSenders starts n senders and returns a stop function.
func (f *fixture) runSenders(n int, mutate func(*Config)) func() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		cfg := f.testConfig(fmt.Sprintf("worker-%d", i))
		if mutate != nil {
			mutate(&cfg)
		}
		s, err := New(f.p, cfg)
		if err != nil {
			cancel()
			f.t.Fatalf("New: %v", err)
		}
		go func() {
			defer func() { done <- struct{}{} }()
			if err := s.Run(ctx); err != nil {
				f.t.Errorf("Run: %v", err)
			}
		}()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			for i := 0; i < n; i++ {
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					f.t.Error("sender did not shut down")
				}
			}
		})
	}
}
