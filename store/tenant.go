package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"time"
)

// UnsubscribeMode selects where the unsubscribe link in an email points
// (ADR-0011).
type UnsubscribeMode string

const (
	// UnsubscribeSendplane puts a signed sendplane tracking URL in the mail and
	// redirects to the host destination after recording the click.
	UnsubscribeSendplane UnsubscribeMode = "sendplane"
	// UnsubscribeHost puts the host URL in the mail; the host notifies
	// sendplane through the API.
	UnsubscribeHost UnsubscribeMode = "host"
	// UnsubscribeNone omits the link and headers (transactional default).
	UnsubscribeNone UnsubscribeMode = "none"
)

// RetryPolicy drives the deferred/failed decision for transient errors
// (architecture 4.2).
type RetryPolicy struct {
	// Backoff is the schedule applied to attempt 1, 2, ... The last entry is
	// reused if MaxAttempts exceeds its length.
	Backoff []time.Duration
	// MaxAttempts is how many transient/rate_limited attempts a delivery may
	// consume before it fails.
	MaxAttempts int
}

// SigningKey is an HMAC key for tracking tokens, identified by KID so that
// keys can rotate while old links stay valid.
//
// Secret is the raw HMAC key and is stored AS-IS: it is NOT passed through
// the host's SecretCipher, unlike an SMTP/IMAP password or a DKIM private
// key. Every path that verifies a tracking token, a VERP address or a
// one-click unsubscribe reads the key straight off the settings row, on
// replicas that may have no cipher configured at all, and a value that had
// been encrypted would verify nothing. Nothing in this repository encrypts
// it, and the API never returns it (architecture 16); keeping the settings
// row out of reach is the host's job.
type SigningKey struct {
	KID       string
	Secret    []byte
	CreatedAt time.Time
}

// TrackingConfig is the tenant's tracking setup (architecture 9).
type TrackingConfig struct {
	// Domain is the tenant tracking domain (t.example.com) that must route to
	// the control public endpoints. Required for opens, clicks and
	// UnsubscribeSendplane.
	Domain      string
	Opens       bool
	Clicks      bool
	SigningKeys []SigningKey
}

