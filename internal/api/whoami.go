package api

import (
	"context"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// GetWhoami reports the authenticated caller and the tenant this request
// resolved to.
//
// It needs authentication and no permission: a console has to render its own
// chrome — which tenant it is looking at, whether a tenant switcher belongs on
// the page, whether the platform pages exist — before it knows which actions
// the caller holds, and an authorization failure at that point is
// indistinguishable from a broken deployment.
//
// It deliberately reports nothing the caller could not already infer. The
// principal ID and the roles are the host's own, echoed back; the tenant is
// whatever the host's TenantResolver returned for this very request; and
// can_switch_tenant asks the resolver itself (host.TenantSwitcher) rather than
// guessing from the roles, because the switch is the host's decision and this
// endpoint must not describe a capability the resolver would refuse.
func (s *server) GetWhoami(ctx context.Context, _ GetWhoamiRequestObject) (GetWhoamiResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	out := Whoami{
		TenantId:        t.id,
		SystemTenant:    t.id == store.SystemTenantID,
		CanSwitchTenant: s.canSwitchTenant(ctx, t.p),
	}
	if t.p != nil {
		out.PrincipalId = t.p.ID
		if len(t.p.Roles) > 0 {
			roles := append([]string(nil), t.p.Roles...)
			out.Roles = &roles
		}
	}
	return GetWhoami200JSONResponse(out), nil
}

// canSwitchTenant asks the configured TenantResolver. A resolver that does not
// implement host.TenantSwitcher cannot switch, which is the honest answer for
// the default one: it returns the principal's own tenant and nothing else.
func (s *server) canSwitchTenant(ctx context.Context, p *host.Principal) bool {
	sw, ok := s.deps.Tenants.(host.TenantSwitcher)
	if !ok {
		return false
	}
	return sw.CanSwitchTenant(ctx, p)
}
