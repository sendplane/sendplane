package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/store"
)

// The HTTP half of ADR-0017: what a tenant may do with the operator's shared
// resources, what it is allowed to see of them, and what only the system
// tenant sees.

// testCatalog is the platform configuration these tests run against: one
// shared relay, one shared sending domain, and one shared From identity whose
// address is a template over the request's tenant variables.
//
// `uses` deliberately lists transactional only, because the interesting case
// is the one an operator actually configures: a shared relay is fine for a
// password reset and not for a newsletter over a reputation everybody shares.
func testCatalog() store.PlatformCatalog {
	return store.PlatformCatalog{
		Transports: []store.PlatformTransport{{
			ID: "shared", Name: "shared relay",
			Host: "relay.dev.local", Port: 587, TLS: store.TLSSTARTTLS,
			Username: "operator", Password: "relay-secret",
			RatePerSecond: 100, PerTenantRatePerSecond: 10,
		}},
		Domains: []store.PlatformDomain{{
			ID: "dev", Domain: "dev.local", ReturnPathDomain: "bounce.dev.local",
		}},
		Senders: []store.PlatformSender{{
			ID: "default", Name: "platform default",
			TransportID: "shared", DomainID: "dev",
			FromName:  "{{ tenant.name }}",
			FromEmail: "sender+{{ tenant.slug }}@dev.local",
			Uses:      []store.UseKind{store.UseTransactional},
			ProbeVars: map[string]any{"slug": "probe", "name": "Platform Probe"},
		}},
	}
}

// The virtual IDs the catalog above resolves to.
const (
	sysTransport = "sys:shared"
	sysSender    = "sys:default"
)

// headerTenants is the harness's TenantResolver. It honours
// X-Sendplane-Tenant: _system so a test can act as the operator, which is
// exactly the shape of the reference binary's resolver
// (cmd/sendplane/tenant.go) minus the role check.
//
// It implements host.TenantSwitcher, because GET /whoami asks the resolver
// itself whether a switch would be honoured rather than guessing from roles.
type headerTenants struct{}

var (
	_ host.TenantResolver = headerTenants{}
	_ host.TenantSwitcher = headerTenants{}
)

func (headerTenants) Resolve(_ context.Context, r *http.Request, p *host.Principal) (string, error) {
	if r.Header.Get("X-Sendplane-Tenant") == store.SystemTenantID {
		return store.SystemTenantID, nil
	}
	if p != nil && p.TenantID != "" {
		return p.TenantID, nil
	}
	return "default", nil
}

func (headerTenants) CanSwitchTenant(context.Context, *host.Principal) bool { return true }

// platformEnv is newEnv with the platform overlay in front of the memstore and
// the resolver, catalog and policy wired the way the root package wires them.
type platformEnv struct {
	*env
	cat store.PlatformCatalog
	res *platform.Resolver
}

// newPlatformEnv builds the handler over a memstore wrapped in
// store.WithPlatform. tune runs after the platform wiring, so a test can
// adjust the catalog-derived Deps or replace a hook.
func newPlatformEnv(t *testing.T, cat store.PlatformCatalog, tune ...func(*Deps)) *platformEnv {
	t.Helper()
	res, err := platform.New(cat)
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	pe := &platformEnv{cat: cat.Normalize(), res: res}
	wire := func(d *Deps) {
		d.Provider = store.WithPlatform(d.Provider, cat, d.Secrets, d.Clock)
		d.Platform = res
		d.Tenants = headerTenants{}
		d.Hooks.SenderPolicy = host.DefaultSenderPolicy(cat)
	}
	pe.env = newEnv(t, append([]func(*Deps){wire}, tune...)...)
	return pe
}

// asSystem makes a request resolve to the system tenant, which is the
// operator's own view.
func asSystem() reqOpt { return withHeader("X-Sendplane-Tenant", store.SystemTenantID) }

// acmeVars are the tenant attributes a request carries for tenant "acme":
// exactly the two keys the catalog's templates read.
func acmeVars() *TenantVars { return &TenantVars{"slug": "acme", "name": "Acme"} }

