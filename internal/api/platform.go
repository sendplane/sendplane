package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/internal/sender"
	"github.com/sendplane/sendplane/store"
)

// The request-time half of the multi-tenant SaaS model (ADR-0017).
//
// Three checks run before anything is queued, and all three exist so that a
// failure is a 4xx on the request rather than a million failed deliveries
// hours later:
//
//   - tenantVars runs the host's TenantVars hook. What it returns is what is
//     stored and what every template sees as `tenant`; the request's own
//     values are only a suggestion, because sendplane keeps no tenant
//     registry and cannot itself tell whose slug "acme" is.
//   - checkSenderUse runs the sender-use policy: may this tenant use this
//     sender for a campaign, a transactional message, a probe.
//   - resolveSharedFrom renders a shared sender's From templates with the
//     variables that just came back, so a missing one is
//     422 tenant_vars_missing naming the keys.

// tenantVars applies Hooks.TenantVars to the request's `tenant_vars`.
//
// A nil hook is pass-through, which is right for a single-tenant deployment
// and a trust decision anywhere else: without a hook, whatever the caller
// sent is what a shared sender's From address is built from. That is why the
// hook's documentation spells out the substituting implementation.
func (s *server) tenantVars(ctx context.Context, t *tenant, requested *TenantVars) (map[string]any, error) {
	in := map[string]any(nil)
	if requested != nil {
		in = map[string]any(*requested)
	}
	if s.deps.Hooks.TenantVars == nil {
		return in, nil
	}
	out, err := s.deps.Hooks.TenantVars(ctx, t.p, t.id, in)
	if err != nil {
		// ErrForbidden maps to 403; anything else the host returns is a
		// rejection of the request's variables, which is a 422.
		if errors.Is(err, host.ErrForbidden) || errors.Is(err, host.ErrUnauthenticated) {
			return nil, err
		}
		return nil, errInvalid("tenant_vars: %v", err)
	}
	return out, nil
}

// checkSenderUse enforces the sender-use policy (Hooks.SenderPolicy, default
// host.DefaultSenderPolicy). Any error is a denial: 403 sender_use_denied
// carrying the policy's own message, which is what tells the caller what
// would have been allowed.
func (s *server) checkSenderUse(
	ctx context.Context, t *tenant, senderID string, kind store.UseKind, vars map[string]any,
) error {
	if s.deps.Hooks.SenderPolicy == nil {
		return nil
	}
	use := host.SenderUse{
		TenantID: t.id, Principal: t.p, SenderID: senderID,
		Shared: store.IsPlatformID(senderID), Kind: kind, TenantVars: vars,
	}
	if err := s.deps.Hooks.SenderPolicy(ctx, use); err != nil {
		return &apiError{
			status: http.StatusForbidden, code: ErrorCodeSenderUseDenied,
			message: err.Error(), cause: err,
		}
	}
	return nil
}

// checkSharedFrom renders a shared sender's From templates with the resolved
// tenant variables, so that a variable the templates need and the request did
// not supply is refused here.
//
// It is a no-op for a tenant's own sender, whose From address is a literal.
func (s *server) checkSharedFrom(senderID string, shared bool, vars map[string]any) error {
	if !shared || s.deps.Platform == nil {
		return nil
	}
	if _, err := s.deps.Platform.Resolve(senderID, vars); err != nil {
		return senderFromError(err)
	}
	return nil
}

// senderFromError maps the resolver's two failures onto the spec's codes.
func senderFromError(err error) error {
	var missing *platform.MissingVarsError
	if errors.As(err, &missing) {
		details := make([]ErrorDetail, 0, len(missing.Keys))
		for _, key := range missing.Keys {
			details = append(details, ErrorDetail{
				Field:   ptr("tenant_vars." + key),
				Message: "required by the shared sender's From template",
			})
		}
		return &apiError{
			status: http.StatusUnprocessableEntity, code: ErrorCodeTenantVarsMissing,
			message: "tenant_vars is missing " + strings.Join(missing.Keys, ", ") +
				", which the shared sender's From template needs",
			details: details, cause: err,
		}
	}
	if errors.Is(err, platform.ErrInvalidFrom) {
		return &apiError{
			status: http.StatusUnprocessableEntity, code: ErrorCodeTenantVarsMissing,
			message: err.Error(), cause: err,
		}
	}
	return err
}

