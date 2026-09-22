package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// The reference binary's TenantResolver is where the trust decision of
// ADR-0017 lives: whether a caller may name a tenant, and therefore whether it
// can reach `_system`, the only scope that shows the platform resources and
// their state.

// resolve runs the resolver for a request carrying (or not carrying) the
// tenant header.
func resolve(t *testing.T, r *headerTenantResolver, p *host.Principal, header string) (string, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/transports", nil)
	if header != "" {
		req.Header.Set(DefaultTenantHeader, header)
	}
	return r.Resolve(req.Context(), req, p)
}

func principal(tenantID string, roles ...string) *host.Principal {
	return &host.Principal{ID: "p1", TenantID: tenantID, Roles: roles}
}

// Off is the default, and off has to mean "no resolver at all" rather than a
// resolver that happens to refuse: sendplane's own principal-tenant default is
// then what runs, and a caller cannot name a tenant because nothing reads a
// header.
func TestNewTenantResolverIsNilWhenDisabled(t *testing.T) {
	if got := newTenantResolver(TenantHeaderConfig{}, "apikey"); got != nil {
		t.Fatalf("newTenantResolver with no `enabled` = %+v, want nil", got)
	}
	if got := newTenantResolver(TenantHeaderConfig{Enabled: ptrTo(false)}, "apikey"); got != nil {
		t.Fatalf("newTenantResolver with `enabled: false` = %+v, want nil", got)
	}
}

// The header is the operator console's switch, and the system tenant is the
// destination that matters: without it there is no way to look at the platform
// transports, domains and mailboxes at all.
func TestTenantResolverHonoursTheHeaderForAPrivilegedPrincipal(t *testing.T) {
	r := newTenantResolver(TenantHeaderConfig{
		Enabled: ptrTo(true), Roles: []string{"admin"},
	}, "apikey")
	p := principal("acme", "admin")

	got, err := resolve(t, r, p, "globex")
	if err != nil || got != "globex" {
		t.Fatalf("resolve = %q, %v; want globex", got, err)
	}
	got, err = resolve(t, r, p, store.SystemTenantID)
	if err != nil || got != store.SystemTenantID {
		t.Fatalf("resolve = %q, %v; want %s", got, err, store.SystemTenantID)
	}
	if !r.CanSwitchTenant(t.Context(), p) {
		t.Fatal("CanSwitchTenant = false for a principal the resolver just honoured")
	}
}

// A 403 rather than a silent fall back to the caller's own tenant: a console
// that sent the header believes it is looking at another tenant, and answering
// with this one's data would be worse than refusing.
func TestTenantResolverRefusesAnUnprivilegedSwitch(t *testing.T) {
	r := newTenantResolver(TenantHeaderConfig{
		Enabled: ptrTo(true), Roles: []string{"admin"},
	}, "apikey")
	p := principal("acme", "member")

	if _, err := resolve(t, r, p, "globex"); !errors.Is(err, host.ErrForbidden) {
		t.Fatalf("err = %v, want host.ErrForbidden", err)
	}
	if _, err := resolve(t, r, p, store.SystemTenantID); !errors.Is(err, host.ErrForbidden) {
		t.Fatalf("err = %v for %s, want host.ErrForbidden", err, store.SystemTenantID)
	}
	if r.CanSwitchTenant(t.Context(), p) {
		t.Fatal("CanSwitchTenant = true for a principal the resolver refuses")
	}
}

// Naming your own tenant, or naming none, is not a switch. Refusing it would
// break every ordinary caller the moment the feature was switched on for the
// console.
func TestTenantResolverAllowsAnUnprivilegedPrincipalItsOwnTenant(t *testing.T) {
	r := newTenantResolver(TenantHeaderConfig{
		Enabled: ptrTo(true), Roles: []string{"admin"},
	}, "apikey")
	p := principal("acme", "member")

	if got, err := resolve(t, r, p, ""); err != nil || got != "acme" {
		t.Fatalf("no header: resolve = %q, %v; want acme", got, err)
	}
	if got, err := resolve(t, r, p, "acme"); err != nil || got != "acme" {
		t.Fatalf("own tenant: resolve = %q, %v; want acme", got, err)
	}
}

