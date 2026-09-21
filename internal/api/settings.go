package api

import (
	"context"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// GetServiceHealth is the liveness probe. It is public and must stay cheap:
// the only work it does is an optional store round trip, and it never reports
// anything a scanner could learn from.
func (s *server) GetServiceHealth(ctx context.Context, _ GetServiceHealthRequestObject) (GetServiceHealthResponseObject, error) {
	out := ServiceHealth{Status: ServiceHealthStatusOk, Version: strPtr(s.deps.Version)}
	// store.Provider has no Ping in the contract, so this is an optional
	// interface: a Provider that offers one gets probed, the others report ok.
	if p, ok := s.deps.Provider.(interface{ Ping(context.Context) error }); ok {
		if err := p.Ping(ctx); err != nil {
			s.deps.Logger.Warn("sendplane: store ping failed", "err", err)
			out.Status = ServiceHealthStatusDegraded
			out.Store = ptr(ServiceHealthStoreError)
			return GetServiceHealth200JSONResponse(out), nil
		}
		out.Store = ptr(ServiceHealthStoreOk)
	}
	return GetServiceHealth200JSONResponse(out), nil
}

func (s *server) GetTenantSettings(ctx context.Context, _ GetTenantSettingsRequestObject) (GetTenantSettingsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := store.LoadTenantSettings(ctx, t.st, t.id, s.now())
	if err != nil {
		return nil, err
	}
	return GetTenantSettings200JSONResponse(settingsOut(cur)), nil
}

func (s *server) UpdateTenantSettings(ctx context.Context, req UpdateTenantSettingsRequestObject) (UpdateTenantSettingsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	cur, err := store.LoadTenantSettings(ctx, t.st, t.id, s.now())
	if err != nil {
		return nil, err
	}
	if err := s.applySettings(ctx, cur, *req.Body); err != nil {
		return nil, err
	}
	cur.Version = req.Body.Version
	if err := t.st.TenantSettings().Update(ctx, cur); err != nil {
		return nil, err
	}
	return UpdateTenantSettings200JSONResponse(settingsOut(cur)), nil
}

// applySettings copies a TenantSettingsUpdate over the stored row. It is a
// full replace of the fields the caller sent; an omitted field keeps its
// stored value, which is what makes the writeOnly signing secrets survive a
// GET/PUT round trip.
func (s *server) applySettings(ctx context.Context, cur *store.TenantSettings, in TenantSettingsUpdate) error {
	if in.Retry != nil {
		policy := store.RetryPolicy{MaxAttempts: cur.Retry.MaxAttempts}
		if in.Retry.MaxAttempts != nil {
			policy.MaxAttempts = int(*in.Retry.MaxAttempts)
		}
		if policy.MaxAttempts < 1 {
			return errInvalid("retry.max_attempts must be at least 1")
		}
		if in.Retry.Backoff != nil {
			for _, d := range *in.Retry.Backoff {
				parsed, err := parseDuration(d)
				if err != nil {
					return err
				}
				if parsed < 0 {
					return errInvalid("retry.backoff entries must not be negative")
				}
				policy.Backoff = append(policy.Backoff, parsed)
			}
		} else {
			policy.Backoff = cur.Retry.Backoff
		}
		cur.Retry = policy
	}
	if in.RetentionDays != nil {
		if *in.RetentionDays < 0 {
			return errInvalid("retention_days must not be negative")
		}
		cur.RetentionDays = int(*in.RetentionDays)
	}
	if in.SuppressionEnabled != nil {
		cur.SuppressionEnabled = *in.SuppressionEnabled
	}
	if in.BounceRetainRaw != nil {
		cur.BounceRetainRaw = *in.BounceRetainRaw
	}
	if mode, err := unsubscribeModeIn(in.UnsubscribeMode); err != nil {
		return err
	} else if mode != "" {
		cur.UnsubscribeMode = mode
	}
	if in.UnsubscribeUrlTemplate != nil {
		if err := noCRLF("unsubscribe_url_template", *in.UnsubscribeUrlTemplate); err != nil {
			return err
		}
		cur.UnsubscribeURLTemplate = *in.UnsubscribeUrlTemplate
	}
	if in.UnsubscribeOneClick != nil {
		cur.UnsubscribeOneClick = *in.UnsubscribeOneClick
	}
	if in.DefaultLocale != nil {
		cur.DefaultLocale = *in.DefaultLocale
	}
	if in.EventTypes != nil {
		types := make([]string, 0, len(*in.EventTypes))
		for _, typ := range *in.EventTypes {
			typ = strings.TrimSpace(typ)
			if typ == "" {
				return errInvalid("event_types must not contain an empty type")
			}
			types = append(types, typ)
		}
		// An empty array is not "subscribe to nothing" but "use the default
		// set", the same as never having set the field (architecture 12).
		// Storing nil rather than an empty slice keeps the two spellings from
		// diverging in the backends.
		if len(types) == 0 {
			types = nil
		}
		cur.EventTypes = types
	}
	if in.Tracking != nil {
		if err := s.applyTracking(ctx, cur, *in.Tracking); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) applyTracking(ctx context.Context, cur *store.TenantSettings, in TrackingConfig) error {
	if in.Domain != nil {
		if err := noCRLF("tracking.domain", *in.Domain); err != nil {
			return err
		}
		cur.Tracking.Domain = *in.Domain
	}
	if in.Opens != nil {
		cur.Tracking.Opens = *in.Opens
	}
	if in.Clicks != nil {
		cur.Tracking.Clicks = *in.Clicks
	}
	// Omitting signing_keys keeps the stored keys, like every other writeOnly
	// field: a client that PUTs back the body it read must not silently
	// invalidate every link already in a mailbox.
	if in.SigningKeys == nil {
		return nil
	}

	stored := map[string]store.SigningKey{}
	for _, k := range cur.Tracking.SigningKeys {
		stored[k.KID] = k
	}
	keys := make([]store.SigningKey, 0, len(*in.SigningKeys))
	seen := map[string]bool{}
	for _, k := range *in.SigningKeys {
		if err := requireNonEmpty("tracking.signing_keys[].kid", k.Kid); err != nil {
			return err
		}
		if seen[k.Kid] {
			return errInvalid("tracking.signing_keys has a duplicate kid %q", k.Kid)
		}
		seen[k.Kid] = true
		old := stored[k.Kid]
		// Unlike an SMTP/IMAP password or a DKIM key, a signing secret is
		// stored as-is and never goes through Deps.Secrets: it is an HMAC key
		// read directly by every path that verifies a token, on replicas that
		// may have no cipher at all (store.SigningKey). Omitting it keeps the
		// stored one, like every other writeOnly field.
		sec := old.Secret
		if k.Secret != nil {
			sec = append([]byte(nil), *k.Secret...)
		}
		if len(sec) == 0 {
			return errInvalid("tracking.signing_keys[%q] has no secret and none is stored", k.Kid)
		}
		created := old.CreatedAt
		if created.IsZero() {
			created = s.now()
		}
		keys = append(keys, store.SigningKey{KID: k.Kid, Secret: sec, CreatedAt: created})
	}
	cur.Tracking.SigningKeys = keys
	return nil
}

func settingsOut(v *store.TenantSettings) TenantSettings {
	types := append([]string{}, v.EventTypes...)
	out := TenantSettings{
		TenantId:               strPtr(v.TenantID),
		RetentionDays:          i32(v.RetentionDays),
		SuppressionEnabled:     ptr(v.SuppressionEnabled),
		BounceRetainRaw:        ptr(v.BounceRetainRaw),
		UnsubscribeUrlTemplate: strPtr(v.UnsubscribeURLTemplate),
		UnsubscribeOneClick:    ptr(v.UnsubscribeOneClick),
		DefaultLocale:          strPtr(v.DefaultLocale),
		EventTypes:             &types,
		Version:                ptr(v.Version),
		CreatedAt:              timePtr(v.CreatedAt),
		UpdatedAt:              timePtr(v.UpdatedAt),
	}
	if v.UnsubscribeMode != "" {
		out.UnsubscribeMode = ptr(UnsubscribeMode(v.UnsubscribeMode))
	}
	backoff := make([]Duration, 0, len(v.Retry.Backoff))
	for _, d := range v.Retry.Backoff {
		backoff = append(backoff, durationStr(d))
	}
	out.Retry = &RetryPolicy{MaxAttempts: i32(v.Retry.MaxAttempts), Backoff: &backoff}

	// The secrets themselves are writeOnly (architecture 16): only the key id
	// and its age come back, which is all a rotation UI needs.
	infos := make([]SigningKeyInfo, 0, len(v.Tracking.SigningKeys))
	for _, k := range v.Tracking.SigningKeys {
		infos = append(infos, SigningKeyInfo{Kid: k.KID, CreatedAt: timePtr(k.CreatedAt)})
	}
	out.Tracking = &TrackingConfig{
		Domain:      strPtr(v.Tracking.Domain),
		Opens:       ptr(v.Tracking.Opens),
		Clicks:      ptr(v.Tracking.Clicks),
		SigningKeys: &infos,
	}
	return out
}

// settingsFor is the read every handler that needs a tenant policy uses.
func settingsFor(ctx context.Context, t *tenant, now time.Time) (*store.TenantSettings, error) {
	return store.LoadTenantSettings(ctx, t.st, t.id, now)
}
