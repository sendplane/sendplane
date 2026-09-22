package sender

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// The send loop's half of ADR-0017: one relay shared by every tenant, with the
// relay's own capacity and each tenant's share of it kept apart, and a From
// address that is a template rather than a row.

const (
	sysTransportID = "sys:shared"
	sysSenderID    = "sys:default"
	tenantA        = "acme"
	tenantB        = "globex"
)

// sharedCatalog is one shared relay with a per-tenant share, and one shared
// sender whose From address is templated over the send's tenant variables.
func sharedCatalog() store.PlatformCatalog {
	return store.PlatformCatalog{
		Transports: []store.PlatformTransport{{
			ID: "shared", Name: "shared relay",
			Host: "relay.example.net", Port: 587,
			RatePerSecond: 100, PerTenantRatePerSecond: 10,
		}},
		Senders: []store.PlatformSender{{
			ID: "default", Name: "platform default", TransportID: "shared",
			FromName:  "{{ tenant.name }}",
			FromEmail: "sender+{{ tenant.slug }}@example.net",
		}},
	}
}

// sharedTransport is the transport row the overlay resolves the catalog's
// shared relay into: the Shared flag is what every rule below keys on.
func sharedTransport() *store.Transport {
	return &store.Transport{ID: sysTransportID, Shared: true, Name: "shared relay"}
}

// --- bucket identity ---------------------------------------------------

// A shared transport's configured rate is the whole relay's capacity, so the
// bucket has to be keyed under the system tenant. Keying it per tenant would
// multiply the operator's relay limit by the number of tenants using it, which
// is the one mistake that gets the relay's account suspended.
func TestLimitScopeIsTheSystemTenantForASharedTransport(t *testing.T) {
	if got := limitScope(tenantA, sharedTransport()); got != store.SystemTenantID {
		t.Fatalf("limitScope = %q, want %q", got, store.SystemTenantID)
	}
	own := &store.Transport{ID: "0191f2c3-4d5e-7a8b-9c0d-0000000000a1"}
	if got := limitScope(tenantA, own); got != tenantA {
		t.Fatalf("limitScope for a tenant's own transport = %q, want %q", got, tenantA)
	}
}

// Two tenants sending through the same shared relay must land in the same
// transport bucket and in different per-tenant buckets. If the per-tenant
// bucket name collided with the transport one, the fair share would silently
// be the transport limit.
func TestSharedTransportBucketsAreSharedAndThePerTenantOnesAreNot(t *testing.T) {
	tr := sharedTransport()
	keyA := sharedTransportKey(tenantA, tr)
	keyB := sharedTransportKey(tenantB, tr)
	if keyA != keyB {
		t.Fatalf("transport bucket = %q for %s and %q for %s, want one bucket",
			keyA, tenantA, keyB, tenantB)
	}
	if tenantBucket(keyA, tenantA) == tenantBucket(keyA, tenantB) {
		t.Fatalf("both tenants share the per-tenant bucket %q", tenantBucket(keyA, tenantA))
	}
	if tenantBucket(keyA, tenantA) == keyA {
		t.Fatalf("the per-tenant bucket collides with the transport bucket %q", keyA)
	}
}

// --- what the two buckets actually do ----------------------------------

// The relay's capacity is spent by everybody on it: one tenant's traffic has
// to slow the next tenant's down, because there is one SMTP account behind
// both.
func TestSharedTransportCapacityIsSpentByEveryTenant(t *testing.T) {
	l, _ := newTestLimiter(1)
	tr := sharedTransport()
	key := sharedTransportKey(tenantA, tr)
	// The relay allows 2/s; each tenant's own share is generous, so only the
	// transport bucket can be what limits anything here.
	transport := Key{Name: key, Rate: 2}
	shareA := Key{Name: tenantBucket(key, tenantA), Rate: 100}
	shareB := Key{Name: tenantBucket(key, tenantB), Rate: 100}

	for i := 0; i < 2; i++ {
		if d, _ := l.Wait(context.Background(), transport, shareA); d != 0 {
			t.Fatalf("%s message %d waited %v, the first second should be free", tenantA, i, d)
		}
	}
	d, _ := l.Wait(context.Background(), transport, shareB)
	if want := 500 * time.Millisecond; d != want {
		t.Fatalf("%s waited %v, want %v: the relay's tokens were spent by %s",
			tenantB, d, want, tenantA)
	}
}

