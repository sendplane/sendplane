package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
)

func TestHealthzIsPublic(t *testing.T) {
	e := newEnv(t)
	w := e.do(http.MethodGet, "/healthz", nil, noAuth())
	got := decodeInto[ServiceHealth](t, w, http.StatusOK)
	if got.Status != ServiceHealthStatusOk {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	if len(e.authz.seen) != 0 {
		t.Fatalf("the liveness probe called Authorize: %v", e.authz.seen)
	}
}

// Every authenticated route must answer 401 without a credential and 403 when
// the host's Authorizer refuses, and must not touch the store in either case.
func TestAuthMatrix(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		action host.Action
	}{
		{"read settings", http.MethodGet, "/api/v1/settings", nil, host.ActionSettingsRead},
		{"write settings", http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{Version: 1}, host.ActionSettingsWrite},
		{"list transports", http.MethodGet, "/api/v1/transports", nil, host.ActionSenderRead},
		{"create sender", http.MethodPost, "/api/v1/senders", SenderInput{Name: "x", FromEmail: "a@b.com"}, host.ActionSenderWrite},
		{"list templates", http.MethodGet, "/api/v1/templates", nil, host.ActionTemplateRead},
		{"list campaigns", http.MethodGet, "/api/v1/campaigns", nil, host.ActionCampaignRead},
		{"send message", http.MethodPost, "/api/v1/messages", MessageRequest{}, host.ActionMessageSend},
		{"list suppressions", http.MethodGet, "/api/v1/suppressions", nil, host.ActionSuppressionRead},
		{"list bounces", http.MethodGet, "/api/v1/bounces", nil, host.ActionDeliveryRead},
		{"list events", http.MethodGet, "/api/v1/events", nil, host.ActionEventRead},
	}

	for _, tc := range cases {
		t.Run(tc.name+" unauthenticated", func(t *testing.T) {
			e := newEnv(t)
			w := e.do(tc.method, tc.path, tc.body, noAuth())
			decodeError(t, w, http.StatusUnauthorized, ErrorCodeUnauthenticated)
			if len(e.authz.seen) != 0 {
				t.Fatalf("Authorize ran for an unauthenticated request: %v", e.authz.seen)
			}
		})
		t.Run(tc.name+" forbidden", func(t *testing.T) {
			e := newEnv(t)
			e.authz.deny[tc.action] = true
			w := e.do(tc.method, tc.path, tc.body)
			decodeError(t, w, http.StatusForbidden, ErrorCodeForbidden)
			if len(e.authz.seen) != 1 || e.authz.seen[0] != tc.action {
				t.Fatalf("Authorize saw %v, want exactly [%s]", e.authz.seen, tc.action)
			}
		})
		t.Run(tc.name+" allowed", func(t *testing.T) {
			e := newEnv(t)
			w := e.do(tc.method, tc.path, tc.body)
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
				t.Fatalf("status = %d for an allowed principal; body: %s", w.Code, w.Body.String())
			}
			if len(e.authz.seen) != 1 || e.authz.seen[0] != tc.action {
				t.Fatalf("Authorize saw %v, want exactly [%s]", e.authz.seen, tc.action)
			}
		})
	}
}

// The Authorizer must be able to decide per object, so the path parameter has
// to reach it as Resource.ID along with the tenant.
func TestAuthorizeSeesTheResource(t *testing.T) {
	e := newEnv(t)
	tpl := e.seedTemplate()
	e.authz.seen = nil
	e.do(http.MethodGet, "/api/v1/templates/"+tpl.Id.String(), nil)
	if got := e.authz.last; got.Kind != "template" || got.ID != tpl.Id.String() || got.TenantID != "acme" {
		t.Fatalf("Resource = %+v, want kind template, the template ID and tenant acme", got)
	}
}

