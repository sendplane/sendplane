// Package storetest is the conformance suite every store.Provider
// implementation must pass (ADR-0007). A new repository method is not done
// until a case here covers it.
//
//	func TestMyStore(t *testing.T) {
//	    storetest.Run(t, func(t *testing.T) store.Provider { ... })
//	}
//
// Every subtest works in a freshly named tenant, so implementations may run
// them in parallel and shared-mode tenant scoping is exercised throughout.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Run executes the whole suite against the provider returned by open. open is
// called once; it is responsible for registering its own cleanup.
func Run(t *testing.T, open func(t *testing.T) store.Provider) {
	t.Helper()
	p := open(t)
	if p == nil {
		t.Fatal("open returned a nil provider")
	}
	if err := p.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t.Run("TenantSettings", func(t *testing.T) { testTenantSettings(t, p) })
	t.Run("Transports", func(t *testing.T) { testTransports(t, p) })
	t.Run("Senders", func(t *testing.T) { testSenders(t, p) })
	t.Run("Domains", func(t *testing.T) { testDomains(t, p) })
	t.Run("ProbeMailboxes", func(t *testing.T) { testProbeMailboxes(t, p) })
	t.Run("ProbeRuns", func(t *testing.T) { testProbeRuns(t, p) })
	t.Run("Layouts", func(t *testing.T) { testLayouts(t, p) })
	t.Run("Templates", func(t *testing.T) { testTemplates(t, p) })
	t.Run("Versions", func(t *testing.T) { testVersions(t, p) })
	t.Run("Campaigns", func(t *testing.T) { testCampaigns(t, p) })
	t.Run("RecipientChunks", func(t *testing.T) { testRecipientChunks(t, p) })
	t.Run("PaginationStability", func(t *testing.T) { testPaginationStability(t, p) })
	t.Run("TenantIsolation", func(t *testing.T) { testTenantIsolation(t, p) })

	t.Run("Deliveries", func(t *testing.T) {
		t.Run("InsertBatchIdempotent", func(t *testing.T) { testInsertBatchIdempotent(t, p) })
		t.Run("InsertBatchAtomic", func(t *testing.T) { testInsertBatchAtomic(t, p) })
		t.Run("TimePrecision", func(t *testing.T) { testTimePrecision(t, p) })
		t.Run("Claim", func(t *testing.T) { testClaim(t, p) })
		t.Run("ClaimConcurrent", func(t *testing.T) { testClaimConcurrent(t, p) })
		t.Run("CompleteCAS", func(t *testing.T) { testCompleteCAS(t, p) })
		t.Run("MarkSent", func(t *testing.T) { testMarkSent(t, p) })
		t.Run("MarkBounced", func(t *testing.T) { testMarkBounced(t, p) })
		t.Run("ReleaseExpiredLeases", func(t *testing.T) { testReleaseExpiredLeases(t, p) })
		t.Run("Requeue", func(t *testing.T) { testRequeue(t, p) })
		t.Run("CountByStatus", func(t *testing.T) { testCountByStatus(t, p) })
		t.Run("BulkTransition", func(t *testing.T) { testBulkTransition(t, p) })
		t.Run("SetFirst", func(t *testing.T) { testSetFirst(t, p) })
		t.Run("ListByCampaign", func(t *testing.T) { testListByCampaign(t, p) })
		t.Run("DeleteBefore", func(t *testing.T) { testDeleteBefore(t, p) })
	})
	t.Run("Attempts", func(t *testing.T) { testAttempts(t, p) })
	t.Run("Suppressions", func(t *testing.T) { testSuppressions(t, p) })
	t.Run("Bounces", func(t *testing.T) { testBounces(t, p) })
	t.Run("Tracking", func(t *testing.T) { testTracking(t, p) })
	t.Run("Outbox", func(t *testing.T) { testOutbox(t, p) })
	t.Run("Locks", func(t *testing.T) { testLocks(t, p) })
	t.Run("Workers", func(t *testing.T) { testWorkers(t, p) })
	t.Run("ActiveTenants", func(t *testing.T) { testActiveTenants(t, p) })
	t.Run("ActiveTenantsCampaignOnly", func(t *testing.T) { testActiveTenantsCampaignOnly(t, p) })
	t.Run("LoadTenantSettings", func(t *testing.T) { testLoadTenantSettings(t, p) })
}

