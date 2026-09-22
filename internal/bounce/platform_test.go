package bounce

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/mailbox"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// The two platform paths of ADR-0017: a shared bounce mailbox, whose mail
// belongs to whoever sent the delivery that bounced, and a shared transport,
// whose reputation every tenant on it depends on.

// otherTenant is the second tenant these tests need: the point of
// HandlePlatform is picking the right one out of several.
const otherTenant = "globex"

// bounceCatalog is one shared relay and one shared sender routed through it,
// which is all the platform configuration the bounce paths read.
func bounceCatalog() store.PlatformCatalog {
	return store.PlatformCatalog{
		Transports: []store.PlatformTransport{{
			ID: "shared", Name: "shared relay",
			Host: "relay.example.net", Port: 587,
		}},
		Senders: []store.PlatformSender{{
			ID: "default", Name: "platform default", TransportID: "shared",
			FromEmail: "sender+{{ tenant.slug }}@example.net",
		}},
	}
}

const (
	sysSender = "sys:default"
	// ownTransport stands in for a tenant's own relay: a UUID-shaped ID, so
	// store.IsPlatformID says no about it.
	ownTransport = "0191f2c3-4d5e-7a8b-9c0d-0000000000a1"
	ownSender    = "0191f2c3-4d5e-7a8b-9c0d-0000000000a2"
)

// platformFixture is a memstore behind the platform overlay, which is how the
// processor sees a store in a deployment with shared resources.
type platformFixture struct {
	t        *testing.T
	provider store.Provider
	proc     *Processor
}

func newPlatformFixture(t *testing.T) *platformFixture {
	t.Helper()
	inner := memstore.New()
	t.Cleanup(func() { _ = inner.Close() })
	clock := func() time.Time { return testNow }
	provider := store.WithPlatform(inner, bounceCatalog(), nil, clock)
	return &platformFixture{
		t: t, provider: provider,
		// Options.Provider is what the platform paths need: the delivery-ID
		// lookup and the system tenant's suppression list.
		proc: NewProcessor(Options{Clock: clock, Provider: provider}),
	}
}

// tenant returns a tenant's store, seeding the settings that make its VERP
// MACs verifiable. Every tenant in these tests shares one signing key, because
// the question is which tenant's keys get used, not which key.
func (f *platformFixture) tenant(id string, suppression bool) store.Store {
	f.t.Helper()
	ctx := context.Background()
	st, err := f.provider.ForTenant(ctx, id)
	if err != nil {
		f.t.Fatalf("ForTenant(%s): %v", id, err)
	}
	settings := store.DefaultTenantSettings(id, testNow)
	settings.SuppressionEnabled = suppression
	settings.Tracking.SigningKeys = testKeys()
	if err := st.TenantSettings().Create(ctx, settings); err != nil {
		f.t.Fatalf("settings(%s): %v", id, err)
	}
	return st
}

// system is the operator's own scope: where an unattributable bounce is
// recorded and where the platform suppression list lives.
func (f *platformFixture) system() store.Store {
	f.t.Helper()
	st, err := f.provider.ForTenant(context.Background(), store.SystemTenantID)
	if err != nil {
		f.t.Fatalf("ForTenant(_system): %v", err)
	}
	return st
}

func (f *platformFixture) seed(st store.Store, d store.Delivery) {
	f.t.Helper()
	if _, err := st.Deliveries().InsertBatch(context.Background(), []store.Delivery{d}); err != nil {
		f.t.Fatalf("InsertBatch: %v", err)
	}
}

// handle runs one message through HandlePlatform with no tenant argument,
// which is the whole point: a shared mailbox cannot say whose mail it holds.
func (f *platformFixture) handle(raw []byte) Outcome {
	f.t.Helper()
	out, err := f.proc.HandlePlatform(context.Background(), f.provider, mailbox.Message{
		ID: "shared", Raw: raw, Received: testNow.Add(-time.Minute),
	})
	if err != nil {
		f.t.Fatalf("HandlePlatform: %v", err)
	}
	return out
}

func (f *platformFixture) events(st store.Store) []store.BounceEvent {
	f.t.Helper()
	res, err := st.Bounces().List(context.Background(), store.Page{Limit: 100})
	if err != nil {
		f.t.Fatalf("list bounces: %v", err)
	}
	return res.Items
}

func (f *platformFixture) suppressed(st store.Store, email string) bool {
	f.t.Helper()
	ok, _, err := st.Suppressions().IsSuppressed(context.Background(), email, testNow)
	if err != nil {
		f.t.Fatalf("IsSuppressed: %v", err)
	}
	return ok
}

