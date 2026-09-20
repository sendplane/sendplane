package api

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/tracking"
	"github.com/sendplane/sendplane/store"
)

var trackingSecret = []byte("0123456789abcdef")

// seedTracked configures the tenant for tracking and creates one delivery that
// was sent well before "now", so the bot heuristics of architecture 9.3 do not
// fire on it.
func (e *env) seedTracked(t *testing.T) *store.Delivery {
	t.Helper()
	settings, err := store.LoadTenantSettings(t.Context(), e.st, e.tenantID, e.now)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.Tracking.Domain = "t.example.com"
	settings.Tracking.Opens, settings.Tracking.Clicks = true, true
	settings.Tracking.SigningKeys = []store.SigningKey{
		{KID: "k1", Secret: trackingSecret, CreatedAt: e.now.Add(-time.Hour)},
	}
	if err := e.st.TenantSettings().Update(t.Context(), settings); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	return e.seedDelivery(t, "a@example.com", e.now.Add(-time.Hour))
}

// seedDelivery inserts one sent delivery. sentAt is explicit because the bot
// heuristic keys off how long ago the mail went out.
func (e *env) seedDelivery(t *testing.T, email string, sentAt time.Time) *store.Delivery {
	t.Helper()
	d := store.Delivery{
		ID: store.NewID(), TenantID: e.tenantID, CampaignID: "camp-1",
		VersionID: store.NewID(), SenderID: store.NewID(),
		Lane: store.LaneBulk, Status: store.DeliverySent,
		Email: email, EmailNorm: email,
		SentAt: sentAt, CreatedAt: sentAt,
	}
	if _, err := e.st.Deliveries().InsertBatch(t.Context(), []store.Delivery{d}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	return &d
}

func (e *env) token(t *testing.T, p tracking.TokenPayload) string {
	t.Helper()
	p.TenantID = e.tenantID
	tok, err := tracking.NewSigner().SignErr("k1", trackingSecret, p)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func TestTrackOpen(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	tok := e.token(t, tracking.TokenPayload{DeliveryID: d.ID, Kind: tracking.KindOpen})

	w := e.do(http.MethodGet, "/t/o/"+url.PathEscape(tok), nil, noAuth())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/gif" {
		t.Fatalf("Content-Type = %q, want image/gif", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if !bytes.Equal(w.Body.Bytes(), transparentGIF) {
		t.Fatalf("body is not the 1x1 gif (%d bytes)", w.Body.Len())
	}
	if len(e.authz.seen) != 0 {
		t.Fatalf("the public pixel route called Authorize: %v", e.authz.seen)
	}

	e.flush()
	got, err := e.st.Deliveries().Get(t.Context(), d.ID)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if got.FirstOpenedAt.IsZero() {
		t.Fatal("first_opened_at was not derived from the open")
	}
}

func TestTrackOpenWithATamperedTokenStillReturnsThePixel(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	tok := e.token(t, tracking.TokenPayload{DeliveryID: d.ID, Kind: tracking.KindOpen})

	kid, body, _ := strings.Cut(tok, ".")
	tampered := kid + "." + flipFirst(body)

	w := e.do(http.MethodGet, "/t/o/"+url.PathEscape(tampered), nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	// The response must be indistinguishable from a good one in content, so a
	// probe cannot learn whether a delivery exists.
	if !bytes.Equal(w.Body.Bytes(), transparentGIF) {
		t.Fatal("a rejected token did not get the pixel")
	}
	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.FirstOpenedAt.IsZero() {
		t.Fatal("a tampered token recorded an open")
	}
}

func flipFirst(s string) string {
	if s == "" {
		return s
	}
	c := byte('A')
	if s[0] == 'A' {
		c = 'B'
	}
	return string(c) + s[1:]
}

func TestTrackClickRedirects(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	const dest = "https://example.com/landing?a=1&b=2"
	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindClick, LinkNo: 3, Dest: dest,
	})

	w := e.do(http.MethodGet,
		"/t/c/"+url.PathEscape(tok)+"?u="+url.QueryEscape(dest), nil, noAuth())
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != dest {
		t.Fatalf("Location = %q, want %q", loc, dest)
	}

	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if got.FirstClickedAt.IsZero() {
		t.Fatal("first_clicked_at was not derived from the click")
	}
	links, err := e.st.Tracking().LinkClicks(t.Context(), d.CampaignID)
	if err != nil {
		t.Fatalf("link clicks: %v", err)
	}
	if len(links) != 1 || links[0].LinkNo != 3 || links[0].URL != dest {
		t.Fatalf("link report = %+v", links)
	}
}

// The destination is covered by the token's MAC, so swapping u cannot turn the
// route into an open redirect (architecture 16).
func TestTrackClickRejectsASwappedDestination(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	const dest = "https://example.com/landing"
	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindClick, LinkNo: 0, Dest: dest,
	})

	w := e.do(http.MethodGet,
		"/t/c/"+url.PathEscape(tok)+"?u="+url.QueryEscape("https://evil.example/"), nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("a rejected click still sent Location: %q", loc)
	}
	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.FirstClickedAt.IsZero() {
		t.Fatal("a rejected click was recorded")
	}
}

