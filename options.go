package sendplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Errors returned across the embedding API. Hosts match them with errors.Is.
var (
	// ErrUnauthenticated is returned by an Authenticator when the request
	// carries no usable credentials.
	ErrUnauthenticated = errors.New("sendplane: unauthenticated")
	// ErrForbidden is returned by an Authorizer when the principal may not
	// perform the action on the resource.
	ErrForbidden = errors.New("sendplane: forbidden")
	// ErrNotImplemented marks the parts of the API that are not built yet.
	ErrNotImplemented = errors.New("sendplane: not implemented")
)

// Options configures New. Store and Auth are required; everything else has a
// default (see New).
type Options struct {
	Store   store.Provider // required
	Auth    Authenticator  // required. request -> Principal
	Authz   Authorizer     // optional. default: allow every authenticated principal
	Tenants TenantResolver // optional. default: Principal.TenantID, else "default"
	Hooks   Hooks
	Secrets SecretCipher // at-rest encryption for SMTP/IMAP passwords and DKIM keys
	Limits  Limits       // zero fields fall back to DefaultLimits
	Logger  *slog.Logger // default: slog.Default()
	Clock   func() time.Time
}

// Principal is the authenticated caller. Roles and Attrs are host-defined;
// sendplane never interprets them.
type Principal struct {
	ID       string
	TenantID string
	Roles    []string
	Attrs    map[string]any
}

// Authenticator turns an HTTP request into a Principal. It returns an error
// wrapping ErrUnauthenticated when the request cannot be authenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// Action is the closed set of permissions sendplane defines. Hosts map these
// onto their own RBAC.
type Action string

const (
	ActionTemplateRead        Action = "template.read"
	ActionTemplateWrite       Action = "template.write"
	ActionCampaignRead        Action = "campaign.read"
	ActionCampaignWrite       Action = "campaign.write"
	ActionCampaignSend        Action = "campaign.send"
	ActionMessageSend         Action = "message.send"
	ActionSenderWrite         Action = "sender.write"
	ActionDeliveryRead        Action = "delivery.read"
	ActionEventRead           Action = "event.read"
	ActionProbeRun            Action = "probe.run"
	ActionTrackingConfigWrite Action = "tracking.config.write"
	ActionSuppressionWrite    Action = "suppression.write"
)

// Resource identifies what an Action is performed on.
type Resource struct{ Kind, ID, TenantID string }

// Authorizer decides whether a principal may perform an action. It returns an
// error wrapping ErrForbidden when it may not.
type Authorizer interface {
	Authorize(ctx context.Context, p *Principal, a Action, r Resource) error
}

// TenantResolver maps a request to a tenant ID.
type TenantResolver interface {
	Resolve(ctx context.Context, r *http.Request, p *Principal) (tenantID string, err error)
}

// SecretCipher encrypts secrets (SMTP/IMAP passwords, DKIM private keys,
// tracking signing keys) before they reach the store.
type SecretCipher interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// Limits bounds request sizes. A zero field takes the DefaultLimits value.
type Limits struct {
	MaxRecipientsPerCampaign int   // recipients accepted for one campaign
	MaxVarsBytes             int   // JSON size of one recipient's vars
	MaxBodyBytes             int64 // template/layout body size
	MaxRecipientLineBytes    int   // one NDJSON ingest line
}

// DefaultLimits are applied to any zero field of Options.Limits.
var DefaultLimits = Limits{
	MaxRecipientsPerCampaign: 10_000_000,
	MaxVarsBytes:             8 << 10,
	MaxBodyBytes:             2 << 20,
	MaxRecipientLineBytes:    64 << 10,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxRecipientsPerCampaign == 0 {
		l.MaxRecipientsPerCampaign = d.MaxRecipientsPerCampaign
	}
	if l.MaxVarsBytes == 0 {
		l.MaxVarsBytes = d.MaxVarsBytes
	}
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = d.MaxBodyBytes
	}
	if l.MaxRecipientLineBytes == 0 {
		l.MaxRecipientLineBytes = d.MaxRecipientLineBytes
	}
	return l
}

// allowAll is the default Authorizer: anything an authenticated principal asks.
type allowAll struct{}

func (allowAll) Authorize(context.Context, *Principal, Action, Resource) error { return nil }

// principalTenant is the default TenantResolver.
type principalTenant struct{}

func (principalTenant) Resolve(_ context.Context, _ *http.Request, p *Principal) (string, error) {
	if p != nil && p.TenantID != "" {
		return p.TenantID, nil
	}
	return DefaultTenantID, nil
}

// DefaultTenantID is used when neither a TenantResolver nor the Principal names
// a tenant.
const DefaultTenantID = "default"