// --- the sender-use policy ---------------------------------------------

// The catalog's `uses` is the operator's decision about what its shared
// reputation may be spent on, and a campaign is the expensive one. Refusing at
// create costs one response instead of a million deliveries.
func TestCreateCampaignWithASharedSenderNotAllowedForCampaigns(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	tpl := e.seedTemplate()

	got := decodeError(t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "newsletter", TemplateId: tpl.Id, SenderId: sysSender,
		TenantVars: acmeVars(),
	}), http.StatusForbidden, ErrorCodeSenderUseDenied)
	// The message is the policy's own, so it has to name what would be
	// allowed instead.
	if got.Message == "" {
		t.Fatal("sender_use_denied came back with no message")
	}
}

// Transactional mail is what the shared sender is configured for, and the
// tenant attributes the request carried have to reach the delivery row: they
// are the only place a transactional send's `tenant` bindings live (a campaign
// keeps its own on the campaign).
func TestSendMessageWithASharedSenderStoresTenantVars(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	tpl := e.seedTemplate()
	version := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)

	got := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: sysSender, VersionId: &version.Id,
		TenantVars: acmeVars(),
		To:         []MessageRecipient{{Email: "user@example.org"}},
	}), http.StatusAccepted)

	stored, err := e.st.Deliveries().Get(t.Context(), got.Deliveries[0].DeliveryId.String())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if stored.SenderID != sysSender {
		t.Fatalf("sender_id = %q, want %q", stored.SenderID, sysSender)
	}
	if stored.TenantVars["slug"] != "acme" || stored.TenantVars["name"] != "Acme" {
		t.Fatalf("tenant_vars = %v, want the request's slug and name", stored.TenantVars)
	}
}

// The resolver is the single place the From identity is decided, so the API's
// accept and the sender's later render cannot disagree. This pins what the
// stored variables actually produce.
func TestResolveRendersTheSharedFromAddress(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	from, err := e.res.Resolve(sysSender, map[string]any{"slug": "acme", "name": "Acme"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if from.Name != "Acme" || from.Email != "sender+acme@dev.local" {
		t.Fatalf("From = %q <%s>, want Acme <sender+acme@dev.local>", from.Name, from.Email)
	}
}

// A missing variable is refused at the door, naming the key. The alternative
// is mail from "sender+@dev.local", which is worse than a 422, and the field
// path is what lets a caller fix the request without reading the config file.
func TestSendMessageMissingTenantVarNamesTheKey(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	tpl := e.seedTemplate()
	version := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)

	got := decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: sysSender, VersionId: &version.Id,
		TenantVars: &TenantVars{"name": "Acme"},
		To:         []MessageRecipient{{Email: "user@example.org"}},
	}), http.StatusUnprocessableEntity, ErrorCodeTenantVarsMissing)

	if got.Details == nil || len(*got.Details) != 1 {
		t.Fatalf("details = %v, want one entry", got.Details)
	}
	if field := (*got.Details)[0].Field; field == nil || *field != "tenant_vars.slug" {
		t.Fatalf("details[0].field = %v, want tenant_vars.slug", field)
	}
}

// The shared sender's From fields go out as the templates they are, verbatim.
// They cannot be declared `format: email` on the wire: a template is not an
// address, and a client (or a generated type) that validated it as one would
// reject the very sender it is meant to render. The shared sender is in every
// tenant's list, so getting this wrong breaks GET /senders for everybody, not
// only for whoever asked for this one.
func TestSharedSenderReportsItsTemplatesVerbatim(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	list := decodeInto[SenderList](t, e.do(http.MethodGet, "/api/v1/senders", nil), http.StatusOK)
	if len(list.Items) != 1 || list.Items[0].Id != sysSender {
		t.Fatalf("sender list = %+v, want just the shared sender", list.Items)
	}
	got := list.Items[0]
	if got.FromEmail != "sender+{{ tenant.slug }}@dev.local" {
		t.Fatalf("from_email = %q, want the configured template", got.FromEmail)
	}
	if deref(got.FromName) != "{{ tenant.name }}" {
		t.Fatalf("from_name = %q, want the configured template", deref(got.FromName))
	}
}

