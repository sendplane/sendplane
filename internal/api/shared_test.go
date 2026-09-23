package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// The HTTP half of ADR-0018: templates and layouts the system tenant shares,
// read through by every tenant, used by key, restricted by `uses`, and
// overridden by an explicit copy.

// sharedFixture is what the system tenant authors: a shared layout keyed
// "base" and a shared template keyed "welcome" that uses it, restricted to
// transactional mail and published.
type sharedFixture struct {
	layout   Layout
	template Template
	version  MessageVersion
}

func (e *platformEnv) seedShared(uses ...string) sharedFixture {
	e.t.Helper()
	l := decodeInto[Layout](e.t, e.do(http.MethodPost, "/api/v1/layouts", LayoutInput{
		Name: "base", Key: ptr("base"), Shared: ptr(true), Mode: ContentModeHtml,
		Body: `<div class="shared-layout">{{ content }}</div>`,
	}, asSystem()), http.StatusCreated)
	in := TemplateInput{
		Name: "welcome", Key: ptr("welcome"), Shared: ptr(true),
		LayoutId: l.Id, Mode: ContentModeHtml,
		Subject: "Shared welcome, {{ recipient.name }}",
		Body:    `<p>shared body</p>`,
	}
	if len(uses) > 0 {
		in.Uses = &uses
	}
	tpl := decodeInto[Template](e.t, e.do(http.MethodPost, "/api/v1/templates", in, asSystem()),
		http.StatusCreated)
	v := decodeInto[MessageVersion](e.t, e.do(http.MethodPost,
		"/api/v1/templates/"+tpl.Id.String()+"/publish", nil, asSystem()), http.StatusCreated)
	return sharedFixture{layout: l, template: tpl, version: v}
}

func findTemplate(items []Template, id string) *Template {
	for i := range items {
		if items[i].Id != nil && items[i].Id.String() == id {
			return &items[i]
		}
	}
	return nil
}

func (e *platformEnv) sendByKey(snd Sender, key string) MessageResult {
	e.t.Helper()
	return decodeInto[MessageResult](e.t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr(key),
		To: []MessageRecipient{{Email: "user@example.org", Name: ptr("U")}},
	}), http.StatusAccepted)
}

func TestSharedTemplateIsReadThroughAndReadOnly(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared(string(store.UseTransactional))
	id := fx.template.Id.String()

	list := decodeInto[TemplateList](t, e.do(http.MethodGet, "/api/v1/templates", nil), http.StatusOK)
	got := findTemplate(list.Items, id)
	if got == nil {
		t.Fatal("the tenant's template list does not contain the shared template")
	}
	if !deref(got.Shared) || deref(got.Key) != "welcome" {
		t.Fatalf("shared=%v key=%q, want a shared template keyed welcome", got.Shared, deref(got.Key))
	}
	if got.Uses == nil || len(*got.Uses) != 1 || (*got.Uses)[0] != "transactional" {
		t.Fatalf("uses = %v, want [transactional]", got.Uses)
	}
	decodeInto[Template](t, e.do(http.MethodGet, "/api/v1/templates/"+id, nil), http.StatusOK)
	decodeInto[PreviewResult](t, e.do(http.MethodPost, "/api/v1/templates/"+id+"/preview",
		PreviewRequest{}), http.StatusOK)
	versions := decodeInto[MessageVersionList](t, e.do(http.MethodGet,
		"/api/v1/templates/"+id+"/versions", nil), http.StatusOK)
	if len(versions.Items) != 1 {
		t.Fatalf("versions of the shared template = %d, want 1", len(versions.Items))
	}

	// Every write is refused with the same answer, whatever it is.
	for _, w := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/templates/" + id, TemplateUpdate{
			Name: "hijacked", Mode: ContentModeHtml, Subject: "x", Body: "x", Version: 1}},
		{http.MethodPost, "/api/v1/templates/" + id + "/publish", nil},
		{http.MethodPut, "/api/v1/templates/" + id + "/i18n", I18nBundle{}},
		{http.MethodDelete, "/api/v1/templates/" + id, nil},
		{http.MethodPut, "/api/v1/layouts/" + fx.layout.Id.String(), LayoutUpdate{
			Name: "x", Mode: ContentModeHtml, Body: "{{ content }}", Version: 1}},
		{http.MethodDelete, "/api/v1/layouts/" + fx.layout.Id.String(), nil},
	} {
		decodeError(t, e.do(w.method, w.path, w.body), http.StatusForbidden, ErrorCodePlatformReadOnly)
	}

	// Nothing was copied into the tenant by reading it.
	own, err := e.st.Templates().List(t.Context(), store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Items) != 0 {
		t.Fatalf("the tenant holds %d template rows after reading a shared one, want 0", len(own.Items))
	}
}