// --- helpers -----------------------------------------------------------

func newTenantID() string { return "tenant-" + store.NewID() }

// fresh returns a store bound to a tenant nothing else uses.
func fresh(t *testing.T, p store.Provider) (store.Store, string) {
	t.Helper()
	id := newTenantID()
	s, err := p.ForTenant(context.Background(), id)
	if err != nil {
		t.Fatalf("ForTenant(%s): %v", id, err)
	}
	if s == nil {
		t.Fatalf("ForTenant(%s) returned nil", id)
	}
	return s, id
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func mustBe(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// eqTime compares a timestamp read back from a store with the instant the
// suite handed in. The contract stores times at millisecond resolution
// (store/doc.go), so want is compared through store.TruncateTime: the suite
// keeps building its instants from an untruncated time.Now, which is what
// exercises the truncation path in every implementation.
func eqTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	want = store.TruncateTime(want)
	if got.IsZero() != want.IsZero() || !got.Equal(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// crudRepo is the uniform shape shared by the simple aggregates.
type crudRepo[T any] interface {
	Create(context.Context, *T) error
	Get(context.Context, string) (*T, error)
	Update(context.Context, *T) error
	Delete(context.Context, string) error
	List(context.Context, store.Page) (store.Result[T], error)
}

type crudSpec[T any] struct {
	repo crudRepo[T]
	// make builds the i-th object with its ID left empty.
	make func(i int) *T
	// accessors into the bookkeeping fields.
	id     func(*T) string
	tenant func(*T) string
	ver    func(*T) int64
	// mutate changes the field that label reads back.
	mutate func(*T)
	label  func(*T) string
}

// runCRUD covers create/read/update/delete, optimistic concurrency, not-found
// and cursor pagination for one aggregate.
func runCRUD[T any](t *testing.T, tenantID string, sp crudSpec[T]) {
	t.Helper()
	ctx := context.Background()

	const n = 5
	ids := make([]string, 0, n)
	for i := range n {
		v := sp.make(i)
		must(t, "Create", sp.repo.Create(ctx, v))
		if sp.id(v) == "" {
			t.Fatal("Create left the ID empty")
		}
		eq(t, "Create tenant", sp.tenant(v), tenantID)
		if sp.ver != nil {
			eq(t, "Create version", sp.ver(v), int64(1))
		}
		ids = append(ids, sp.id(v))
	}

	got, err := sp.repo.Get(ctx, ids[0])
	must(t, "Get", err)
	eq(t, "Get id", sp.id(got), ids[0])
	eq(t, "Get tenant", sp.tenant(got), tenantID)

	_, err = sp.repo.Get(ctx, store.NewID())
	mustBe(t, "Get unknown", err, store.ErrNotFound)

	// Update bumps the version and writes it back into the caller's struct.
	stale := *got
	sp.mutate(got)
	must(t, "Update", sp.repo.Update(ctx, got))
	if sp.ver != nil {
		eq(t, "Update version", sp.ver(got), int64(2))
	}
	reread, err := sp.repo.Get(ctx, ids[0])
	must(t, "Get after update", err)
	eq(t, "Update field", sp.label(reread), sp.label(got))

	if sp.ver != nil {
		sp.mutate(&stale)
		mustBe(t, "stale Update", sp.repo.Update(ctx, &stale), store.ErrConflict)
	}

	// Pagination: walk the whole aggregate two at a time.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := sp.repo.List(ctx, store.Page{Limit: 2, Cursor: cursor})
		must(t, "List", err)
		if len(res.Items) > 2 {
			t.Fatalf("List returned %d items for limit 2", len(res.Items))
		}
		for i := range res.Items {
			id := sp.id(&res.Items[i])
			if seen[id] {
				t.Fatalf("List returned %s twice", id)
			}
			seen[id] = true
		}
		pages++
		if pages > n+2 {
			t.Fatal("List does not terminate")
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	eq(t, "List total", len(seen), n)
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("List missed %s", id)
		}
	}

	must(t, "Delete", sp.repo.Delete(ctx, ids[0]))
	_, err = sp.repo.Get(ctx, ids[0])
	mustBe(t, "Get after Delete", err, store.ErrNotFound)
	mustBe(t, "Delete twice", sp.repo.Delete(ctx, ids[0]), store.ErrNotFound)
}

// --- simple aggregates -------------------------------------------------

func testTenantSettings(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.TenantSettings()

	_, err := r.Get(ctx)
	mustBe(t, "Get before Create", err, store.ErrNotFound)

	def := store.DefaultTenantSettings(tenantID, time.Now().UTC())
	must(t, "Create", r.Create(ctx, def))
	eq(t, "Create version", def.Version, int64(1))
	mustBe(t, "Create twice", r.Create(ctx, store.DefaultTenantSettings(tenantID, time.Now().UTC())), store.ErrConflict)

	got, err := r.Get(ctx)
	must(t, "Get", err)
	eq(t, "tenant", got.TenantID, tenantID)
	eq(t, "suppression default", got.SuppressionEnabled, true)
	eq(t, "unsubscribe mode default", got.UnsubscribeMode, store.UnsubscribeSendplane)
	eq(t, "backoff length", len(got.Retry.Backoff), 6)

	eq(t, "one-click default", got.UnsubscribeOneClick, false)

	stale := *got
	got.RetentionDays = 30
	got.UnsubscribeMode = store.UnsubscribeHost
	got.UnsubscribeOneClick = true
	got.Tracking.Domain = "t.example.com"
	got.Tracking.SigningKeys = []store.SigningKey{{KID: "k1", Secret: []byte("s"), CreatedAt: time.Now().UTC()}}
	must(t, "Update", r.Update(ctx, got))
	eq(t, "Update version", got.Version, int64(2))

	stale.RetentionDays = 7
	mustBe(t, "stale Update", r.Update(ctx, &stale), store.ErrConflict)

	after, err := r.Get(ctx)
	must(t, "Get after Update", err)
	eq(t, "retention", after.RetentionDays, 30)
	eq(t, "unsubscribe mode", after.UnsubscribeMode, store.UnsubscribeHost)
	eq(t, "one-click", after.UnsubscribeOneClick, true)
	eq(t, "tracking domain", after.Tracking.Domain, "t.example.com")
	eq(t, "signing keys", len(after.Tracking.SigningKeys), 1)
}

// LoadTenantSettings creates the default row on first access and is a plain
// read afterwards (ADR-0006).
func testLoadTenantSettings(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	now := time.Now().UTC()

	first, err := store.LoadTenantSettings(ctx, s, tenantID, now)
	must(t, "LoadTenantSettings on an empty tenant", err)
	eq(t, "tenant", first.TenantID, tenantID)
	eq(t, "version", first.Version, int64(1))
	eq(t, "defaults applied", first.UnsubscribeMode, store.UnsubscribeSendplane)

	first.RetentionDays = 7
	must(t, "Update", s.TenantSettings().Update(ctx, first))

	again, err := store.LoadTenantSettings(ctx, s, tenantID, now)
	must(t, "LoadTenantSettings on an existing row", err)
	eq(t, "no second create", again.Version, int64(2))
	eq(t, "reads the stored row", again.RetentionDays, 7)
}

func testTransports(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Transport]{
		repo: s.Transports(),
		make: func(i int) *store.Transport {
			return &store.Transport{
				Name: fmt.Sprintf("smtp-%d", i), Host: "mail.example.com", Port: 587,
				TLS: store.TLSSTARTTLS, Username: "u", Password: []byte("enc"),
				MaxConns: 4, RatePerSecond: 10,
				DomainRatePerSecond: map[string]float64{"gmail.com": 20},
				Status:              store.TransportHealthy,
			}
		},
		id:     func(v *store.Transport) string { return v.ID },
		tenant: func(v *store.Transport) string { return v.TenantID },
		ver:    func(v *store.Transport) int64 { return v.Version },
		mutate: func(v *store.Transport) {
			v.Status = store.TransportUnhealthy
			v.StatusReason = "auth"
			// StatusUntil is what lets another replica re-probe a transport it
			// never saw fail, so it has to survive a round trip.
			v.StatusUntil = time.Now().UTC().Add(time.Minute)
		},
		label: func(v *store.Transport) string {
			return v.Status.String() + "/" + v.StatusReason + "/" +
				store.TruncateTime(v.StatusUntil).Format(time.RFC3339Nano)
		},
	})
}