// --- what a tenant may point its own sender at -------------------------

// A tenant sends through the shared relay by using the shared *sender*, with
// the From address the operator chose. Letting it bind its own sender to the
// relay would let it send as anything it likes over somebody else's
// reputation.
func TestCreateSenderRefusesAPlatformTransport(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	e.seedSendingDomain("example.com")

	decodeError(t, e.do(http.MethodPost, "/api/v1/senders", SenderInput{
		Name: "mine", FromEmail: "news@example.com", TransportId: sysTransport,
	}), http.StatusUnprocessableEntity, ErrorCodeTransportNotAssignable)
}

// Without this check a tenant could put another tenant's domain in its From
// address, which is the same forgery the shared sender's templates exist to
// prevent.
func TestCreateSenderRefusesAFromDomainTheTenantDoesNotOwn(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	e.seedSendingDomain("example.com")
	tr := decodeInto[Transport](t, e.do(http.MethodPost, "/api/v1/transports", TransportInput{
		Name: "relay", Host: "smtp.example.com", Port: 587,
	}), http.StatusCreated)

	decodeError(t, e.do(http.MethodPost, "/api/v1/senders", SenderInput{
		Name: "mine", FromEmail: "news@notmine.example", TransportId: tr.Id,
	}), http.StatusUnprocessableEntity, ErrorCodeFromDomainNotOwned)
}

// --- visibility --------------------------------------------------------

// A shared transport is the operator's infrastructure, down to the host it
// dials. A tenant hears about it only through the sender it may send with, so
// it is absent from the list and a direct Get is a 404 rather than a 403: a
// 403 would confirm it exists.
func TestTransportsHideThePlatformTransportFromATenant(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	list := decodeInto[TransportList](t, e.do(http.MethodGet, "/api/v1/transports", nil), http.StatusOK)
	for _, item := range list.Items {
		if item.Id == sysTransport {
			t.Fatalf("a normal tenant's transport list contains %s", sysTransport)
		}
	}
	decodeError(t, e.do(http.MethodGet, "/api/v1/transports/"+sysTransport, nil),
		http.StatusNotFound, ErrorCodeNotFound)
}

// The shared sender itself is visible: it is the thing a tenant sends with.
// What is stripped is the operator's internals — the relay and domain behind
// it, and the probe verdict of a reputation every tenant shares.
func TestGetSharedSenderHidesThePlatformState(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	got := decodeInto[Sender](t, e.do(http.MethodGet, "/api/v1/senders/"+sysSender, nil), http.StatusOK)
	if got.Shared == nil || !*got.Shared {
		t.Fatalf("shared = %v, want true", got.Shared)
	}
	if got.Uses == nil || len(*got.Uses) != 1 || (*got.Uses)[0] != SenderUsesTransactional {
		t.Fatalf("uses = %v, want [transactional]", got.Uses)
	}
	if got.Health != nil || got.HealthReason != nil || got.HealthCheckedAt != nil {
		t.Fatalf("health = %v/%v/%v, want all absent for a normal tenant",
			got.Health, got.HealthReason, got.HealthCheckedAt)
	}
	// Neither reference is reported: what relay and what domain the operator
	// routes the shared identity through is the operator's business. Both are
	// optional in the schema precisely so they can be left out here.
	if got.DomainId != nil {
		t.Fatalf("domain_id = %v, want absent", *got.DomainId)
	}
	if got.TransportId != nil {
		t.Fatalf("transport_id = %v, want absent", *got.TransportId)
	}
}

