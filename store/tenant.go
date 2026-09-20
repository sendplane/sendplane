package store

import (
	"context"
	"errors"
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
// keys can rotate while old links stay valid. Secret is stored encrypted by
// the host's SecretCipher.
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

	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultTenantSettings is the settings row created on first access.
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
		Tracking:           TrackingConfig{Opens: true, Clicks: true},
		CreatedAt:          now.UTC(),
		UpdatedAt:          now.UTC(),
	}
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
