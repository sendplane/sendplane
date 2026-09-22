// Package api is sendplane's HTTP layer: the chi router the host mounts, the
// middleware that turns a request into an authenticated, tenant-bound context,
// and one handler per operation of api/openapi.yaml.
//
// The spec is the source of truth. gen.go is generated from it
// (`make gen`) and provides the request and response types plus the strict
// server interface; server implements that interface, so an operation that is
// added to the spec does not compile until a handler exists for it.
//
// Two things are deliberately not done here. Authentication and authorization
// are the host's (the package only calls host.Authenticator and
// host.Authorizer and maps their errors to 401/403), and no handler ever picks
// a tenant: the tenant store comes out of the context, put there by one
// middleware that ran host.TenantResolver exactly once.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/control"
	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml ../../api/openapi.yaml

// ProbeTrigger enqueues a loopback probe run for a sender (architecture 11.2).
// It is an interface rather than a direct call into internal/probe so that the
// HTTP layer does not depend on the probe implementation; a Deps without one
// answers 501 on POST /senders/{id}/probe.
type ProbeTrigger interface {
	// Trigger enqueues one probe delivery per enabled mailbox of the sender
	// and returns the ID of the run it created.
	Trigger(ctx context.Context, st store.Store, senderID string) (runID string, err error)
}

// Deps is everything the HTTP layer needs. Provider and Auth are required;
// New fills in the rest.
type Deps struct {
	Provider store.Provider
	Auth     host.Authenticator
	Authz    host.Authorizer
	Tenants  host.TenantResolver
	Hooks    host.Hooks
	Limits   host.Limits
	Secrets  host.SecretCipher

	// Control provides the campaign state machine and the tracking buffer the
	// public routes record into. Required.
	Control *control.Control
	// Renderer renders template previews through the same code path sending
	// uses. New creates one if it is nil.
	Renderer *render.Renderer
	// Platform resolves the operator's shared senders: their templated From
	// addresses and the tenant variables those need, so that a request whose
	// tenant_vars do not supply one is refused here rather than failing every
	// delivery it queued (ADR-0017). Nil in a deployment with no platform
	// resources.
	Platform *platform.Resolver
	// Probe is optional; without it the manual probe trigger answers 501.
	Probe ProbeTrigger
	// ProbeInbound are the inbound probe webhooks to mount, one per provider
	// the host configured (host.ProbeConfig.Webhooks, ADR-0016). Empty mounts
	// nothing, and POST /probe/inbound/{provider} is then a plain 404.
	ProbeInbound []ProbeInboundRoute
	// ProbeCompleter completes a run from an inbound webhook. Required when
	// ProbeInbound is not empty; New drops the routes without it rather than
	// serve a route that can only fail.
	ProbeCompleter ProbeCompleter
	// MailboxTester runs the credential checks the mailbox test endpoints
	// answer with. New binds internal/mailbox to Secrets if it is nil; a test
	// replaces it so the HTTP layer needs no IMAP server.
	MailboxTester MailboxTester

	Logger  *slog.Logger
	Clock   func() time.Time
	Metrics host.Metrics

	// Version is reported by GET /healthz.
	Version string
}

// server implements StrictServerInterface.
type server struct {
	deps   Deps
	signer tracking.Signer
	limit  *ipLimiter
	// probeSeen deduplicates inbound webhook redeliveries (ADR-0016).
	probeSeen *deliveryMemo
}

