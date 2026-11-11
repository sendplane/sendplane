package render

import (
	"strings"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestSecretUsage(t *testing.T) {
	for _, tc := range []struct {
		src   string
		reads bool
		keys  string
	}{
		// Not the secret binding: a property of another object, text in a
		// string literal, a bracket key on another object.
		{src: `{{ vars.secret.code }}`},
		{src: `{{ recipient.vars.secret }}`},
		{src: `{{ "secret.code" | t }}`},
		{src: `{{ x["secret"] }}`},
		{src: `{{ x['secret'].code }}`},
		{src: `{{ subsecret.code }} {{ secret_x.code }} {{ secret-x.code }} {{ secret?.code }}`},
		{src: `{% if vars.secret.code == "secret.code" %}x{% endif %}`},
		{src: `{{ "k" | t: secret: vars.x }}`}, // a keyword argument's name, and a declaration
		{src: `{% comment %}{{ secret.code }}{% endcomment %}`},
		{src: `{% raw %}{{ secret.code }}{% endraw %}`},
		{src: `plain text about a secret`},

		// The secret binding.
		{src: `{{ secret.code }}`, reads: true, keys: "code"},
		{src: `{{secret.code}}`, reads: true, keys: "code"},
		{src: `{{- secret.code -}}`, reads: true, keys: "code"},
		{src: `{{ secret["code"] }}`, reads: true, keys: "code"},
		{src: `{{ secret['code'] }}`, reads: true, keys: "code"},
		{src: `{{ secret.link.url }}`, reads: true, keys: "link"},
		{src: `{{ secret.reset-link }}`, reads: true, keys: "reset-link"},
		{src: `{{ secret[k] }}`, reads: true},
		{src: `{{ secret | json }}`, reads: true},
		{src: `{{ secret }}`, reads: true},
		{src: `{% if secret.code %}x{% endif %}`, reads: true, keys: "code"},
		{src: `{% for x in secret %}{{ x }}{% endfor %}`, reads: true},
		{src: `{% for i in (1..secret.n) %}{% endfor %}`, reads: true, keys: "n"},
		{src: `{{ x | default: secret.code }}`, reads: true, keys: "code"},
		{src: `{{ "k" | t: code: secret.code }}`, reads: true, keys: "code"},
		{src: `{% t "k", code: secret.code %}`, reads: true, keys: "code"},
		{src: `{{ vars.secret }} {{ secret.code }}`, reads: true, keys: "code"},
		{src: `{{ "secret.x" }} {{ secret.b }} {{ secret.a }} {{ secret.a }}`, reads: true, keys: "a,b"},
		{src: `{% comment %}x{% endcomment %}{{ secret.code }}`, reads: true, keys: "code"},
	} {
		keys, reads := SecretUsage(&store.MessageVersion{HTMLTpl: tc.src})
		if reads != tc.reads || strings.Join(keys, ",") != tc.keys {
			t.Errorf("%s: SecretUsage = %q, %v; want %q, %v", tc.src, keys, reads, tc.keys, tc.reads)
		}
	}
}

// A version that declares its own `secret` is a legacy template, not a
// secret template, wherever the declaration is and whatever else reads it.
func TestSecretUsageShadowed(t *testing.T) {
	for _, decl := range []string{
		`{% assign secret = "x" %}`,
		`{%- assign secret="x" -%}`,
		`{% capture secret %}x{% endcapture %}`,
		`{% for secret in vars.list %}{% endfor %}`,
		`{% tablerow secret in vars.list %}{% endtablerow %}`,
		`{% increment secret %}`,
		`{% decrement secret %}`,
		`{% t "k", secret: vars.x %}`,
		`{{ "k" | t: secret: vars.x }}`,
	} {
		v := &store.MessageVersion{SubjectTpl: decl, HTMLTpl: `{{ secret.code }}`}
		if keys, reads := SecretUsage(v); reads || keys != nil {
			t.Errorf("%s: SecretUsage = %q, %v; want nil, false", decl, keys, reads)
		}
	}
	// Not declarations: cycle and assigning another name from the secret.
	for _, src := range []string{
		`{% cycle secret.a, "b" %}`,
		`{% assign code = secret.a %}`,
	} {
		if keys, reads := SecretUsage(&store.MessageVersion{HTMLTpl: src}); !reads || strings.Join(keys, ",") != "a" {
			t.Errorf("%s: SecretUsage = %q, %v; want [a], true", src, keys, reads)
		}
	}
}

// Every part is scanned, translations included.
func TestSecretUsageAllParts(t *testing.T) {
	v := &store.MessageVersion{
		SubjectTpl: `{{ secret.code }}`,
		TextTpl:    `{{ secret["token"] }}`,
		I18n: store.I18nBundle{Locales: map[string]map[string]string{
			"en": {"k": "{{ secret.name }}"},
		}},
	}
	if keys, reads := SecretUsage(v); !reads || strings.Join(keys, ",") != "code,name,token" {
		t.Fatalf("SecretUsage = %q, %v", keys, reads)
	}
	if keys, reads := SecretUsage(nil); reads || keys != nil {
		t.Fatal("a nil version reads a secret")
	}
}

// The same root rules apply to the tenant binding (ADR-0017).
func TestExtractVarsOnlyTheRoot(t *testing.T) {
	for _, src := range []string{
		`{{ vars.tenant.slug }}`, `{{ "tenant.slug" | t }}`, `{{ x["tenant"].slug }}`,
		`{% comment %}{{ tenant.slug }}{% endcomment %}`,
	} {
		if got := ExtractVars("tenant", src); len(got) != 0 {
			t.Errorf("%s: ExtractVars = %q, want nothing", src, got)
		}
	}
	if got := ExtractVars("tenant", `{{ vars.tenant.x }}{{ tenant.slug }}`); strings.Join(got, ",") != "slug" {
		t.Errorf("ExtractVars = %q, want [slug]", got)
	}
}
