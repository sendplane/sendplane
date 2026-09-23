// Package platformtest is the conformance suite for store.WithPlatform, the
// platform overlay of ADR-0017. It is separate from storetest because it does
// not test a Provider implementation: it tests the wrapper, against whatever
// Provider it is handed, so that every backend proves the same invariants.
//
//	func TestPlatformOverlay(t *testing.T) {
//	    platformtest.Run(t, func(t *testing.T) store.Provider { ... })
//	}
//
// The invariants, in one sentence each:
//
//   - A normal tenant sees the shared senders and nothing else.
//   - A normal tenant's shared sender carries no state.
//   - The system tenant sees everything, with state merged in.
//   - A configuration write to a shared entity is ErrReadOnly.
//   - A state write lands in a shadow row that contains no configuration.
//   - A system-tenant template or layout marked Shared is read through by
//     every tenant, never copied into it, and is read-only there; a tenant's
//     own row with the same key overrides it (ADR-0018).
package platformtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

// The catalog every case runs against. It is deliberately small and fully
// populated: one of each aggregate, with a password or a key wherever a model
// has one, so that "no configuration in the shadow row" has something to fail
// on.
const (
	transportID = "sys:shared-a"
	domainID    = "sys:shared-dom"
	senderID    = "sys:default"
	probeBoxID  = "sys:probe-a"
	bounceBoxID = "sys:bounce-a"

	transportPassword = "relay-secret"
	dkimKey           = "-----BEGIN PRIVATE KEY-----\nnot-a-key\n-----END PRIVATE KEY-----"
)

func catalog() store.PlatformCatalog {
	return store.PlatformCatalog{
		Transports: []store.PlatformTransport{{
			ID: "shared-a", Name: "Shared relay",
			Host: "relay.example.com", Port: 587, TLS: store.TLSSTARTTLS,
			Username: "relay-user", Password: transportPassword,
			MaxConns: 4, RatePerSecond: 50, PerTenantRatePerSecond: 5,
			DomainRatePerSecond: map[string]float64{"gmail.com": 10},
		}},
		Domains: []store.PlatformDomain{{
			ID: "shared-dom", Domain: "mail.example.com",
			DKIMSelector: "sp1", DKIMPrivateKey: dkimKey,
			ReturnPathDomain: "bounce.example.com",
			ExpectedSPF:      "include:example.com",
			OutboundIPs:      []string{"203.0.113.7"},
		}},
		Senders: []store.PlatformSender{{
			ID: "default", Name: "Shared sender",
			TransportID: "shared-a", DomainID: "shared-dom",
			FromName:  "{{ tenant.name }}",
			FromEmail: "sender+{{ tenant.slug }}@mail.example.com",
			Uses:      []store.UseKind{store.UseTransactional},
			ProbeVars: map[string]any{"slug": "probe", "name": "Probe"},
		}},
		ProbeMailboxes: []store.PlatformProbeMailbox{{
			ID: "probe-a", Name: "Shared probe box",
			Address: "probe@mail.example.com",
			Host:    "imap.example.com", Port: 993, TLS: store.TLSImplicit,
			Username: "probe-user", Password: "probe-secret",
			InboxFolder: "INBOX", SpamFolder: "Junk", AuthServID: "mx.example.com",
		}},
		BounceMailboxes: []store.PlatformBounceMailbox{{
			ID: "bounce-a", Name: "Shared bounce box",
			Address: "bounces@bounce.example.com", Protocol: "imap",
			Host: "imap.example.com", Port: 993, TLS: store.TLSImplicit,
			Username: "bounce-user", Password: "bounce-secret",
			Folder: "INBOX", AfterProcess: "delete",
		}},
		TrackingDomain:         "t.example.com",
		UnsubscribeURLTemplate: "https://example.com/u/{{ delivery_id }}",
	}
}