// The fair share of architecture 8.2: one tenant's campaign filling its own
// slice of the relay must not stop another tenant's mail, or a single big
// sender would own the shared relay for as long as its campaign runs.
func TestPerTenantShareDoesNotBlockTheOtherTenant(t *testing.T) {
	l, _ := newTestLimiter(1)
	tr := sharedTransport()
	key := sharedTransportKey(tenantA, tr)
	// The relay is generous; each tenant's slice is 2/s.
	transport := Key{Name: key, Rate: 1000}
	shareA := Key{Name: tenantBucket(key, tenantA), Rate: 2}
	shareB := Key{Name: tenantBucket(key, tenantB), Rate: 2}

	for i := 0; i < 2; i++ {
		if d, _ := l.Wait(context.Background(), transport, shareA); d != 0 {
			t.Fatalf("%s message %d waited %v", tenantA, i, d)
		}
	}
	if d, _ := l.Wait(context.Background(), transport, shareA); d != 500*time.Millisecond {
		t.Fatalf("%s's third message waited %v, want its share to be spent", tenantA, d)
	}
	// The other tenant's slice is untouched by that.
	if d, _ := l.Wait(context.Background(), transport, shareB); d != 0 {
		t.Fatalf("%s waited %v because %s spent its own share", tenantB, d, tenantA)
	}
}

// --- tenant variables per delivery -------------------------------------

// A transactional or probe delivery carries its own tenant variables; a
// campaign delivery carries none and reads its campaign's, because copying one
// object onto a million rows is what ADR-0002 keeps campaign start from doing.
func TestTenantVarsForPrefersTheDeliverysOwn(t *testing.T) {
	d := &store.Delivery{TenantVars: map[string]any{"slug": "from-delivery"}}
	c := &store.Campaign{TenantVars: map[string]any{"slug": "from-campaign"}}

	if got := tenantVarsFor(d, c)["slug"]; got != "from-delivery" {
		t.Fatalf("slug = %v, want the delivery's own", got)
	}
}

func TestTenantVarsForFallsBackToTheCampaigns(t *testing.T) {
	d := &store.Delivery{}
	c := &store.Campaign{TenantVars: map[string]any{"slug": "from-campaign"}}

	if got := tenantVarsFor(d, c)["slug"]; got != "from-campaign" {
		t.Fatalf("slug = %v, want the campaign's", got)
	}
	if got := tenantVarsFor(d, nil); got != nil {
		t.Fatalf("tenantVarsFor with no campaign = %v, want nil", got)
	}
}

// --- the From identity -------------------------------------------------

// newPlatformSender is a Sender over an empty memstore, which is all
// resolveFrom needs: the answer comes from the catalog and the send's
// variables, never from a row.
func newPlatformSender(t *testing.T, cat store.PlatformCatalog) *Sender {
	t.Helper()
	res, err := platform.New(cat)
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	p := memstore.New()
	t.Cleanup(func() { _ = p.Close() })
	s, err := New(p, Config{
		WorkerID: "worker-1",
		Lanes:    map[store.Lane]int{store.LaneBulk: 1},
		Platform: res,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// A shared sender's From address is rendered per delivery from the send's
// tenant variables, which is how one configuration entry gives every tenant
// its own sub-address — and therefore its own bounces and its own reputation
// slice (ADR-0017).
func TestResolveFromRendersASharedSendersTemplates(t *testing.T) {
	s := newPlatformSender(t, sharedCatalog())
	snd := &store.Sender{
		ID: sysSenderID, Shared: true,
		// The row carries the templates verbatim; nothing but the resolver may
		// read them, which is why a caller that mistook one for an address
		// would get an obviously templated string.
		FromName: "{{ tenant.name }}", FromEmail: "sender+{{ tenant.slug }}@example.net",
	}

	got, err := s.resolveFrom(snd, map[string]any{"slug": "acme", "name": "Acme"})
	if err != nil {
		t.Fatalf("resolveFrom: %v", err)
	}
	if got.Name != "Acme" || got.Email != "sender+acme@example.net" {
		t.Fatalf("From = %q <%s>, want Acme <sender+acme@example.net>", got.Name, got.Email)
	}
}

// A tenant's own sender is a row with literal addresses on it, and must not go
// anywhere near the resolver: a From address containing "{{" is the tenant's
// own choice and is sent as written.
func TestResolveFromReturnsTheRowForATenantsOwnSender(t *testing.T) {
	s := newPlatformSender(t, sharedCatalog())
	snd := &store.Sender{
		ID:       "0191f2c3-4d5e-7a8b-9c0d-0000000000a2",
		FromName: "Example News", FromEmail: "news@example.com", ReplyTo: "reply@example.com",
	}

	got, err := s.resolveFrom(snd, nil)
	if err != nil {
		t.Fatalf("resolveFrom: %v", err)
	}
	if got.Name != "Example News" || got.Email != "news@example.com" || got.ReplyTo != "reply@example.com" {
		t.Fatalf("From = %+v, want the sender row's own fields", got)
	}
}
