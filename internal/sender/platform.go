package sender

import (
	"context"
	"sync"
	"time"

	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/store"
)

// Everything the send loop needs to know about a shared ("platform") sender
// (ADR-0017).
//
// Three things are different about one, and all three follow from the same
// rule: a platform resource is the operator's, not the tenant's.
//
//   - Its transport and sending domain are only visible in the system tenant's
//     view (store.PlatformView), so they are loaded from there rather than
//     from the tenant's own store. The tenant-facing visibility rules are not
//     relaxed to make this work.
//   - Its From address is a Liquid template over the send's tenant variables,
//     so it is rendered per delivery instead of read off the row.
//   - Its rate limit and its circuit state are cluster-wide, not per tenant:
//     one relay, one reputation. The per-tenant share is a second bucket on
//     top (architecture 8.2).

// platformState is the tenantState bound to the system tenant, through which
// the shared transports and domains are read. It is built once and shared by
// every tenant's processing, because a shared transport is the same row
// whoever is sending through it.
type platformSource struct {
	once sync.Once
	st   *tenantState
	err  error
}

// sysState returns the system-tenant state, building it on first use. It
// returns nil when the provider cannot serve the system tenant, which the
// caller turns into a requeue rather than a failure: nothing about the
// delivery is wrong.
func (s *Sender) sysState(ctx context.Context) (*tenantState, error) {
	s.platform.once.Do(func() {
		st, err := store.PlatformView(ctx, s.provider)
		if err != nil {
			s.platform.err = err
			return
		}
		s.platform.st = newTenantState(store.SystemTenantID, st, s.cfg.TenantCacheTTL)
	})
	return s.platform.st, s.platform.err
}

// configState is the state a configuration row with this ID has to be read
// from: the tenant's own for a tenant's own row, the system tenant's for a
// platform one.
func (s *Sender) configState(ctx context.Context, t *tenantState, id string) (*tenantState, error) {
	// A caller that is already the system tenant keeps its own state, so the
	// two do not each hold a cache of the same rows and invalidate only one.
	if !store.IsPlatformID(id) || t.id == store.SystemTenantID {
		return t, nil
	}
	return s.sysState(ctx)
}

// transportFor loads the transport a sender routes through.
func (s *Sender) transportFor(ctx context.Context, t *tenantState, id string, now time.Time) (*store.Transport, error) {
	st, err := s.configState(ctx, t, id)
	if err != nil {
		return nil, err
	}
	return st.transport(ctx, id, now)
}

// domainFor loads the sending domain a sender signs with. An empty ID is no
// domain, which is not an error (the relay signs).
func (s *Sender) domainFor(ctx context.Context, t *tenantState, id string, now time.Time) (*store.SendingDomain, error) {
	st, err := s.configState(ctx, t, id)
	if err != nil {
		return nil, err
	}
	return st.domain(ctx, id, now)
}

// limitScope is the identity a transport's rate buckets and circuit state are
// keyed by.
//
// For a tenant's own transport it is the tenant: two tenants with their own
// relay accounts share nothing. For a shared one it is the system tenant,
// because the configured RatePerSecond is the whole relay's capacity and
// keying it per tenant would multiply it by the number of tenants sending.
func limitScope(tenantID string, tr *store.Transport) string {
	if tr.Shared {
		return store.SystemTenantID
	}
	return tenantID
}

// perTenantRate is the shared transport's per-tenant share, from
// configuration. Zero means no per-tenant cap.
//
// It lives in the catalog rather than on store.Transport because it is
// configuration of a platform resource, and platform configuration is never
// stored (ADR-0017).
func (s *Sender) perTenantRate(tr *store.Transport) float64 {
	if !tr.Shared || s.cfg.Platform == nil {
		return 0
	}
	cfg, ok := s.cfg.Platform.Catalog().Transport(tr.ID)
	if !ok {
		return 0
	}
	return cfg.PerTenantRatePerSecond
}

// tenantBucket is the (transport, tenant) bucket name of architecture 8.2's
// fair share. It is distinct from the transport bucket, which a shared
// transport keys under the system tenant, so the two do not collide.
func tenantBucket(transportKey, tenantID string) string {
	return transportKey + "|tenant|" + tenantID
}

