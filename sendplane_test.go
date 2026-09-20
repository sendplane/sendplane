package sendplane_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sendplane/sendplane"
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
	if o.Logger == nil || o.Clock == nil || o.Authz == nil || o.Tenants == nil {
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

func TestRunsAreNotImplementedYet(t *testing.T) {
	s, err := sendplane.New(sendplane.Options{Store: memstore.New(), Auth: staticAuth{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for name, err := range map[string]error{
		"RunControl": s.RunControl(ctx),
		"RunSender":  s.RunSender(ctx, sendplane.SenderConfig{}),
		"RunBounce":  s.RunBounce(ctx),
	} {
		if !errors.Is(err, sendplane.ErrNotImplemented) {
			t.Errorf("%s = %v, want ErrNotImplemented", name, err)
		}
	}
	if s.Handler() == nil {
		t.Fatal("Handler returned nil")
	}
}
