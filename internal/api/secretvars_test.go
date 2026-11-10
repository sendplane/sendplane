package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/store"
)

// secretCode is the plaintext the tests send. Every response body is checked
// for it, and for the ciphertext it became.
const secretCode = "482913-otp"

// countingCipher is the harness's xorCipher, counting the encryptions so a
// test can tell "nothing was encrypted" from "encrypted and then dropped".
// Only secret_vars count; the seeded transport's password is encrypted too.
type countingCipher struct {
	xorCipher
	encrypts atomic.Int32
}

func (c *countingCipher) Encrypt(ctx context.Context, p []byte, sc store.SecretContext) ([]byte, error) {
	if sc.Purpose == store.PurposeDeliverySecretVars {
		c.encrypts.Add(1)
	}
	return c.xorCipher.Encrypt(ctx, p, sc)
}

// publishedSecretTemplate publishes an HTML template whose body reads
// secret.code (and, through a translation, secret.name) and returns the
// sender and the version.
func (e *env) publishedSecretTemplate() (Sender, MessageVersion) {
	e.t.Helper()
	snd := e.seedSender()
	locales := map[string]map[string]string{"en": {"hello": "Hello {{ secret.name }}"}}
	tpl := decodeInto[Template](e.t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "otp", Mode: ContentModeHtml, Subject: "Your code",
		Body:          `<html><body><p>{{ "hello" | t }}</p><p>{{ secret.code }}</p><a href="https://example.com/x">x</a></body></html>`,
		DefaultLocale: ptr("en"),
		I18n:          &I18nBundle{DefaultLocale: ptr("en"), Locales: &locales},
	}), http.StatusCreated)
	v := decodeInto[MessageVersion](e.t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
	return snd, v
}

func secretsOf(kv ...string) *map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return &m
}

// assertNoSecret fails when a response body carries the plaintext or the
// ciphertext, raw or base64.
func assertNoSecret(t *testing.T, what, body string, ciphertexts ...[]byte) {
	t.Helper()
	if strings.Contains(body, secretCode) {
		t.Errorf("%s contains the secret plaintext: %s", what, body)
	}
	for _, ct := range ciphertexts {
		for _, form := range []string{
			base64.StdEncoding.EncodeToString(ct), base64.RawStdEncoding.EncodeToString(ct),
			base64.URLEncoding.EncodeToString(ct), base64.RawURLEncoding.EncodeToString(ct),
		} {
			if strings.Contains(body, form) {
				t.Errorf("%s contains the ciphertext: %s", what, body)
			}
		}
	}
	for _, field := range []string{`"secret_vars":`, `"secret_ct":`, `"secret_ciphertext":`} {
		if strings.Contains(body, field) {
			t.Errorf("%s has a %q field: %s", what, field, body)
		}
	}
}

