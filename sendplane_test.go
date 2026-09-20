package sendplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store/memstore"
)

type staticAuth struct{ p *sendplane.Principal }

func (a staticAuth) Authenticate(*http.Request) (*sendplane.Principal, error) {
	if a.p == nil {
		return nil, sendplane.ErrUnauthenticated
	}
	return a.p, nil
}

func TestNewRequiresStoreAndAuth(t *testing.T) {
	if _, err := sendplane.New(sendplane.Options{Auth: staticAuth{}}); err == nil {
		t.Fatal("New without a Store: want an error")
	}
	if _, err := sendplane.New(sendplane.Options{Store: memstore.New()}); err == nil {
		t.Fatal("New without an Authenticator: want an error")
	}
}

func TestNewDefaults(t *testing.T) {
	ctx := context.Background()
	p := &sendplane.Principal{ID: "u1"}
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{p: p}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o := s.Options()
	if o.Logger == nil || o.Clock == nil || o.Authz == nil || o.Tenants == nil || o.Metrics == nil {
		t.Fatalf("New left a default unset: %+v", o)
	}
	if o.Limits != sendplane.DefaultLimits {
		t.Fatalf("Limits = %+v, want %+v", o.Limits, sendplane.DefaultLimits)
	}
	if err := o.Authz.Authorize(ctx, p, sendplane.ActionCampaignSend, sendplane.Resource{}); err != nil {
		t.Fatalf("default Authorizer denied: %v", err)
	}

	tenant, err := o.Tenants.Resolve(ctx, nil, p)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tenant != sendplane.DefaultTenantID {
		t.Fatalf("tenant = %q, want %q", tenant, sendplane.DefaultTenantID)
	}
	p.TenantID = "acme"
	if tenant, _ = o.Tenants.Resolve(ctx, nil, p); tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", tenant)
	}
}

func TestPartialLimitsKeepDefaults(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{
		Store: memstore.New(), Auth: staticAuth{},
		Limits: sendplane.Limits{MaxVarsBytes: 1024},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l := s.Options().Limits
	if l.MaxVarsBytes != 1024 {
		t.Fatalf("MaxVarsBytes = %d, want 1024", l.MaxVarsBytes)
	}
	if l.MaxRecipientsPerCampaign != sendplane.DefaultLimits.MaxRecipientsPerCampaign {
		t.Fatalf("MaxRecipientsPerCampaign = %d, want the default", l.MaxRecipientsPerCampaign)
	}
}

// The types a host injects are aliases for the ones in package host, not
// copies: a sendplane.Hooks must be usable wherever a host.Hooks is, or the
// internal packages and the host would be talking about different types.
func TestPublicTypesAliasHost(t *testing.T) {
	var (
		_ host.Hooks            = sendplane.Hooks{}
		_ sendplane.Hooks       = host.Hooks{}
		_ host.Limits           = sendplane.Limits{}
		_ host.Principal        = sendplane.Principal{}
		_ host.Action           = sendplane.ActionCampaignSend
		_ host.Resource         = sendplane.Resource{}
		_ host.Metrics          = sendplane.NopMetrics{}
		_ host.RecipientContext = sendplane.RecipientContext{}
	)
	if !errors.Is(sendplane.ErrSkip, host.ErrSkip) {
		t.Error("sendplane.ErrSkip is not host.ErrSkip")
	}
	if !errors.Is(sendplane.ErrForbidden, host.ErrForbidden) {
		t.Error("sendplane.ErrForbidden is not host.ErrForbidden")
	}
	if !errors.Is(sendplane.ErrUnauthenticated, host.ErrUnauthenticated) {
		t.Error("sendplane.ErrUnauthenticated is not host.ErrUnauthenticated")
	}
	if sendplane.DefaultLimits != host.DefaultLimits {
		t.Error("sendplane.DefaultLimits differs from host.DefaultLimits")
	}
}

// RunControl and RunSender block until the context is done. A cancelled
// context therefore has to come straight back out, which is also what a
// graceful shutdown looks like.
func TestRunControlStopsWithTheContext(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.RunControl(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunControl on a cancelled context = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunControl did not return on a cancelled context")
	}
}

func TestRunSenderNeedsAWorkerID(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.RunSender(context.Background(), sendplane.SenderConfig{})
	if err == nil || !strings.Contains(err.Error(), "WorkerID") {
		t.Fatalf("RunSender without a WorkerID = %v, want an error naming WorkerID", err)
	}
}

func TestRunSenderStopsWithTheContext(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.RunSender(ctx, sendplane.SenderConfig{WorkerID: "w1"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSender on a cancelled context = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSender did not return on a cancelled context")
	}
}

func TestBounceIsNotImplementedYet(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.RunBounce(context.Background()); !errors.Is(err, sendplane.ErrNotImplemented) {
		t.Errorf("RunBounce = %v, want ErrNotImplemented", err)
	}
}

// Handler is the mounted router, built once and shared with RunControl. The
// liveness probe is the cheapest proof that the wiring is real: it is a route
// of api/openapi.yaml, it is public, and it answers JSON.
func TestHandlerServesTheAPI(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{
		Store: memstore.New(), Auth: staticAuth{p: &sendplane.Principal{ID: "u1", TenantID: "acme"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := s.Handler()
	if h == nil {
		t.Fatal("Handler returned nil")
	}
	if s.Handler() != h {
		t.Fatal("Handler built a second router on the second call")
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body: %s", err, w.Body.String())
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want ok", body.Status)
	}

	// An authenticated route goes through the injected Authenticator.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/settings = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

// Handler and RunControl must share one control plane: the public tracking
// routes record into its buffer, and two instances would mean two buffers,
// one of which nobody flushes.
func TestHandlerAndRunControlShareTheControlPlane(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("Handler returned nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RunControl(ctx); err != nil {
		t.Fatalf("RunControl after Handler: %v", err)
	}
}
