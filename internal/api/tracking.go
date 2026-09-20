package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

// transparentGIF is the 1x1 pixel of architecture 9.1. It is served whatever
// the token turns out to be, so the response never says whether a delivery
// exists.
var transparentGIF = mustDecode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")

func mustDecode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// botWindow is the "clicked too soon after the mail was sent" threshold of
// architecture 9.3: a human does not open a link three seconds after delivery,
// a security scanner does.
const botWindow = 3 * time.Second

// scannerUAs are the user agents of the mail-security products that fetch
// every link in a message. The match is a lowercase substring, because these
// products version their UA string freely.
var scannerUAs = []string{
	"proofpoint", "barracuda", "mimecast", "symantec", "microsoft office",
	"bitdefender", "forcepoint", "trendmicro", "trend micro", "fireeye",
	"cloudmark", "sophos", "messagelabs", "gfi ", "spamtitan", "zscaler",
	"urldefense", "safelinks", "yandexbot", "googlebot", "bingbot",
	"slackbot", "twitterbot", "facebookexternalhit", "discordbot",
	"headlesschrome", "python-requests", "curl/", "wget/", "go-http-client",
	"apache-httpclient", "java/", "okhttp", "libwww-perl",
}

// maxUABytes caps the summarized user agent. Only a summary is kept
// (architecture 9.4).
const maxUABytes = 160

func summarizeUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) > maxUABytes {
		ua = ua[:maxUABytes]
	}
	return ua
}

func looksLikeBot(ua string, sentAt, at time.Time) bool {
	low := strings.ToLower(ua)
	for _, s := range scannerUAs {
		if strings.Contains(low, s) {
			return true
		}
	}
	if !sentAt.IsZero() && at.Sub(sentAt) < botWindow && !at.Before(sentAt) {
		return true
	}
	return false
}

// --- rate limiting -----------------------------------------------------

// The public routes are the only unauthenticated surface, so they carry their
// own limiter (architecture 16). It is a plain in-memory token bucket per
// client address: a replica limits what it sees, which is enough to keep one
// source from turning the pixel endpoint into a write amplifier, and needs no
// shared state.
const (
	ipBurst      = 60
	ipRefillRate = 20 // tokens per second
	ipIdleTTL    = 10 * time.Minute
	ipMaxEntries = 50_000
	retryAfterS  = 1
)

type bucket struct {
	tokens float64
	seen   time.Time
}

type ipLimiter struct {
	clock func() time.Time
	mu    sync.Mutex
	buds  map[string]*bucket
	swept time.Time
}

func newIPLimiter(clock func() time.Time) *ipLimiter {
	if clock == nil {
		clock = time.Now
	}
	return &ipLimiter{clock: clock, buds: map[string]*bucket{}}
}

