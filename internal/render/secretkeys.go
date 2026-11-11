package render

import (
	"sort"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// SecretRoot is the binding a sensitive delivery's secret_vars are rendered
// under (ADR-0019).
const SecretRoot = "secret"

// SecretUsage reports whether a version reads the `secret` binding and which
// of its top-level keys it reads statically, sorted: `{{ secret.link.url }}`
// needs "link". Every part a recipient can render is scanned, the
// translations included, since a translation is Liquid too.
//
// reads is true for any reference to the top-level `secret` variable: dotted
// (`secret.code`), bracketed (`secret["code"]`), a computed index
// (`secret[k]`), or the whole of it handed to a filter or a tag
// (`{{ secret | json }}`, `{% for x in secret %}`). keys lists only the keys
// that can be known without rendering, so a version can read the secret and
// still have no keys. What only looks like it is not a reference: a property
// of another object (`vars.secret.code`), text in a string literal
// (`{{ "secret.code" | t }}`, `x["secret"]`), a keyword argument's name, and
// anything inside {% comment %} or {% raw %} (scanRootRefs, liveSpans).
//
// Shadowing: before secret_vars, `secret` was not a binding, and a template
// was free to declare its own. A version that declares a local `secret`
// anywhere — `{% assign secret = … %}`, `{% capture secret %}`,
// `{% for secret in … %}`, `{% tablerow secret in … %}`,
// `{% increment secret %}`, `{% decrement secret %}`, or a `secret:` keyword
// argument to `t`, which binds `secret` in the translation it renders — is
// not a secret template: SecretUsage reports reads=false and no keys, which
// keeps such a legacy template sending as it always did. A send with
// secret_vars against it is refused like for any version that reads no
// secret.
//
// The API uses it to demand the keys from a transactional send and to refuse
// secret_vars nothing reads, and to refuse a campaign on a version that
// reads the secret; the sender uses it to refuse a delivery that carries no
// secret (ADR-0019).
func SecretUsage(v *store.MessageVersion) (keys []string, reads bool) {
	if v == nil {
		return nil, false
	}
	srcs := []string{v.SubjectTpl, v.HTMLTpl, v.TextTpl}
	for _, msgs := range v.I18n.Locales {
		for _, m := range msgs {
			srcs = append(srcs, m)
		}
	}
	seen := map[string]bool{}
	for _, src := range srcs {
		for _, sp := range liveSpans(src) {
			if declaresRoot(SecretRoot, sp) {
				return nil, false
			}
			scanRootRefs(SecretRoot, sp.inner, func(path string) {
				reads = true
				if top, _, _ := strings.Cut(path, "."); top != "" && !seen[top] {
					seen[top] = true
					keys = append(keys, top)
				}
			})
		}
	}
	sort.Strings(keys)
	return keys, reads
}

// declaresRoot reports whether one construct declares a local variable named
// root (see SecretUsage for the list).
func declaresRoot(root string, sp span) bool {
	if sp.kind == spanObject {
		recv, filters := filterChain(sp.inner)
		for _, f := range filters {
			if filterName(f) == "t" {
				if a := filterArgs(f); a != "" && tKwargsName(recv+", "+a, root) {
					return true
				}
			}
		}
		return false
	}
	name, args := tagNameArgs(sp.inner)
	switch name {
	case "assign", "capture", "for", "tablerow", "increment", "decrement":
		return args[:identEnd(args, 0)] == root
	case "t":
		return tKwargsName(args, root)
	}
	return false
}

// tKwargsName reports whether the argument text of a `t` tag passes a
// keyword argument called name.
func tKwargsName(args, name string) bool {
	_, _, kwargs, err := parseTagArgs(args)
	if err != nil {
		return false
	}
	for _, kw := range kwargs {
		if kw.name == name {
			return true
		}
	}
	return false
}
