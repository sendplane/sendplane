package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sendplane/sendplane/store"
)

// The path a campaign actually takes: publish the template, preview it,
// create the campaign against the published version, stream recipients in as
// NDJSON, start it, and read the deliveries back.
func TestCampaignFlow(t *testing.T) {
	e := newEnv(t)
	snd := e.seedSender()
	tpl := e.seedTemplate()

	version := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", PublishRequest{}), http.StatusCreated)
	if version.Links == nil || len(*version.Links) != 1 {
		t.Fatalf("published links = %v, want the one anchor of the fixture", version.Links)
	}

	// The template now points at the version it published.
	after := decodeInto[Template](t, e.do(http.MethodGet,
		"/api/v1/templates/"+tpl.Id.String(), nil), http.StatusOK)
	if after.PublishedVersionId == nil || *after.PublishedVersionId != version.Id {
		t.Fatalf("published_version_id = %v, want %v", after.PublishedVersionId, version.Id)
	}

	// Preview goes through the same render path, so the locale fallback and
	// the bindings behave exactly as they will at send time.
	preview := decodeInto[PreviewResult](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/preview", PreviewRequest{
			Locale:    ptr("ko"),
			Recipient: &PreviewRecipient{Name: ptr("지민"), Email: ptr(Email("a@example.com"))},
		}), http.StatusOK)
	if preview.Locale != "ko" {
		t.Fatalf("preview locale = %q, want ko", preview.Locale)
	}
	if !strings.Contains(preview.Subject, "안녕하세요") || !strings.Contains(preview.Subject, "지민") {
		t.Fatalf("preview subject = %q, want the Korean greeting and the name", preview.Subject)
	}
	if !strings.Contains(preview.Html, "안녕하세요") {
		t.Fatalf("preview html = %q", preview.Html)
	}

	// template_id is kept and resolved at start; version_id would pin one now.
	camp := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "spring", SenderId: snd.Id, TemplateId: tpl.Id, DefaultLocale: ptr("en"),
	}), http.StatusCreated)
	if camp.TemplateId == nil || *camp.TemplateId != *tpl.Id {
		t.Fatalf("campaign template_id = %v, want %v", camp.TemplateId, *tpl.Id)
	}
	if camp.VersionId != nil {
		t.Fatalf("campaign version_id = %v, want it unpinned until start", camp.VersionId)
	}
	if camp.Status != CampaignStatusDraft {
		t.Fatalf("new campaign status = %q, want draft", camp.Status)
	}

	ndjson := strings.Join([]string{
		`{"email":"a@example.com","name":"A","locale":"ko","vars":{"plan":"pro"}}`,
		`{"email":"B@Example.com","name":"B"}`,
		`{"email":"a@example.com","name":"dup"}`,
		`{"email":"not-an-email"}`,
		``,
	}, "\n")
	ingested := decodeInto[RecipientIngestResult](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/recipients", strings.NewReader(ndjson),
		withHeader("Content-Type", "application/x-ndjson"),
		withHeader("Idempotency-Key", "chunk-1")), http.StatusOK)
	if ingested.Accepted != 2 || ingested.Duplicates != 1 || ingested.Invalid != 1 || ingested.Total != 2 {
		t.Fatalf("ingest result = %+v, want accepted 2, duplicates 1, invalid 1, total 2", ingested)
	}

	// Replaying the chunk must not insert anything twice.
	replay := decodeInto[RecipientIngestResult](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/recipients", strings.NewReader(ndjson),
		withHeader("Content-Type", "application/x-ndjson"),
		withHeader("Idempotency-Key", "chunk-1")), http.StatusOK)
	if replay.IdempotentReplay == nil || !*replay.IdempotentReplay {
		t.Fatalf("second chunk = %+v, want idempotent_replay", replay)
	}
	if replay.Total != 2 {
		t.Fatalf("total after replay = %d, want 2", replay.Total)
	}

	started := decodeInto[Campaign](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/start", nil), http.StatusOK)
	if started.Status != CampaignStatusRunning {
		t.Fatalf("status after start = %q, want running", started.Status)
	}
	// Start is where the template's published version gets pinned.
	if started.VersionId == nil || *started.VersionId != version.Id {
		t.Fatalf("version_id after start = %v, want the published %v", started.VersionId, version.Id)
	}

	// A running campaign no longer accepts recipients.
	w := e.do(http.MethodPost, "/api/v1/campaigns/"+camp.Id.String()+"/recipients",
		strings.NewReader(`{"email":"c@example.com"}`+"\n"),
		withHeader("Content-Type", "application/x-ndjson"))
	decodeError(t, w, http.StatusConflict, ErrorCodeInvalidState)

	list := decodeInto[DeliveryList](t, e.do(http.MethodGet,
		"/api/v1/campaigns/"+camp.Id.String()+"/deliveries", nil), http.StatusOK)
	if len(list.Items) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(list.Items))
	}
	for _, d := range list.Items {
		if d.Status != DeliveryStatusPending {
			t.Fatalf("delivery %s status = %q, want pending", d.Id, d.Status)
		}
		if d.Lane != LaneBulk {
			t.Fatalf("delivery %s lane = %q, want bulk", d.Id, d.Lane)
		}
	}
	// The address filter matches the normalized form, so the mixed-case line
	// is found by its lowercase address.
	filtered := decodeInto[DeliveryList](t, e.do(http.MethodGet,
		"/api/v1/campaigns/"+camp.Id.String()+"/deliveries?email=b@example.com", nil), http.StatusOK)
	if len(filtered.Items) != 1 {
		t.Fatalf("filtered deliveries = %d, want 1", len(filtered.Items))
	}

	// Links come from the published version even before anybody clicked.
	links := decodeInto[LinkClickList](t, e.do(http.MethodGet,
		"/api/v1/campaigns/"+camp.Id.String()+"/links", nil), http.StatusOK)
	if len(links.Items) != 1 || links.Items[0].Url != "https://example.com/go" || links.Items[0].LinkNo != 0 {
		t.Fatalf("links = %+v", links.Items)
	}

	paused := decodeInto[Campaign](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/pause", nil), http.StatusOK)
	if paused.Status != CampaignStatusPaused {
		t.Fatalf("status after pause = %q", paused.Status)
	}
	resumed := decodeInto[Campaign](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/resume", nil), http.StatusOK)
	if resumed.Status != CampaignStatusRunning {
		t.Fatalf("status after resume = %q", resumed.Status)
	}
	cancelled := decodeInto[Campaign](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/cancel", nil), http.StatusAccepted)
	if cancelled.Status != CampaignStatusCancelled {
		t.Fatalf("status after cancel = %q", cancelled.Status)
	}
}

// The host-mode unsubscribe notification: sendplane never saw the click, the
// host reports it, and it has to land in tracking, suppression and the outbox.
func TestCampaignUnsubscribeNotification(t *testing.T) {
	e := newEnv(t)
	snd := e.seedSender()
	tpl := e.seedTemplate()
	version := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil), http.StatusCreated)
	camp := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "c", SenderId: snd.Id, VersionId: &version.Id,
	}), http.StatusCreated)
	e.do(http.MethodPost, "/api/v1/campaigns/"+camp.Id.String()+"/recipients",
		strings.NewReader(`{"email":"a@example.com"}`+"\n"),
		withHeader("Content-Type", "application/x-ndjson"))

	got := decodeInto[UnsubscribeResult](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/unsubscribes",
		map[string]any{"email": "A@Example.com", "source": "host"}), http.StatusOK)
	if !got.Recorded {
		t.Fatal("recorded = false on the first notification")
	}
	if got.Suppressed == nil || !*got.Suppressed {
		t.Fatal("suppression is enabled by default but the address was not suppressed")
	}

	e.flush()
	list, err := e.st.Deliveries().ListByCampaign(t.Context(), camp.Id.String(),
		store.DeliveryFilter{}, store.Page{})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].UnsubscribedAt.IsZero() {
		t.Fatalf("unsubscribed_at was not derived: %+v", list.Items)
	}

	events, err := e.st.Outbox().List(t.Context(), store.OutboxPending, store.Page{})
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	found := false
	for _, ev := range events.Items {
		if ev.Type == eventRecipientUnsubscribed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event in the outbox: %+v", eventRecipientUnsubscribed, events.Items)
	}

	// An unknown address in the campaign is a 404, not a silent success.
	w := e.do(http.MethodPost, "/api/v1/campaigns/"+camp.Id.String()+"/unsubscribes",
		map[string]any{"email": "nobody@example.com"})
	decodeError(t, w, http.StatusNotFound, ErrorCodeNotFound)
}

