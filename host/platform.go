package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// Platform is the operator's shared sending infrastructure: the transports,
// sending domains, From identities and probe/bounce mailboxes it runs for its
// tenants (ADR-0017, architecture 5.4).
//
// It is *configuration*, not data. sendplane never writes any of it to the
// store: a read resolves it from this struct every time, and only runtime
// state about it — a transport's circuit status, a mailbox's reachability, a
// sender's probe verdict — is persisted, in the system tenant, as a shadow row
// that carries no configuration field at all.
//
// The same decision is why there is no tenant registry. A shared sender's From
// address is a Liquid template over the tenant attributes a request carries
// (Hooks.TenantVars), so the operator's own database stays the only place a
// tenant's name and slug live.
//
// The types are aliases of the store's, which is where they are declared so
// that the platform overlay can build virtual entities from them without
// importing this package.
type (
	// Platform is the whole shared catalog. Options.Platform takes one.
	Platform = store.PlatformCatalog
	// PlatformTransport is a shared SMTP account.
	PlatformTransport = store.PlatformTransport
	// PlatformDomain is a shared sending domain (DKIM, return path).
	PlatformDomain = store.PlatformDomain
	// PlatformSender is a shared From identity with templated addresses.
	PlatformSender = store.PlatformSender
	// PlatformProbeMailbox is a shared loopback probe mailbox.
	PlatformProbeMailbox = store.PlatformProbeMailbox
	// PlatformBounceMailbox is a shared bounce mailbox.
	PlatformBounceMailbox = store.PlatformBounceMailbox

	// UseKind is what a sender is being used for.
	UseKind = store.UseKind
)

// The uses a sender may be restricted to.
const (
	UseCampaign      = store.UseCampaign
	UseTransactional = store.UseTransactional
	UseProbe         = store.UseProbe
)

// ErrSenderUseDenied is what a SenderPolicy returns to refuse a send. The API
// answers 403 sender_use_denied with the error's message, so the message is
// shown to the caller and should say what would be allowed instead.
//
// Any non-nil error from the hook is a denial; the sentinel exists so that a
// host chaining DefaultSenderPolicy can tell a refusal from a bug in its own
// code with errors.Is.
var ErrSenderUseDenied = errors.New("sendplane: sender use denied")

// SenderUse is one request to send something with a sender, handed to
// Hooks.SenderPolicy before anything is written or queued.
type SenderUse struct {
	TenantID string
	// Principal is the authenticated caller, nil for a use that no request
	// triggered (the probe loop).
	Principal *Principal

	SenderID string
	// Shared is true when SenderID names a platform sender (store.IsPlatformID).
	// A policy that only cares about shared senders checks this first: a
	// tenant's own sender is its own business.
	Shared bool

	Kind UseKind
	// TenantVars are the tenant attributes the request carried, after
	// Hooks.TenantVars. A policy keyed on the host's own notion of a plan
	// ("the free plan may not use the shared sender for campaigns") reads them
	// from here rather than looking the tenant up again.
	TenantVars map[string]any
}

// DefaultSenderPolicy is the policy sendplane applies when Hooks.SenderPolicy
// is nil: a platform sender may only be used for what its configuration's
// `uses` list names, and a tenant's own sender may be used for anything.
//
// It is exported so that a host replacing the hook can still enforce the
// configured list and add to it:
//
//	base := host.DefaultSenderPolicy(opts.Platform)
//	opts.Hooks.SenderPolicy = func(ctx context.Context, u host.SenderUse) error {
//	    if err := base(ctx, u); err != nil {
//	        return err
//	    }
//	    if u.Kind == host.UseCampaign && plan(u.TenantVars) == "free" {
//	        return fmt.Errorf("%w: the free plan cannot run campaigns on the shared sender",
//	            host.ErrSenderUseDenied)
//	    }
//	    return nil
//	}
func DefaultSenderPolicy(p Platform) func(context.Context, SenderUse) error {
	cat := p.Normalize()
	return func(_ context.Context, u SenderUse) error {
		if !store.IsPlatformID(u.SenderID) {
			return nil
		}
		if u.Kind == UseProbe && u.TenantID == store.SystemTenantID {
			// The loopback probe of a shared sender runs in the system tenant,
			// once, on the operator's own behalf (architecture 11.2). It is
			// not a use a tenant requested, so the tenant-facing `uses` list
			// does not govern it — an operator who has restricted its shared
			// sender to transactional mail still wants to know whether that
			// mail arrives.
			return nil
		}
		allowed := cat.SenderUses(u.SenderID)
		if len(allowed) == 0 {
			// The sender is not in the catalog at all. Denying is the safe
			// answer: the reference is stale, so nothing has vouched for it.
			return fmt.Errorf("%w: sender %s is not a configured platform sender",
				ErrSenderUseDenied, u.SenderID)
		}
		if slices.Contains(allowed, u.Kind) {
			return nil
		}
		return fmt.Errorf("%w: the shared sender %s may only be used for %s, not %s",
			ErrSenderUseDenied, u.SenderID, joinUses(allowed), u.Kind)
	}
}

func joinUses(uses []UseKind) string {
	out := make([]string, 0, len(uses))
	for _, u := range uses {
		out = append(out, u.String())
	}
	return strings.Join(out, ", ")
}

// ErrTemplateUseDenied is what a TemplatePolicy returns to refuse a send. The
// API answers 403 template_use_denied with the error's message.
var ErrTemplateUseDenied = errors.New("sendplane: template use denied")

// TemplateUse is one request to send with a template, handed to
// Hooks.TemplatePolicy before anything is written or queued (ADR-0018).
type TemplateUse struct {
	TenantID string
	// Principal is the authenticated caller.
	Principal *Principal

	TemplateID string
	// TemplateKey is the template's key, empty for a template without one.
	TemplateKey string
	// Shared is true when the template is the system tenant's shared one,
	// read through by this tenant. A tenant's own template — including its
	// override of a shared one — is not shared.
	Shared bool
	// Uses is the template's own `uses` list: what its author restricted it
	// to. Empty means unrestricted.
	Uses []UseKind

	Kind UseKind
	// TenantVars are the tenant attributes the request carried, after
	// Hooks.TenantVars.
	TenantVars map[string]any
}

// DefaultTemplatePolicy is the policy sendplane applies when
// Hooks.TemplatePolicy is nil: a shared template may only be used for what its
// `uses` names, and a tenant's own template for anything.
//
// It is exported so a host replacing the hook can keep it and add to it, the
// same way DefaultSenderPolicy is chained:
//
//	opts.Hooks.TemplatePolicy = func(ctx context.Context, u host.TemplateUse) error {
//	    if err := host.DefaultTemplatePolicy(ctx, u); err != nil {
//	        return err
//	    }
//	    if u.Shared && plan(u.TenantVars) == "free" && u.Kind == host.UseCampaign {
//	        return fmt.Errorf("%w: the free plan cannot run campaigns from shared templates",
//	            host.ErrTemplateUseDenied)
//	    }
//	    return nil
//	}
func DefaultTemplatePolicy(_ context.Context, u TemplateUse) error {
	if !u.Shared || len(u.Uses) == 0 || slices.Contains(u.Uses, u.Kind) {
		return nil
	}
	name := u.TemplateKey
	if name == "" {
		name = u.TemplateID
	}
	return fmt.Errorf("%w: the shared template %s may only be used for %s, not %s",
		ErrTemplateUseDenied, name, joinUses(u.Uses), u.Kind)
}
