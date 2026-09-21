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
	"strings"
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
	t.Run("BounceMailboxes", func(t *testing.T) { testBounceMailboxes(t, p) })
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
		t.Run("List", func(t *testing.T) { testList(t, p) })
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
	t.Run("Tenants", func(t *testing.T) { testTenants(t, p) })
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
	eq(t, "bounce raw retention default", got.BounceRetainRaw, false)
	// An empty subscription is the default set, not "no events": the API and
	// both loops read it that way (store.TenantSettings.SubscribedTo), so a
	// backend that turned nil into [] or [] into nil would be fine, but one
	// that invented a value would not.
	eq(t, "event types default", len(got.EventTypes), 0)
	eq(t, "delivery.failed subscribed by default", got.SubscribedTo("delivery.failed"), true)
	eq(t, "delivery.sent off by default", got.SubscribedTo("delivery.sent"), false)
	// A default row carries a usable tracking key: opens, clicks and one-click
	// unsubscribe all sign with one (store.DefaultTenantSettings).
	eq(t, "default signing keys", len(got.Tracking.SigningKeys), 1)
	if k := got.Tracking.SigningKeys[0]; k.KID == "" || len(k.Secret) != 32 {
		t.Fatalf("default signing key = %+v, want a kid and a 32-byte secret", k)
	}

	stale := *got
	got.RetentionDays = 30
	got.UnsubscribeMode = store.UnsubscribeHost
	got.UnsubscribeOneClick = true
	got.BounceRetainRaw = true
	got.Tracking.Domain = "t.example.com"
	got.Tracking.SigningKeys = []store.SigningKey{{KID: "k1", Secret: []byte("s"), CreatedAt: time.Now().UTC()}}
	got.EventTypes = []string{"delivery.sent", "campaign.completed"}
	must(t, "Update", r.Update(ctx, got))
	eq(t, "Update version", got.Version, int64(2))

	stale.RetentionDays = 7
	mustBe(t, "stale Update", r.Update(ctx, &stale), store.ErrConflict)

	after, err := r.Get(ctx)
	must(t, "Get after Update", err)
	eq(t, "retention", after.RetentionDays, 30)
	eq(t, "unsubscribe mode", after.UnsubscribeMode, store.UnsubscribeHost)
	eq(t, "one-click", after.UnsubscribeOneClick, true)
	eq(t, "bounce raw retention", after.BounceRetainRaw, true)
	eq(t, "tracking domain", after.Tracking.Domain, "t.example.com")
	eq(t, "signing keys", len(after.Tracking.SigningKeys), 1)
	eq(t, "event types", strings.Join(after.EventTypes, ","), "delivery.sent,campaign.completed")
	eq(t, "explicit subscription", after.SubscribedTo("delivery.sent"), true)
	eq(t, "explicit subscription excludes the rest", after.SubscribedTo("delivery.failed"), false)
}

// Tenants reports a tenant that is configured but idle: that is exactly the
// tenant whose bounce mailbox still has to be polled days after its campaign
// finished (store.Provider.Tenants).
func testTenants(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)

	// A tenant nothing has been written for is not known yet.
	before, err := p.Tenants(ctx)
	must(t, "Tenants before", err)
	for _, id := range before {
		if id == tenantID {
			t.Fatalf("Tenants %v contains %s before it has any row", before, tenantID)
		}
		if id == store.SystemTenantID {
			t.Fatalf("Tenants returned %s", store.SystemTenantID)
		}
	}

	// A configuration row is enough: this tenant has no delivery and no
	// campaign, so ActiveTenants does not see it.
	must(t, "Create bounce mailbox", s.BounceMailboxes().Create(ctx, &store.BounceMailbox{
		Name: "bounces", Host: "imap.example.com", Port: 993, Enabled: true,
	}))

	active, err := p.ActiveTenants(ctx)
	must(t, "ActiveTenants", err)
	for _, id := range active {
		if id == tenantID {
			t.Fatalf("ActiveTenants %v contains %s, which has no work", active, tenantID)
		}
	}

	after, err := p.Tenants(ctx)
	must(t, "Tenants", err)
	found := false
	for i, id := range after {
		if id == tenantID {
			found = true
		}
		if i > 0 && after[i-1] > id {
			t.Fatalf("Tenants %v is not sorted", after)
		}
	}
	if !found {
		t.Fatalf("Tenants %v does not contain %s, which has a bounce mailbox", after, tenantID)
	}

	// A settings row alone also makes a tenant known.
	s2, tenant2 := fresh(t, p)
	_, err = store.LoadTenantSettings(ctx, s2, tenant2, time.Now().UTC())
	must(t, "LoadTenantSettings", err)
	withSettings, err := p.Tenants(ctx)
	must(t, "Tenants with settings", err)
	for _, id := range withSettings {
		if id == tenant2 {
			return
		}
	}
	t.Fatalf("Tenants %v does not contain %s, which has a settings row", withSettings, tenant2)
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