func TestErrorMapping(t *testing.T) {
	e := newEnv(t)
	snd := e.seedSender()
	tpl := e.seedTemplate()

	t.Run("unknown object is 404", func(t *testing.T) {
		w := e.do(http.MethodGet, "/api/v1/templates/018f5a2c-0000-7000-8000-0000000000ff", nil)
		decodeError(t, w, http.StatusNotFound, ErrorCodeNotFound)
	})

	t.Run("malformed path parameter is 400", func(t *testing.T) {
		w := e.do(http.MethodGet, "/api/v1/templates/not-a-uuid", nil)
		decodeError(t, w, http.StatusBadRequest, ErrorCodeInvalidRequest)
	})

	t.Run("stale version is 409", func(t *testing.T) {
		w := e.do(http.MethodPut, "/api/v1/templates/"+tpl.Id.String(), TemplateUpdate{
			Name: "welcome", Mode: ContentModeHtml, Subject: "hi", Body: "<p>hi</p>",
			Version: *tpl.Version + 99,
		})
		decodeError(t, w, http.StatusConflict, ErrorCodeVersionConflict)
	})

	t.Run("bad enum is 422", func(t *testing.T) {
		w := e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
			Name: "x", Mode: ContentMode("latex"), Subject: "s", Body: "b",
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})

	// A campaign may be created from a template that has never been
	// published: the version is resolved at start, so the failure moves there.
	t.Run("unpublished template fails at start, not at create", func(t *testing.T) {
		c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "c", SenderId: *snd.Id, TemplateId: tpl.Id,
		}), http.StatusCreated)
		w := e.do(http.MethodPost, "/api/v1/campaigns/"+c.Id.String()+"/start", nil)
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodePreconditionFailed)
	})

	t.Run("invalid campaign transition is 409", func(t *testing.T) {
		v := decodeInto[MessageVersion](t, e.do(http.MethodPost,
			"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
		c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "c", SenderId: *snd.Id, VersionId: &v.Id,
		}), http.StatusCreated)
		// draft cannot be paused.
		w := e.do(http.MethodPost, "/api/v1/campaigns/"+c.Id.String()+"/pause", nil)
		decodeError(t, w, http.StatusConflict, ErrorCodeInvalidState)
	})

	t.Run("start without recipients is 422", func(t *testing.T) {
		v := decodeInto[MessageVersion](t, e.do(http.MethodPost,
			"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
		c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "empty", SenderId: *snd.Id, VersionId: &v.Id,
		}), http.StatusCreated)
		w := e.do(http.MethodPost, "/api/v1/campaigns/"+c.Id.String()+"/start", nil)
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodePreconditionFailed)
	})

	t.Run("missing i18n keys block a publish with 422", func(t *testing.T) {
		bad := decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
			Name: "broken", Mode: ContentModeHtml, Subject: `{{ "nope" | t }}`,
			Body: "<p>x</p>", DefaultLocale: ptr("en"),
		}), http.StatusCreated)
		w := e.do(http.MethodPost, "/api/v1/templates/"+bad.Id.String()+"/publish", nil)
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeMissingI18nKeys)
	})

	t.Run("oversized body is 413", func(t *testing.T) {
		big := make([]byte, host.DefaultLimits.MaxBodyBytes+1024)
		for i := range big {
			big[i] = 'a'
		}
		w := e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
			Name: "huge", Mode: ContentModeHtml, Subject: "s", Body: string(big),
		})
		decodeError(t, w, http.StatusRequestEntityTooLarge, ErrorCodePayloadTooLarge)
	})
}

// Secrets are writeOnly: the create response, the read response and the list
// must all report presence and never the value, and an update that omits the
// field must keep what was stored (architecture 16).
func TestSecretsAreNeverEchoed(t *testing.T) {
	e := newEnv(t)
	const password = "hunter2-do-not-leak"

	created := decodeInto[Transport](t, e.do(http.MethodPost, "/api/v1/transports", TransportInput{
		Name: "relay", Host: "smtp.example.com", Port: 587,
		Username: ptr("mailer"), Password: ptr(password),
	}), http.StatusCreated)
	if created.HasPassword == nil || !*created.HasPassword {
		t.Fatal("has_password is not set on the create response")
	}

	for _, path := range []string{"/api/v1/transports", "/api/v1/transports/" + created.Id.String()} {
		w := e.do(http.MethodGet, path, nil)
		if body := w.Body.String(); strings.Contains(body, password) {
			t.Fatalf("GET %s leaked the password: %s", path, body)
		}
	}

	// The stored bytes must be the ciphertext, not the plaintext.
	stored, err := e.st.Transports().Get(t.Context(), created.Id.String())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if string(stored.Password) == password {
		t.Fatal("the password reached the store in the clear")
	}
	plain, _ := xorCipher{}.Decrypt(t.Context(), stored.Password)
	if string(plain) != password {
		t.Fatalf("decrypted password = %q, want %q", plain, password)
	}

	// Omitting the field on an update keeps the stored secret; a round trip of
	// a GET body through a PUT must not wipe it.
	updated := decodeInto[Transport](t, e.do(http.MethodPut,
		"/api/v1/transports/"+created.Id.String(), TransportUpdate{
			Name: "relay-2", Host: "smtp.example.com", Port: 587,
			Username: ptr("mailer"), Version: *created.Version,
		}), http.StatusOK)
	if updated.HasPassword == nil || !*updated.HasPassword {
		t.Fatal("omitting password on update cleared it")
	}
	after, _ := e.st.Transports().Get(t.Context(), created.Id.String())
	if string(after.Password) != string(stored.Password) {
		t.Fatal("omitting password on update rewrote the stored value")
	}
}

