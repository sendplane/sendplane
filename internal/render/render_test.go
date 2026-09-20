package render

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func TestRenderGolden(t *testing.T) {
	v := publishFixture(t)
	r := NewRenderer()

	for _, tc := range []struct{ name, locale string }{
		{"en", "en"},
		{"ko", "ko-KR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := r.Prepare(v, tc.locale)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			b := fixtureBindings()
			b.Recipient.Locale = tc.locale
			out, warnings, err := p.Render(context.Background(), b)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
			golden(t, "golden/"+tc.name+".subject.txt", out.Subject)
			golden(t, "golden/"+tc.name+".html", out.HTML)
			golden(t, "golden/"+tc.name+".txt", out.Text)
		})
	}
}

// architecture 6.2 fallback chain:
// recipient locale -> language -> campaign default -> version default -> key.
func TestFallbackChain(t *testing.T) {
	v := &store.MessageVersion{
		ID:            "v1",
		SubjectTpl:    `{% t "hello" %}`,
		HTMLTpl:       `<p>{% t "hello" %}</p>`,
		TextTpl:       `{% t "hello" %}`,
		DefaultLocale: "en",
		I18n: store.I18nBundle{
			DefaultLocale: "en",
			Locales: map[string]map[string]string{
				"en":    {"hello": "hello"},
				"ko":    {"hello": "안녕"},
				"ko-KR": {},
				"de-AT": {"hello": "servus"},
			},
		},
	}
	r := NewRenderer()

	cases := []struct {
		name            string
		recipientLocale string
		campaignDefault string
		want            string
		wantWarning     bool
	}{
		{"exact", "ko", "", "안녕", false},
		{"region falls back to language", "ko-KR", "", "안녕", false},
		{"region with its own table", "de-AT", "", "servus", false},
		{"unknown falls back to campaign default", "fr", "ko", "안녕", false},
		{"unknown falls back to version default", "fr", "", "hello", false},
		{"empty recipient locale uses campaign default", "", "ko", "안녕", false},
		{"nothing at all uses version default", "", "", "hello", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := r.PrepareChain(v, tc.recipientLocale, tc.campaignDefault)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			out, warnings, err := p.Render(context.Background(), Bindings{
				Recipient: Recipient{Locale: tc.recipientLocale},
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if out.Subject != tc.want {
				t.Errorf("subject = %q, want %q (chain %v)", out.Subject, tc.want, p.LocaleChain())
			}
			if got := len(warnings) > 0; got != tc.wantWarning {
				t.Errorf("warnings = %v", warnings)
			}
		})
	}
}

// A key no locale defines renders as the key itself plus a warning, and the
// warning is returned rather than logged (architecture 6.2).
func TestMissingKeyRendersKeyAndWarns(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: `{% t "nope" %}`, HTMLTpl: `<p>{% t "nope" %}</p>`, TextTpl: "",
		DefaultLocale: "en",
		I18n:          store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{"en": {}}},
	}
	p, err := NewRenderer().Prepare(v, "ko")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out, warnings, err := p.Render(context.Background(), Bindings{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out.Subject != "nope" {
		t.Errorf("subject = %q, want the key", out.Subject)
	}
	if !hasWarning(warnings, WarnMissingKey, "nope") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestTranslationArgumentsAndFilterForm(t *testing.T) {
	v := &store.MessageVersion{
		ID:            "v1",
		SubjectTpl:    `{% t "greeting" name: recipient.name, count: 3 %}`,
		HTMLTpl:       `<p>{{ "greeting" | t: name: recipient.name, count: 3 }}</p>`,
		TextTpl:       `{{ "plain" | t }}`,
		DefaultLocale: "en",
		I18n: store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{
			"en": {
				"greeting": "Hi {{ name }}, you have {{ count }} orders",
				"plain":    "plain value",
			},
		}},
	}
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out, _, err := p.Render(context.Background(), Bindings{Recipient: Recipient{Name: "Ada"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "Hi Ada, you have 3 orders"
	if out.Subject != want {
		t.Errorf("subject = %q, want %q", out.Subject, want)
	}
	if out.HTML != "<p>"+want+"</p>" {
		t.Errorf("html = %q", out.HTML)
	}
	if out.Text != "plain value" {
		t.Errorf("text = %q", out.Text)
	}
}

// ADR-0004: bindings are HTML-escaped for the HTML part only.
func TestEscaping(t *testing.T) {
	v := &store.MessageVersion{
		ID:            "v1",
		SubjectTpl:    `{{ recipient.name }}`,
		HTMLTpl:       `<p>{{ recipient.name }}</p><div>{{ vars.bio }}</div><span>{{ vars.trusted }}</span>`,
		TextTpl:       `{{ recipient.name }} {{ vars.trusted }}`,
		DefaultLocale: "en",
		I18n:          store.I18nBundle{DefaultLocale: "en"},
	}
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	evil := `<script>alert("xss")</script>`
	out, _, err := p.Render(context.Background(), Bindings{
		Recipient: Recipient{Name: evil},
		Vars: map[string]any{
			"bio":     []any{evil},
			"trusted": map[string]any{HTMLMarkerKey: "<b>ok</b>"},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out.HTML, "<script>") {
		t.Errorf("html part is not escaped: %q", out.HTML)
	}
	if !strings.Contains(out.HTML, "&lt;script&gt;") {
		t.Errorf("html part lost the escaped value: %q", out.HTML)
	}
	if !strings.Contains(out.HTML, "<b>ok</b>") {
		t.Errorf("$html marker did not pass through: %q", out.HTML)
	}
	if out.Subject != evil {
		t.Errorf("subject was escaped: %q", out.Subject)
	}
	if !strings.Contains(out.Text, evil) || !strings.Contains(out.Text, "<b>ok</b>") {
		t.Errorf("text part was escaped: %q", out.Text)
	}
}

func TestEscapeBindingsIsPure(t *testing.T) {
	in := map[string]any{"a": "<b>", "n": []any{"<i>"}, "deep": map[string]any{"k": "&"}}
	got := escapeBindings(in).(map[string]any)
	if in["a"] != "<b>" {
		t.Error("input was mutated")
	}
	if got["a"] != "&lt;b&gt;" {
		t.Errorf("a = %v", got["a"])
	}
	if got["n"].([]any)[0] != "&lt;i&gt;" {
		t.Errorf("n = %v", got["n"])
	}
	if got["deep"].(map[string]any)["k"] != "&amp;" {
		t.Errorf("deep = %v", got["deep"])
	}
}

func TestHTMLMarkerNeedsToBeAlone(t *testing.T) {
	got := escapeBindings(map[string]any{HTMLMarkerKey: "<b>", "other": "x"}).(map[string]any)
	if got[HTMLMarkerKey] != "&lt;b&gt;" {
		t.Errorf("a marker with siblings must not pass raw: %v", got)
	}
}

// architecture 16: no CR/LF in a subject.
func TestHeaderInjectionInRenderedSubject(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: `Hi {{ recipient.name }}`, HTMLTpl: "<p>x</p>", TextTpl: "x",
		DefaultLocale: "en",
	}
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	_, _, err = p.Render(context.Background(), Bindings{
		Recipient: Recipient{Name: "Ada\r\nBcc: evil@example.com"},
	})
	if !errors.Is(err, ErrHeaderInjection) {
		t.Fatalf("err = %v, want ErrHeaderInjection", err)
	}
}

func TestRecipientVarsOverrideCampaignVars(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: `{{ vars.a }}/{{ vars.b }}/{{ recipient.vars.a }}`,
		HTMLTpl: "<p>x</p>", TextTpl: "x", DefaultLocale: "en",
	}
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out, _, err := p.Render(context.Background(), Bindings{
		Vars:      map[string]any{"a": "campaign", "b": "only-campaign"},
		Recipient: Recipient{Vars: map[string]any{"a": "recipient"}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out.Subject != "recipient/only-campaign/recipient" {
		t.Errorf("subject = %q", out.Subject)
	}
}

func TestOutputCap(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: "s", DefaultLocale: "en",
		HTMLTpl: `{% for i in (1..10000) %}<p>xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx</p>{% endfor %}`,
		TextTpl: "x",
	}
	p, err := NewRenderer(WithMaxOutputBytes(4096)).Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, _, err := p.Render(context.Background(), Bindings{}); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err = %v, want ErrOutputTooLarge", err)
	}
}

func TestRenderTimeout(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: "s", DefaultLocale: "en",
		HTMLTpl: `{% for i in (1..2000000) %}<p>x</p>{% endfor %}`, TextTpl: "x",
	}
	p, err := NewRenderer(WithMaxOutputBytes(0)).Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := p.Render(ctx, Bindings{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("render took %v after the deadline", elapsed)
	}
}

// architecture 16: the Liquid sandbox has no file access.
func TestPartialTagsDisabled(t *testing.T) {
	e := NewEngine()
	for _, src := range []string{`{% include "x" %}`, `{% render "x" %}`} {
		if err := e.Validate(src); err == nil {
			t.Errorf("%s parsed; it must be rejected", src)
		}
	}
}

func TestStrictVariables(t *testing.T) {
	lax := NewEngine()
	if _, err := renderWith(lax, `{{ nope }}`); err != nil {
		t.Errorf("lax engine: %v", err)
	}
	strict := NewEngine(WithStrictVariables())
	if _, err := renderWith(strict, `{{ nope }}`); err == nil {
		t.Error("strict engine accepted an undefined variable")
	}
}

func renderWith(e *Engine, src string) (string, error) {
	tpl, err := e.Parse(src)
	if err != nil {
		return "", err
	}
	out, serr := tpl.RenderString(map[string]any{})
	if serr != nil {
		return "", serr
	}
	return out, nil
}

func TestPrepareCaches(t *testing.T) {
	v := publishFixture(t)
	r := NewRenderer()
	a, err := r.Prepare(v, "ko")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Prepare(v, "ko")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("Prepare did not reuse the cached parse")
	}
	if _, err := r.Prepare(v, "en"); err != nil {
		t.Fatal(err)
	}
	if r.CacheLen() != 2 {
		t.Errorf("cache len = %d, want 2", r.CacheLen())
	}
	if _, err := r.Prepare(nil, "en"); !errors.Is(err, ErrNoVersion) {
		t.Errorf("err = %v", err)
	}
}

func TestCacheEviction(t *testing.T) {
	r := NewRenderer(WithCacheSize(2))
	for i := 0; i < 5; i++ {
		v := &store.MessageVersion{ID: fmt.Sprintf("v%d", i), SubjectTpl: "s", DefaultLocale: "en"}
		if _, err := r.Prepare(v, "en"); err != nil {
			t.Fatal(err)
		}
	}
	if r.CacheLen() != 2 {
		t.Errorf("cache len = %d, want 2", r.CacheLen())
	}
}

// One Prepared serves every sender goroutine.
func TestConcurrentRender(t *testing.T) {
	v := publishFixture(t)
	p, err := NewRenderer().Prepare(v, "ko-KR")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want, _, err := p.Render(context.Background(), fixtureBindings())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := fixtureBindings()
			b.Recipient.Email = fmt.Sprintf("user%d@example.com", i)
			out, warnings, err := p.Render(context.Background(), b)
			if err != nil {
				errs <- err
				return
			}
			if len(warnings) != 0 {
				errs <- fmt.Errorf("goroutine %d: warnings %v", i, warnings)
				return
			}
			if out.Subject != want.Subject {
				errs <- fmt.Errorf("goroutine %d: subject %q", i, out.Subject)
			}
			if !strings.Contains(out.HTML, b.Recipient.Email) {
				errs <- fmt.Errorf("goroutine %d: html lost its own binding", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// architecture 6.1: standard Liquid filters only. url_encode and default ship
// with osteele/liquid, so the engine adds none of its own.
func TestStandardFilters(t *testing.T) {
	cases := []struct{ src, want string }{
		{`{{ "a b&c" | url_encode }}`, "a+b%26c"},
		{`{{ missing | default: "fallback" }}`, "fallback"},
		{`{{ "x" | upcase }}`, "X"},
		{`{{ "<b>" | escape }}`, "&lt;b&gt;"},
		{`{% if 1 > 0 %}yes{% endif %}`, "yes"},
		{`{% for i in (1..3) %}{{ i }}{% endfor %}`, "123"},
	}
	e := NewEngine()
	for _, tc := range cases {
		got, err := renderWith(e, tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
	}
}