func testBounceMailboxes(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.BounceMailbox]{
		repo: s.BounceMailboxes(),
		make: func(i int) *store.BounceMailbox {
			return &store.BounceMailbox{
				Name: fmt.Sprintf("bounces-%d", i), Address: fmt.Sprintf("bounce+%d@example.com", i),
				Protocol: "imap", Host: "imap.example.com", Port: 993, TLS: store.TLSImplicit,
				Username: "bounces", Password: []byte("enc"),
				Folder: "INBOX", AfterProcess: "move:Handled", Enabled: true,
			}
		},
		id:     func(v *store.BounceMailbox) string { return v.ID },
		tenant: func(v *store.BounceMailbox) string { return v.TenantID },
		ver:    func(v *store.BounceMailbox) int64 { return v.Version },
		mutate: func(v *store.BounceMailbox) { v.Enabled = false; v.AfterProcess = "delete" },
		label:  func(v *store.BounceMailbox) string { return v.AfterProcess },
	})

	// ListEnabled is what the poller reads: the whole enabled set, unpaginated.
	s2, _ := fresh(t, p)
	r := s2.BounceMailboxes()
	on := &store.BounceMailbox{
		Name: "on", Address: "b@example.com", Protocol: "pop3",
		Host: "pop.example.com", Port: 995, TLS: store.TLSImplicit,
		Username: "u", Password: []byte("enc"), AfterProcess: "delete", Enabled: true,
	}
	must(t, "Create enabled", r.Create(ctx, on))
	must(t, "Create disabled", r.Create(ctx, &store.BounceMailbox{
		Name: "off", Host: "pop.example.com", Port: 995, Enabled: false,
	}))

	enabled, err := r.ListEnabled(ctx)
	must(t, "ListEnabled", err)
	eq(t, "ListEnabled count", len(enabled), 1)
	eq(t, "ListEnabled id", enabled[0].ID, on.ID)
	eq(t, "ListEnabled protocol", enabled[0].Protocol, "pop3")
	eq(t, "ListEnabled after-process", enabled[0].AfterProcess, "delete")
	eq(t, "ListEnabled password", string(enabled[0].Password), "enc")

	// Disabling a mailbox takes it out of the poller's set.
	on.Enabled = false
	must(t, "Update", r.Update(ctx, on))
	enabled, err = r.ListEnabled(ctx)
	must(t, "ListEnabled after disable", err)
	eq(t, "ListEnabled empty", len(enabled), 0)

	testMailboxHealth(t, p, func(t *testing.T) (mailboxHealthRepo, string) {
		t.Helper()
		s, _ := fresh(t, p)
		repo := s.BounceMailboxes()
		m := &store.BounceMailbox{
			Name: "bounces", Protocol: "imap",
			Host: "imap.example.com", Port: 993, TLS: store.TLSImplicit, Enabled: true,
		}
		must(t, "Create bounce mailbox", repo.Create(context.Background(), m))
		return bounceMailboxHealth{repo}, m.ID
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

	testMailboxHealth(t, p, func(t *testing.T) (mailboxHealthRepo, string) {
		t.Helper()
		s, _ := fresh(t, p)
		r := s.ProbeMailboxes()
		m := &store.ProbeMailbox{
			Name: "gmail", Address: "probe@gmail.com",
			Host: "imap.gmail.com", Port: 993, TLS: store.TLSImplicit, Enabled: true,
		}
		must(t, "Create probe mailbox", r.Create(context.Background(), m))
		return probeMailboxHealth{r}, m.ID
	})
}

// --- mailbox health ----------------------------------------------------

// mailboxHealthRepo is the slice of ProbeMailboxRepo and BounceMailboxRepo the
// health cases need, so one body covers both aggregates.
type mailboxHealthRepo interface {
	UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error
	// read returns the row's health plus the bookkeeping UpdateHealth must
	// leave alone.
	read(ctx context.Context, id string) (store.MailboxHealth, int64, time.Time, error)
}

type probeMailboxHealth struct{ r store.ProbeMailboxRepo }

func (p probeMailboxHealth) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return p.r.UpdateHealth(ctx, id, h)
}