// Run executes the whole suite. open is called once and must register its own
// cleanup; the provider it returns is wrapped, migrated and then shared by
// every case (each works in its own fresh tenant).
func Run(t *testing.T, open func(t *testing.T) store.Provider) {
	t.Helper()
	inner := open(t)
	if inner == nil {
		t.Fatal("open returned a nil provider")
	}
	ctx := context.Background()
	if err := inner.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := catalog().Validate(); err != nil {
		t.Fatalf("the suite's own catalog does not validate: %v", err)
	}
	p := store.WithPlatform(inner, catalog(), nil, time.Now)

	t.Run("CatalogValidation", testCatalogValidation)
	t.Run("TenantVisibility", func(t *testing.T) { testTenantVisibility(t, p) })
	t.Run("SystemVisibility", func(t *testing.T) { testSystemVisibility(t, p) })
	t.Run("ReadOnly", func(t *testing.T) { testReadOnly(t, p) })
	t.Run("ShadowRows", func(t *testing.T) { testShadowRows(t, inner, p) })
	t.Run("MergeAndPassThrough", func(t *testing.T) { testMergeAndPassThrough(t, p) })
	t.Run("ListPaging", func(t *testing.T) { testListPaging(t, p) })
	t.Run("SharedTemplates", func(t *testing.T) { testSharedTemplates(t, inner, p) })
	t.Run("SharedLayouts", func(t *testing.T) { testSharedLayouts(t, inner, p) })
	t.Run("SharedVersions", func(t *testing.T) { testSharedVersions(t, p) })
	t.Run("SharedContentWithoutCatalog", func(t *testing.T) { testSharedContentWithoutCatalog(t, inner) })
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

func tenantStore(t *testing.T, p store.Provider) (store.Store, string) {
	t.Helper()
	id := "tenant-" + store.NewID()
	s, err := p.ForTenant(context.Background(), id)
	must(t, "ForTenant", err)
	return s, id
}

func systemStore(t *testing.T, p store.Provider) store.Store {
	t.Helper()
	s, err := p.ForTenant(context.Background(), store.SystemTenantID)
	must(t, "ForTenant(_system)", err)
	return s
}

// testCatalogValidation is the config half: unique IDs, resolvable
// references, known enum values.
func testCatalogValidation(t *testing.T) {
	if err := (store.PlatformCatalog{}).Validate(); err != nil {
		t.Fatalf("an empty catalog must validate: %v", err)
	}

	dup := catalog()
	dup.Transports = append(dup.Transports, dup.Transports[0])
	if err := dup.Validate(); err == nil {
		t.Fatal("a duplicate transport id must not validate")
	}

	dangling := catalog()
	dangling.Senders[0].TransportID = "nope"
	if err := dangling.Validate(); err == nil {
		t.Fatal("a sender naming no platform transport must not validate")
	}

	badUse := catalog()
	badUse.Senders[0].Uses = []store.UseKind{"newsletter"}
	if err := badUse.Validate(); err == nil {
		t.Fatal("an unknown use must not validate")
	}

	badID := catalog()
	badID.Domains[0].ID = "Shared Domain"
	if err := badID.Validate(); err == nil {
		t.Fatal("an id with spaces and capitals must not validate")
	}

	// Normalize is idempotent and prefixes both ids and references.
	norm := catalog().Normalize()
	eq(t, "normalized sender id", norm.Senders[0].ID, senderID)
	eq(t, "normalized transport ref", norm.Senders[0].TransportID, transportID)
	eq(t, "normalized twice", norm.Normalize().Senders[0].ID, senderID)
	eq(t, "default uses", len(store.DefaultSenderUses()), 2)
	eq(t, "configured uses", norm.SenderUses(senderID)[0], store.UseTransactional)
}

// testTenantVisibility is principle 4 on the read side: a tenant sees the
// shared sender it may send with, and no shared infrastructure at all.
func testTenantVisibility(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := tenantStore(t, p)

	snd, err := s.Senders().Get(ctx, senderID)
	must(t, "Get shared sender", err)
	eq(t, "shared", snd.Shared, true)
	eq(t, "name", snd.Name, "Shared sender")
	eq(t, "from_email template", snd.FromEmail, "sender+{{ tenant.slug }}@mail.example.com")
	eq(t, "tenant", snd.TenantID, tenantID)
	// No state: the shared reputation's probe verdict is the operator's.
	eq(t, "health", snd.Health, store.HealthUnknown)
	eq(t, "health reason", snd.HealthReason, "")
	if !snd.HealthCheckedAt.IsZero() {
		t.Fatalf("health_checked_at = %v, want zero for a tenant's view", snd.HealthCheckedAt)
	}

	list, err := s.Senders().List(ctx, store.Page{Limit: 50})
	must(t, "List senders", err)
	if !hasID(list.Items, func(v *store.Sender) string { return v.ID }, senderID) {
		t.Fatal("List senders does not include the shared sender")
	}

	// Everything else is invisible, both by ID and in a listing.
	for _, tc := range []struct {
		what string
		get  func() error
	}{
		{"transport", func() error { _, err := s.Transports().Get(ctx, transportID); return err }},
		{"domain", func() error { _, err := s.Domains().Get(ctx, domainID); return err }},
		{"probe mailbox", func() error { _, err := s.ProbeMailboxes().Get(ctx, probeBoxID); return err }},
		{"bounce mailbox", func() error { _, err := s.BounceMailboxes().Get(ctx, bounceBoxID); return err }},
	} {
		mustBe(t, "Get shared "+tc.what+" as a tenant", tc.get(), store.ErrNotFound)
	}

	trs, err := s.Transports().List(ctx, store.Page{Limit: 50})
	must(t, "List transports", err)
	if hasID(trs.Items, func(v *store.Transport) string { return v.ID }, transportID) {
		t.Fatal("a tenant's transport listing includes the shared transport")
	}
	doms, err := s.Domains().List(ctx, store.Page{Limit: 50})
	must(t, "List domains", err)
	if hasID(doms.Items, func(v *store.SendingDomain) string { return v.ID }, domainID) {
		t.Fatal("a tenant's domain listing includes the shared domain")
	}
	boxes, err := s.ProbeMailboxes().List(ctx, store.Page{Limit: 50})
	must(t, "List probe mailboxes", err)
	if hasID(boxes.Items, func(v *store.ProbeMailbox) string { return v.ID }, probeBoxID) {
		t.Fatal("a tenant's probe mailbox listing includes the shared mailbox")
	}
	bb, err := s.BounceMailboxes().ListEnabled(ctx)
	must(t, "ListEnabled bounce mailboxes", err)
	for i := range bb {
		if bb[i].ID == bounceBoxID {
			t.Fatal("a tenant's enabled bounce mailboxes include the shared mailbox")
		}
	}
}

// testSystemVisibility is the other half: the system tenant sees the
// infrastructure, with the configured secrets present so that the sender can
// actually dial.
func testSystemVisibility(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s := systemStore(t, p)

	tr, err := s.Transports().Get(ctx, transportID)
	must(t, "Get shared transport", err)
	eq(t, "shared", tr.Shared, true)
	eq(t, "host", tr.Host, "relay.example.com")
	eq(t, "port", tr.Port, 587)
	eq(t, "username", tr.Username, "relay-user")
	eq(t, "password", string(tr.Password), transportPassword)
	eq(t, "rate", tr.RatePerSecond, float64(50))
	eq(t, "domain rate", tr.DomainRatePerSecond["gmail.com"], float64(10))

	dom, err := s.Domains().Get(ctx, domainID)
	must(t, "Get shared domain", err)
	eq(t, "domain", dom.Domain, "mail.example.com")
	eq(t, "selector", dom.DKIMSelector, "sp1")
	eq(t, "dkim key", string(dom.DKIMPrivateKey), dkimKey)
	eq(t, "return path", dom.ReturnPathDomain, "bounce.example.com")

	box, err := s.ProbeMailboxes().Get(ctx, probeBoxID)
	must(t, "Get shared probe mailbox", err)
	eq(t, "probe password", string(box.Password), "probe-secret")
	eq(t, "spam folder", box.SpamFolder, "Junk")
	eq(t, "enabled", box.Enabled, true)

	bounce, err := s.BounceMailboxes().Get(ctx, bounceBoxID)
	must(t, "Get shared bounce mailbox", err)
	eq(t, "bounce password", string(bounce.Password), "bounce-secret")
	eq(t, "after process", bounce.AfterProcess, "delete")

	enabled, err := s.BounceMailboxes().ListEnabled(ctx)
	must(t, "ListEnabled", err)
	found := false
	for i := range enabled {
		if enabled[i].ID == bounceBoxID {
			found = true
		}
	}
	if !found {
		t.Fatal("the system tenant's enabled bounce mailboxes miss the shared one")
	}

	snd, err := s.Senders().Get(ctx, senderID)
	must(t, "Get shared sender as system", err)
	eq(t, "transport ref", snd.TransportID, transportID)
	eq(t, "domain ref", snd.DomainID, domainID)
}

// testReadOnly is principle 2 on the write side.
func testReadOnly(t *testing.T, p store.Provider) {
	ctx := context.Background()
	tenant, _ := tenantStore(t, p)
	system := systemStore(t, p)

	for _, name := range []string{"tenant", "system"} {
		s := tenant
		if name == "system" {
			s = system
		}
		t.Run(name, func(t *testing.T) {
			// Creating something under a virtual ID is refused even before the
			// catalog is consulted: the ID space belongs to configuration.
			mustBe(t, "Create transport", s.Transports().Create(ctx,
				&store.Transport{ID: transportID, Name: "mine", Host: "h", Port: 25}),
				store.ErrReadOnly)
			mustBe(t, "Create sender", s.Senders().Create(ctx,
				&store.Sender{ID: senderID, Name: "mine"}), store.ErrReadOnly)
			mustBe(t, "Create domain", s.Domains().Create(ctx,
				&store.SendingDomain{ID: domainID, Domain: "d"}), store.ErrReadOnly)
			mustBe(t, "Create probe mailbox", s.ProbeMailboxes().Create(ctx,
				&store.ProbeMailbox{ID: probeBoxID}), store.ErrReadOnly)
			mustBe(t, "Create bounce mailbox", s.BounceMailboxes().Create(ctx,
				&store.BounceMailbox{ID: bounceBoxID}), store.ErrReadOnly)

			mustBe(t, "Delete transport", s.Transports().Delete(ctx, transportID), store.ErrReadOnly)
			mustBe(t, "Delete sender", s.Senders().Delete(ctx, senderID), store.ErrReadOnly)
			mustBe(t, "Delete domain", s.Domains().Delete(ctx, domainID), store.ErrReadOnly)
			mustBe(t, "Delete probe mailbox", s.ProbeMailboxes().Delete(ctx, probeBoxID), store.ErrReadOnly)
			mustBe(t, "Delete bounce mailbox", s.BounceMailboxes().Delete(ctx, bounceBoxID), store.ErrReadOnly)

			// A probe or bounce mailbox has no row state at all, so every
			// Update of one is a configuration edit.
			mustBe(t, "Update probe mailbox", s.ProbeMailboxes().Update(ctx,
				&store.ProbeMailbox{ID: probeBoxID, Host: "elsewhere"}), store.ErrReadOnly)
			mustBe(t, "Update bounce mailbox", s.BounceMailboxes().Update(ctx,
				&store.BounceMailbox{ID: bounceBoxID, Host: "elsewhere"}), store.ErrReadOnly)
		})
	}

	// A config edit disguised as a state write is refused: the incoming row
	// has to match the configuration exactly.
	tr, err := system.Transports().Get(ctx, transportID)
	must(t, "Get shared transport", err)
	tr.Host = "attacker.example.com"
	tr.Status = store.TransportUnhealthy
	mustBe(t, "Update with a changed host", system.Transports().Update(ctx, tr), store.ErrReadOnly)

	// And a state write from a tenant's own view is refused outright: the
	// tenant's copy has the state zeroed, so honouring it would erase the
	// operator's.
	snd, err := tenant.Senders().Get(ctx, senderID)
	must(t, "Get shared sender", err)
	snd.Health = store.HealthGreen
	mustBe(t, "tenant Update of a shared sender", tenant.Senders().Update(ctx, snd), store.ErrReadOnly)
}

// testShadowRows is the invariant the whole design rests on: state persists,
// configuration does not.
func testShadowRows(t *testing.T, inner, p store.Provider) {
	ctx := context.Background()
	system := systemStore(t, p)
	// The unwrapped system store is what the raw rows are read from: the
	// overlay hides them, which is the point, so a test of their contents has
	// to go around it.
	raw, err := inner.ForTenant(ctx, store.SystemTenantID)
	must(t, "unwrapped ForTenant(_system)", err)

	at := store.TruncateTime(time.Now().UTC())

	// --- transport circuit state ---
	tr, err := system.Transports().Get(ctx, transportID)
	must(t, "Get transport", err)
	tr.Status = store.TransportUnhealthy
	tr.StatusReason = "auth failed"
	tr.StatusChangedAt = at
	tr.StatusUntil = at.Add(time.Hour)
	must(t, "Update transport state", system.Transports().Update(ctx, tr))

	shadow, err := raw.Transports().Get(ctx, transportID)
	must(t, "raw Get transport shadow", err)
	eq(t, "shadow shared", shadow.Shared, true)
	eq(t, "shadow status", shadow.Status, store.TransportUnhealthy)
	eq(t, "shadow reason", shadow.StatusReason, "auth failed")
	// Not one configuration column.
	eq(t, "shadow name", shadow.Name, "")
	eq(t, "shadow host", shadow.Host, "")
	eq(t, "shadow port", shadow.Port, 0)
	eq(t, "shadow tls", shadow.TLS, store.TLSMode(""))
	eq(t, "shadow username", shadow.Username, "")
	eq(t, "shadow password", len(shadow.Password), 0)
	eq(t, "shadow max conns", shadow.MaxConns, 0)
	eq(t, "shadow rate", shadow.RatePerSecond, float64(0))
	eq(t, "shadow domain rates", len(shadow.DomainRatePerSecond), 0)

	// The merged read has both halves.
	merged, err := system.Transports().Get(ctx, transportID)
	must(t, "Get merged transport", err)
	eq(t, "merged host", merged.Host, "relay.example.com")
	eq(t, "merged password", string(merged.Password), transportPassword)
	eq(t, "merged status", merged.Status, store.TransportUnhealthy)
	eq(t, "merged reason", merged.StatusReason, "auth failed")

	// A second state write is an ordinary optimistic-concurrency update.
	merged.StatusReason = "still failing"
	must(t, "second Update", system.Transports().Update(ctx, merged))
	merged, err = system.Transports().Get(ctx, transportID)
	must(t, "Get after second Update", err)
	eq(t, "merged reason again", merged.StatusReason, "still failing")

	// --- sender health ---
	snd, err := system.Senders().Get(ctx, senderID)
	must(t, "Get sender", err)
	snd.Health = store.HealthRed
	snd.HealthReason = "not delivered"
	snd.HealthCheckedAt = at
	must(t, "Update sender state", system.Senders().Update(ctx, snd))

	sndShadow, err := raw.Senders().Get(ctx, senderID)
	must(t, "raw Get sender shadow", err)
	eq(t, "sender shadow health", sndShadow.Health, store.HealthRed)
	eq(t, "sender shadow name", sndShadow.Name, "")
	eq(t, "sender shadow from_email", sndShadow.FromEmail, "")
	eq(t, "sender shadow from_name", sndShadow.FromName, "")
	eq(t, "sender shadow transport", sndShadow.TransportID, "")
	eq(t, "sender shadow domain", sndShadow.DomainID, "")

	// A normal tenant still sees no state, however red the operator's is.
	tenant, _ := tenantStore(t, p)
	tsnd, err := tenant.Senders().Get(ctx, senderID)
	must(t, "tenant Get sender", err)
	eq(t, "tenant sees no health", tsnd.Health, store.HealthUnknown)
	eq(t, "tenant sees no reason", tsnd.HealthReason, "")
	if !tsnd.HealthCheckedAt.IsZero() {
		t.Fatalf("tenant sees health_checked_at %v", tsnd.HealthCheckedAt)
	}

	// --- domain health ---
	dom, err := system.Domains().Get(ctx, domainID)
	must(t, "Get domain", err)
	dom.Health = store.HealthYellow
	dom.HealthReason = "spf softfail"
	dom.HealthCheckedAt = at
	must(t, "Update domain state", system.Domains().Update(ctx, dom))

	domShadow, err := raw.Domains().Get(ctx, domainID)
	must(t, "raw Get domain shadow", err)
	eq(t, "domain shadow health", domShadow.Health, store.HealthYellow)
	eq(t, "domain shadow domain", domShadow.Domain, "")
	eq(t, "domain shadow selector", domShadow.DKIMSelector, "")
	eq(t, "domain shadow dkim key", len(domShadow.DKIMPrivateKey), 0)
	eq(t, "domain shadow return path", domShadow.ReturnPathDomain, "")

	// --- mailbox health ---
	h := store.MailboxHealth{
		Status: store.MailboxError, Stage: store.MailboxStageAuth,
		Reason: "invalid credentials", CheckedAt: at, ConsecutiveFailures: 2,
	}
	must(t, "UpdateHealth probe mailbox", system.ProbeMailboxes().UpdateHealth(ctx, probeBoxID, h))
	pbShadow, err := raw.ProbeMailboxes().Get(ctx, probeBoxID)
	must(t, "raw Get probe mailbox shadow", err)
	eq(t, "probe shadow status", pbShadow.Health.Status, store.MailboxError)
	eq(t, "probe shadow stage", pbShadow.Health.Stage, store.MailboxStageAuth)
	eq(t, "probe shadow host", pbShadow.Host, "")
	eq(t, "probe shadow address", pbShadow.Address, "")
	eq(t, "probe shadow password", len(pbShadow.Password), 0)
	eq(t, "probe shadow username", pbShadow.Username, "")

	mergedBox, err := system.ProbeMailboxes().Get(ctx, probeBoxID)
	must(t, "Get merged probe mailbox", err)
	eq(t, "merged probe host", mergedBox.Host, "imap.example.com")
	eq(t, "merged probe health", mergedBox.Health.Status, store.MailboxError)

	// A second UpdateHealth goes through the existing shadow row.
	h.ConsecutiveFailures = 3
	must(t, "UpdateHealth again", system.ProbeMailboxes().UpdateHealth(ctx, probeBoxID, h))
	mergedBox, err = system.ProbeMailboxes().Get(ctx, probeBoxID)
	must(t, "Get after second UpdateHealth", err)
	eq(t, "merged failures", mergedBox.Health.ConsecutiveFailures, 3)

	must(t, "UpdateHealth bounce mailbox", system.BounceMailboxes().UpdateHealth(ctx, bounceBoxID, h))
	bbShadow, err := raw.BounceMailboxes().Get(ctx, bounceBoxID)
	must(t, "raw Get bounce mailbox shadow", err)
	eq(t, "bounce shadow status", bbShadow.Health.Status, store.MailboxError)
	eq(t, "bounce shadow host", bbShadow.Host, "")
	eq(t, "bounce shadow password", len(bbShadow.Password), 0)
	eq(t, "bounce shadow after process", bbShadow.AfterProcess, "")
	// A shadow row is not a mailbox to poll: it has no host, so the poller
	// must never be handed one.
	enabled, err := raw.BounceMailboxes().ListEnabled(ctx)
	must(t, "raw ListEnabled", err)
	for i := range enabled {
		if enabled[i].ID == bounceBoxID && enabled[i].Host == "" {
			t.Fatal("the raw shadow row is enabled; it would be polled with no host")
		}
	}

	// A tenant's own mailbox health still works through the overlay.
	tenant2, _ := tenantStore(t, p)
	own := &store.BounceMailbox{Name: "mine", Host: "imap.tenant.example", Port: 993, Enabled: true}
	must(t, "Create own bounce mailbox", tenant2.BounceMailboxes().Create(ctx, own))
	must(t, "UpdateHealth own", tenant2.BounceMailboxes().UpdateHealth(ctx, own.ID, h))
	back, err := tenant2.BounceMailboxes().Get(ctx, own.ID)
	must(t, "Get own bounce mailbox", err)
	eq(t, "own health", back.Health.Status, store.MailboxError)
	eq(t, "own not shared", back.Shared, false)
}

// testMergeAndPassThrough checks that wrapping does not disturb a tenant's own
// rows, and that a shadow row never shows up as an entity.
func testMergeAndPassThrough(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, tenantID := tenantStore(t, p)

	tr := &store.Transport{Name: "mine", Host: "smtp.tenant.example", Port: 587}
	must(t, "Create transport", s.Transports().Create(ctx, tr))
	if store.IsPlatformID(tr.ID) {
		t.Fatalf("a minted ID looks like a platform ID: %s", tr.ID)
	}
	got, err := s.Transports().Get(ctx, tr.ID)
	must(t, "Get own transport", err)
	eq(t, "own tenant", got.TenantID, tenantID)
	eq(t, "own not shared", got.Shared, false)

	got.RatePerSecond = 3
	must(t, "Update own transport", s.Transports().Update(ctx, got))
	must(t, "Delete own transport", s.Transports().Delete(ctx, got.ID))
	mustBe(t, "Get deleted", errGet(s.Transports().Get(ctx, got.ID)), store.ErrNotFound)

	// The system tenant's transport listing shows the shared transport once,
	// not twice (a merged entity plus its shadow row).
	sys := systemStore(t, p)
	sysTr, err := sys.Transports().Get(ctx, transportID)
	must(t, "Get shared transport", err)
	sysTr.Status = store.TransportCooldown
	must(t, "write a shadow row", sys.Transports().Update(ctx, sysTr))

	list, err := sys.Transports().List(ctx, store.Page{Limit: store.MaxPageLimit})
	must(t, "List system transports", err)
	n := 0
	for i := range list.Items {
		if list.Items[i].ID == transportID {
			n++
		}
	}
	eq(t, "shared transport appears once", n, 1)

	// PlatformView is the system store, and PlatformCatalogOf reports the
	// catalog the overlay was built with.
	pv, err := store.PlatformView(ctx, p)
	must(t, "PlatformView", err)
	if _, err := pv.Transports().Get(ctx, transportID); err != nil {
		t.Fatalf("PlatformView cannot read the shared transport: %v", err)
	}
	eq(t, "catalog senders", len(store.PlatformCatalogOf(p).Senders), 1)
	eq(t, "catalog tracking domain", store.PlatformCatalogOf(p).TrackingDomain, "t.example.com")
}

// testListPaging walks the merged listing one row at a time, which is the case
// the two cursor spaces exist for.
func testListPaging(t *testing.T, p store.Provider) {
	ctx := context.Background()
	s, _ := tenantStore(t, p)

	want := map[string]bool{senderID: false}
	for i := 0; i < 3; i++ {
		snd := &store.Sender{
			Name: "own", FromEmail: "a@b.example", TransportID: "t",
		}
		must(t, "Create sender", s.Senders().Create(ctx, snd))
		want[snd.ID] = false
	}

	page := store.Page{Limit: 1}
	for i := 0; i < 10; i++ {
		res, err := s.Senders().List(ctx, page)
		must(t, "List senders", err)
		for j := range res.Items {
			id := res.Items[j].ID
			seen, known := want[id]
			if !known {
				t.Fatalf("List returned an unexpected sender %s", id)
			}
			if seen {
				t.Fatalf("List returned sender %s twice", id)
			}
			want[id] = true
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("paging one row at a time never returned %s", id)
		}
	}
}

func hasID[T any](items []T, id func(*T) string, want string) bool {
	for i := range items {
		if id(&items[i]) == want {
			return true
		}
	}
	return false
}

// errGet drops the value of a Get so a two-result call fits mustBe.
func errGet[T any](_ *T, err error) error { return err }