func TestSendByKeyUsesTheSharedTemplateAndItsPolicy(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared(string(store.UseTransactional))
	snd := e.seedSender()

	res := e.sendByKey(snd, "welcome")
	if res.VersionId != fx.version.Id {
		t.Fatalf("version_id = %v, want the shared version %v", res.VersionId, fx.version.Id)
	}
	// The delivery references the system tenant's version; the sender reads
	// it through the overlay, so nothing was copied.
	d, err := e.st.Deliveries().Get(t.Context(), res.Deliveries[0].DeliveryId.String())
	if err != nil {
		t.Fatal(err)
	}
	if d.VersionID != fx.version.Id.String() {
		t.Fatalf("delivery version = %s, want the shared one", d.VersionID)
	}

	// `uses` excludes campaigns: refused at create, by key and by ID.
	decodeError(t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "newsletter", SenderId: snd.Id, TemplateKey: ptr("welcome"),
	}), http.StatusForbidden, ErrorCodeTemplateUseDenied)
	decodeError(t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "newsletter", SenderId: snd.Id, TemplateId: fx.template.Id,
	}), http.StatusForbidden, ErrorCodeTemplateUseDenied)

	// Naming the template twice, or not at all, is a 422.
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr("welcome"), TemplateId: fx.template.Id,
		To: []MessageRecipient{{Email: "user@example.org"}},
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr("nope"),
		To: []MessageRecipient{{Email: "user@example.org"}},
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