func testSenders(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Sender]{
		repo: s.Senders(),
		make: func(i int) *store.Sender {
			return &store.Sender{
				Name: fmt.Sprintf("sender-%d", i), FromName: "Acme",
				FromEmail: fmt.Sprintf("no-reply+%d@example.com", i),
				ReplyTo:   "support@example.com", Health: store.HealthUnknown,
			}
		},
		id:     func(v *store.Sender) string { return v.ID },
		tenant: func(v *store.Sender) string { return v.TenantID },
		ver:    func(v *store.Sender) int64 { return v.Version },
		mutate: func(v *store.Sender) { v.Health = store.HealthYellow; v.HealthReason = "spam folder" },
		label:  func(v *store.Sender) string { return v.Health.String() + "/" + v.HealthReason },
	})
}

func testDomains(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.SendingDomain]{
		repo: s.Domains(),
		make: func(i int) *store.SendingDomain {
			return &store.SendingDomain{
				Domain: fmt.Sprintf("mail%d.example.com", i), DKIMSelector: "sp",
				DKIMPrivateKey: []byte("enc"), ReturnPathDomain: "bounce.example.com",
				OutboundIPs: []string{"203.0.113.10"},
			}
		},
		id:     func(v *store.SendingDomain) string { return v.ID },
		tenant: func(v *store.SendingDomain) string { return v.TenantID },
		ver:    func(v *store.SendingDomain) int64 { return v.Version },
		mutate: func(v *store.SendingDomain) { v.Health = store.HealthRed; v.HealthReason = "spf fail" },
		label:  func(v *store.SendingDomain) string { return v.Health.String() + "/" + v.HealthReason },
	})
}