// New returns the handler for /api/v1, /t and /healthz.
func New(d Deps) http.Handler {
	if d.Provider == nil {
		panic("api: Deps.Provider is required")
	}
	if d.Auth == nil {
		panic("api: Deps.Auth is required")
	}
	if d.Control == nil {
		panic("api: Deps.Control is required")
	}
	if d.Authz == nil {
		d.Authz = allowAll{}
	}
	if d.Tenants == nil {
		d.Tenants = principalTenant{}
	}
	if d.Renderer == nil {
		d.Renderer = render.NewRenderer()
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Metrics == nil {
		d.Metrics = host.NopMetrics{}
	}
	if d.MailboxTester == nil {
		d.MailboxTester = cipherTester{cipher: d.Secrets}
	}
	d.Limits = d.Limits.WithDefaults()

	if d.ProbeCompleter == nil && len(d.ProbeInbound) > 0 {
		// A route that authenticates the provider and then has nothing to
		// complete the run with would answer 200 to every probe and lose it.
		// Not mounting it at all is the honest failure.
		d.Logger.Error("sendplane: probe inbound webhooks are configured without a probe runner; " +
			"the routes are not mounted")
		d.ProbeInbound = nil
	}

	s := &server{
		deps: d, signer: tracking.NewSigner(), limit: newIPLimiter(d.Clock),
		probeSeen: newDeliveryMemo(),
	}

	strict := NewStrictHandlerWithOptions(s,
		[]StrictMiddlewareFunc{s.gate},
		StrictHTTPServerOptions{
			// A binding failure (bad UUID in the path, missing required query
			// parameter) is a 400 with the spec's Error body, not net/http's
			// text/plain default.
			RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
				writeError(w, bindError(err))
			},
			ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
				e := errorFor(err)
				if e.status >= 500 {
					d.Logger.Error("sendplane: request failed",
						"request_id", requestIDFrom(r.Context()),
						"method", r.Method, "path", r.URL.Path, "err", err)
				}
				writeError(w, e)
			},
		})

	r := chi.NewRouter()
	r.Use(requestIDMiddleware)
	r.Use(recoverMiddleware(d.Logger))
	r.Use(bodyLimitMiddleware(d.Limits.MaxBodyBytes))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, newErr(http.StatusNotFound, ErrorCodeNotFound, "no such route"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, newErr(http.StatusMethodNotAllowed, ErrorCodeInvalidRequest,
			"method not allowed"))
	})
	// Mounted on the base router, before the generated routes: the spec
	// documents one path and a deployment may configure several, or another
	// one entirely (internal/api/probeinbound.go).
	for _, route := range d.ProbeInbound {
		r.Post(route.Path, s.probeInboundHandler(route))
		d.Logger.Info("sendplane: inbound probe webhook mounted",
			"provider", route.Provider.Name(), "path", route.Path)
	}
	return HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, bindError(err))
		},
	})
}

// allowAll is the default Authorizer, matching the root package's.
type allowAll struct{}

func (allowAll) Authorize(context.Context, *host.Principal, host.Action, host.Resource) error {
	return nil
}

// principalTenant is the default TenantResolver, matching the root package's.
type principalTenant struct{}

func (principalTenant) Resolve(_ context.Context, _ *http.Request, p *host.Principal) (string, error) {
	if p != nil && p.TenantID != "" {
		return p.TenantID, nil
	}
	return "default", nil
}

// --- context -----------------------------------------------------------

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
	ctxKeyTenantID
	ctxKeyStore
	ctxKeyAccept
	ctxKeyRequest
)

// reqInfo is the handful of request facts a strict handler still needs. The
// generated signatures only carry a context, and putting the whole
// *http.Request in there would hand every handler a mutable request it has no
// business touching.
type reqInfo struct {
	ip string
	ua string
}

func requestOf(ctx context.Context) reqInfo {
	ri, _ := ctx.Value(ctxKeyRequest).(reqInfo)
	return ri
}

func requestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyRequestID).(string)
	return s
}

// tenant is the per-request tenant binding every authenticated handler works
// through. Handlers never see the Provider, so they cannot reach another
// tenant's rows even by accident (ADR-0006).
type tenant struct {
	id string
	st store.Store
	p  *host.Principal
}

func tenantFrom(ctx context.Context) (*tenant, error) {
	st, _ := ctx.Value(ctxKeyStore).(store.Store)
	if st == nil {
		return nil, fmt.Errorf("api: no tenant store in context")
	}
	id, _ := ctx.Value(ctxKeyTenantID).(string)
	p, _ := ctx.Value(ctxKeyPrincipal).(*host.Principal)
	return &tenant{id: id, st: st, p: p}, nil
}

// --- middleware --------------------------------------------------------

var requestCounter atomic.Uint64

// requestIDMiddleware honours an inbound X-Request-Id and otherwise mints one,
// so that a log line from a handler and the host's access log line describe
// the same request.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 128 {
			id = fmt.Sprintf("sp-%d-%d", time.Now().UnixNano(), requestCounter.Add(1))
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		// The handlers see a context, not the request, so the one header that
		// selects a representation (i18n export) travels with it.
		ctx = context.WithValue(ctx, ctxKeyAccept, r.Header.Get("Accept"))
		ctx = context.WithValue(ctx, ctxKeyRequest, reqInfo{ip: clientIP(r), ua: r.UserAgent()})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverMiddleware turns a panic in a handler into a 500 with the spec's