func TestTrackClickWithoutTheDestinationParameterIs400(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindClick, Dest: "https://example.com/x",
	})
	// u is required by the spec, so the binder rejects the request.
	w := e.do(http.MethodGet, "/t/c/"+url.PathEscape(tok), nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// A GET on the unsubscribe route is not a confirmation: it redirects and
// records unsubscribe_clicked only (ADR-0011).
func TestUnsubscribeGETRedirectsWithoutConfirming(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	const dest = "https://app.example.com/unsubscribe?u=42"
	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindUnsubscribe, Dest: dest,
	})

	w := e.do(http.MethodGet, "/t/u/"+url.PathEscape(tok), nil, noAuth())
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != dest {
		t.Fatalf("Location = %q, want %q", loc, dest)
	}

	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.UnsubscribedAt.IsZero() {
		t.Fatal("a GET confirmed the unsubscribe; only the one-click POST may")
	}
	counts, err := e.st.Tracking().CountUnique(t.Context(), d.CampaignID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.UnsubscribeClicked != 1 || counts.Unsubscribed != 0 {
		t.Fatalf("counts = %+v, want one click and no confirmation", counts)
	}
}

func TestOneClickUnsubscribeConfirms(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	const dest = "https://app.example.com/unsubscribe?u=42"

	var hooked *host.UnsubscribeNotice
	e.h = New(Deps{
		Provider: e.provider, Auth: e.auth, Authz: e.authz, Control: e.ctrl,
		Clock: func() time.Time { return e.now },
		Hooks: host.Hooks{Unsubscribed: func(_ context.Context, u host.UnsubscribeNotice) error {
			hooked = &u
			return nil
		}},
	})

	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindUnsubscribe, Dest: dest,
	})
	w := e.do(http.MethodPost, "/t/u/"+url.PathEscape(tok),
		strings.NewReader("List-Unsubscribe=One-Click"), noAuth(),
		withHeader("Content-Type", "application/x-www-form-urlencoded"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", w.Body.String())
	}

	if hooked == nil {
		t.Fatal("Hooks.Unsubscribed was not called")
	}
	if hooked.Source != "one_click" || hooked.DeliveryID != d.ID || hooked.EmailNorm != d.EmailNorm {
		t.Fatalf("notice = %+v", *hooked)
	}

	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if got.UnsubscribedAt.IsZero() {
		t.Fatal("unsubscribed_at was not derived from the one-click POST")
	}
	ok, _, err := e.st.Suppressions().IsSuppressed(t.Context(), d.EmailNorm, e.now)
	if err != nil || !ok {
		t.Fatalf("address not suppressed: ok=%v err=%v", ok, err)
	}
	events, _ := e.st.Outbox().List(t.Context(), store.OutboxPending, store.Page{})
	found := false
	for _, ev := range events.Items {
		if ev.Type == eventRecipientUnsubscribed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event enqueued: %+v", eventRecipientUnsubscribed, events.Items)
	}
}