func (f *platformFixture) delivery(st store.Store, id string) *store.Delivery {
	f.t.Helper()
	d, err := st.Deliveries().Get(context.Background(), id)
	if err != nil {
		f.t.Fatalf("Get delivery: %v", err)
	}
	return d
}

// --- finding the tenant ------------------------------------------------

// The VERP return path has no room for a tenant — it has to survive every MTA
// on the way back — so the delivery ID is the only thing a shared mailbox has
// to go on. It is enough, because it is unique across tenants, and the bounce
// has to land in the tenant that sent the mail and nowhere else.
func TestHandlePlatformFindsTheTenantFromTheDeliveryID(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)
	f.seed(acme, sentDelivery(testDelivery, "nosuch@example.org"))

	out := f.handle(loadFixture(t, "postfix_hard.eml"))
	if !out.Processed || out.NewStatus != store.DeliveryBounced {
		t.Fatalf("Outcome = %+v, want a processed hard bounce", out)
	}
	if got := f.delivery(acme, testDelivery).Status; got != store.DeliveryBounced {
		t.Fatalf("delivery status = %v, want bounced", got)
	}
	if evs := f.events(acme); len(evs) != 1 || evs[0].DeliveryID != testDelivery {
		t.Fatalf("events in %s = %+v, want one for %s", testTenant, evs, testDelivery)
	}
	// The mailbox is the operator's, but the bounce is not: recording it in
	// the system tenant as well would put a tenant's recipient in the
	// operator's data.
	if evs := f.events(f.system()); len(evs) != 0 {
		t.Fatalf("events in the system tenant = %+v, want none", evs)
	}
}

// A delivery ID that matches nothing — never existed, or retention removed it
// — still means the mailbox is receiving bounces. The evidence is kept in the
// system tenant, because inventing an owner for it would be worse.
func TestHandlePlatformRecordsAnUnknownDeliveryInTheSystemTenant(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)

	out := f.handle(loadFixture(t, "postfix_hard.eml"))
	if out.Skipped != SkipUnknownDelivery || out.Processed {
		t.Fatalf("Outcome = %+v, want %s and nothing processed", out, SkipUnknownDelivery)
	}
	if !out.Recorded {
		t.Fatal("the event was not recorded at all")
	}
	if evs := f.events(f.system()); len(evs) != 1 || evs[0].DeliveryID != testDelivery {
		t.Fatalf("events in the system tenant = %+v, want one for %s", evs, testDelivery)
	}
	if evs := f.events(acme); len(evs) != 0 {
		t.Fatalf("events in %s = %+v, want none", testTenant, evs)
	}
}

// X-Sendplane-ID is the only correlation that carries a tenant, so it settles
// the question without a lookup. Here the header says one tenant and the
// delivery ID exists in another; the header has to win, or a returned original
// would be filed against whoever happens to hold that ID.
func TestHandlePlatformPrefersTheCorrelationHeaderOverTheLookup(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)
	globex := f.tenant(otherTenant, true)
	// exim_hard.eml's envelope sender was rewritten by the relay, so the
	// header is the only plant left; it names acme/testDelivery.
	f.seed(globex, sentDelivery(testDelivery, "gone@example.org"))

	out := f.handle(loadFixture(t, "exim_hard.eml"))
	// acme does not hold that delivery, which is exactly how the test can tell
	// the header's tenant was the one used.
	if out.Skipped != SkipUnknownDelivery {
		t.Fatalf("Outcome = %+v, want the bounce handled in %s", out, testTenant)
	}
	if evs := f.events(acme); len(evs) != 1 {
		t.Fatalf("events in %s = %+v, want the one the header named", testTenant, evs)
	}
	if got := f.delivery(globex, testDelivery).Status; got != store.DeliverySent {
		t.Fatalf("%s's delivery = %v, want it untouched", otherTenant, got)
	}
	if evs := f.events(globex); len(evs) != 0 {
		t.Fatalf("events in %s = %+v, want none", otherTenant, evs)
	}
}

// The lookup happens before verification and verification still happens: the
// delivery ID in a VERP address is read unverified (there is no tenant yet
// whose keys the MAC could be checked against), and the MAC is then checked
// with the keys of the tenant it resolved to. A forged ID therefore resolves
// to the real tenant and is recorded there as unverified — the same outcome a
// forged VERP gets in a tenant's own mailbox (ADR-0008).
func TestHandlePlatformVerifiesAfterResolvingTheTenant(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)
	f.seed(acme, sentDelivery(testDelivery, "victim@example.org"))

	out := f.handle(loadFixture(t, "forged_verp.eml"))
	if !out.Unverified || out.Skipped != SkipUnverified {
		t.Fatalf("Outcome = %+v, want unverified and %s", out, SkipUnverified)
	}
	if out.Processed {
		t.Fatal("a forged VERP changed the delivery")
	}
	if got := f.delivery(acme, testDelivery).Status; got != store.DeliverySent {
		t.Fatalf("delivery status = %v, want it untouched", got)
	}
	// Recorded in the resolved tenant, not in the system tenant: the lookup
	// did succeed, only the MAC did not.
	if evs := f.events(acme); len(evs) != 1 || evs[0].Verified {
		t.Fatalf("events in %s = %+v, want one unverified event", testTenant, evs)
	}
}