func TestOverrideShadowsTheSharedTemplateUntilDeleted(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared()
	snd := e.seedSender()
	sharedID := fx.template.Id.String()

	over := decodeInto[Template](t, e.do(http.MethodPost,
		"/api/v1/templates/"+sharedID+"/override", nil), http.StatusCreated)
	if over.Id == nil || over.Id.String() == sharedID {
		t.Fatal("the override has no ID of its own")
	}
	if deref(over.Key) != "welcome" || deref(over.Shared) || !deref(over.Overridden) {
		t.Fatalf("override key=%q shared=%v overridden=%v", deref(over.Key), deref(over.Shared),
			deref(over.Overridden))
	}
	if over.Subject != fx.template.Subject || deref(over.Body) != deref(fx.template.Body) {
		t.Fatal("the override is not a full copy of the shared template")
	}
	if deref(over.LayoutId) != deref(fx.template.LayoutId) {
		t.Fatal("the override does not keep the shared layout reference")
	}
	// It starts out published with the shared version, by reference.
	if deref(over.PublishedVersionId) != fx.version.Id || deref(over.OverriddenFromVersionId) != fx.version.Id {
		t.Fatalf("override published=%v from=%v, want the shared version", over.PublishedVersionId,
			over.OverriddenFromVersionId)
	}
	if deref(over.SharedUpdatedSinceOverride) {
		t.Fatal("a fresh override reports the shared template as changed")
	}
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates/"+sharedID+"/override", nil),
		http.StatusConflict, ErrorCodeDuplicate)
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates/"+over.Id.String()+"/override", nil),
		http.StatusUnprocessableEntity, ErrorCodeValidationFailed)

	// Sending by key keeps working before the tenant publishes anything.
	if res := e.sendByKey(snd, "welcome"); res.VersionId != fx.version.Id {
		t.Fatalf("send before publishing the override used %v, want the shared version", res.VersionId)
	}

	// The list shows the override in place of the shared template.
	list := decodeInto[TemplateList](t, e.do(http.MethodGet, "/api/v1/templates", nil), http.StatusOK)
	if findTemplate(list.Items, sharedID) != nil {
		t.Fatal("the overridden shared template is still listed")
	}
	if got := findTemplate(list.Items, over.Id.String()); got == nil || !deref(got.Overridden) {
		t.Fatal("the override is not listed as overridden")
	}

	// Edit, publish, send by key: the tenant's own content goes out.
	over.Subject = "Tenant welcome"
	updated := decodeInto[Template](t, e.do(http.MethodPut, "/api/v1/templates/"+over.Id.String(),
		TemplateUpdate{
			Name: over.Name, Key: over.Key, LayoutId: over.LayoutId, Mode: over.Mode,
			Subject: over.Subject, Body: deref(over.Body), Version: deref(over.Version),
		}), http.StatusOK)
	own := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+updated.Id.String()+"/publish", nil), http.StatusCreated)
	if own.SubjectTpl != "Tenant welcome" {
		t.Fatalf("override version subject = %q", own.SubjectTpl)
	}
	// The shared layout is merged, read through.
	if !strings.Contains(own.HtmlTpl, "shared-layout") {
		t.Fatalf("the override was published without the shared layout: %s", own.HtmlTpl)
	}
	if res := e.sendByKey(snd, "welcome"); res.VersionId != own.Id {
		t.Fatalf("send by key used %v, want the override's version %v", res.VersionId, own.Id)
	}

	// The operator republishes the shared original: the override says so.
	decodeInto[MessageVersion](t, e.do(http.MethodPost, "/api/v1/templates/"+sharedID+"/publish",
		nil, asSystem()), http.StatusCreated)
	got := decodeInto[Template](t, e.do(http.MethodGet, "/api/v1/templates/"+over.Id.String(), nil),
		http.StatusOK)
	if !deref(got.SharedUpdatedSinceOverride) {
		t.Fatal("shared_updated_since_override is false after the shared template was republished")
	}

	// Deleting the override returns the tenant to the shared template.
	if w := e.do(http.MethodDelete, "/api/v1/templates/"+over.Id.String(), nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete override: %d %s", w.Code, w.Body.String())
	}
	res := e.sendByKey(snd, "welcome")
	sys, err := store.PlatformView(t.Context(), e.provider)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := sys.Templates().Get(t.Context(), sharedID)
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionId.String() != cur.PublishedVersionID {
		t.Fatalf("after deleting the override the send used %v, want the shared %s",
			res.VersionId, cur.PublishedVersionID)
	}
	// Only the explicit override was ever a row in the tenant, and it is gone.
	rows, err := e.st.Templates().List(t.Context(), store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Items) != 0 {
		t.Fatalf("the tenant holds %d templates, want 0", len(rows.Items))
	}
}