// senderUses is the `uses` a shared sender reports. Nil for a tenant's own
// sender, which may be used for anything.
func (s *server) senderUses(snd *store.Sender) *[]SenderUses {
	if !snd.Shared || s.deps.Platform == nil {
		return nil
	}
	spec, ok := s.deps.Platform.Spec(snd.ID)
	if !ok {
		return nil
	}
	out := make([]SenderUses, 0, len(spec.Uses))
	for _, u := range spec.Uses {
		out = append(out, SenderUses(u))
	}
	return &out
}

// senderVars is the tenant variable set to validate a send against.
//
// A campaign carries its own from when it was created, so starting one does
// not need the caller to send them again; a transactional message carries the
// request's.
func campaignTenantVars(c *store.Campaign) map[string]any { return c.TenantVars }

// refuseSystemTenantSend is the system tenant's one restriction: it is the
// operator's own scope for platform state and platform probes, not a tenant
// that mails anybody.
//
// Letting it send would give the operator a tenant whose deliveries no tenant
// listing reports (Provider.Tenants never returns it), so its campaigns would
// never be scheduled and its stats never recomputed. Refusing at the door is
// the honest version of that.
func refuseSystemTenantSend(t *tenant, what string) error {
	if t.id != store.SystemTenantID {
		return nil
	}
	return errInvalid(
		"the system tenant cannot %s: it is the operator's scope for platform "+
			"configuration and platform probe state, not a sending tenant", what)
}

// checkSenderAssignable is the tenant-facing rule for the two reference
// fields of a sender a tenant creates or edits:
//
//   - A platform transport or domain cannot be assigned. A tenant sends
//     through a shared relay by using the shared *sender* the operator
//     configured, with the From address the operator chose; letting it point
//     its own sender at the relay would let it send as anything it likes over
//     somebody else's reputation.
//   - from_email must be on a sending domain this tenant owns, matched on the
//     domain exactly. Without it a tenant could put another tenant's domain in
//     its From address.
//
// The system tenant is exempt: its rows are the managed ones the operator
// writes, and it has no sending domains of its own to own an address on.
func (s *server) checkSenderAssignable(
	ctx context.Context, t *tenant, fromEmail, transportID, domainID string,
) error {
	if t.id == store.SystemTenantID {
		return nil
	}
	if store.IsPlatformID(transportID) {
		return &apiError{
			status: http.StatusUnprocessableEntity, code: ErrorCodeTransportNotAssignable,
			message: "transport " + transportID + " is a shared platform transport and cannot be " +
				"assigned to a sender; use the shared sender the operator configured instead",
		}
	}
	if store.IsPlatformID(domainID) {
		return &apiError{
			status: http.StatusUnprocessableEntity, code: ErrorCodeTransportNotAssignable,
			message: "sending domain " + domainID + " is a shared platform domain and cannot be " +
				"assigned to a sender; use the shared sender the operator configured instead",
		}
	}
	return s.checkFromDomainOwned(ctx, t, fromEmail)
}

// checkFromDomainOwned requires from_email's domain to be one of the tenant's
// own SendingDomain rows, compared exactly (no subdomain match: a sending
// domain is what DKIM and the return path are configured for, and
// "mail.acme.com" is not "acme.com").
func (s *server) checkFromDomainOwned(ctx context.Context, t *tenant, fromEmail string) error {
	want := domainOfAddress(fromEmail)
	if want == "" {
		return errInvalid("from_email has no domain")
	}
	page := store.Page{Limit: store.MaxPageLimit}
	var owned []string
	for {
		res, err := t.st.Domains().List(ctx, page)
		if err != nil {
			return err
		}
		for i := range res.Items {
			d := &res.Items[i]
			if d.Shared {
				// A shared domain is the operator's; it does not make a
				// tenant's From address legitimate.
				continue
			}
			if strings.EqualFold(d.Domain, want) {
				return nil
			}
			owned = append(owned, d.Domain)
		}
		if res.NextCursor == "" {
			break
		}
		page.Cursor = res.NextCursor
	}
	msg := "from_email is on " + want + ", which is not one of this tenant's sending domains"
	if len(owned) > 0 {
		msg += " (" + strings.Join(owned, ", ") + ")"
	} else {
		msg += "; add the sending domain first"
	}
	return &apiError{
		status: http.StatusUnprocessableEntity, code: ErrorCodeFromDomainNotOwned,
		message: msg,
	}
}