// Error body instead of a dropped connection, and logs the stack.
func recoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error("sendplane: panic in handler",
					"request_id", requestIDFrom(r.Context()),
					"method", r.Method, "path", r.URL.Path, "panic", rec)
				writeError(w, newErr(http.StatusInternalServerError, ErrorCodeInternal, "internal error"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ndjsonPathSuffix is the one route whose body is a stream rather than a
// document. It is exempt from MaxBodyBytes: a recipient chunk is 10k-100k
// lines by design (architecture 7.2) and is bounded by
// MaxRecipientLineBytes per line and MaxRecipientsPerCampaign overall, not by
// the template body cap.
const ndjsonPathSuffix = "/recipients"

func bodyLimitMiddleware(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && max > 0 && !isStreamingBody(r) {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isStreamingBody(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ndjsonPathSuffix)
}

// gate is the StrictMiddlewareFunc that carries out architecture 16's
// "every handler is forced through Authorize". It is a strict middleware and
// not a chi one because only here is the operation ID known, and the operation
// ID is what the action table is keyed by.
//
// Writing the error straight to w and returning (nil, nil) is how the
// generated strict handler is told the request is finished: a nil response is
// not visited and not reported.
func (s *server) gate(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if publicOps[operationID] {
			return f(ctx, w, r, request)
		}
		authOnly := authOnlyOps[operationID]
		action, ok := opActions[operationID]
		if !ok && !authOnly {
			// Unreachable while the table test passes. It is a refusal rather
			// than a default-allow because the failure mode of the other
			// choice is serving an operation nobody authorized.
			s.deps.Logger.Error("sendplane: operation has no action mapping", "operation", operationID)
			writeError(w, newErr(http.StatusInternalServerError, ErrorCodeInternal,
				"operation %s has no action mapping", operationID))
			return nil, nil
		}

		p, err := s.deps.Auth.Authenticate(r)
		if err != nil {
			writeError(w, errorFor(fmt.Errorf("%w: %w", host.ErrUnauthenticated, err)))
			return nil, nil
		}
		if p == nil {
			writeError(w, errorFor(host.ErrUnauthenticated))
			return nil, nil
		}

		tenantID, err := s.deps.Tenants.Resolve(ctx, r, p)
		if err != nil {
			writeError(w, errorFor(err))
			return nil, nil
		}
		if tenantID == "" {
			writeError(w, errorFor(fmt.Errorf("%w: no usable tenant", host.ErrForbidden)))
			return nil, nil
		}
		// store.SystemTenantID used to be refused here. It is now reachable,
		// because it is the operator's own view: the only scope that shows the
		// platform transports, domains, mailboxes and state of ADR-0017, and
		// the scope the platform probes run in. Getting there is still
		// entirely the host's decision — the TenantResolver has to return it,
		// and the reference resolver only does so for a principal holding a
		// configured role (cmd/sendplane, auth.tenant_header). What the system
		// tenant may then do is narrowed where it matters: it cannot create a
		// campaign or send a message (refuseSystemTenantSend).

		if !authOnly {
			res := host.Resource{Kind: resourceKindFor(action), ID: resourceID(r), TenantID: tenantID}
			if err := s.deps.Authz.Authorize(ctx, p, action, res); err != nil {
				writeError(w, errorFor(fmt.Errorf("%w: %w", host.ErrForbidden, err)))
				return nil, nil
			}
		}

		st, err := s.deps.Provider.ForTenant(ctx, tenantID)
		if err != nil {
			writeError(w, errorFor(err))
			return nil, nil
		}

		ctx = context.WithValue(ctx, ctxKeyPrincipal, p)
		ctx = context.WithValue(ctx, ctxKeyTenantID, tenantID)
		ctx = context.WithValue(ctx, ctxKeyStore, st)
		return f(ctx, w, r, request)
	}
}

// resourceID picks the object the request names, so an Authorizer can decide
// per object and not only per kind. Routes with no path parameter get "".
func resourceID(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return ""
	}
	for i, k := range rctx.URLParams.Keys {
		switch k {
		case "campaignId", "templateId", "deliveryId", "senderId", "transportId",
			"domainId", "mailboxId", "layoutId", "versionId", "bounceId", "eventId", "runId", "email":
			return rctx.URLParams.Values[i]
		}
	}
	return ""
}