func TestOverriddenLayoutIsUsedByKey(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared()

	// A template of the tenant's own that names the shared layout.
	mine := decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "mine", LayoutId: fx.layout.Id, Mode: ContentModeHtml, Subject: "s", Body: "<p>mine</p>",
	}), http.StatusCreated)
	v := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+mine.Id.String()+"/publish", nil), http.StatusCreated)
	if !strings.Contains(v.HtmlTpl, "shared-layout") {
		t.Fatalf("published without the shared layout: %s", v.HtmlTpl)
	}

	lo := decodeInto[Layout](t, e.do(http.MethodPost,
		"/api/v1/layouts/"+fx.layout.Id.String()+"/override", nil), http.StatusCreated)
	if deref(lo.Key) != "base" || !deref(lo.Overridden) {
		t.Fatalf("layout override key=%q overridden=%v", deref(lo.Key), deref(lo.Overridden))
	}
	decodeInto[Layout](t, e.do(http.MethodPut, "/api/v1/layouts/"+lo.Id.String(), LayoutUpdate{
		Name: "base", Key: ptr("base"), Mode: ContentModeHtml,
		Body: `<div class="tenant-layout">{{ content }}</div>`, Version: deref(lo.Version),
	}), http.StatusOK)
	decodeError(t, e.do(http.MethodPost, "/api/v1/layouts/"+fx.layout.Id.String()+"/override", nil),
		http.StatusConflict, ErrorCodeDuplicate)

	v = decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+mine.Id.String()+"/publish", nil), http.StatusCreated)
	if !strings.Contains(v.HtmlTpl, "tenant-layout") {
		t.Fatalf("the tenant's layout override was not used: %s", v.HtmlTpl)
	}

	// The shared template, previewed from the tenant, still renders with the
	// shared layout it was published with.
	p := decodeInto[PreviewResult](t, e.do(http.MethodPost,
		"/api/v1/templates/"+fx.template.Id.String()+"/preview", PreviewRequest{}), http.StatusOK)
	if !strings.Contains(p.Html, "shared-layout") {
		t.Fatalf("the shared template's preview picked up the tenant's layout: %s", p.Html)
	}
}

func TestSharingRules(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())

	// Only the system tenant shares.
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "x", Key: ptr("x"), Shared: ptr(true), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	decodeError(t, e.do(http.MethodPost, "/api/v1/layouts", LayoutInput{
		Name: "x", Key: ptr("x"), Shared: ptr(true), Mode: ContentModeHtml, Body: "{{ content }}",
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	// A shared template needs a key.
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "x", Shared: ptr(true), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}, asSystem()), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	// Keys are validated.
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "x", Key: ptr("Not A Key"), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "x", Key: ptr("k"), Uses: &[]string{"probe"}, Mode: ContentModeHtml, Subject: "s", Body: "b",
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	// A key is unique per tenant.
	decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "a", Key: ptr("dup"), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}), http.StatusCreated)
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "b", Key: ptr("dup"), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}), http.StatusConflict, "")
	// A shared template's layout must be shared.
	plain := decodeInto[Layout](t, e.do(http.MethodPost, "/api/v1/layouts", LayoutInput{
		Name: "plain", Mode: ContentModeHtml, Body: "{{ content }}",
	}, asSystem()), http.StatusCreated)
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "x", Key: ptr("x"), Shared: ptr(true), LayoutId: plain.Id,
		Mode: ContentModeHtml, Subject: "s", Body: "b",
	}, asSystem()), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
	// The system tenant has nothing to override.
	fx := e.seedShared()
	decodeError(t, e.do(http.MethodPost, "/api/v1/templates/"+fx.template.Id.String()+"/override",
		nil, asSystem()), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)

	// An unshared system template is invisible, and so are its versions.
	priv := decodeInto[Template](t, e.do(http.MethodPost, "/api/v1/templates", TemplateInput{
		Name: "private", Key: ptr("private"), Mode: ContentModeHtml, Subject: "s", Body: "b",
	}, asSystem()), http.StatusCreated)
	pv := decodeInto[MessageVersion](t, e.do(http.MethodPost,
		"/api/v1/templates/"+priv.Id.String()+"/publish", nil, asSystem()), http.StatusCreated)
	decodeError(t, e.do(http.MethodGet, "/api/v1/templates/"+priv.Id.String(), nil),
		http.StatusNotFound, ErrorCodeNotFound)
	decodeError(t, e.do(http.MethodGet, "/api/v1/message-versions/"+pv.Id.String(), nil),
		http.StatusNotFound, ErrorCodeNotFound)
	decodeInto[MessageVersion](t, e.do(http.MethodGet,
		"/api/v1/message-versions/"+fx.version.Id.String(), nil), http.StatusOK)
	snd := e.seedSender()
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, VersionId: &pv.Id, To: []MessageRecipient{{Email: "u@example.org"}},
	}), http.StatusUnprocessableEntity, ErrorCodeValidationFailed)
}