// The system tenant is the operator's own view, and it is what makes a
// platform page in a console possible without a second API: the same routes,
// with the internals attached.
func TestSystemTenantSeesThePlatformInternals(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	list := decodeInto[TransportList](t, e.do(http.MethodGet, "/api/v1/transports", nil,
		asSystem()), http.StatusOK)
	var found *Transport
	for i := range list.Items {
		if list.Items[i].Id == sysTransport {
			found = &list.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("the system tenant's transport list does not contain %s", sysTransport)
	}
	if found.Host != "relay.dev.local" {
		t.Fatalf("host = %q, want the configured relay host", found.Host)
	}

	snd := decodeInto[Sender](t, e.do(http.MethodGet, "/api/v1/senders/"+sysSender, nil,
		asSystem()), http.StatusOK)
	// transport_id is a pointer because it is absent from a tenant's view of a
	// shared sender; the operator's view has it.
	if snd.TransportId == nil || *snd.TransportId != sysTransport {
		t.Fatalf("transport_id = %v, want %q", snd.TransportId, sysTransport)
	}
	if snd.Health == nil {
		t.Fatal("health is absent in the system tenant's view")
	}
}

// --- read-only and the system tenant's one restriction -----------------

// A platform resource's configuration lives in the operator's config file, so
// there is nothing an API caller could change about it — not even the
// operator, whose only edit is a deploy.
func TestUpdatePlatformTransportIsReadOnly(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	decodeError(t, e.do(http.MethodPut, "/api/v1/transports/"+sysTransport, TransportUpdate{
		Name: "hijacked", Host: "relay.attacker.example", Port: 587,
	}, asSystem()), http.StatusForbidden, ErrorCodePlatformReadOnly)
}

// The system tenant is the operator's scope for platform state and platform
// probes, not a tenant that mails anybody: its campaigns would never be
// scheduled (Provider.Tenants never returns it), so refusing at the door is
// the honest version of that.
func TestSystemTenantCannotSend(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	tpl := e.seedTemplate()
	version := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)

	decodeError(t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "newsletter", TemplateId: tpl.Id, SenderId: sysSender,
	}, asSystem()), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)

	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: sysSender, VersionId: &version.Id,
		TenantVars: acmeVars(),
		To:         []MessageRecipient{{Email: "user@example.org"}},
	}, asSystem()), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

// --- whoami ------------------------------------------------------------

// A console has to render its own chrome — which tenant it is looking at,
// whether a switcher belongs on the page — before it knows which actions the
// caller holds, so this reports the resolved tenant and echoes the host's own
// principal back.
func TestWhoamiReportsTheResolvedTenant(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	e.auth.roles = []string{"operator"}

	got := decodeInto[Whoami](t, e.do(http.MethodGet, "/api/v1/whoami", nil), http.StatusOK)
	if got.TenantId != e.tenantID || got.SystemTenant {
		t.Fatalf("whoami = %+v, want tenant %q and system_tenant false", got, e.tenantID)
	}
	if !got.CanSwitchTenant {
		t.Fatal("can_switch_tenant = false with a switching resolver installed")
	}
	if got.PrincipalId != "u1" {
		t.Fatalf("principal_id = %q, want u1", got.PrincipalId)
	}
	if got.Roles == nil || len(*got.Roles) != 1 || (*got.Roles)[0] != "operator" {
		t.Fatalf("roles = %v, want [operator]", got.Roles)
	}
}

// The header is the operator's switch, and whoami has to say so: a console
// that has switched must be able to tell that it is looking at the platform
// scope and not at a tenant.
func TestWhoamiReportsTheSystemTenant(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	got := decodeInto[Whoami](t, e.do(http.MethodGet, "/api/v1/whoami", nil, asSystem()), http.StatusOK)
	if got.TenantId != store.SystemTenantID || !got.SystemTenant {
		t.Fatalf("whoami = %+v, want tenant %q and system_tenant true", got, store.SystemTenantID)
	}
}

// can_switch_tenant asks the resolver rather than guessing from the roles,
// so a deployment whose resolver cannot switch must not advertise a switcher
// the resolver would refuse. The default resolver is exactly that case.
func TestWhoamiCannotSwitchWithoutASwitchingResolver(t *testing.T) {
	e := newEnv(t)

	got := decodeInto[Whoami](t, e.do(http.MethodGet, "/api/v1/whoami", nil), http.StatusOK)
	if got.CanSwitchTenant {
		t.Fatal("can_switch_tenant = true with a resolver that is not a host.TenantSwitcher")
	}
}

// --- the platform settings fallbacks -----------------------------------