func testProbeMailboxes(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.ProbeMailbox]{
		repo: s.ProbeMailboxes(),
		make: func(i int) *store.ProbeMailbox {
			return &store.ProbeMailbox{
				Name: fmt.Sprintf("gmail-%d", i), Address: fmt.Sprintf("probe+%d@gmail.com", i),
				Host: "imap.gmail.com", Port: 993, TLS: store.TLSImplicit,
				Username: "probe", Password: []byte("enc"),
				InboxFolder: "INBOX", SpamFolder: "[Gmail]/Spam",
				AuthServID: "mx.google.com", Enabled: true,
			}
		},
		id:     func(v *store.ProbeMailbox) string { return v.ID },
		tenant: func(v *store.ProbeMailbox) string { return v.TenantID },
		ver:    func(v *store.ProbeMailbox) int64 { return v.Version },
		mutate: func(v *store.ProbeMailbox) { v.Enabled = false; v.SpamFolder = "Junk" },
		label:  func(v *store.ProbeMailbox) string { return v.SpamFolder },
	})
}

func testLayouts(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Layout]{
		repo: s.Layouts(),
		make: func(i int) *store.Layout {
			return &store.Layout{
				Name: fmt.Sprintf("layout-%d", i), Mode: store.ContentMJML,
				Body: "<mjml>{{ content }}</mjml>",
				I18n: store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{
					"en": {"footer": "Unsubscribe"},
				}},
			}
		},
		id:     func(v *store.Layout) string { return v.ID },
		tenant: func(v *store.Layout) string { return v.TenantID },
		ver:    func(v *store.Layout) int64 { return v.Version },
		mutate: func(v *store.Layout) { v.Body = "<mjml>changed {{ content }}</mjml>" },
		label:  func(v *store.Layout) string { return v.Body },
	})
}