func TestSendMessageSecretVars(t *testing.T) {
	c := &countingCipher{}
	e := newEnv(t, func(d *Deps) { d.Secrets = c })
	snd, version := e.publishedSecretTemplate()

	w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		Vars: &Vars{"product": "sendplane"},
		To: []MessageRecipient{
			{Email: "A@Example.com", SecretVars: secretsOf("code", secretCode, "name", "Ann")},
		},
	})
	res := decodeInto[MessageResult](t, w, http.StatusAccepted)
	id := res.Deliveries[0].DeliveryId.String()

	stored, err := e.st.Deliveries().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SecretState != store.SecretPending || !stored.Sensitive() {
		t.Fatalf("secret state = %s, want pending", stored.SecretState)
	}
	if want := e.now.Add(defaultSecretTTL); !stored.SecretExpiresAt.Equal(want) {
		t.Fatalf("secret_expires_at = %v, want now + 10m = %v", stored.SecretExpiresAt, want)
	}
	if strings.Contains(string(stored.SecretCiphertext), secretCode) {
		t.Fatal("the stored ciphertext contains the plaintext")
	}
	// Sealed for this row and this recipient, and for nothing else.
	if got := xorPlain(stored.SecretCiphertext, store.DeliverySecretContext(stored)); got != `{"code":"`+secretCode+`","name":"Ann"}` {
		t.Fatalf("ciphertext decrypts to %q", got)
	}
	other := *stored
	other.EmailNorm = "b@example.com"
	if _, err := (xorCipher{}).Decrypt(t.Context(), stored.SecretCiphertext, store.DeliverySecretContext(&other)); err == nil {
		t.Fatal("the ciphertext decrypts for another recipient")
	}
	// secret_vars never leak into the ordinary variables.
	if _, ok := stored.Vars["code"]; ok || stored.Vars["product"] != "sendplane" {
		t.Fatalf("vars = %v", stored.Vars)
	}
	if c.encrypts.Load() != 1 {
		t.Fatalf("encryptions = %d, want 1", c.encrypts.Load())
	}

	assertNoSecret(t, "POST /messages", w.Body.String(), stored.SecretCiphertext)
	g := e.do(http.MethodGet, "/api/v1/deliveries/"+id, nil)
	got := decodeInto[Delivery](t, g, http.StatusOK)
	assertNoSecret(t, "GET /deliveries/{id}", g.Body.String(), stored.SecretCiphertext)
	if got.Sensitive == nil || !*got.Sensitive || got.SecretState == nil || *got.SecretState != SecretStatePending {
		t.Fatalf("delivery = sensitive %v state %v", got.Sensitive, got.SecretState)
	}
	if got.SecretExpiresAt == nil || !got.SecretExpiresAt.Equal(stored.SecretExpiresAt) {
		t.Fatalf("secret_expires_at = %v", got.SecretExpiresAt)
	}
	l := e.do(http.MethodGet, "/api/v1/deliveries", nil)
	if l.Code != http.StatusOK {
		t.Fatalf("list: %d %s", l.Code, l.Body.String())
	}
	assertNoSecret(t, "GET /deliveries", l.Body.String(), stored.SecretCiphertext)

	// An ordinary delivery says so.
	plainSnd, plainVersion := e.publishedTemplateWith(snd)
	pr := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: plainSnd.Id, VersionId: &plainVersion.Id, To: []MessageRecipient{{Email: "p@example.com"}},
	}), http.StatusAccepted)
	pd := decodeInto[Delivery](t, e.do(http.MethodGet, "/api/v1/deliveries/"+pr.Deliveries[0].DeliveryId.String(), nil), http.StatusOK)
	if pd.Sensitive == nil || *pd.Sensitive || pd.SecretState == nil || *pd.SecretState != SecretStateNone || pd.SecretExpiresAt != nil {
		t.Fatalf("ordinary delivery = sensitive %v state %v expires %v", pd.Sensitive, pd.SecretState, pd.SecretExpiresAt)
	}
}

// publishedTemplateWith publishes the harness's ordinary template for an
// existing sender (seedSender cannot run twice in one env).
func (e *env) publishedTemplateWith(snd Sender) (Sender, MessageVersion) {
	e.t.Helper()
	tpl := e.seedTemplate()
	v := decodeInto[MessageVersion](e.t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
	return snd, v
}

func TestSendMessageSecretTTL(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedSecretTemplate()
	ok := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id, SecretTtlSeconds: ptr[int32](30),
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")}},
	}), http.StatusAccepted)
	d, err := e.st.Deliveries().Get(t.Context(), ok.Deliveries[0].DeliveryId.String())
	if err != nil {
		t.Fatal(err)
	}
	if want := e.now.Add(30 * time.Second); !d.SecretExpiresAt.Equal(want) {
		t.Fatalf("secret_expires_at = %v, want %v", d.SecretExpiresAt, want)
	}

	for _, ttl := range []int32{29, 0, -1, 86401} {
		w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
			SenderId: snd.Id, VersionId: &version.Id, SecretTtlSeconds: ptr(ttl),
			To: []MessageRecipient{{Email: "b@example.com", SecretVars: secretsOf("code", secretCode, "name", "B")}},
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	}

	// A TTL with nothing to expire is a mistake, not a no-op.
	_, plainVersion := e.publishedTemplateWith(snd)
	w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &plainVersion.Id, SecretTtlSeconds: ptr[int32](60),
		To: []MessageRecipient{{Email: "c@example.com"}},
	})
	decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func TestSendMessageSecretVarsNeedCipher(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Secrets = nil })
	snd, version := e.publishedSecretTemplate()
	w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")}},
	})
	decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeSecretCipherRequired)
	assertNoSecret(t, "error body", w.Body.String())
	all, err := e.st.Deliveries().ListByCampaign(t.Context(), "", store.DeliveryFilter{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 0 {
		t.Fatalf("%d deliveries stored after the refusal", len(all.Items))
	}
}

func TestSendMessageSecretKeysMatchTemplate(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedSecretTemplate()
	send := func(to ...MessageRecipient) Error {
		w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
			SenderId: snd.Id, VersionId: &version.Id, To: to,
		})
		er := decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
		assertNoSecret(t, "error body", w.Body.String())
		return er
	}

	// The template reads secret.code and, through a translation, secret.name.
	er := send(
		MessageRecipient{Email: "a@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")},
		MessageRecipient{Email: "b@example.com", SecretVars: secretsOf("code", secretCode)},
	)
	if !strings.Contains(er.Message, "to[1]") || !strings.Contains(er.Message, `"name"`) {
		t.Fatalf("message = %q, want it to name to[1] and the key", er.Message)
	}
	// A recipient with no secret_vars at all fails the same way: the mail
	// would go out with an empty code.
	er = send(
		MessageRecipient{Email: "a@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")},
		MessageRecipient{Email: "b@example.com"},
	)
	if !strings.Contains(er.Message, "to[1]") || !strings.Contains(er.Message, `"code"`) {
		t.Fatalf("message = %q", er.Message)
	}
	// So does a request with none.
	send(MessageRecipient{Email: "a@example.com"})

	// secret_vars for a template that reads no secret.* are refused.
	_, plainVersion := e.publishedTemplateWith(snd)
	w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &plainVersion.Id,
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", secretCode)}},
	})
	decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	assertNoSecret(t, "error body", w.Body.String())
}

