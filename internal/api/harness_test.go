package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/control"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
)

// testNow is a fixed clock: every timestamp a test asserts on is derived from
// it, so nothing depends on how long the test took.
var testNow = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

// stubAuth is the host's Authenticator. It accepts a request carrying the
// bearer token it was built with and rejects everything else, which is all the
// API layer is allowed to care about.
type stubAuth struct {
	token    string
	tenantID string
	roles    []string
}

func (a *stubAuth) Authenticate(r *http.Request) (*host.Principal, error) {
	got := r.Header.Get("Authorization")
	if a.token == "" || got != "Bearer "+a.token {
		return nil, fmt.Errorf("%w: bad bearer token", host.ErrUnauthenticated)
	}
	return &host.Principal{ID: "u1", TenantID: a.tenantID, Roles: a.roles}, nil
}

// stubAuthz denies the actions in deny and records what it was asked.
type stubAuthz struct {
	deny map[host.Action]bool
	seen []host.Action
	last host.Resource
}

func (a *stubAuthz) Authorize(_ context.Context, _ *host.Principal, act host.Action, res host.Resource) error {
	a.seen = append(a.seen, act)
	a.last = res
	if a.deny[act] {
		return fmt.Errorf("%w: role may not %s", host.ErrForbidden, act)
	}
	return nil
}

// xorCipher is a stand-in SecretCipher. It is not a cipher anybody should
// use; it exists so the tests can tell "encrypted before it reached the
// store" apart from "written through".
type xorCipher struct{}

func (xorCipher) Encrypt(_ context.Context, p []byte) ([]byte, error) { return xorAll(p), nil }
func (xorCipher) Decrypt(_ context.Context, c []byte) ([]byte, error) { return xorAll(c), nil }

func xorAll(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0x5a
	}
	return out
}

type env struct {
	t        *testing.T
	h        http.Handler
	provider *memstore.Provider
	st       store.Store
	ctrl     *control.Control
	auth     *stubAuth
	authz    *stubAuthz
	tenantID string
	now      time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	now := testNow
	clock := func() time.Time { return now }

	p := memstore.New(memstore.WithClock(clock))
	c, err := control.New(p, host.Hooks{}, slog.New(slog.DiscardHandler), clock)
	if err != nil {
		t.Fatalf("control.New: %v", err)
	}
	auth := &stubAuth{token: "t0ken", tenantID: "acme"}
	authz := &stubAuthz{deny: map[host.Action]bool{}}

	e := &env{t: t, provider: p, ctrl: c, auth: auth, authz: authz, tenantID: "acme", now: now}
	e.h = New(Deps{
		Provider: p, Auth: auth, Authz: authz,
		Control: c, Logger: slog.New(slog.DiscardHandler), Clock: clock,
		Secrets: xorCipher{}, Version: "test",
	})
	st, err := p.ForTenant(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	e.st = st
	t.Cleanup(func() { _ = c.Tracking().Close() })
	return e
}

// flush forces the tracking buffer to write, so a test can assert on what a
// public route recorded without waiting for the one-second flush tick.
func (e *env) flush() {
	e.t.Helper()
	_ = e.ctrl.Tracking().Close()
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func noAuth() reqOpt {
	return func(r *http.Request) { r.Header.Del("Authorization") }
}

// do issues a request against the handler. body may be nil, a []byte, an
// io.Reader or any value that JSON-encodes.
func (e *env) do(method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	case io.Reader:
		r = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		r = bytes.NewReader(raw)
		contentType = "application/json"
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = "192.0.2.10:1234"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+e.auth.token)
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

// decode unmarshals a successful response, failing the test on an unexpected
// status so that an assertion never runs against an error body.
func decodeInto[T any](t *testing.T, w *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, want, w.Body.String())
	}
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %T: %v; body: %s", out, err, w.Body.String())
	}
	return out
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode ErrorCode) Error {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, wantStatus, w.Body.String())
	}
	var e Error
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body: %v; body: %s", err, w.Body.String())
	}
	if wantCode != "" && e.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body: %s", e.Code, wantCode, w.Body.String())
	}
	return e
}

// --- fixtures ----------------------------------------------------------

// seedSender creates a transport and a sender through the API, which is also
// what keeps the create handlers exercised by every flow test.
func (e *env) seedSender() Sender {
	e.t.Helper()
	tr := decodeInto[Transport](e.t, e.do(http.MethodPost, "/api/v1/transports", TransportInput{
		Name: "relay", Host: "smtp.example.com", Port: 587, Password: ptr("s3cret"),
		Username: ptr("mailer"),
	}), http.StatusCreated)
	return decodeInto[Sender](e.t, e.do(http.MethodPost, "/api/v1/senders", SenderInput{
		Name: "marketing", FromEmail: "news@example.com", FromName: ptr("Example"),
		TransportId: *tr.Id,
	}), http.StatusCreated)
}

// seedTemplate creates an HTML-mode template. HTML rather than MJML on
// purpose: compiling MJML costs hundreds of milliseconds per publish and none
// of these tests are about MJML.
func (e *env) seedTemplate() Template {
	e.t.Helper()
	locales := map[string]map[string]string{
		"en": {"greeting": "Hello"},
		"ko": {"greeting": "안녕하세요"},
	}
	return decodeInto[Template](e.t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "welcome", Mode: ContentModeHtml, Subject: `{{ "greeting" | t }}, {{ recipient.name }}`,
		Body: `<html><body><p>{{ "greeting" | t }}, {{ recipient.name }}</p>` +
			`<a href="https://example.com/go">go</a></body></html>`,
		DefaultLocale: ptr("en"),
		I18n:          &I18nBundle{DefaultLocale: ptr("en"), Locales: &locales},
	}), http.StatusCreated)
}