// The i18n bundle has two representations of the same data; a YAML export must
// import back into the identical bundle.
func TestI18nYAMLRoundTripOverHTTP(t *testing.T) {
	e := newEnv(t)
	tpl := e.seedTemplate()
	base := "/api/v1/templates/" + tpl.Id.String() + "/i18n"

	w := e.do(http.MethodGet, base+"?format=yaml", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-yaml" {
		t.Fatalf("Content-Type = %q, want application/x-yaml", ct)
	}
	yamlBody := w.Body.String()
	if !strings.Contains(yamlBody, "default_locale: en") || !strings.Contains(yamlBody, "greeting") {
		t.Fatalf("yaml export = %q", yamlBody)
	}

	// Accept negotiates the same representation when format is absent.
	viaAccept := e.do(http.MethodGet, base, nil, withHeader("Accept", "application/x-yaml"))
	if viaAccept.Body.String() != yamlBody {
		t.Fatalf("Accept negotiation differs from format=yaml:\n%s\n---\n%s", viaAccept.Body.String(), yamlBody)
	}

	// Round trip: PUT the YAML back and read it again.
	imported := decodeInto[I18nImportResult](t, e.do(http.MethodPut, base,
		[]byte(yamlBody), withHeader("Content-Type", "application/x-yaml")), http.StatusOK)
	if imported.Locales != 2 || imported.Keys != 1 {
		t.Fatalf("import result = %+v, want 2 locales and 1 key", imported)
	}
	if imported.MissingKeys != nil {
		t.Fatalf("round trip reported missing keys: %+v", *imported.MissingKeys)
	}

	again := e.do(http.MethodGet, base+"?format=yaml", nil)
	if again.Body.String() != yamlBody {
		t.Fatalf("round trip changed the bundle:\n%s\n---\n%s", again.Body.String(), yamlBody)
	}

	// JSON is the default representation.
	asJSON := decodeInto[I18nBundle](t, e.do(http.MethodGet, base, nil), http.StatusOK)
	if asJSON.Locales == nil || (*asJSON.Locales)["ko"]["greeting"] != "안녕하세요" {
		t.Fatalf("json export = %+v", asJSON)
	}

	// A YAML body that is not a bundle is a 422, not a 500.
	bad := e.do(http.MethodPut, base, []byte("default_locale: [nope\n"),
		withHeader("Content-Type", "application/x-yaml"))
	decodeError(t, bad, http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func TestI18nKeysReportCoverage(t *testing.T) {
	e := newEnv(t)
	tpl := e.seedTemplate()
	// Drop the Korean translation so the report has something to say.
	locales := map[string]map[string]string{"en": {"greeting": "Hello"}, "ko": {}}
	e.do(http.MethodPut, "/api/v1/templates/"+tpl.Id.String()+"/i18n",
		I18nBundle{DefaultLocale: ptr("en"), Locales: &locales})

	got := decodeInto[I18nKeyList](t, e.do(http.MethodGet,
		"/api/v1/templates/"+tpl.Id.String()+"/i18n/keys", nil), http.StatusOK)
	if len(got.Items) != 1 || got.Items[0].Key != "greeting" {
		t.Fatalf("keys = %+v", got.Items)
	}
	if got.Items[0].UsedIn == nil || len(*got.Items[0].UsedIn) != 2 {
		t.Fatalf("used_in = %v, want subject and html", got.Items[0].UsedIn)
	}
	if got.Items[0].MissingLocales == nil || (*got.Items[0].MissingLocales)[0] != "ko" {
		t.Fatalf("missing_locales = %v, want [ko]", got.Items[0].MissingLocales)
	}
}