// TenantSettings holds everything sendplane knows about a tenant. sendplane
// does not manage tenant lifecycle; settings are created on first access
// (ADR-0006).
type TenantSettings struct {
	TenantID string

	Retry              RetryPolicy
	RetentionDays      int
	SuppressionEnabled bool
	// BounceRetainRaw keeps the whole bounce message on the BounceEvent
	// (architecture 10: "raw 보존은 테넌트 설정"). Off by default: a returned
	// original can be megabytes and it is kept for diagnosis, not archival.
	BounceRetainRaw bool

	UnsubscribeMode        UnsubscribeMode
	UnsubscribeURLTemplate string // Liquid, evaluated per recipient
	// UnsubscribeOneClick declares that the unsubscribe destination accepts an
	// RFC 8058 POST. It only matters in UnsubscribeHost mode, where the host
	// owns the endpoint: sendplane adds List-Unsubscribe-Post only when the
	// tenant has said so, because announcing one-click on an endpoint that
	// answers a POST with a login page makes mailbox providers treat the
	// unsubscribe as failed. In UnsubscribeSendplane mode sendplane owns the
	// endpoint and sets the header itself (ADR-0011).
	UnsubscribeOneClick bool

	DefaultLocale string
	Tracking      TrackingConfig

	// PlatformDefaults names the fields whose value came from the operator's
	// platform configuration rather than from this row, because the tenant
	// had set none (PlatformCatalog.TrackingDomain,
	// PlatformCatalog.UnsubscribeURLTemplate).
	//
	// It is computed on read by the platform overlay and is never stored: no
	// backend has a column for it, and Update strips a field it names back out
	// so that reading the effective value and writing it back does not turn
	// the operator's default into the tenant's own copy of it (ADR-0017).
	//
	// It exists so that every consumer — the sender, the control plane, the
	// settings endpoint — sees one effective value without each applying the
	// fallback itself, while the settings endpoint can still say which one is
	// in force.
	PlatformDefaults []string

	// EventTypes is the tenant's outbox subscription (architecture 12).
	// Empty means the default set: everything sendplane emits except the
	// per-recipient firehose that a bulk campaign turns into one outbox row
	// per recipient. A non-empty list is exact — only the types it names are
	// enqueued, and only they are dispatched.
	//
	// The filter is applied twice on purpose: when the event is enqueued, so
	// a tenant that never wanted delivery.sent never pays for the rows, and
	// again when it is dispatched, so unsubscribing stops delivery of the
	// rows already queued.
	EventTypes []string

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultOffEventTypes are the event types an empty TenantSettings.EventTypes
// does NOT subscribe to (architecture 12: "대량 캠페인에서 delivery 단위
// 이벤트는 폭주하므로 테넌트별 구독 필터(기본: 실패류만)").
//
// All three are per-recipient events of a successful path, so a campaign with
// a million recipients produces a million outbox rows, one HTTP POST batch
// after another, for information the host usually reads out of the delivery
// listing instead. A tenant that does want them says so explicitly.
var DefaultOffEventTypes = []string{
	"delivery.sent",
	"delivery.opened",
	"delivery.clicked",
}

// SubscribedTo reports whether this tenant wants outbox events of type typ.
// A nil receiver, like an empty EventTypes, means the default set: everything
// but DefaultOffEventTypes.
func (s *TenantSettings) SubscribedTo(typ string) bool {
	if s == nil || len(s.EventTypes) == 0 {
		return !slices.Contains(DefaultOffEventTypes, typ)
	}
	return slices.Contains(s.EventTypes, typ)
}

// NewSigningKey mints a tracking token key: 32 random bytes and a short random
// KID. The KID is generated separately from the secret, not derived from it,
// so that publishing it in every link reveals nothing about the key.
func NewSigningKey(now time.Time) SigningKey {
	var buf [36]byte
	// crypto/rand.Read fills the buffer completely and never returns an error
	// (it crashes the program if the system source fails), so there is nothing
	// here to fall back to.
	_, _ = rand.Read(buf[:])
	return SigningKey{
		KID:       hex.EncodeToString(buf[32:]),
		Secret:    append([]byte(nil), buf[:32]...),
		CreatedAt: TruncateTime(now),
	}
}

// DefaultTenantSettings is the settings row created on first access.
//
// It carries a freshly generated signing key. Opens, clicks, one-click
// unsubscribe and VERP bounce correlation all sign with one, so a tenant that
// nobody has configured yet should track rather than silently not; rotating or
// replacing the key is a settings PUT away.
func DefaultTenantSettings(tenantID string, now time.Time) *TenantSettings {
	return &TenantSettings{
		TenantID: tenantID,
		Retry: RetryPolicy{
			Backoff: []time.Duration{
				time.Minute, 5 * time.Minute, 15 * time.Minute,
				time.Hour, 4 * time.Hour, 12 * time.Hour,
			},
			MaxAttempts: 6,
		},
		RetentionDays:      90,
		SuppressionEnabled: true,
		UnsubscribeMode:    UnsubscribeSendplane,
		DefaultLocale:      "en",
		Tracking: TrackingConfig{
			Opens: true, Clicks: true,
			SigningKeys: []SigningKey{NewSigningKey(now)},
		},
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
}

// The fields PlatformDefaults may name.
const (
	SettingTrackingDomain         = "tracking.domain"
	SettingUnsubscribeURLTemplate = "unsubscribe_url_template"
)

// FromPlatform reports whether the named field's effective value came from the
// platform configuration.
func (s *TenantSettings) FromPlatform(field string) bool {
	if s == nil {
		return false
	}
	return slices.Contains(s.PlatformDefaults, field)
}

// TenantSettingsRepo is a singleton per tenant.
type TenantSettingsRepo interface {
	Get(ctx context.Context) (*TenantSettings, error)
	Create(ctx context.Context, s *TenantSettings) error
	Update(ctx context.Context, s *TenantSettings) error
}

// LoadTenantSettings returns the tenant's settings, creating the default row
// on first access (ADR-0006: sendplane does not manage tenant lifecycle, so
// the row appears when something first needs it).
//
// A concurrent creator is not an error: ErrConflict means somebody else won
// the race, and the row it wrote is read back instead. Callers get settings or
// an error, never a silent fallback to defaults that nothing persisted.
func LoadTenantSettings(ctx context.Context, st Store, tenantID string, now time.Time) (*TenantSettings, error) {
	repo := st.TenantSettings()
	s, err := repo.Get(ctx)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	def := DefaultTenantSettings(tenantID, now)
	if err := repo.Create(ctx, def); err != nil {
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		return repo.Get(ctx)
	}
	return def, nil
}