func (p probeMailboxHealth) read(ctx context.Context, id string) (store.MailboxHealth, int64, time.Time, error) {
	v, err := p.r.Get(ctx, id)
	if err != nil {
		return store.MailboxHealth{}, 0, time.Time{}, err
	}
	return v.Health, v.Version, v.UpdatedAt, nil
}

type bounceMailboxHealth struct{ r store.BounceMailboxRepo }

func (b bounceMailboxHealth) UpdateHealth(ctx context.Context, id string, h store.MailboxHealth) error {
	return b.r.UpdateHealth(ctx, id, h)
}

func (b bounceMailboxHealth) read(ctx context.Context, id string) (store.MailboxHealth, int64, time.Time, error) {
	v, err := b.r.Get(ctx, id)
	if err != nil {
		return store.MailboxHealth{}, 0, time.Time{}, err
	}
	return v.Health, v.Version, v.UpdatedAt, nil
}

// testMailboxHealth covers store.MailboxHealth for one mailbox aggregate: a
// fresh row reads back unknown, UpdateHealth round-trips every field without
// touching Version or UpdatedAt, and a row of another tenant is ErrNotFound.
func testMailboxHealth(t *testing.T, p store.Provider, open func(t *testing.T) (mailboxHealthRepo, string)) {
	t.Helper()
	ctx := context.Background()
	repo, id := open(t)

	h0, ver0, updated0, err := repo.read(ctx, id)
	must(t, "Get before UpdateHealth", err)
	eq(t, "fresh health status", h0.Status, store.MailboxUnknown)
	eq(t, "fresh health stage", h0.Stage, "")
	eq(t, "fresh health failures", h0.ConsecutiveFailures, 0)
	eqTime(t, "fresh health checked_at", h0.CheckedAt, time.Time{})

	// A failed check: no LastOKAt, a stage and a streak.
	failed := time.Now().UTC().Add(-time.Minute)
	must(t, "UpdateHealth error", repo.UpdateHealth(ctx, id, store.MailboxHealth{
		Status: store.MailboxError, Stage: store.MailboxStageAuth,
		Reason: "login rejected", CheckedAt: failed, ConsecutiveFailures: 2,
	}))
	got, ver, updated, err := repo.read(ctx, id)
	must(t, "Get after UpdateHealth error", err)
	eq(t, "health status", got.Status, store.MailboxError)
	eq(t, "health stage", got.Stage, store.MailboxStageAuth)
	eq(t, "health reason", got.Reason, "login rejected")
	eq(t, "health failures", got.ConsecutiveFailures, 2)
	eqTime(t, "health checked_at", got.CheckedAt, failed)
	eqTime(t, "health last_ok_at", got.LastOKAt, time.Time{})
	// UpdateHealth is not an aggregate edit: an operator's optimistic
	// concurrency must survive a background check (store.MailboxHealth).
	eq(t, "UpdateHealth version", ver, ver0)
	eqTime(t, "UpdateHealth updated_at", updated, updated0)

	// A recovery clears the streak and stamps LastOKAt.
	ok := time.Now().UTC()
	must(t, "UpdateHealth ok", repo.UpdateHealth(ctx, id, store.MailboxHealth{
		Status: store.MailboxOK, Stage: store.MailboxStageOK,
		CheckedAt: ok, LastOKAt: ok,
	}))
	got, _, _, err = repo.read(ctx, id)
	must(t, "Get after UpdateHealth ok", err)
	eq(t, "recovered status", got.Status, store.MailboxOK)
	eq(t, "recovered reason", got.Reason, "")
	eq(t, "recovered failures", got.ConsecutiveFailures, 0)
	eqTime(t, "recovered last_ok_at", got.LastOKAt, ok)

	// Another tenant's ID is not reachable from this one.
	other, otherID := open(t)
	_ = other
	mustBe(t, "cross-tenant UpdateHealth", repo.UpdateHealth(ctx, otherID,
		store.MailboxHealth{Status: store.MailboxOK}), store.ErrNotFound)
	mustBe(t, "unknown UpdateHealth", repo.UpdateHealth(ctx, store.NewID(),
		store.MailboxHealth{Status: store.MailboxOK}), store.ErrNotFound)
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
	groupID := store.NewID()
	run := &store.ProbeRun{
		SenderID: senderA, MailboxID: store.NewID(), DeliveryID: store.NewID(),
		GroupID: groupID,
		Status:  store.HealthGreen, Delivered: true, Folder: "inbox",
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

	eq(t, "group", got.GroupID, groupID)

	res, err := r.ListBySender(ctx, senderA, store.Page{Limit: 10})
	must(t, "ListBySender", err)
	eq(t, "ListBySender count", len(res.Items), 1)

	// A run is created pending and updated once when its mail arrives or its
	// timeout passes. ListPending is what the collector reads.
	pending := &store.ProbeRun{
		SenderID: senderA, MailboxID: store.NewID(), DeliveryID: store.NewID(),
		GroupID: groupID, Pending: true, Status: store.HealthUnknown,
		StartedAt: time.Now().UTC(),
	}
	must(t, "Create pending", r.Create(ctx, pending))

	list, err := r.ListPending(ctx, store.Page{Limit: 10})
	must(t, "ListPending", err)
	eq(t, "ListPending count", len(list.Items), 1)
	eq(t, "ListPending item", list.Items[0].ID, pending.ID)
	eq(t, "ListPending flag", list.Items[0].Pending, true)

	pending.Pending = false
	pending.Status = store.HealthYellow
	pending.Reason = "spam folder"
	pending.ReceivedAt = time.Now().UTC()
	must(t, "Update", r.Update(ctx, pending))

	got, err = r.Get(ctx, pending.ID)
	must(t, "Get after Update", err)
	eq(t, "updated status", got.Status, store.HealthYellow)
	eq(t, "updated reason", got.Reason, "spam folder")
	eq(t, "no longer pending", got.Pending, false)
	eqTime(t, "received at", got.ReceivedAt, pending.ReceivedAt)

	list, err = r.ListPending(ctx, store.Page{Limit: 10})
	must(t, "ListPending after Update", err)
	eq(t, "nothing pending", len(list.Items), 0)

	mustBe(t, "Update unknown", r.Update(ctx, &store.ProbeRun{ID: store.NewID()}), store.ErrNotFound)
}

func testCampaigns(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := fresh(t, p)
	runCRUD(t, tenantID, crudSpec[store.Campaign]{
		repo: s.Campaigns(),
		make: func(i int) *store.Campaign {
			return &store.Campaign{
				Name:       fmt.Sprintf("campaign-%d", i),
				TemplateID: store.NewID(), VersionID: store.NewID(),
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

	// TemplateID round-trips: a campaign created from a template with no
	// published version yet keeps the template and resolves it at start.
	unpinned := &store.Campaign{Name: "unpinned", TemplateID: store.NewID(), Status: store.CampaignDraft}
	must(t, "Create unpinned", r.Create(ctx, unpinned))
	back, err := r.Get(ctx, unpinned.ID)
	must(t, "Get unpinned", err)
	eq(t, "template id", back.TemplateID, unpinned.TemplateID)
	eq(t, "no version pinned", back.VersionID, "")

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
