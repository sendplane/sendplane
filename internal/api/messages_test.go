package api

import (
	"net/http"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func (e *env) publishedTemplate() (Sender, MessageVersion) {
	e.t.Helper()
	snd := e.seedSender()
	tpl := e.seedTemplate()
	v := decodeInto[MessageVersion](e.t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
	return snd, v
}

func TestSendMessageAndIdempotentReplay(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedTemplate()

	req := MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		Vars: &Vars{"product": "sendplane"},
		To: []MessageRecipient{
			{Email: "a@example.com", Name: ptr("A"), Vars: &Vars{"plan": "pro"}},
			{Email: "B@Example.com"},
		},
	}
	first := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", req,
		withHeader("Idempotency-Key", "order-42")), http.StatusAccepted)
	if first.VersionId != version.Id {
		t.Fatalf("version_id = %v, want %v", first.VersionId, version.Id)
	}
	if len(first.Deliveries) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(first.Deliveries))
	}
	for _, d := range first.Deliveries {
		if d.Status != DeliveryStatusQueued {
			t.Fatalf("status = %q, want queued", d.Status)
		}
	}
	if first.IdempotentReplay != nil && *first.IdempotentReplay {
		t.Fatal("the first call reported itself as a replay")
	}

	// Request-level vars are merged under the recipient's own.
	stored, err := e.st.Deliveries().Get(t.Context(), first.Deliveries[0].DeliveryId.String())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if stored.Lane != store.LaneTransactional || stored.CampaignID != "" {
		t.Fatalf("delivery = lane %s campaign %q, want a transactional delivery with no campaign",
			stored.Lane, stored.CampaignID)
	}
	if stored.Vars["product"] != "sendplane" || stored.Vars["plan"] != "pro" {
		t.Fatalf("vars = %v, want the request vars merged with the recipient's", stored.Vars)
	}
	if stored.EmailNorm != "a@example.com" {
		t.Fatalf("email_norm = %q", stored.EmailNorm)
	}

	replay := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", req,
		withHeader("Idempotency-Key", "order-42")), http.StatusAccepted)
	if replay.IdempotentReplay == nil || !*replay.IdempotentReplay {
		t.Fatalf("replay = %+v, want idempotent_replay", replay)
	}
	for i := range replay.Deliveries {
		if replay.Deliveries[i].DeliveryId != first.Deliveries[i].DeliveryId {
			t.Fatalf("replay delivery %d = %v, want the original %v",
				i, replay.Deliveries[i].DeliveryId, first.Deliveries[i].DeliveryId)
		}
	}

	// The replay must not have created a second set of rows.
	all, err := e.st.Deliveries().ListByCampaign(t.Context(), "", store.DeliveryFilter{}, store.Page{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("deliveries in the store = %d, want 2", len(all.Items))
	}

	// A different key is a different send.
	other := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", req,
		withHeader("Idempotency-Key", "order-43")), http.StatusAccepted)
	if other.Deliveries[0].DeliveryId == first.Deliveries[0].DeliveryId {
		t.Fatal("a different Idempotency-Key reused the same delivery ID")
	}
}

func TestSendMessageSuppressesKnownAddresses(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedTemplate()

	e.do(http.MethodPut, "/api/v1/suppressions/blocked%40example.com",
		SuppressionInput{Reason: SuppressionReasonHardBounce})

	got := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "ok@example.com"}, {Email: "Blocked@Example.com"}},
	}), http.StatusAccepted)

	if got.Deliveries[0].Status != DeliveryStatusQueued {
		t.Fatalf("first recipient = %q, want queued", got.Deliveries[0].Status)
	}
	if got.Deliveries[1].Status != DeliveryStatusSuppressed {
		t.Fatalf("suppressed recipient = %q, want suppressed", got.Deliveries[1].Status)
	}
	d, err := e.st.Deliveries().Get(t.Context(), got.Deliveries[1].DeliveryId.String())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if d.Status != store.DeliverySuppressed {
		t.Fatalf("stored status = %s, want suppressed", d.Status)
	}
}

// Headers are stored on the delivery and merged into the outbound message by
// the sender, through the same whitelist a Hooks.BeforeSend header passes.
func TestSendMessageHeaders(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedTemplate()

	got := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To:      []MessageRecipient{{Email: "a@example.com"}},
		Headers: &map[string]string{"X-Campaign-Tag": "spring", "In-Reply-To": "<x@example.com>"},
	}), http.StatusAccepted)

	d, err := e.st.Deliveries().Get(t.Context(), got.Deliveries[0].DeliveryId.String())
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if d.Headers["X-Campaign-Tag"] != "spring" || d.Headers["In-Reply-To"] != "<x@example.com>" {
		t.Fatalf("stored headers = %v, want both of them", d.Headers)
	}

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"not on the whitelist", map[string]string{"Bcc": "spy@example.com"}},
		{"set by sendplane", map[string]string{"Message-ID": "<mine@example.com>"}},
		{"header injection", map[string]string{"X-Tag": "a\r\nBcc: spy@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
				SenderId: snd.Id, VersionId: &version.Id,
				To: []MessageRecipient{{Email: "b@example.com"}}, Headers: &tc.headers,
			})
			decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
		})
	}
}

func TestSendMessageValidation(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedTemplate()

	t.Run("no recipients", func(t *testing.T) {
		w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
			SenderId: snd.Id, VersionId: &version.Id, To: nil,
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})

	t.Run("too many recipients", func(t *testing.T) {
		to := make([]MessageRecipient, maxTransactionalRecipients+1)
		for i := range to {
			to[i] = MessageRecipient{Email: Email("a" + itoaTest(i) + "@example.com")}
		}
		w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
			SenderId: snd.Id, VersionId: &version.Id, To: to,
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})

	t.Run("bad address", func(t *testing.T) {
		// Sent as raw JSON: the generated Email type refuses to marshal an
		// address that is not one, so the struct form cannot express this.
		body := []byte(`{"sender_id":"` + snd.Id + `","version_id":"` +
			version.Id.String() + `","to":[{"email":"not-an-email"}]}`)
		w := e.do(http.MethodPost, "/api/v1/messages", body,
			withHeader("Content-Type", "application/json"))
		if w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 422 or 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("neither template nor version", func(t *testing.T) {
		w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
			SenderId: snd.Id, To: []MessageRecipient{{Email: "a@example.com"}},
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A finished delivery can be requeued; the retry keeps the attempt history and
// bumps the generation (ADR-0003).
func TestRetryDelivery(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedTemplate()
	res := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "a@example.com"}},
	}), http.StatusAccepted)
	id := res.Deliveries[0].DeliveryId.String()

	// A queued delivery has nothing to retry.
	w := e.do(http.MethodPost, "/api/v1/deliveries/"+id+"/retry", nil)
	decodeError(t, w, http.StatusConflict, ErrorCodeInvalidState)

	d, _ := e.st.Deliveries().Get(t.Context(), id)
	d.Status = store.DeliveryFailed
	if _, err := e.st.Deliveries().InsertBatch(t.Context(), []store.Delivery{*d}); err != nil {
		t.Fatalf("seed failed delivery: %v", err)
	}

	got := decodeInto[Delivery](t, e.do(http.MethodPost, "/api/v1/deliveries/"+id+"/retry", nil), http.StatusOK)
	if got.Status != DeliveryStatusQueued {
		t.Fatalf("status after retry = %q, want queued", got.Status)
	}
	if got.RetryGen == nil || *got.RetryGen != 1 {
		t.Fatalf("retry_gen = %v, want 1", got.RetryGen)
	}
}