// The start of a campaign re-runs the template policy: the operator may have
// narrowed a shared template's `uses` while the campaign sat in draft.
func TestStartRechecksTheTemplatePolicy(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared()
	snd := e.seedSender()
	camp := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "news", SenderId: snd.Id, TemplateKey: ptr("welcome"),
	}), http.StatusCreated)
	if deref(camp.TemplateId) != *fx.template.Id {
		t.Fatalf("campaign template_id = %v, want the shared template resolved from its key", camp.TemplateId)
	}
	decodeInto[RecipientIngestResult](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/recipients",
		strings.NewReader(`{"email":"r@example.com"}`+"\n"),
		withHeader("Content-Type", "application/x-ndjson")), http.StatusOK)

	cur := decodeInto[Template](t, e.do(http.MethodGet, "/api/v1/templates/"+fx.template.Id.String(),
		nil, asSystem()), http.StatusOK)
	decodeInto[Template](t, e.do(http.MethodPut, "/api/v1/templates/"+fx.template.Id.String(),
		TemplateUpdate{
			Name: cur.Name, Key: cur.Key, Shared: ptr(true), Uses: &[]string{"transactional"},
			LayoutId: cur.LayoutId, Mode: cur.Mode, Subject: cur.Subject, Body: deref(cur.Body),
			Version: deref(cur.Version),
		}, asSystem()), http.StatusOK)

	decodeError(t, e.do(http.MethodPost, "/api/v1/campaigns/"+camp.Id.String()+"/start", nil),
		http.StatusForbidden, ErrorCodeTemplateUseDenied)
}

// The hook replaces the default and may restrict a tenant's own templates,
// overrides included, which the default leaves alone.
func TestTemplatePolicyHook(t *testing.T) {
	var seen host.TemplateUse
	e := newPlatformEnv(t, testCatalog(), func(d *Deps) {
		d.Hooks.TemplatePolicy = func(ctx context.Context, u host.TemplateUse) error {
			seen = u
			if err := host.DefaultTemplatePolicy(ctx, u); err != nil {
				return err
			}
			if u.TemplateKey == "welcome" && !u.Shared && !contains(u.Uses, u.Kind) && len(u.Uses) > 0 {
				return errors.Join(host.ErrTemplateUseDenied, errors.New("overrides keep the shared uses"))
			}
			return nil
		}
	})
	fx := e.seedShared(string(store.UseCampaign))
	snd := e.seedSender()
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr("welcome"), To: []MessageRecipient{{Email: "u@example.org"}},
	}), http.StatusForbidden, ErrorCodeTemplateUseDenied)
	if !seen.Shared || seen.TemplateID != fx.template.Id.String() || seen.Kind != store.UseTransactional {
		t.Fatalf("the hook saw %+v", seen)
	}

	// The override is the tenant's own: the default allows it, the hook
	// above does not.
	decodeInto[Template](t, e.do(http.MethodPost,
		"/api/v1/templates/"+fx.template.Id.String()+"/override", nil), http.StatusCreated)
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr("welcome"), To: []MessageRecipient{{Email: "u@example.org"}},
	}), http.StatusForbidden, ErrorCodeTemplateUseDenied)
	if seen.Shared {
		t.Fatal("an override was reported to the policy as shared")
	}
}

func TestDefaultTemplatePolicyLeavesOverridesAlone(t *testing.T) {
	e := newPlatformEnv(t, testCatalog())
	fx := e.seedShared(string(store.UseCampaign))
	snd := e.seedSender()
	decodeError(t, e.do(http.MethodPost, "/api/v1/messages", MessageRequest{
		SenderId: snd.Id, TemplateKey: ptr("welcome"), To: []MessageRecipient{{Email: "u@example.org"}},
	}), http.StatusForbidden, ErrorCodeTemplateUseDenied)
	decodeInto[Template](t, e.do(http.MethodPost,
		"/api/v1/templates/"+fx.template.Id.String()+"/override", nil), http.StatusCreated)
	e.sendByKey(snd, "welcome")
}

func contains(uses []store.UseKind, k store.UseKind) bool {
	for _, u := range uses {
		if u == k {
			return true
		}
	}
	return false
}
