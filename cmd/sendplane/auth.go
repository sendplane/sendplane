// Authenticator and Authorizer implementations for the reference binary
// (docs/architecture.md 3: "reference binary provides API Key and JWT
// authenticators").
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/sendplane/sendplane/host"
)

// credentialFromRequest extracts the bearer credential from either the
// X-API-Key header or an "Authorization: Bearer <token>" header. It is shared
// by the API key and JWT authenticators, which differ only in what they do
// with the string.
func credentialFromRequest(r *http.Request) (string, bool) {
	if v := r.Header.Get("X-API-Key"); v != "" {
		return v, true
	}
	if v := r.Header.Get("Authorization"); v != "" {
		const prefix = "Bearer "
		if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			return v[len(prefix):], true
		}
	}
	return "", false
}

// --- API key authenticator -------------------------------------------------

// apiKeyAuthenticator authenticates X-API-Key / Authorization: Bearer against
// a fixed list of sha256(key) hashes loaded from config. The presented key is
// hashed and then compared to every configured hash with
// subtle.ConstantTimeCompare, so neither "which key matched" nor "did any key
// match" leaks through timing beyond what hashing itself takes.
type apiKeyAuthenticator struct {
	entries []apiKeyHashEntry
}

type apiKeyHashEntry struct {
	hash      []byte // decoded from hex, sha256.Size bytes
	tenant    string
	principal string
	roles     []string
}

func newAPIKeyAuthenticator(cfgKeys []APIKeyEntry) (*apiKeyAuthenticator, error) {
	entries := make([]apiKeyHashEntry, 0, len(cfgKeys))
	for i, k := range cfgKeys {
		h, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(k.KeyHash)))
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("auth.api_keys[%d].key_hash: must be %d hex-encoded sha256 bytes", i, sha256.Size)
		}
		entries = append(entries, apiKeyHashEntry{hash: h, tenant: k.Tenant, principal: k.Principal, roles: k.Roles})
	}
	return &apiKeyAuthenticator{entries: entries}, nil
}

var _ host.Authenticator = (*apiKeyAuthenticator)(nil)

func (a *apiKeyAuthenticator) Authenticate(r *http.Request) (*host.Principal, error) {
	presented, ok := credentialFromRequest(r)
	if !ok || presented == "" {
		return nil, fmt.Errorf("apikey: %w", host.ErrUnauthenticated)
	}
	sum := sha256.Sum256([]byte(presented))

	var match *apiKeyHashEntry
	for i := range a.entries {
		if subtle.ConstantTimeCompare(sum[:], a.entries[i].hash) == 1 {
			match = &a.entries[i]
			// Deliberately do not break: comparing every entry keeps the
			// loop's duration independent of where (or whether) the match
			// was found.
		}
	}
	if match == nil {
		return nil, fmt.Errorf("apikey: %w", host.ErrUnauthenticated)
	}
	return &host.Principal{ID: match.principal, TenantID: match.tenant, Roles: match.roles}, nil
}

// --- JWT authenticator -------------------------------------------------

// jwtAuthenticator verifies bearer tokens against a JWKS, caching and
// refreshing the key set the way keyfunc.NewDefaultCtx does (background
// refresh on a timer plus on unknown-kid), and maps claims onto a Principal.
type jwtAuthenticator struct {
	kf          keyfunc.Keyfunc
	issuer      string
	audience    string
	tenantClaim string
	rolesClaim  string
}

var _ host.Authenticator = (*jwtAuthenticator)(nil)

func newJWTAuthenticator(ctx context.Context, cfg JWTConfig) (*jwtAuthenticator, error) {
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
	if err != nil {
		return nil, fmt.Errorf("jwt: jwks: %w", err)
	}
	return &jwtAuthenticator{
		kf:          kf,
		issuer:      cfg.Issuer,
		audience:    cfg.Audience,
		tenantClaim: cfg.TenantClaim,
		rolesClaim:  cfg.RolesClaim,
	}, nil
}

func (a *jwtAuthenticator) Authenticate(r *http.Request) (*host.Principal, error) {
	presented, ok := credentialFromRequest(r)
	if !ok || presented == "" {
		return nil, fmt.Errorf("jwt: %w", host.ErrUnauthenticated)
	}

	var opts []jwt.ParserOption
	if a.issuer != "" {
		opts = append(opts, jwt.WithIssuer(a.issuer))
	}
	if a.audience != "" {
		opts = append(opts, jwt.WithAudience(a.audience))
	}

	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(presented, claims, a.kf.Keyfunc, opts...); err != nil {
		return nil, fmt.Errorf("jwt: %w: %v", host.ErrUnauthenticated, err)
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("jwt: %w: token has no sub claim", host.ErrUnauthenticated)
	}
	tenant, _ := claims[a.tenantClaim].(string)
	if tenant == "" {
		return nil, fmt.Errorf("jwt: %w: token has no %s claim", host.ErrUnauthenticated, a.tenantClaim)
	}

	return &host.Principal{ID: sub, TenantID: tenant, Roles: stringSliceClaim(claims, a.rolesClaim)}, nil
}

// stringSliceClaim reads claims[name] as either a JSON array of strings or a
// single space-separated string (the OAuth2 "scope" convention), since JWT
// issuers disagree on which shape a roles/scope claim takes.
func stringSliceClaim(claims jwt.MapClaims, name string) []string {
	v, ok := claims[name]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	default:
		return nil
	}
}

// --- none authenticator -------------------------------------------------

// noneAuthenticator authenticates every request as a fixed local-dev
// principal. Constructing it logs a loud warning once; it does not log again
// per request, since that would drown everything else at any real traffic
// level.
type noneAuthenticator struct {
	principal host.Principal
}

var _ host.Authenticator = (*noneAuthenticator)(nil)

func newNoneAuthenticator(logger *slog.Logger) *noneAuthenticator {
	logger.Warn("auth.mode=none: every request is authenticated as a fixed local principal (tenant=default, roles=[admin]); do not use this outside local development")
	return &noneAuthenticator{principal: host.Principal{ID: "local-dev", TenantID: "default", Roles: []string{"admin"}}}
}

func (a *noneAuthenticator) Authenticate(*http.Request) (*host.Principal, error) {
	p := a.principal
	return &p, nil
}

// --- authorizer -------------------------------------------------

// allowAllAuthorizer allows every authenticated principal to perform every
// action. It is the explicit, config-driven counterpart to sendplane's own
// nil-Authz default.
type allowAllAuthorizer struct{}

var _ host.Authorizer = allowAllAuthorizer{}

func (allowAllAuthorizer) Authorize(context.Context, *host.Principal, host.Action, host.Resource) error {
	return nil
}

// rolesAuthorizer allows an action when at least one of the principal's roles
// has a glob pattern (path.Match syntax: *, ?, [...]) matching the action, per
// authz.roles in config.yaml.
type rolesAuthorizer struct {
	roles map[string][]string
}

var _ host.Authorizer = (*rolesAuthorizer)(nil)

func newRolesAuthorizer(roles map[string][]string) *rolesAuthorizer {
	return &rolesAuthorizer{roles: roles}
}

func (a *rolesAuthorizer) Authorize(_ context.Context, p *host.Principal, act host.Action, _ host.Resource) error {
	if p == nil {
		return fmt.Errorf("authz: %w", host.ErrForbidden)
	}
	for _, role := range p.Roles {
		for _, pattern := range a.roles[role] {
			if matched, _ := path.Match(pattern, string(act)); matched {
				return nil
			}
		}
	}
	return fmt.Errorf("authz: %w: role(s) %v may not perform %s", host.ErrForbidden, p.Roles, act)
}
