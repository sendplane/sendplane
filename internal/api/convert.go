package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/sendplane/sendplane/store"
)

// --- small generic helpers ---------------------------------------------

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// timePtr omits a zero timestamp instead of serialising 0001-01-01: the store
// contract uses the zero time for "no value" (store/time.go).
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func timeVal(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return store.TruncateTime(*p)
}

// uuidOf converts a store ID to the wire type. Every ID sendplane mints is a
// UUIDv7 string (store.NewID); an ID a custom Provider invented that is not a
// UUID comes back as the nil UUID rather than failing the whole response.
func uuidOf(s string) openapi_types.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func uuidPtrOf(s string) *openapi_types.UUID {
	if s == "" {
		return nil
	}
	id := uuidOf(s)
	return &id
}

// idOf is the reverse: a wire UUID as the string the store uses. The nil UUID
// means "absent", so it maps to the empty string.
func idOf(u openapi_types.UUID) string {
	if u == uuid.Nil {
		return ""
	}
	return u.String()
}

func idPtrOf(u *openapi_types.UUID) string {
	if u == nil {
		return ""
	}
	return idOf(*u)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func i32(n int) *int32 {
	v := clampInt32(n)
	return &v
}

// clampInt32 narrows a count or an index to the int32 the wire schema
// declares. Every value that goes through it (link numbers, locale counts,
// attempt counters) is orders of magnitude below the limit; the clamp is here
// so that a corrupted row cannot wrap into a negative on the way out.
func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

// --- pagination --------------------------------------------------------

func pageOf(limit *Limit, cursor *Cursor) store.Page {
	p := store.Page{Cursor: deref(cursor)}
	if limit != nil {
		p.Limit = int(*limit)
	}
	return p.Normalize()
}

func nextCursor(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// --- durations ---------------------------------------------------------

// The wire format of a Duration is a Go duration string ("30s", "1h30m"),
// which is exactly what api/openapi.yaml's Duration pattern describes.

func durationStr(d time.Duration) Duration { return d.String() }

func parseDuration(s Duration) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(string(s)))
	if err != nil {
		return 0, errInvalid("invalid duration %q: it must be a Go duration string such as 30s or 1h30m", s)
	}
	return d, nil
}

// --- enums -------------------------------------------------------------

// The store enums marshal as text (store/enums.go), so both directions go
// through their TextMarshaler rather than a second table that could drift.

func deliveryStatusOut(v store.DeliveryStatus) DeliveryStatus { return DeliveryStatus(v.String()) }

func deliveryStatusIn(v DeliveryStatus) (store.DeliveryStatus, error) {
	var out store.DeliveryStatus
	if err := out.UnmarshalText([]byte(v)); err != nil {
		return 0, errInvalid("unknown delivery status %q", v)
	}
	return out, nil
}

func errorClassOut(v store.ErrorClass) ErrorClass { return ErrorClass(v.String()) }

func errorClassIn(v ErrorClass) (store.ErrorClass, error) {
	var out store.ErrorClass
	if err := out.UnmarshalText([]byte(v)); err != nil {
		return 0, errInvalid("unknown error class %q", v)
	}
	return out, nil
}

func campaignStatusOut(v store.CampaignStatus) CampaignStatus { return CampaignStatus(v.String()) }

func campaignStatusIn(v CampaignStatus) (store.CampaignStatus, error) {
	var out store.CampaignStatus
	if err := out.UnmarshalText([]byte(v)); err != nil {
		return 0, errInvalid("unknown campaign status %q", v)
	}
	return out, nil
}

func laneOut(v store.Lane) Lane { return Lane(v.String()) }

func laneIn(v Lane) (store.Lane, error) {
	var out store.Lane
	if err := out.UnmarshalText([]byte(v)); err != nil {
		return 0, errInvalid("unknown lane %q", v)
	}
	return out, nil
}

func healthOut(v store.HealthStatus) HealthStatus { return HealthStatus(v.String()) }

func transportStatusOut(v store.TransportStatus) TransportStatus {
	return TransportStatus(v.String())
}

func bounceTypeOut(v store.BounceType) BounceType { return BounceType(v.String()) }

func bounceTypeIn(v BounceType) (store.BounceType, error) {
	var out store.BounceType
	if err := out.UnmarshalText([]byte(v)); err != nil {
		return 0, errInvalid("unknown bounce type %q", v)
	}
	return out, nil
}

func tlsModeOut(m store.TLSMode) *TLSMode {
	if m == "" {
		return nil
	}
	v := TLSMode(m)
	return &v
}

func tlsModeIn(m *TLSMode) (store.TLSMode, error) {
	if m == nil || *m == "" {
		return store.TLSSTARTTLS, nil
	}
	switch store.TLSMode(*m) {
	case store.TLSNone, store.TLSSTARTTLS, store.TLSImplicit:
		return store.TLSMode(*m), nil
	}
	return "", errInvalid("unknown tls mode %q", *m)
}