// In auth.mode=none there is one fixed local principal, so gating the switch
// on a role would only make local development harder without protecting
// anything: the credential is "having reached the process".
func TestTenantResolverAllowsAnySwitchWithoutAuth(t *testing.T) {
	r := newTenantResolver(TenantHeaderConfig{Enabled: ptrTo(true)}, "none")
	p := principal("default") // no roles at all

	if got, err := resolve(t, r, p, store.SystemTenantID); err != nil || got != store.SystemTenantID {
		t.Fatalf("resolve = %q, %v; want %s", got, err, store.SystemTenantID)
	}
	if !r.CanSwitchTenant(t.Context(), p) {
		t.Fatal("CanSwitchTenant = false in auth.mode=none")
	}
}

// --- the platform: block -----------------------------------------------

// The `platform:` section is the whole shared catalog, and the IDs an operator
// writes are local names: everything downstream — the API, the store contract,
// a `sys:` reference in a sender — only ever sees the virtual form, so
// Normalize has to be the only place that knows the difference.
func TestPlatformConfigToHostRoundTrip(t *testing.T) {
	path := writeConfig(t, `
store:
  driver: postgres
  dsn: postgres://localhost/sendplane
secrets:
  key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
platform:
  tracking_domain: t.example.com
  transports:
    - id: shared
      name: shared relay
      host: relay.example.net
      port: 587
      username: operator
      password: relay-secret
      rate_per_second: 100
      per_tenant_rate_per_second: 10
  domains:
    - id: mail
      domain: mail.example.com
      dkim_selector: sp1
  senders:
    - id: default
      name: platform default
      transport_id: shared
      domain_id: mail
      from_name: "{{ tenant.name }}"
      from_email: "sender+{{ tenant.slug }}@mail.example.com"
      uses: [transactional]
      probe_vars:
        slug: probe
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cat := cfg.Platform.ToHost()
	if len(cat.Transports) != 1 || len(cat.Domains) != 1 || len(cat.Senders) != 1 {
		t.Fatalf("catalog = %+v, want one of each", cat)
	}
	if cat.TrackingDomain != "t.example.com" {
		t.Fatalf("tracking_domain = %q", cat.TrackingDomain)
	}
	if cat.Transports[0].Password != "relay-secret" ||
		cat.Transports[0].PerTenantRatePerSecond != 10 {
		t.Fatalf("transport = %+v", cat.Transports[0])
	}

	norm := cat.Normalize()
	if norm.Transports[0].ID != "sys:shared" || norm.Domains[0].ID != "sys:mail" {
		t.Fatalf("normalized IDs = %q / %q", norm.Transports[0].ID, norm.Domains[0].ID)
	}
	// The references are prefixed too, so a config file may spell them either
	// way and nothing downstream has to care.
	s := norm.Senders[0]
	if s.ID != "sys:default" || s.TransportID != "sys:shared" || s.DomainID != "sys:mail" {
		t.Fatalf("normalized sender = %+v", s)
	}
	if len(s.Uses) != 1 || s.Uses[0] != store.UseTransactional {
		t.Fatalf("uses = %v, want [transactional]", s.Uses)
	}
	if err := norm.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := cfg.Validate(true); err != nil {
		t.Fatalf("Config.Validate: %v", err)
	}
}

// A `transport_id` naming nothing is a sender that can never send, and the
// only symptom at run time would be every delivery on it failing. Refusing at
// startup is what turns that into one line of output.
func TestPlatformConfigRejectsADanglingTransportReference(t *testing.T) {
	cat := host.Platform{
		Transports: []host.PlatformTransport{{
			ID: "shared", Name: "shared relay", Host: "relay.example.net", Port: 587,
		}},
		Senders: []host.PlatformSender{{
			ID: "default", Name: "platform default",
			TransportID: "missing", FromEmail: "no-reply@mail.example.com",
		}},
	}

	err := cat.Validate()
	if err == nil {
		t.Fatal("a sender pointing at no platform transport was accepted")
	}
	if !strings.Contains(err.Error(), "transport_id") {
		t.Fatalf("error does not name the field to fix: %v", err)
	}
}