func TestSendMessageSecretVarsSizeCap(t *testing.T) {
	e := newEnv(t)
	snd, version := e.publishedSecretTemplate()
	big := strings.Repeat("x", maxSecretVarsBytes)
	w := e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", big, "name", "A")}},
	})
	er := decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeLimitExceeded)
	if strings.Contains(er.Message, big[:64]) {
		t.Fatal("the error echoes the value")
	}
	// Just under the cap is fine.
	fits := strings.Repeat("x", maxSecretVarsBytes-len(`{"code":"","name":"A"}`))
	decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", fits, "name", "A")}},
	}), http.StatusAccepted)
}

// A suppressed recipient is never sent, so its secret is never encrypted or
// stored: the row is discarded from the start. The other recipient's is.
func TestSendMessageSecretSuppressedRecipient(t *testing.T) {
	c := &countingCipher{}
	e := newEnv(t, func(d *Deps) { d.Secrets = c })
	snd, version := e.publishedSecretTemplate()
	e.do(http.MethodPut, "/api/v1/suppressions/blocked%40example.com",
		SuppressionInput{Reason: SuppressionReasonHardBounce})

	res := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{
			{Email: "ok@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")},
			{Email: "blocked@example.com", SecretVars: secretsOf("code", secretCode, "name", "B")},
		},
	}), http.StatusAccepted)
	if res.Deliveries[1].Status != DeliveryStatusSuppressed {
		t.Fatalf("status = %s, want suppressed", res.Deliveries[1].Status)
	}
	if c.encrypts.Load() != 1 {
		t.Fatalf("encryptions = %d, want 1 (the suppressed recipient's secret is not encrypted)", c.encrypts.Load())
	}
	d, err := e.st.Deliveries().Get(t.Context(), res.Deliveries[1].DeliveryId.String())
	if err != nil {
		t.Fatal(err)
	}
	if d.SecretState != store.SecretDiscarded || d.SecretCiphertext != nil {
		t.Fatalf("suppressed: state %s ciphertext %v", d.SecretState, d.SecretCiphertext != nil)
	}

	// Neither one can be retried, finished or not.
	for _, item := range res.Deliveries {
		id := item.DeliveryId.String()
		w := e.do(http.MethodPost, "/api/v1/deliveries/"+id+"/retry", nil)
		decodeError(t, w, http.StatusConflict, ErrorCodeNotRetryable)
		after, err := e.st.Deliveries().Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if after.RetryGen != 0 {
			t.Fatalf("%s: retry_gen = %d after a refused retry", id, after.RetryGen)
		}
	}
}

// A replay under the same Idempotency-Key returns the original result and
// neither encrypts again nor touches the stored row.
func TestSendMessageSecretIdempotentReplay(t *testing.T) {
	c := &countingCipher{}
	e := newEnv(t, func(d *Deps) { d.Secrets = c })
	snd, version := e.publishedSecretTemplate()
	req := MessageRequest{
		SenderId: snd.Id, VersionId: &version.Id,
		To: []MessageRecipient{{Email: "a@example.com", SecretVars: secretsOf("code", secretCode, "name", "A")}},
	}
	first := decodeInto[MessageResult](t, e.do(http.MethodPost, "/api/v1/messages", req,
		withHeader("Idempotency-Key", "otp-1")), http.StatusAccepted)
	id := first.Deliveries[0].DeliveryId.String()
	before, err := e.st.Deliveries().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}

	w := e.do(http.MethodPost, "/api/v1/messages", req, withHeader("Idempotency-Key", "otp-1"))
	replay := decodeInto[MessageResult](t, w, http.StatusAccepted)
	if replay.IdempotentReplay == nil || !*replay.IdempotentReplay || replay.Deliveries[0].DeliveryId != first.Deliveries[0].DeliveryId {
		t.Fatalf("replay = %+v", replay)
	}
	assertNoSecret(t, "replay", w.Body.String(), before.SecretCiphertext)
	if c.encrypts.Load() != 1 {
		t.Fatalf("encryptions = %d, want 1: the replay encrypted again", c.encrypts.Load())
	}
	after, err := e.st.Deliveries().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.SecretCiphertext) != string(before.SecretCiphertext) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("the replay rewrote the stored row")
	}
}