// allow reports whether one more request from ip may be served.
func (l *ipLimiter) allow(ip string) bool {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, ok := l.buds[ip]
	if !ok {
		if len(l.buds) >= ipMaxEntries {
			// The table is full of fresh entries, which is a distributed
			// flood rather than one abusive client. Serving is the safer
			// failure: the alternative is refusing everybody.
			return true
		}
		b = &bucket{tokens: ipBurst, seen: now}
		l.buds[ip] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * ipRefillRate
	if b.tokens > ipBurst {
		b.tokens = ipBurst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets nobody has touched for a while, at most once per TTL, so
// the map cannot grow without bound.
func (l *ipLimiter) sweep(now time.Time) {
	if now.Sub(l.swept) < ipIdleTTL {
		return
	}
	l.swept = now
	for ip, b := range l.buds {
		if now.Sub(b.seen) > ipIdleTTL {
			delete(l.buds, ip)
		}
	}
}

// --- shared token handling ---------------------------------------------

// trackCtx is what a verified public request resolved to.
type trackCtx struct {
	t        *tenant
	settings *store.TenantSettings
	payload  tracking.TokenPayload
	delivery *store.Delivery
}

// resolveToken verifies a tracking token and loads what the handler needs.
//
// The tenant comes out of the token itself (tracking.TenantOf) so that
// verification is one key lookup rather than a sweep over every tenant; the
// value is only trusted after the MAC over it has checked out.
//
// A token whose delivery retention already deleted verifies but resolves to a
// nil delivery: the route still answers normally and records nothing
// (architecture 9.1).
func (s *server) resolveToken(ctx context.Context, token string, kind tracking.Kind, dest string) (*trackCtx, error) {
	tenantID, err := tracking.TenantOf(token)
	if err != nil {
		return nil, err
	}
	st, err := s.deps.Provider.ForTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	t := &tenant{id: tenantID, st: st}
	settings, err := settingsFor(ctx, t, s.now())
	if err != nil {
		return nil, err
	}
	payload, err := s.signer.VerifyDest(settings.Tracking.SigningKeys, token, dest)
	if err != nil {
		return nil, err
	}
	if payload.Kind != kind {
		return nil, errors.New("api: tracking token is for another route")
	}

	out := &trackCtx{t: t, settings: settings, payload: payload}
	d, err := st.Deliveries().Get(ctx, payload.DeliveryID)
	switch {
	case err == nil:
		out.delivery = d
	case errors.Is(err, store.ErrNotFound):
		// Retention removed it; nothing left to attribute the event to.
	default:
		return nil, err
	}
	return out, nil
}

// record buffers one tracking event. The buffer derives first_opened_at and
// friends and is what makes unique counts correct (control.TrackingBuffer).
func (s *server) record(tc *trackCtx, kind store.TrackingKind, ri reqInfo, at time.Time) {
	if tc.delivery == nil {
		return
	}
	ua := summarizeUA(ri.ua)
	ev := store.TrackingEvent{
		TenantID: tc.t.id, DeliveryID: tc.delivery.ID, CampaignID: tc.delivery.CampaignID,
		Kind: kind, LinkNo: tc.payload.LinkNo, URL: tc.payload.Dest,
		UserAgent: ua, CreatedAt: at,
	}
	if kind != store.TrackingUnsubscribed {
		ev.SuspectedBot = looksLikeBot(ua, tc.delivery.SentAt, at)
	}
	if kind == store.TrackingOpen {
		ev.LinkNo = 0
		ev.URL = ""
	}
	s.deps.Control.Tracking().Record(ev)
}

// --- routes ------------------------------------------------------------

func (s *server) TrackOpen(ctx context.Context, req TrackOpenRequestObject) (TrackOpenResponseObject, error) {
	ri := requestOf(ctx)
	if !s.limit.allow(ri.ip) {
		return TrackOpen429JSONResponse{TooManyRequestsJSONResponse{
			Body:    Error{Code: ErrorCodeRateLimited, Message: "too many requests"},
			Headers: TooManyRequestsResponseHeaders{RetryAfter: i32(retryAfterS)},
		}}, nil
	}
	tc, err := s.resolveToken(ctx, req.Token, tracking.KindOpen, "")
	if err != nil {
		// A bad token still gets a pixel: the status code is the only signal,
		// and nothing about the delivery leaks either way.
		return TrackOpen400ImagegifResponse{
			Body: bytes.NewReader(transparentGIF), ContentLength: int64(len(transparentGIF)),
		}, nil
	}
	if tc.settings.Tracking.Opens {
		s.record(tc, store.TrackingOpen, ri, s.now())
	}
	return TrackOpen200ImagegifResponse{
		Body:          bytes.NewReader(transparentGIF),
		ContentLength: int64(len(transparentGIF)),
		Headers:       TrackOpen200ResponseHeaders{CacheControl: ptr("no-store")},
	}, nil
}

func (s *server) TrackClick(ctx context.Context, req TrackClickRequestObject) (TrackClickResponseObject, error) {
	ri := requestOf(ctx)
	if !s.limit.allow(ri.ip) {
		return TrackClick429JSONResponse{TooManyRequestsJSONResponse{
			Body:    Error{Code: ErrorCodeRateLimited, Message: "too many requests"},
			Headers: TooManyRequestsResponseHeaders{RetryAfter: i32(retryAfterS)},
		}}, nil
	}
	// The destination is part of the MAC, so a swapped u fails verification
	// rather than redirecting: this route cannot be an open redirect
	// (architecture 16).
	tc, err := s.resolveToken(ctx, req.Token, tracking.KindClick, req.Params.U)
	if err != nil {
		return TrackClick400Response{}, nil
	}
	if tc.settings.Tracking.Clicks {
		s.record(tc, store.TrackingClick, ri, s.now())
	}
	return TrackClick302Response{
		Headers: TrackClick302ResponseHeaders{Location: ptr(req.Params.U)},
	}, nil
}

func (s *server) TrackUnsubscribeClick(ctx context.Context, req TrackUnsubscribeClickRequestObject) (TrackUnsubscribeClickResponseObject, error) {
	ri := requestOf(ctx)
	if !s.limit.allow(ri.ip) {
		return TrackUnsubscribeClick429JSONResponse{TooManyRequestsJSONResponse{
			Body:    Error{Code: ErrorCodeRateLimited, Message: "too many requests"},
			Headers: TooManyRequestsResponseHeaders{RetryAfter: i32(retryAfterS)},
		}}, nil
	}
	tc, err := s.resolveToken(ctx, req.Token, tracking.KindUnsubscribe, "")
	if err != nil || tc.payload.Dest == "" {
		return TrackUnsubscribeClick400Response{}, nil
	}
	// A GET is not a confirmation: a scanner following the link must not
	// unsubscribe anybody, so only unsubscribe_clicked is recorded (ADR-0011).
	s.record(tc, store.TrackingUnsubscribeClicked, ri, s.now())
	return TrackUnsubscribeClick302Response{
		Headers: TrackUnsubscribeClick302ResponseHeaders{Location: ptr(tc.payload.Dest)},
	}, nil
}

// OneClickUnsubscribe is the RFC 8058 List-Unsubscribe-Post target: a POST is
// a confirmation, so this is the one public route that marks the recipient
// unsubscribed.
func (s *server) OneClickUnsubscribe(ctx context.Context, req OneClickUnsubscribeRequestObject) (OneClickUnsubscribeResponseObject, error) {
	ri := requestOf(ctx)
	if !s.limit.allow(ri.ip) {
		return OneClickUnsubscribe429JSONResponse{TooManyRequestsJSONResponse{
			Body:    Error{Code: ErrorCodeRateLimited, Message: "too many requests"},
			Headers: TooManyRequestsResponseHeaders{RetryAfter: i32(retryAfterS)},
		}}, nil
	}
	tc, err := s.resolveToken(ctx, req.Token, tracking.KindUnsubscribe, "")
	if err != nil {
		return OneClickUnsubscribe400Response{}, nil
	}
	at := s.now()
	s.record(tc, store.TrackingUnsubscribed, ri, at)

	if tc.delivery == nil {
		return OneClickUnsubscribe200Response{}, nil
	}
	if tc.settings.SuppressionEnabled && tc.delivery.EmailNorm != "" {
		if err := tc.t.st.Suppressions().Upsert(ctx, &store.Suppression{
			EmailNorm: tc.delivery.EmailNorm, Reason: store.SuppressionManual,
			SourceDeliveryID: tc.delivery.ID, CreatedAt: at,
		}); err != nil {
			s.deps.Logger.Error("sendplane: one-click unsubscribe could not suppress",
				"delivery", tc.delivery.ID, "err", err)
		}
	}
	// The hook is synchronous and best effort; the outbox event goes out
	// either way, which is the path a host that has no Go hook relies on
	// (architecture 9.3).
	if s.deps.Hooks.Unsubscribed != nil {
		if err := s.deps.Hooks.Unsubscribed(ctx, host.UnsubscribeNotice{
			TenantID: tc.t.id, CampaignID: tc.delivery.CampaignID, DeliveryID: tc.delivery.ID,
			Email: tc.delivery.Email, EmailNorm: tc.delivery.EmailNorm,
			Source: "one_click", At: at,
		}); err != nil {
			s.deps.Logger.Warn("sendplane: Hooks.Unsubscribed failed",
				"delivery", tc.delivery.ID, "err", err)
		}
	}
	if err := s.emitUnsubscribed(ctx, tc.t, tc.delivery, "one_click", at); err != nil {
		s.deps.Logger.Error("sendplane: cannot enqueue recipient.unsubscribed",
			"delivery", tc.delivery.ID, "err", err)
	}
	return OneClickUnsubscribe200Response{}, nil
}