// settingsCatalog adds the two settings an operator can supply on a tenant's
// behalf, so a tenant that has configured neither still gets working opens,
// clicks and unsubscribe links off the operator's domain.
func settingsCatalog() store.PlatformCatalog {
	cat := testCatalog()
	cat.TrackingDomain = "t.dev.local"
	cat.UnsubscribeURLTemplate = "https://dev.local/u/{{ recipient.email }}"
	return cat
}

// The effective value is the platform's and is labelled as such. The value
// itself matters because it is the one the sender uses: a console must never
// show a tracking domain that is not actually in force.
func TestSettingsFallBackToThePlatformDefaults(t *testing.T) {
	e := newPlatformEnv(t, settingsCatalog())

	got := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if got.Tracking == nil || deref(got.Tracking.Domain) != "t.dev.local" {
		t.Fatalf("tracking.domain = %v, want the platform default", got.Tracking)
	}
	if src := got.Tracking.DomainSource; src == nil || *src != SettingSourcePlatform {
		t.Fatalf("tracking.domain_source = %v, want platform", src)
	}
	if deref(got.UnsubscribeUrlTemplate) != "https://dev.local/u/{{ recipient.email }}" {
		t.Fatalf("unsubscribe_url_template = %v, want the platform default", got.UnsubscribeUrlTemplate)
	}
	if src := got.UnsubscribeUrlTemplateSource; src == nil || *src != SettingSourcePlatform {
		t.Fatalf("unsubscribe_url_template_source = %v, want platform", src)
	}
}

// A tenant that sets its own wins, and the source says so. The other field is
// untouched, which is what makes the two independent rather than one "uses
// platform defaults" flag.
func TestSettingsTenantValueOverridesThePlatformDefault(t *testing.T) {
	e := newPlatformEnv(t, settingsCatalog())
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)

	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version:  deref(cur.Version),
		Tracking: &TrackingConfig{Domain: ptr("t.acme.example")},
	}), http.StatusOK)

	if deref(got.Tracking.Domain) != "t.acme.example" {
		t.Fatalf("tracking.domain = %v, want the tenant's own", got.Tracking.Domain)
	}
	if src := got.Tracking.DomainSource; src == nil || *src != SettingSourceTenant {
		t.Fatalf("tracking.domain_source = %v, want tenant", src)
	}
	if src := got.UnsubscribeUrlTemplateSource; src == nil || *src != SettingSourcePlatform {
		t.Fatalf("unsubscribe_url_template_source = %v, want platform", src)
	}
}

// The reason store.TenantSettings.PlatformDefaults exists: a client that reads
// the effective settings and PUTs the body back unchanged must not silently
// adopt the operator's default as the tenant's own copy, or a later change to
// the platform default would never reach that tenant.
func TestSettingsRoundTripDoesNotAdoptThePlatformDefault(t *testing.T) {
	e := newPlatformEnv(t, settingsCatalog())
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)

	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version:                deref(cur.Version),
		Tracking:               &TrackingConfig{Domain: cur.Tracking.Domain},
		UnsubscribeUrlTemplate: cur.UnsubscribeUrlTemplate,
	}), http.StatusOK)

	if src := got.Tracking.DomainSource; src == nil || *src != SettingSourcePlatform {
		t.Fatalf("tracking.domain_source = %v after a round trip, want platform", src)
	}
	if src := got.UnsubscribeUrlTemplateSource; src == nil || *src != SettingSourcePlatform {
		t.Fatalf("unsubscribe_url_template_source = %v after a round trip, want platform", src)
	}
	// e.st is the memstore underneath the overlay, so this is the row as
	// actually stored: the defaults are a read-time overlay and must not have
	// been written.
	row, err := e.st.TenantSettings().Get(t.Context())
	if err != nil {
		t.Fatalf("settings row: %v", err)
	}
	if row.Tracking.Domain != "" || row.UnsubscribeURLTemplate != "" {
		t.Fatalf("stored row = %q / %q, want both empty",
			row.Tracking.Domain, row.UnsubscribeURLTemplate)
	}
}