func testTemplates(t *testing.T, p store.Provider) {
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Template]{
		repo: s.Templates(),
		make: func(i int) *store.Template {
			return &store.Template{
				Name: fmt.Sprintf("template-%d", i), Subject: "{% t \"welcome.title\" %}",
				Mode: store.ContentHTML, Body: "<p>{{ recipient.name }}</p>",
				Blocks:        []byte(`{"editor":"grapesjs-mjml"}`),
				DefaultLocale: "en",
				I18n: store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{
					"en": {"welcome.title": "Welcome, {{ recipient.name }}"},
					"ko": {"welcome.title": "{{ recipient.name }}님, 환영합니다"},
				}},
			}
		},
		id:     func(v *store.Template) string { return v.ID },
		tenant: func(v *store.Template) string { return v.TenantID },
		ver:    func(v *store.Template) int64 { return v.Version },
		mutate: func(v *store.Template) { v.Subject = "changed" },
		label:  func(v *store.Template) string { return v.Subject },
	})
}

func testVersions(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.Versions()

	tplA, tplB := store.NewID(), store.NewID()
	var ids []string
	for i := range 3 {
		v := &store.MessageVersion{
			TemplateID: tplA, SubjectTpl: fmt.Sprintf("s%d", i),
			HTMLTpl: "<p>hi</p>", TextTpl: "hi", DefaultLocale: "en",
			Links: []string{"https://example.com"}, Checksum: fmt.Sprintf("c%d", i),
		}
		must(t, "Create", r.Create(ctx, v))
		eq(t, "tenant", v.TenantID, tenantID)
		ids = append(ids, v.ID)
	}
	other := &store.MessageVersion{TemplateID: tplB, SubjectTpl: "other"}
	must(t, "Create other", r.Create(ctx, other))

	got, err := r.Get(ctx, ids[0])
	must(t, "Get", err)
	eq(t, "links", len(got.Links), 1)
	_, err = r.Get(ctx, store.NewID())
	mustBe(t, "Get unknown", err, store.ErrNotFound)

	res, err := r.ListByTemplate(ctx, tplA, store.Page{Limit: 10})
	must(t, "ListByTemplate", err)
	eq(t, "ListByTemplate count", len(res.Items), 3)
	eq(t, "ListByTemplate cursor", res.NextCursor, "")
}

func testProbeRuns(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.ProbeRuns()

	senderA, senderB := store.NewID(), store.NewID()
	run := &store.ProbeRun{
		SenderID: senderA, MailboxID: store.NewID(), DeliveryID: store.NewID(),
		Status: store.HealthGreen, Delivered: true, Folder: "inbox",
		Latency: 12 * time.Second, SPF: "pass", DKIM: "pass", DMARC: "pass",
		DKIMDomain: "example.com", DKIMSelector: "sp", DMARCPolicy: "reject",
		TLS: true, ObservedIP: "203.0.113.10", PTR: "mail.example.com", PTRMatch: true,
		DNS: []byte(`{"spf":"ok"}`), RawHeaders: "Authentication-Results: mx.google.com; spf=pass",
		StartedAt: time.Now().UTC(), ReceivedAt: time.Now().UTC(),
	}
	must(t, "Create", r.Create(ctx, run))
	eq(t, "tenant", run.TenantID, tenantID)

	must(t, "Create other", r.Create(ctx, &store.ProbeRun{SenderID: senderB, Status: store.HealthRed}))

	got, err := r.Get(ctx, run.ID)
	must(t, "Get", err)
	eq(t, "status", got.Status, store.HealthGreen)
	eq(t, "latency", got.Latency, 12*time.Second)

	res, err := r.ListBySender(ctx, senderA, store.Page{Limit: 10})
	must(t, "ListBySender", err)
	eq(t, "ListBySender count", len(res.Items), 1)
}