// resolveFrom is the From identity a delivery is sent with: the sender row's
// own fields, or, for a shared sender, its templates rendered with this send's
// tenant variables.
//
// A template that cannot be rendered — a missing variable, an address that
// comes out malformed — is a permanent failure, not a retry: the same inputs
// produce the same result forever. The API refuses such a request up front
// (422 tenant_vars_missing), so reaching this is either a request that
// predates a config change or a host hook that stopped supplying a variable.
func (s *Sender) resolveFrom(snd *store.Sender, tenantVars map[string]any) (platform.From, error) {
	if !snd.Shared {
		return platform.From{
			Name: snd.FromName, Email: snd.FromEmail, ReplyTo: snd.ReplyTo,
		}, nil
	}
	if s.cfg.Platform == nil {
		return platform.From{}, &missingPlatformError{senderID: snd.ID}
	}
	return s.cfg.Platform.Resolve(snd.ID, tenantVars)
}

// missingPlatformError is what a shared sender resolves to in a process that
// was not configured with the platform catalog. It cannot happen through the
// root package, which builds both from the same Options, and it is an explicit
// error rather than a nil-map render so that a wiring mistake says so.
type missingPlatformError struct{ senderID string }

func (e *missingPlatformError) Error() string {
	return "sender: " + e.senderID + " is a shared sender but this process has no platform configuration"
}

// tenantVarsFor is the tenant variable set a delivery renders with: its own
// for a transactional or probe delivery, its campaign's for a campaign one.
//
// Campaign deliveries deliberately carry none of their own: a campaign has one
// set, and copying it onto every row would be a million copies of the same
// object (ADR-0002, store.Delivery.TenantVars).
func tenantVarsFor(d *store.Delivery, c *store.Campaign) map[string]any {
	if len(d.TenantVars) > 0 {
		return d.TenantVars
	}
	if c != nil {
		return c.TenantVars
	}
	return nil
}

// platformSuppressed reports whether the address is on the *platform*
// suppression list, which is the system tenant's own.
//
// A hard bounce or a complaint against a shared relay damages the reputation
// every tenant on it depends on, so the address is suppressed once for the
// platform as well as for the tenant that sent to it, and a send through a
// shared transport checks both lists (ADR-0008, architecture 10). A tenant's
// own transport is unaffected: its reputation is its own.
func (s *Sender) platformSuppressed(
	ctx context.Context, tr *store.Transport, emailNorm string, now time.Time,
) (bool, error) {
	if !tr.Shared {
		return false, nil
	}
	sys, err := s.sysState(ctx)
	if err != nil {
		return false, err
	}
	ok, _, err := sys.st.Suppressions().IsSuppressed(ctx, emailNorm, now)
	return ok, err
}

// sharedSenderReason is the campaign-start and delivery-failure reason a
// tenant is told when a shared sender is unhealthy.
//
// It carries no verdict detail on purpose: "dkim=fail on the shared domain" is
// the operator's diagnosis and would tell one tenant about the platform's
// state (ADR-0017).
const sharedSenderReason = "shared sender unavailable"

// FailureReason is what a tenant may be told about why a send could not go
// out: the real reason for its own resources, the generic one for a shared
// resource.
func FailureReason(shared bool, reason string) string {
	if !shared {
		return reason
	}
	return sharedSenderReason
}

// SharedSenderReason is the generic reason string, exported so that the HTTP
// layer says exactly the same thing on campaign start.
func SharedSenderReason() string { return sharedSenderReason }

// probeVarsFor is the tenant variable set a loopback probe of a shared sender
// renders with, from configuration (PlatformSender.ProbeVars).
func (s *Sender) probeVarsFor(senderID string) map[string]any {
	if s.cfg.Platform == nil {
		return nil
	}
	spec, ok := s.cfg.Platform.Spec(senderID)
	if !ok {
		return nil
	}
	return spec.ProbeVars
}

// sharedTransportKey is the rate-limiter bucket name of a transport, scoped
// the way limitScope says.
func sharedTransportKey(tenantID string, tr *store.Transport) string {
	return healthKey(limitScope(tenantID, tr), tr.ID)
}
