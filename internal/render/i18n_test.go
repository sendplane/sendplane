package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestYAMLRoundTrip(t *testing.T) {
	src := readFixture(t, "template.i18n.yaml")
	bundle, warnings, err := ImportYAML([]byte(src))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if bundle.DefaultLocale != "en" {
		t.Errorf("default locale = %q", bundle.DefaultLocale)
	}
	if got := bundle.Locales["ko"]["welcome.title"]; got != "가입을 환영합니다" {
		t.Errorf("ko welcome.title = %q", got)
	}
	if kv, ok := bundle.Locales["ko-KR"]; !ok || len(kv) != 0 {
		t.Errorf("ko-KR = %#v, want an empty map", kv)
	}
	if !hasWarningLocale(warnings, WarnMissingTranslation, "ko-KR") {
		t.Errorf("no warning for the empty locale: %v", warnings)
	}

	out, err := ExportYAML(bundle)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if string(out) != src {
		t.Errorf("round trip changed the file:\n--- got ---\n%s\n--- want ---\n%s", out, src)
	}

	again, _, err := ImportYAML(out)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if bundleChecksum(again) != bundleChecksum(bundle) {
		t.Error("re-imported bundle differs")
	}
}

func TestExportYAMLIsStable(t *testing.T) {
	b := store.I18nBundle{
		DefaultLocale: "en",
		Locales: map[string]map[string]string{
			"en":    {"b": "B", "a": "A", "c": "{{ x }}"},
			"ko":    {"a": "가"},
			"ko-KR": {},
		},
	}
	first, err := ExportYAML(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, err := ExportYAML(b)
		if err != nil {
			t.Fatal(err)
		}
		if string(next) != string(first) {
			t.Fatalf("export is not deterministic:\n%s\n%s", first, next)
		}
	}
	want := "default_locale: en\nlocales:\n  en:\n    a: \"A\"\n    b: \"B\"\n    c: \"{{ x }}\"\n  ko:\n    a: \"가\"\n  ko-KR: {}\n"
	if string(first) != want {
		t.Errorf("got:\n%s\nwant:\n%s", first, want)
	}
}

// ADR-0004: a translator can break a Liquid tag, so import must catch it.
func TestImportYAMLRejectsBrokenLiquid(t *testing.T) {
	_, _, err := ImportYAML([]byte("default_locale: en\nlocales:\n  en:\n    a: \"{% if %}\"\n"))
	if !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("err = %v, want ErrInvalidBundle", err)
	}
	if !strings.Contains(err.Error(), "en/a") {
		t.Errorf("error does not name the key: %v", err)
	}
}

func TestImportYAMLErrors(t *testing.T) {
	if _, _, err := ImportYAML([]byte("\t not: yaml:")); !errors.Is(err, ErrInvalidBundle) {
		t.Errorf("malformed yaml: %v", err)
	}
	if _, _, err := ImportYAML([]byte("locales:\n  en:\n    a: b\n")); !errors.Is(err, ErrInvalidBundle) {
		t.Errorf("missing default_locale: %v", err)
	}
}

func TestExtractKeys(t *testing.T) {
	subject := `{% t "subject.key" %}`
	body := `<p>{{ "filter.key" | t }}</p><p>{% t "args.key" name: recipient.name, count: 3 %}</p>` +
		`<p>{{ "chained.key" | t | upcase }}</p><p>{% t dynamic.key %}</p>{% if x %}{% t "subject.key" %}{% endif %}`
	text := `{{ "text.key" | t: name: "x" }}`

	got, err := ExtractKeys(subject, body, text)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []string{"args.key", "chained.key", "filter.key", "subject.key", "text.key"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// The filter form is rewritten into the tag form before parsing, and `t` may
// not sit in the middle of a filter chain.
func TestRewriteTFilter(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{{ "a" | t }}`, `{% t "a" %}`},
		{`{{- "a" | t -}}`, `{%- t "a" -%}`},
		{`{{ "a" | t: name: x, count: 3 }}`, `{% t "a", name: x, count: 3 %}`},
		{`x {{ "a" | t }} y {{ b }} z`, `x {% t "a" %} y {{ b }} z`},
		{`{{ b }}`, `{{ b }}`},
		{`{{ "a|b" | t }}`, `{% t "a|b" %}`},
		{`{% t "a" %}`, `{% t "a" %}`},
	}
	for _, tc := range cases {
		got, err := rewriteTFilter(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := rewriteTFilter(`{{ "a" | t | upcase }}`); !errors.Is(err, ErrTFilterChain) {
		t.Errorf("err = %v, want ErrTFilterChain", err)
	}
}

func TestParseTagArgs(t *testing.T) {
	key, literal, kwargs, err := parseTagArgs(`"greeting" name: recipient.name, count: 3`)
	if err != nil {
		t.Fatal(err)
	}
	if key != "greeting" || !literal {
		t.Errorf("key = %q literal = %v", key, literal)
	}
	if len(kwargs) != 2 || kwargs[0].name != "name" || kwargs[1].expr != "3" {
		t.Errorf("kwargs = %#v", kwargs)
	}

	key, literal, _, err = parseTagArgs(`page.key`)
	if err != nil || key != "page.key" || literal {
		t.Errorf("dynamic key: %q %v %v", key, literal, err)
	}
	if _, _, _, err := parseTagArgs(""); err == nil {
		t.Error("want an error for an empty argument list")
	}
	if _, _, _, err := parseTagArgs(`"a" bad`); err == nil {
		t.Error("want an error for a malformed keyword argument")
	}
}

// A translation may itself use {% t %}, but not without bound.
func TestTranslationRecursionIsBounded(t *testing.T) {
	v := &store.MessageVersion{
		ID: "v1", SubjectTpl: `{% t "a" %}`, DefaultLocale: "en",
		I18n: store.I18nBundle{DefaultLocale: "en", Locales: map[string]map[string]string{
			"en": {"a": `A{% t "a" %}`},
		}},
	}
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatal(err)
	}
	out, warnings, err := p.Render(context.Background(), Bindings{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.HasPrefix(out.Subject, "AAAA") {
		t.Errorf("subject = %q", out.Subject)
	}
	if !hasWarning(warnings, WarnTranslationDepth, "a") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestLocaleChain(t *testing.T) {
	cases := []struct {
		requested []string
		version   string
		bundle    string
		want      string
	}{
		{[]string{"ko-KR"}, "en", "en", "ko-KR,ko,en"},
		{[]string{"ko-KR", "ja"}, "en", "en", "ko-KR,ko,ja,en"},
		{[]string{""}, "en-GB", "en", "en-GB,en"},
		{[]string{"en", "en"}, "en", "en", "en"},
	}
	for _, tc := range cases {
		got := strings.Join(localeChain(tc.requested, tc.version, tc.bundle), ",")
		if got != tc.want {
			t.Errorf("localeChain(%v, %q, %q) = %q, want %q", tc.requested, tc.version, tc.bundle, got, tc.want)
		}
	}
}

func hasWarningLocale(ws []Warning, code WarningCode, locale string) bool {
	for _, w := range ws {
		if w.Code == code && w.Locale == locale {
			return true
		}
	}
	return false
}