func contentModeIn(m ContentMode) (store.ContentMode, error) {
	switch store.ContentMode(m) {
	case store.ContentBlocks, store.ContentMJML, store.ContentHTML:
		return store.ContentMode(m), nil
	}
	return "", errInvalid("unknown content mode %q", m)
}

func suppressionReasonIn(r SuppressionReason) (store.SuppressionReason, error) {
	switch store.SuppressionReason(r) {
	case store.SuppressionHardBounce, store.SuppressionComplaint, store.SuppressionManual:
		return store.SuppressionReason(r), nil
	}
	return "", errInvalid("unknown suppression reason %q", r)
}

func unsubscribeModeIn(m *UnsubscribeMode) (store.UnsubscribeMode, error) {
	if m == nil || *m == "" {
		return "", nil
	}
	switch store.UnsubscribeMode(*m) {
	case store.UnsubscribeSendplane, store.UnsubscribeHost, store.UnsubscribeNone:
		return store.UnsubscribeMode(*m), nil
	}
	return "", errInvalid("unknown unsubscribe mode %q", *m)
}

// --- validation --------------------------------------------------------

// noCRLF rejects the header-injection vector of architecture 16 wherever a
// caller-supplied string ends up in a header (names, subjects, reply-to).
func noCRLF(field, v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return errInvalid("%s must not contain a line break", field)
	}
	return nil
}

func requireNonEmpty(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return errInvalid("%s is required", field)
	}
	return nil
}

// varsOf converts the wire Vars into the map the store keeps.
func varsOf(v *Vars) map[string]any {
	if v == nil {
		return nil
	}
	return map[string]any(*v)
}

func varsOut(m map[string]any) *Vars {
	if len(m) == 0 {
		return nil
	}
	v := Vars(m)
	return &v
}

// --- secrets -----------------------------------------------------------

// secret encrypts a caller-supplied secret. A nil SecretCipher stores the
// bytes as-is, which is the same rule the sender reads them back under
// (internal/sender: "without a SecretCipher the stored bytes are used
// as-is"), so a host that keeps its secrets outside sendplane still works.
//
// keep is the currently stored value: omitting the field on an update keeps
// it, which is what makes a round trip of a GET body through a PUT
// non-destructive even though the response never contains the secret.
func (s *server) secret(ctx context.Context, in *string, keep []byte) ([]byte, error) {
	if in == nil {
		return keep, nil
	}
	if *in == "" {
		return nil, nil
	}
	if s.deps.Secrets == nil {
		return []byte(*in), nil
	}
	enc, err := s.deps.Secrets.Encrypt(ctx, []byte(*in))
	if err != nil {
		return nil, fmt.Errorf("api: encrypt secret: %w", err)
	}
	return enc, nil
}

// --- misc --------------------------------------------------------------

func (s *server) now() time.Time { return store.TruncateTime(s.deps.Clock()) }

// clientIP is the best address available without trusting a proxy header the
// caller can forge. X-Forwarded-For is honoured only when the host asked for
// it by setting the header itself; sendplane does not parse a chain.
func clientIP(r *http.Request) string {
	h := r.RemoteAddr
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return strings.Trim(h, "[]")
}

// UUID is the wire ID type, aliased so that the anonymous structs the
// generator emits (ProbeTriggerResult.Runs) can be written out by hand.
type UUID = openapi_types.UUID

// Email is the wire address type, aliased for the same reason as UUID.
type Email = openapi_types.Email

func openapiEmail(s string) openapi_types.Email { return openapi_types.Email(s) }

// jsonMap decodes a stored json.RawMessage into the generic object the wire
// schema declares.
func jsonMap(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// --- resource IDs ------------------------------------------------------

// A ResourceId is a plain string on the wire, not a UUID: a platform resource
// is named by a virtual ID (`sys:default`) that no UUID parser accepts
// (store.IsPlatformID, ADR-0017). These two are the whole conversion, and they
// exist so that a handler reads as "this field is a resource ID" rather than
// as a bare assignment that a later refactor could get wrong.

func rid(s string) ResourceId { return s }

// ridPtr omits an empty reference instead of sending "", which the schema's
// pattern does not allow.
func ridPtr(s string) *ResourceId {
	if s == "" {
		return nil
	}
	v := ResourceId(s)
	return &v
}

// ridVal is the reverse: an absent optional reference is the empty string the
// store uses for "no reference".
func ridVal(p *ResourceId) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// sharedOut reports a platform resource on the wire. It omits the field for a
// tenant's own object rather than sending `false`: `shared` is a marker, and a
// UI that shows a badge for it should not have to compare against false.
func sharedOut(shared bool) *Shared {
	if !shared {
		return nil
	}
	v := Shared(true)
	return &v
}