func testCampaigns(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Campaign]{
		repo: s.Campaigns(),
		make: func(i int) *store.Campaign {
			return &store.Campaign{
				Name: fmt.Sprintf("campaign-%d", i), VersionID: store.NewID(),
				SenderID: store.NewID(), DefaultLocale: "en",
				Vars:   map[string]any{"product": "sendplane"},
				Status: store.CampaignDraft,
			}
		},
		id:     func(v *store.Campaign) string { return v.ID },
		tenant: func(v *store.Campaign) string { return v.TenantID },
		ver:    func(v *store.Campaign) int64 { return v.Version },
		mutate: func(v *store.Campaign) { v.Status = store.CampaignRunning },
		label:  func(v *store.Campaign) string { return v.Status.String() },
	})

	r := s.Campaigns()
	running := &store.Campaign{Name: "running", Status: store.CampaignRunning}
	paused := &store.Campaign{Name: "paused", Status: store.CampaignPaused}
	must(t, "Create running", r.Create(ctx, running))
	must(t, "Create paused", r.Create(ctx, paused))

	res, err := r.ListByStatus(ctx, []store.CampaignStatus{store.CampaignPaused}, store.Page{Limit: 10})
	must(t, "ListByStatus", err)
	eq(t, "ListByStatus count", len(res.Items), 1)
	eq(t, "ListByStatus item", res.Items[0].ID, paused.ID)

	stats := store.CampaignStats{
		ByStatus:    map[store.DeliveryStatus]int64{store.DeliverySent: 7, store.DeliveryFailed: 1},
		UniqueOpens: 3, UniqueClicks: 2, Unsubscribed: 1, ComputedAt: time.Now().UTC(),
	}
	must(t, "UpdateStats", r.UpdateStats(ctx, running.ID, stats))
	got, err := r.Get(ctx, running.ID)
	must(t, "Get after UpdateStats", err)
	eq(t, "stats sent", got.Stats.ByStatus[store.DeliverySent], int64(7))
	eq(t, "stats opens", got.Stats.UniqueOpens, int64(3))
	mustBe(t, "UpdateStats unknown", r.UpdateStats(ctx, store.NewID(), stats), store.ErrNotFound)
}

func testRecipientChunks(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	r := s.RecipientChunks()
	campaignID := store.NewID()

	_, err := r.Get(ctx, campaignID, "chunk-0001")
	mustBe(t, "Get missing", err, store.ErrNotFound)

	c := &store.RecipientChunk{
		CampaignID: campaignID, Key: "chunk-0001",
		State: store.ChunkPending, Accepted: 0,
	}
	must(t, "Put", r.Put(ctx, c))

	c.State = store.ChunkCompleted
	c.Accepted = 99871
	c.Duplicates = 129
	must(t, "Put again", r.Put(ctx, c))

	got, err := r.Get(ctx, campaignID, "chunk-0001")
	must(t, "Get", err)
	eq(t, "tenant", got.TenantID, tenantID)
	eq(t, "state", got.State, store.ChunkCompleted)
	eq(t, "accepted", got.Accepted, 99871)
	eq(t, "duplicates", got.Duplicates, 129)

	// Chunk keys are scoped to their campaign.
	_, err = r.Get(ctx, store.NewID(), "chunk-0001")
	mustBe(t, "Get other campaign", err, store.ErrNotFound)
}

func testPaginationStability(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := fresh(t, p)
	r := s.Transports()

	original := map[string]bool{}
	for i := range 6 {
		v := &store.Transport{Name: fmt.Sprintf("t%d", i), Host: "h", Port: 25}
		must(t, "Create", r.Create(ctx, v))
		original[v.ID] = true
	}

	first, err := r.List(ctx, store.Page{Limit: 3})
	must(t, "List page 1", err)
	eq(t, "page 1 size", len(first.Items), 3)
	if first.NextCursor == "" {
		t.Fatal("page 1 has no cursor although more rows exist")
	}

	// Rows inserted between pages must not shift the ones already read.
	for i := range 3 {
		must(t, "Create late", r.Create(ctx, &store.Transport{Name: fmt.Sprintf("late%d", i), Host: "h", Port: 25}))
	}

	seen := map[string]bool{}
	for i := range first.Items {
		seen[first.Items[i].ID] = true
	}
	cursor := first.NextCursor
	for cursor != "" {
		res, err := r.List(ctx, store.Page{Limit: 3, Cursor: cursor})
		must(t, "List page n", err)
		for i := range res.Items {
			id := res.Items[i].ID
			if seen[id] {
				t.Fatalf("row %s returned twice across pages", id)
			}
			seen[id] = true
		}
		cursor = res.NextCursor
	}
	for id := range original {
		if !seen[id] {
			t.Fatalf("row %s was skipped by pagination", id)
		}
	}
}