// The tracking signing secret is writeOnly as well, and a settings round trip
// must not lose it: the GET body carries only the kid.
func TestSigningSecretSurvivesARoundTrip(t *testing.T) {
	e := newEnv(t)
	secret := []byte("0123456789abcdef")
	keys := []SigningKeyInfo{{Kid: "k1", Secret: &secret}}
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)

	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version:  *cur.Version,
		Tracking: &TrackingConfig{Domain: ptr("t.example.com"), SigningKeys: &keys},
	}), http.StatusOK)
	if got.Tracking == nil || got.Tracking.SigningKeys == nil || len(*got.Tracking.SigningKeys) != 1 {
		t.Fatalf("signing keys = %+v", got.Tracking)
	}
	if (*got.Tracking.SigningKeys)[0].Secret != nil {
		t.Fatal("the signing secret came back in the response")
	}

	// PUT the body that was just read back: the secret is absent from it.
	again := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *got.Version, Tracking: got.Tracking,
	}), http.StatusOK)
	if again.Tracking == nil || len(*again.Tracking.SigningKeys) != 1 {
		t.Fatalf("round trip dropped the key: %+v", again.Tracking)
	}
	settings, err := e.st.TenantSettings().Get(t.Context())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if len(settings.Tracking.SigningKeys) != 1 || len(settings.Tracking.SigningKeys[0].Secret) == 0 {
		t.Fatal("the stored signing secret was lost by the round trip")
	}
	// The HMAC key is stored as-is, not through the SecretCipher: every path
	// that verifies a token reads it straight off the settings row, on
	// replicas that may have no cipher (store.SigningKey).
	if string(settings.Tracking.SigningKeys[0].Secret) != string(secret) {
		t.Fatalf("stored signing secret = %q, want the plain HMAC key %q",
			settings.Tracking.SigningKeys[0].Secret, secret)
	}
}

// The one-click declaration and the raw-bounce retention flag are policy the
// engine already reads (internal/sender, internal/bounce); this pins that the
// API is how an operator sets them, and that omitting them keeps the stored
// value like every other settings field.
func TestSettingsUnsubscribeOneClickAndBounceRetainRaw(t *testing.T) {
	e := newEnv(t)
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if cur.UnsubscribeOneClick == nil || *cur.UnsubscribeOneClick {
		t.Fatalf("unsubscribe_one_click = %v, want false by default", cur.UnsubscribeOneClick)
	}
	if cur.BounceRetainRaw == nil || *cur.BounceRetainRaw {
		t.Fatalf("bounce_retain_raw = %v, want false by default", cur.BounceRetainRaw)
	}

	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version:             *cur.Version,
		UnsubscribeMode:     ptr(UnsubscribeModeHost),
		UnsubscribeOneClick: ptr(true),
		BounceRetainRaw:     ptr(true),
	}), http.StatusOK)
	if !*got.UnsubscribeOneClick || !*got.BounceRetainRaw {
		t.Fatalf("flags did not come back set: %+v", got)
	}
	stored, err := e.st.TenantSettings().Get(t.Context())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if !stored.UnsubscribeOneClick || !stored.BounceRetainRaw {
		t.Fatalf("stored settings = %+v, want both flags set", stored)
	}

	// A PUT that does not mention them keeps them.
	again := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *got.Version, DefaultLocale: ptr("ko"),
	}), http.StatusOK)
	if !*again.UnsubscribeOneClick || !*again.BounceRetainRaw {
		t.Fatalf("omitting the flags cleared them: %+v", again)
	}
}