// domainOfAddress is the domain part of an already-normalized address.
func domainOfAddress(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 && i < len(addr)-1 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// checkCampaignSenderUse re-runs the policy for a campaign about to start,
// against the tenant variables it was created with.
//
// A campaign can sit in draft for days. The operator may have taken campaign
// out of its shared sender's `uses` since, or removed the sender altogether,
// and start is the last moment at which saying so costs one response instead
// of a million deliveries.
func (s *server) checkCampaignSenderUse(ctx context.Context, t *tenant, campaignID string) error {
	c, err := t.st.Campaigns().Get(ctx, campaignID)
	if err != nil {
		// StartCampaign reports a missing campaign properly; this is only the
		// policy pre-check, so it defers to it.
		return nil //nolint:nilerr // the caller reports the real error
	}
	if c.SenderID == "" {
		return nil
	}
	snd, err := t.st.Senders().Get(ctx, c.SenderID)
	if err != nil {
		return nil //nolint:nilerr // StartCampaign turns a missing sender into ErrNoSender
	}
	vars := campaignTenantVars(c)
	if err := s.checkSenderUse(ctx, t, snd.ID, store.UseCampaign, vars); err != nil {
		return err
	}
	if err := s.checkSharedFrom(snd.ID, snd.Shared, vars); err != nil {
		return err
	}
	return s.checkSharedSenderUsable(ctx, snd)
}

// probeTenantVars is what a probe of a sender renders with: the configured
// ProbeVars for a shared sender (there is no request carrying any), and
// nothing for a tenant's own, whose From address is a literal.
func (s *server) probeTenantVars(snd *store.Sender) map[string]any {
	if !snd.Shared || s.deps.Platform == nil {
		return nil
	}
	spec, ok := s.deps.Platform.Spec(snd.ID)
	if !ok {
		return nil
	}
	return spec.ProbeVars
}

// sharedSenderStatus is the one thing a tenant is told about a shared
// sender's health: whether it is red.
//
// The verdict itself — "dkim=fail on mail.example.com", the probe run, the
// mailbox it did not arrive in — is the operator's diagnosis of a reputation
// every tenant shares, and no tenant has any business reading another's share
// of it (ADR-0017). But "your campaign will not go out" is the tenant's
// business, so exactly one bit crosses the line, under the generic reason
// sender.SharedSenderReason().
//
// It reads the platform view (store.PlatformView) rather than the tenant's
// store, because that is where the state lives; that is also why it is here
// and not a relaxation of the overlay's visibility rules.
func (s *server) sharedSenderStatus(ctx context.Context, snd *store.Sender) (store.HealthStatus, error) {
	if !snd.Shared {
		return snd.Health, nil
	}
	pv, err := store.PlatformView(ctx, s.deps.Provider)
	if err != nil {
		return store.HealthUnknown, err
	}
	full, err := pv.Senders().Get(ctx, snd.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.HealthUnknown, nil
		}
		return store.HealthUnknown, err
	}
	return full.Health, nil
}

// checkSharedSenderUsable refuses to start a campaign on a shared sender the
// platform probe has judged red. It is the same precondition class as "no
// recipients" and "no version": something about the campaign means it cannot
// go out, and finding out now beats finding out per delivery.
func (s *server) checkSharedSenderUsable(ctx context.Context, snd *store.Sender) error {
	if !snd.Shared {
		return nil
	}
	status, err := s.sharedSenderStatus(ctx, snd)
	if err != nil {
		return err
	}
	if status != store.HealthRed {
		return nil
	}
	return &apiError{
		status: http.StatusUnprocessableEntity, code: ErrorCodePreconditionFailed,
		message: sender.SharedSenderReason(),
	}
}

// refusePlatformWrite answers a write that targets a platform resource before
// the handler reads anything.
//
// It has to come first, ahead of the Get every update handler starts with, or
// the answer depends on who is asking: a tenant's Get of a shared transport is
// ErrNotFound (it is not the tenant's to see), so the PUT would be a 404, and
// a shared sender's PUT would trip the "a platform transport is not
// assignable" check on its own transport_id and answer 422. Both are true
// statements about the wrong question. The caller asked to change a resource
// that is defined in the operator's config file, and `403 platform_read_only`
// is the answer to that in every scope (ADR-0017).
//
// It reveals only that the ID names a platform resource, which the `sys:`
// prefix already says.
func refusePlatformWrite(kind, id string) error {
	if !store.IsPlatformID(id) {
		return nil
	}
	return &apiError{
		status: http.StatusForbidden, code: ErrorCodePlatformReadOnly,
		message: kind + " " + id + " is defined in the platform configuration and " +
			"cannot be changed through the API; edit the operator's config and redeploy",
	}
}