func testTenantIsolation(t *testing.T, p store.Provider) {
	ctx := context.Background()
	a, _ := fresh(t, p)
	b, _ := fresh(t, p)

	tr := &store.Transport{Name: "a-transport", Host: "h", Port: 25}
	must(t, "Create transport", a.Transports().Create(ctx, tr))
	cp := &store.Campaign{Name: "a-campaign", Status: store.CampaignDraft}
	must(t, "Create campaign", a.Campaigns().Create(ctx, cp))
	d := store.Delivery{
		CampaignID: cp.ID, Email: "x@example.com", EmailNorm: "x@example.com",
		Status: store.DeliveryQueued, NextAttemptAt: time.Now().UTC(),
	}
	n, err := a.Deliveries().InsertBatch(ctx, []store.Delivery{d})
	must(t, "InsertBatch", err)
	eq(t, "inserted", n, 1)
	da, err := a.Deliveries().ListByCampaign(ctx, cp.ID, store.DeliveryFilter{}, store.Page{Limit: 10})
	must(t, "ListByCampaign", err)
	eq(t, "delivery count", len(da.Items), 1)

	_, err = b.Transports().Get(ctx, tr.ID)
	mustBe(t, "cross-tenant transport Get", err, store.ErrNotFound)
	_, err = b.Campaigns().Get(ctx, cp.ID)
	mustBe(t, "cross-tenant campaign Get", err, store.ErrNotFound)
	_, err = b.Deliveries().Get(ctx, da.Items[0].ID)
	mustBe(t, "cross-tenant delivery Get", err, store.ErrNotFound)
	mustBe(t, "cross-tenant Update", b.Transports().Update(ctx, tr), store.ErrNotFound)
	mustBe(t, "cross-tenant Delete", b.Transports().Delete(ctx, tr.ID), store.ErrNotFound)

	list, err := b.Transports().List(ctx, store.Page{Limit: 10})
	must(t, "cross-tenant List", err)
	eq(t, "cross-tenant List size", len(list.Items), 0)

	counts, err := b.Deliveries().CountByStatus(ctx, cp.ID)
	must(t, "cross-tenant CountByStatus", err)
	eq(t, "cross-tenant counts", len(counts), 0)
}

func testActiveTenants(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	now := time.Now().UTC()
	_, err := s.Deliveries().InsertBatch(ctx, []store.Delivery{{
		Email: "a@example.com", EmailNorm: "a@example.com", Lane: store.LaneTransactional,
		Status: store.DeliveryQueued, NextAttemptAt: now,
	}})
	must(t, "InsertBatch", err)

	tenants, err := p.ActiveTenants(ctx)
	must(t, "ActiveTenants", err)
	for _, id := range tenants {
		if id == tenantID {
			return
		}
	}
	t.Fatalf("ActiveTenants %v does not contain %s, which has queued work", tenants, tenantID)
}

// A tenant whose campaign is running but whose deliveries have all gone
// terminal must still be listed: that is the exact moment the finalizer has to
// tick it to move the campaign to completed (store.Provider.ActiveTenants).
func testActiveTenantsCampaignOnly(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)

	c := &store.Campaign{Name: "running, no deliveries", Status: store.CampaignRunning}
	must(t, "Create campaign", s.Campaigns().Create(ctx, c))

	counts, err := s.Deliveries().CountByStatus(ctx, c.ID)
	must(t, "CountByStatus", err)
	eq(t, "no deliveries", len(counts), 0)

	tenants, err := p.ActiveTenants(ctx)
	must(t, "ActiveTenants", err)
	for _, id := range tenants {
		if id == tenantID {
			return
		}
	}
	t.Fatalf("ActiveTenants %v does not contain %s, which has a running campaign", tenants, tenantID)
}
