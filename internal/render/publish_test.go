package render

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func publishFixture(t testing.TB) *store.MessageVersion {
	t.Helper()
	tpl, layout := fixture(t)
	v, warnings, err := Publish(context.Background(), tpl, layout, PublishOptions{
		NewID: func() string { return "version-1" },
		Now:   func() time.Time { return time.Unix(0, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	for _, w := range warnings {
		t.Logf("publish warning: %s", w)
	}
	return v
}

func TestPublishGolden(t *testing.T) {
	v := publishFixture(t)
	golden(t, "golden/version.html", v.HTMLTpl)
	golden(t, "golden/version.txt", v.TextTpl)

	if v.TenantID != "tenant-1" || v.TemplateID != "template-1" || v.LayoutID != "layout-1" {
		t.Errorf("identity not carried over: %+v", v)
	}
	if v.DefaultLocale != "en" {
		t.Errorf("default locale = %q, want en", v.DefaultLocale)
	}
	if v.SubjectTpl != fixtureSubject {
		t.Errorf("subject = %q", v.SubjectTpl)
	}
	if len(v.Checksum) != 64 {
		t.Errorf("checksum = %q, want 64 hex chars", v.Checksum)
	}
}

// The template bundle must win over the layout bundle, and the layout's
// default locale must be ignored (architecture 6.2).
func TestPublishMergesBundles(t *testing.T) {
	v := publishFixture(t)
	if got := v.I18n.Locales["en"]["cta.label"]; got != "Open dashboard" {
		t.Errorf("template key did not win: %q", got)
	}
	if got := v.I18n.Locales["ko"]["layout.greeting"]; got == "" {
		t.Error("layout key missing after merge")
	}
	if _, ok := v.I18n.Locales["ko-KR"]; !ok {
		t.Error("empty locale dropped in merge")
	}
}

// architecture 9.2: link_no is document order, the unsubscribe link and
// mailto/anchor/data-sp-track="off" links are excluded.
func TestPublishExtractsLinksInOrder(t *testing.T) {
	v := publishFixture(t)
	want := []string{
		"https://example.com/orders/{{ vars.order.id }}",
		"https://example.com/dashboard",
	}
	if len(v.Links) != len(want) {
		t.Fatalf("links = %#v, want %#v", v.Links, want)
	}
	for i := range want {
		if v.Links[i] != want[i] {
			t.Errorf("link %d = %q, want %q", i, v.Links[i], want[i])
		}
	}
	if !strings.Contains(v.HTMLTpl, `data-sp-track="off"`) {
		t.Error("unsubscribe anchor was not marked data-sp-track=off")
	}
}

// ADR-0009: Liquid control flow inside <mj-raw> must survive MJML compilation
// and text derivation, in the right order.
func TestPublishMJMLRawSurvives(t *testing.T) {
	v := publishFixture(t)
	for _, part := range []struct{ name, src string }{{"html", v.HTMLTpl}, {"text", v.TextTpl}} {
		iff := strings.Index(part.src, "{% if vars.vip %}")
		note := strings.Index(part.src, `{% t "vip.note"`)
		end := strings.Index(part.src, "{% endif %}")
		if iff < 0 || note < 0 || end < 0 {
			t.Fatalf("%s: mj-raw control flow lost: if=%d note=%d endif=%d", part.name, iff, note, end)
		}
		if iff >= note || note >= end {
			t.Errorf("%s: mj-raw control flow reordered: if=%d note=%d endif=%d", part.name, iff, note, end)
		}
	}
}

func TestPublishChecksumStability(t *testing.T) {
	a := publishFixture(t)
	b := publishFixture(t)
	if a.Checksum != b.Checksum {
		t.Errorf("checksum not deterministic: %s vs %s", a.Checksum, b.Checksum)
	}

	tpl, layout := fixture(t)
	tpl.Subject += "!"
	c, _, err := Publish(context.Background(), tpl, layout, PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if c.Checksum == a.Checksum {
		t.Error("checksum did not change with the subject")
	}
}

func TestPublishMissingKey(t *testing.T) {
	tpl, layout := fixture(t)
	delete(tpl.I18n.Locales["en"], "welcome.title")

	_, _, err := Publish(context.Background(), tpl, layout, PublishOptions{})
	if !errors.Is(err, ErrMissingKeys) {
		t.Fatalf("err = %v, want ErrMissingKeys", err)
	}
	if !strings.Contains(err.Error(), "welcome.title") {
		t.Errorf("error does not name the key: %v", err)
	}

	v, warnings, err := Publish(context.Background(), tpl, layout, PublishOptions{AllowMissingKeys: true})
	if err != nil {
		t.Fatalf("AllowMissingKeys: %v", err)
	}
	if v == nil {
		t.Fatal("no version")
	}
	if !hasWarning(warnings, WarnMissingKey, "welcome.title") {
		t.Errorf("no missing_key warning: %v", warnings)
	}
}

func TestPublishHTMLModePassesThrough(t *testing.T) {
	tpl := &store.Template{
		ID: "t", TenantID: "tn", Mode: store.ContentHTML,
		Subject: "Hi", Body: `<html><body><a href="https://example.com/x">x</a></body></html>`,
		I18n: store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{"en": {}}},
	}
	v, _, err := Publish(context.Background(), tpl, nil, PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !strings.HasPrefix(v.HTMLTpl, "<html>") {
		t.Errorf("html mode was compiled: %q", v.HTMLTpl)
	}
	if v.TextTpl != "x ( https://example.com/x )" {
		t.Errorf("derived text = %q", v.TextTpl)
	}
}

func TestPublishKeepsExplicitText(t *testing.T) {
	tpl := &store.Template{
		ID: "t", Mode: store.ContentHTML, Subject: "Hi",
		Body: "<p>hello</p>", Text: "written by hand",
		I18n: store.I18nBundle{DefaultLocale: "en"},
	}
	v, _, err := Publish(context.Background(), tpl, nil, PublishOptions{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if v.TextTpl != "written by hand" {
		t.Errorf("text = %q", v.TextTpl)
	}
}

func TestPublishErrors(t *testing.T) {
	base := func() *store.Template {
		return &store.Template{ID: "t", Mode: store.ContentHTML, Subject: "Hi", Body: "<p>x</p>",
			I18n: store.I18nBundle{DefaultLocale: "en"}}
	}
	t.Run("no template", func(t *testing.T) {
		if _, _, err := Publish(context.Background(), nil, nil, PublishOptions{}); !errors.Is(err, ErrNoTemplate) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown mode", func(t *testing.T) {
		tpl := base()
		tpl.Mode = "wat"
		if _, _, err := Publish(context.Background(), tpl, nil, PublishOptions{}); !errors.Is(err, ErrUnknownMode) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no content slot", func(t *testing.T) {
		layout := &store.Layout{Mode: store.ContentHTML, Body: "<html><body>no slot</body></html>"}
		if _, _, err := Publish(context.Background(), base(), layout, PublishOptions{}); !errors.Is(err, ErrNoContentSlot) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("mode mismatch", func(t *testing.T) {
		layout := &store.Layout{Mode: store.ContentMJML, Body: "<mjml><mj-body>{{ content }}</mj-body></mjml>"}
		if _, _, err := Publish(context.Background(), base(), layout, PublishOptions{}); !errors.Is(err, ErrModeMismatch) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("subject header injection", func(t *testing.T) {
		tpl := base()
		tpl.Subject = "Hi\r\nBcc: evil@example.com"
		if _, _, err := Publish(context.Background(), tpl, nil, PublishOptions{}); !errors.Is(err, ErrHeaderInjection) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("liquid syntax error", func(t *testing.T) {
		tpl := base()
		tpl.Body = "<p>{% if %}</p>"
		if _, _, err := Publish(context.Background(), tpl, nil, PublishOptions{}); err == nil {
			t.Error("want a parse error")
		}
	})
	t.Run("invalid mjml", func(t *testing.T) {
		tpl := base()
		tpl.Mode = store.ContentMJML
		tpl.Body = "<mjml><mj-body><mj-nope/></mj-body></mjml>"
		if _, _, err := Publish(context.Background(), tpl, nil, PublishOptions{}); err == nil {
			t.Error("want an mjml error")
		}
	})
}

func TestMergeLayout(t *testing.T) {
	got, err := mergeLayout("<a>{{ content }}</a>", "<b/>")
	if err != nil {
		t.Fatal(err)
	}
	if got != "<a><b/></a>" {
		t.Errorf("got %q", got)
	}
	if _, err := mergeLayout("<a>{{content}}</a>", "x"); err != nil {
		t.Errorf("unspaced slot: %v", err)
	}
	if _, err := mergeLayout("<a/>", "x"); !errors.Is(err, ErrNoContentSlot) {
		t.Errorf("err = %v", err)
	}
}

func hasWarning(ws []Warning, code WarningCode, key string) bool {
	for _, w := range ws {
		if w.Code == code && w.Key == key {
			return true
		}
	}
	return false
}