func TestSecretKeys(t *testing.T) {
	v := &store.MessageVersion{
		SubjectTpl: `{{ secret.code }}`,
		HTMLTpl:    `{% if secret.link.url %}<a href="{{ secret.link.url }}">x</a>{% endif %} {{ subsecret.no }} {{ vars.secret }}`,
		TextTpl:    `{{ secret["token"] }}`,
		I18n: store.I18nBundle{Locales: map[string]map[string]string{
			"en": {"k": "{{ secret.name }}"},
		}},
	}
	got := strings.Join(render.SecretKeys(v), ",")
	if got != "code,link,name,token" {
		t.Fatalf("secretKeysOf = %q", got)
	}
	if keys := render.SecretKeys(&store.MessageVersion{HTMLTpl: `{{ vars.code }}`}); len(keys) != 0 {
		t.Fatalf("a version without secret.* reports %v", keys)
	}
}

// A campaign delivery carries no secret_vars, so a campaign on a version that
// reads secret.* is refused up front: at create, at update and at start.
func TestCampaignOnSecretVersionIsRefused(t *testing.T) {
	e := newEnv(t)
	snd, secretV := e.publishedSecretTemplate()
	secretTpl := secretV.TemplateId

	plain := decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "plain", Mode: ContentModeHtml, Subject: "Hi", Body: "<p>hello</p>", DefaultLocale: ptr("en"),
	}), http.StatusCreated)
	plainV := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+plain.Id.String()+"/publish", nil), http.StatusCreated)

	t.Run("create pinned", func(t *testing.T) {
		w := e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{Name: "c", SenderId: snd.Id, VersionId: &secretV.Id})
		er := decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
		if !strings.Contains(er.Message, "secret") {
			t.Errorf("message %q does not mention secret", er.Message)
		}
	})
	t.Run("create from a template whose published version reads secret", func(t *testing.T) {
		w := e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{Name: "c", SenderId: snd.Id, TemplateId: &secretTpl})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})
	t.Run("a normal campaign still works", func(t *testing.T) {
		decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "ok", SenderId: snd.Id, VersionId: &plainV.Id,
		}), http.StatusCreated)
	})
	t.Run("update", func(t *testing.T) {
		c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "c", SenderId: snd.Id, VersionId: &plainV.Id,
		}), http.StatusCreated)
		w := e.do(http.MethodPut, "/api/v1/campaigns/"+c.Id.String(), CampaignUpdate{
			Name: "c", SenderId: snd.Id, VersionId: &secretV.Id, Version: *c.Version,
		})
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})
	t.Run("start, after the template was republished with a secret", func(t *testing.T) {
		// A template that read nothing when the campaign was created...
		tpl := decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
			Name: "later", Mode: ContentModeHtml, Subject: "Hi", Body: "<p>hello</p>", DefaultLocale: ptr("en"),
		}), http.StatusCreated)
		decodeInto[MessageVersion](t, e.do(http.MethodPost,
			"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
		c := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
			Name: "c", SenderId: snd.Id, TemplateId: tpl.Id,
		}), http.StatusCreated)
		// ...and reads secret.code when it is started.
		cur := decodeInto[Template](t, e.do(http.MethodGet, "/api/v1/templates/"+tpl.Id.String(), nil), http.StatusOK)
		decodeInto[Template](t, e.do(http.MethodPut, "/api/v1/templates/"+tpl.Id.String(), TemplateUpdate{
			Name: "later", Mode: ContentModeHtml, Subject: "Hi", Body: "<p>{{ secret.code }}</p>",
			DefaultLocale: ptr("en"), Version: *cur.Version,
		}), http.StatusOK)
		decodeInto[MessageVersion](t, e.do(http.MethodPost,
			"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
		w := e.do(http.MethodPost, "/api/v1/campaigns/"+c.Id.String()+"/start", nil)
		decodeError(t, w, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	})
}