func TestOneClickUnsubscribeRejectsAForeignToken(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	// Signed with a key the tenant does not have.
	tok, err := tracking.NewSigner().SignErr("k1", []byte("a-different-secret"),
		tracking.TokenPayload{
			TenantID: e.tenantID, DeliveryID: d.ID,
			Kind: tracking.KindUnsubscribe, Dest: "https://evil.example/",
		})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	w := e.do(http.MethodPost, "/t/u/"+url.PathEscape(tok), nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.UnsubscribedAt.IsZero() {
		t.Fatal("a token signed with the wrong key confirmed an unsubscribe")
	}
}

// A token is only valid on the route its kind names: an open token must not
// work as an unsubscribe.
func TestTokenKindIsBoundToTheRoute(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	open := e.token(t, tracking.TokenPayload{DeliveryID: d.ID, Kind: tracking.KindOpen})
	w := e.do(http.MethodPost, "/t/u/"+url.PathEscape(open), nil, noAuth())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A known scanner user agent is recorded but excluded from the counts
// (architecture 9.3).
func TestScannerOpensAreMarkedAsBots(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	tok := e.token(t, tracking.TokenPayload{DeliveryID: d.ID, Kind: tracking.KindOpen})

	w := e.do(http.MethodGet, "/t/o/"+url.PathEscape(tok), nil, noAuth(),
		withHeader("User-Agent", "Mozilla/5.0 (compatible; Proofpoint URL Defense)"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.FirstOpenedAt.IsZero() {
		t.Fatal("a scanner set first_opened_at")
	}
	counts, _ := e.st.Tracking().CountUnique(t.Context(), d.CampaignID)
	if counts.UniqueOpens != 0 {
		t.Fatalf("unique opens = %d, want 0 for a scanner", counts.UniqueOpens)
	}
}

// A click that lands within seconds of the send is a scanner too.
func TestClicksRightAfterSendingAreBots(t *testing.T) {
	e := newEnv(t)
	e.seedTracked(t)
	d := e.seedDelivery(t, "fresh@example.com", e.now.Add(-time.Second))
	const dest = "https://example.com/x"
	tok := e.token(t, tracking.TokenPayload{
		DeliveryID: d.ID, Kind: tracking.KindClick, Dest: dest,
	})
	e.do(http.MethodGet, "/t/c/"+url.PathEscape(tok)+"?u="+url.QueryEscape(dest), nil, noAuth())

	e.flush()
	got, _ := e.st.Deliveries().Get(t.Context(), d.ID)
	if !got.FirstClickedAt.IsZero() {
		t.Fatal("a click one second after sending counted as human")
	}
}

func TestPublicRoutesAreRateLimited(t *testing.T) {
	e := newEnv(t)
	d := e.seedTracked(t)
	tok := e.token(t, tracking.TokenPayload{DeliveryID: d.ID, Kind: tracking.KindOpen})

	// The clock is frozen, so the bucket never refills and the burst is the
	// whole budget.
	limited := false
	for i := 0; i < ipBurst+5; i++ {
		w := e.do(http.MethodGet, "/t/o/"+url.PathEscape(tok), nil, noAuth())
		if w.Code == http.StatusTooManyRequests {
			limited = true
			if ra := w.Header().Get("Retry-After"); ra == "" {
				t.Fatal("429 without a Retry-After header")
			}
			break
		}
	}
	if !limited {
		t.Fatalf("no request was rate limited after %d attempts", ipBurst+5)
	}
}

func TestIPLimiterRefills(t *testing.T) {
	now := testNow
	l := newIPLimiter(func() time.Time { return now })
	for i := 0; i < ipBurst; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("request %d of the burst was refused", i)
		}
	}
	if l.allow("10.0.0.1") {
		t.Fatal("the bucket did not run out")
	}
	if !l.allow("10.0.0.2") {
		t.Fatal("one client's budget affected another's")
	}
	now = now.Add(time.Second)
	if !l.allow("10.0.0.1") {
		t.Fatal("the bucket did not refill after a second")
	}
}