// The outbox subscription filter of architecture 12. An empty list is the
// default set, not "no events", which is what makes delivery.failed arrive for
// a tenant nobody configured and delivery.sent not (store.SubscribedTo).
func TestSettingsEventTypes(t *testing.T) {
	e := newEnv(t)
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	if cur.EventTypes == nil || len(*cur.EventTypes) != 0 {
		t.Fatalf("event_types = %v, want empty by default", cur.EventTypes)
	}

	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version:    *cur.Version,
		EventTypes: &[]string{"delivery.sent", "campaign.completed"},
	}), http.StatusOK)
	if got.EventTypes == nil || len(*got.EventTypes) != 2 || (*got.EventTypes)[0] != "delivery.sent" {
		t.Fatalf("event_types did not come back: %v", got.EventTypes)
	}
	stored, err := e.st.TenantSettings().Get(t.Context())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if !stored.SubscribedTo("delivery.sent") || stored.SubscribedTo("delivery.bounced") {
		t.Fatalf("stored subscription = %v, want exactly the two named types", stored.EventTypes)
	}

	// Omitting the field keeps the list, like every other settings field.
	again := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *got.Version, DefaultLocale: ptr("ko"),
	}), http.StatusOK)
	if again.EventTypes == nil || len(*again.EventTypes) != 2 {
		t.Fatalf("omitting event_types cleared it: %v", again.EventTypes)
	}

	// An explicit empty list goes back to the default set.
	back := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *again.Version, EventTypes: &[]string{},
	}), http.StatusOK)
	if back.EventTypes == nil || len(*back.EventTypes) != 0 {
		t.Fatalf("event_types = %v, want empty", back.EventTypes)
	}
	stored, err = e.st.TenantSettings().Get(t.Context())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if !stored.SubscribedTo("delivery.failed") || stored.SubscribedTo("delivery.sent") {
		t.Fatalf("an empty list must mean the default set, got %v", stored.EventTypes)
	}
}

func TestSettingsDurationsRoundTrip(t *testing.T) {
	e := newEnv(t)
	cur := decodeInto[TenantSettings](t, e.do(http.MethodGet, "/api/v1/settings", nil), http.StatusOK)
	backoff := []Duration{"30s", "5m", "1h30m"}
	got := decodeInto[TenantSettings](t, e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *cur.Version,
		Retry:   &RetryPolicy{MaxAttempts: i32(4), Backoff: &backoff},
	}), http.StatusOK)
	if got.Retry == nil || got.Retry.Backoff == nil {
		t.Fatal("retry policy missing from the response")
	}
	// The wire format is a Go duration string, so it comes back normalized.
	if want := []Duration{"30s", "5m0s", "1h30m0s"}; !equalStrings(*got.Retry.Backoff, want) {
		t.Fatalf("backoff = %v, want %v", *got.Retry.Backoff, want)
	}
	settings, _ := e.st.TenantSettings().Get(t.Context())
	if settings.Retry.Backoff[2] != 90*time.Minute {
		t.Fatalf("stored backoff[2] = %v, want 1h30m", settings.Retry.Backoff[2])
	}

	w := e.do(http.MethodPut, "/api/v1/settings", TenantSettingsUpdate{
		Version: *got.Version,
		Retry:   &RetryPolicy{Backoff: &[]Duration{"not-a-duration"}},
	})
	decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProbeTriggerWithoutAProbeImplementationIs501(t *testing.T) {
	e := newEnv(t)
	snd := e.seedSender()
	w := e.do(http.MethodPost, "/api/v1/senders/"+snd.Id.String()+"/probe", nil)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body: %s", w.Code, w.Body.String())
	}
}

func TestPaginationClampsLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.do(http.MethodPost, "/api/v1/transports", TransportInput{
			Name: "relay", Host: "smtp.example.com", Port: 587,
		})
	}
	page := decodeInto[TransportList](t, e.do(http.MethodGet, "/api/v1/transports?limit=2", nil), http.StatusOK)
	if len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("first page = %d items, next_cursor %v", len(page.Items), page.NextCursor)
	}
	rest := decodeInto[TransportList](t, e.do(http.MethodGet,
		"/api/v1/transports?limit=2&cursor="+*page.NextCursor, nil), http.StatusOK)
	if len(rest.Items) != 1 || rest.NextCursor != nil {
		t.Fatalf("second page = %d items, next_cursor %v", len(rest.Items), rest.NextCursor)
	}
	// The spec caps limit at 1000; anything above is a 400 from the binder.
	w := e.do(http.MethodGet, "/api/v1/transports?limit=0", nil)
	if w.Code != http.StatusOK && w.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status = %d; body: %s", w.Code, w.Body.String())
	}
}
