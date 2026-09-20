package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sendplane/sendplane/host"
)

func hashHex(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func TestAPIKeyAuthenticatorHit(t *testing.T) {
	auth, err := newAPIKeyAuthenticator([]APIKeyEntry{
		{KeyHash: hashHex("s3cr3t"), Tenant: "acme", Principal: "acme-ci", Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatalf("newAPIKeyAuthenticator: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-API-Key", "s3cr3t")
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID != "acme" || p.ID != "acme-ci" || len(p.Roles) != 1 || p.Roles[0] != "admin" {
		t.Errorf("Principal = %+v, want tenant=acme id=acme-ci roles=[admin]", p)
	}
}

func TestAPIKeyAuthenticatorAuthorizationBearer(t *testing.T) {
	auth, err := newAPIKeyAuthenticator([]APIKeyEntry{
		{KeyHash: hashHex("s3cr3t"), Tenant: "acme", Principal: "acme-ci"},
	})
	if err != nil {
		t.Fatalf("newAPIKeyAuthenticator: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	if _, err := auth.Authenticate(r); err != nil {
		t.Fatalf("Authenticate via Authorization header: %v", err)
	}
}

func TestAPIKeyAuthenticatorMiss(t *testing.T) {
	auth, err := newAPIKeyAuthenticator([]APIKeyEntry{
		{KeyHash: hashHex("s3cr3t"), Tenant: "acme", Principal: "acme-ci"},
	})
	if err != nil {
		t.Fatalf("newAPIKeyAuthenticator: %v", err)
	}

	cases := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil), // no credential at all
		func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("X-API-Key", "wrong")
			return r
		}(),
	}
	for i, r := range cases {
		if _, err := auth.Authenticate(r); !errors.Is(err, host.ErrUnauthenticated) {
			t.Errorf("case %d: err = %v, want ErrUnauthenticated", i, err)
		}
	}
}

func TestAPIKeyAuthenticatorRejectsBadHashConfig(t *testing.T) {
	if _, err := newAPIKeyAuthenticator([]APIKeyEntry{{KeyHash: "not-hex", Tenant: "t", Principal: "p"}}); err == nil {
		t.Fatal("expected an error for a non-hex key_hash")
	}
	if _, err := newAPIKeyAuthenticator([]APIKeyEntry{{KeyHash: "ab", Tenant: "t", Principal: "p"}}); err == nil {
		t.Fatal("expected an error for a key_hash that is not 32 bytes")
	}
}

// TestAPIKeyAuthenticatorComparesEveryEntry pins the constant-time-compare
// design: Authenticate must not stop at the first non-matching hash, so a
// match later in the list is found exactly like one at the front. This is a
// behavioural proxy for the timing property (a real timing measurement would
// be flaky in CI); the implementation in auth.go never breaks its scan loop.
func TestAPIKeyAuthenticatorComparesEveryEntry(t *testing.T) {
	entries := []APIKeyEntry{
		{KeyHash: hashHex("first"), Tenant: "t1", Principal: "p1"},
		{KeyHash: hashHex("second"), Tenant: "t2", Principal: "p2"},
		{KeyHash: hashHex("third"), Tenant: "t3", Principal: "p3"},
	}
	auth, err := newAPIKeyAuthenticator(entries)
	if err != nil {
		t.Fatalf("newAPIKeyAuthenticator: %v", err)
	}
	for _, want := range []string{"first", "second", "third"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-API-Key", want)
		p, err := auth.Authenticate(r)
		if err != nil {
			t.Fatalf("Authenticate(%q): %v", want, err)
		}
		if p.ID == "" {
			t.Errorf("Authenticate(%q): empty principal ID", want)
		}
	}
}

func TestNoneAuthenticatorAlwaysAuthenticates(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := newNoneAuthenticator(logger)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID == "" || p.ID == "" {
		t.Errorf("Principal = %+v, want non-empty tenant and id", p)
	}
}

func TestRolesAuthorizerGlobMatching(t *testing.T) {
	authz := newRolesAuthorizer(map[string][]string{
		"admin":  {"*"},
		"editor": {"template.*", "campaign.read"},
	})

	cases := []struct {
		name    string
		roles   []string
		action  host.Action
		allowed bool
	}{
		{"admin allows anything", []string{"admin"}, host.ActionSuppressionWrite, true},
		{"editor prefix match", []string{"editor"}, host.ActionTemplateWrite, true},
		{"editor exact match", []string{"editor"}, host.ActionCampaignRead, true},
		{"editor no match", []string{"editor"}, host.ActionCampaignWrite, false},
		{"unknown role", []string{"nobody"}, host.ActionTemplateRead, false},
		{"no roles at all", nil, host.ActionTemplateRead, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &host.Principal{ID: "u", TenantID: "t", Roles: tc.roles}
			err := authz.Authorize(context.Background(), p, tc.action, host.Resource{})
			allowed := err == nil
			if allowed != tc.allowed {
				t.Errorf("Authorize(roles=%v, action=%s) allowed=%v, want %v (err=%v)", tc.roles, tc.action, allowed, tc.allowed, err)
			}
			if !allowed && !errors.Is(err, host.ErrForbidden) {
				t.Errorf("expected ErrForbidden, got %v", err)
			}
		})
	}
}

func TestAllowAllAuthorizer(t *testing.T) {
	authz := allowAllAuthorizer{}
	err := authz.Authorize(context.Background(), &host.Principal{}, host.ActionCampaignSend, host.Resource{})
	if err != nil {
		t.Errorf("allowAllAuthorizer.Authorize = %v, want nil", err)
	}
}