// --- the platform suppression list -------------------------------------

// sharedSenderDelivery is a sent delivery that went out over the shared relay,
// which is what makes its bounce the platform's business too.
func sharedSenderDelivery(id, email string) store.Delivery {
	d := sentDelivery(id, email)
	d.SenderID = sysSender
	return d
}

// A hard bounce on a shared relay damages the reputation every tenant on it
// depends on, so the address goes on the system tenant's list as well as the
// tenant's, and every later send through a shared transport checks both.
func TestSuppressPlatformListsAHardBounceThroughASharedTransport(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)
	f.seed(acme, sharedSenderDelivery(testDelivery, "nosuch@example.org"))

	out := f.handle(loadFixture(t, "postfix_hard.eml"))
	if !out.Processed || !out.Suppressed {
		t.Fatalf("Outcome = %+v, want a processed and suppressed hard bounce", out)
	}
	if !f.suppressed(acme, "nosuch@example.org") {
		t.Fatal("the address is not on the tenant's suppression list")
	}
	if !f.suppressed(f.system(), "nosuch@example.org") {
		t.Fatal("the address is not on the platform suppression list")
	}
}

// The tenant's SuppressionEnabled governs the tenant's own list. The platform
// list protects a relay everybody shares and is not the tenant's to turn off:
// a tenant that has switched its own suppression off still may not keep
// mailing a dead address through the operator's relay.
func TestSuppressPlatformIgnoresTheTenantsSuppressionSetting(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, false)
	f.seed(acme, sharedSenderDelivery(testDelivery, "nosuch@example.org"))

	out := f.handle(loadFixture(t, "postfix_hard.eml"))
	if !out.Processed {
		t.Fatalf("Outcome = %+v, want a processed hard bounce", out)
	}
	if out.Suppressed || f.suppressed(acme, "nosuch@example.org") {
		t.Fatal("the tenant's own list was written with suppression disabled")
	}
	if !f.suppressed(f.system(), "nosuch@example.org") {
		t.Fatal("the address is not on the platform suppression list")
	}
}

// A tenant sending through its own relay has its own reputation, so nothing
// about its bounces belongs on the operator's list. Without this the platform
// list would collect every tenant's dead addresses and start blocking sends
// that never touched the shared relay.
func TestSuppressPlatformLeavesTheOwnTransportAlone(t *testing.T) {
	f := newPlatformFixture(t)
	acme := f.tenant(testTenant, true)
	ctx := context.Background()
	if err := acme.Transports().Create(ctx, &store.Transport{
		ID: ownTransport, Name: "own relay", Host: "smtp.acme.example", Port: 587,
	}); err != nil {
		t.Fatalf("transport: %v", err)
	}
	if err := acme.Senders().Create(ctx, &store.Sender{
		ID: ownSender, Name: "acme news", FromEmail: "news@acme.example",
		TransportID: ownTransport,
	}); err != nil {
		t.Fatalf("sender: %v", err)
	}
	d := sentDelivery(testDelivery, "nosuch@example.org")
	d.SenderID = ownSender
	f.seed(acme, d)

	out := f.handle(loadFixture(t, "postfix_hard.eml"))
	if !out.Processed || !out.Suppressed {
		t.Fatalf("Outcome = %+v, want a processed and suppressed hard bounce", out)
	}
	if !f.suppressed(acme, "nosuch@example.org") {
		t.Fatal("the address is not on the tenant's own suppression list")
	}
	if f.suppressed(f.system(), "nosuch@example.org") {
		t.Fatal("a bounce through the tenant's own relay reached the platform list")
	}
}

// HandlePlatform is only reachable for a shared mailbox, and a shared mailbox
// exists because a Provider does. Saying so is better than resolving every
// message to the system tenant and looking like it worked.
func TestHandlePlatformRequiresAProvider(t *testing.T) {
	f := newPlatformFixture(t)

	_, err := f.proc.HandlePlatform(context.Background(), nil, mailbox.Message{
		ID: "shared", Raw: loadFixture(t, "postfix_hard.eml"),
	})
	if err == nil || !strings.Contains(err.Error(), "store.Provider") {
		t.Fatalf("err = %v, want it to name the missing Provider", err)
	}
}
