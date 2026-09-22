package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// The reference binary's TenantResolver.
//
// By default it is the principal's own tenant and nothing else, which is what
// a multi-tenant API should be: a caller cannot name a tenant, so it cannot
// reach one it was not issued a credential for.
//
// `auth.tenant_header` opts into a second behaviour: a caller holding one of
// the configured roles may name a tenant in a header, which is how an operator
// console switches between its own tenant and the system tenant (`_system`) —
// the only view that shows platform transports, domains, mailboxes and their
// state (ADR-0017).
//
// The trust decision is entirely here, in the host. An embedding application
// implements its own resolver and makes its own decision; sendplane only asks
// the resolver what tenant a request is for, and (through host.TenantSwitcher)
// whether it would honour a switch, so GET /api/v1/whoami can tell a console
// whether to render the switcher.
type headerTenantResolver struct {
	header string
	roles  []string
	// anyone is true in auth.mode=none, where there is one fixed local
	// principal and gating the switch on a role would only make local
	// development harder without protecting anything.
	anyone bool
}

var (
	_ host.TenantResolver = (*headerTenantResolver)(nil)
	_ host.TenantSwitcher = (*headerTenantResolver)(nil)
)

// newTenantResolver builds the resolver, or returns nil when the header
// feature is off — in which case the caller leaves Options.Tenants nil and
// sendplane uses its own principal-tenant default.
func newTenantResolver(cfg TenantHeaderConfig, authMode string) *headerTenantResolver {
	if cfg.Enabled == nil || !*cfg.Enabled {
		return nil
	}
	header := strings.TrimSpace(cfg.Header)
	if header == "" {
		header = DefaultTenantHeader
	}
	roles := make([]string, 0, len(cfg.Roles))
	for _, r := range cfg.Roles {
		if r = strings.TrimSpace(r); r != "" {
			roles = append(roles, r)
		}
	}
	return &headerTenantResolver{header: header, roles: roles, anyone: authMode == "none"}
}

func (r *headerTenantResolver) Resolve(ctx context.Context, req *http.Request, p *host.Principal) (string, error) {
	own := ""
	if p != nil {
		own = p.TenantID
	}
	if own == "" {
		own = defaultTenantID
	}
	want := strings.TrimSpace(req.Header.Get(r.header))
	if want == "" || want == own {
		return own, nil
	}
	if !r.CanSwitchTenant(ctx, p) {
		// A 403 rather than a silent fall back to the caller's own tenant: a
		// console that sent the header believes it is looking at another
		// tenant, and answering with this one's data would be worse than
		// refusing.
		return "", fmt.Errorf("%w: %s may not select a tenant", host.ErrForbidden, r.header)
	}
	return want, nil
}

// CanSwitchTenant reports whether this principal may name a tenant
// (host.TenantSwitcher). It is what GET /api/v1/whoami answers
// can_switch_tenant with.
func (r *headerTenantResolver) CanSwitchTenant(_ context.Context, p *host.Principal) bool {
	if r.anyone {
		return true
	}
	if p == nil || len(r.roles) == 0 {
		return false
	}
	for _, role := range p.Roles {
		if slices.Contains(r.roles, role) {
			return true
		}
	}
	return false
}

// DefaultTenantHeader is the header auth.tenant_header reads when it names
// none. It is prefixed like sendplane's other own headers so that a proxy
// stripping `X-Sendplane-*` from untrusted traffic catches it too.
const DefaultTenantHeader = "X-Sendplane-Tenant"

// defaultTenantID matches sendplane.DefaultTenantID: the tenant a principal
// with none resolves to.
const defaultTenantID = "default"

// systemTenantID is re-exported for the config validator's error message.
const systemTenantID = store.SystemTenantID
