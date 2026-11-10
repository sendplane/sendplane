package render

import (
	"sort"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// SecretKeys returns the top-level `secret.*` keys a version reads, sorted:
// `{{ secret.link.url }}` needs "link". Every part a recipient can render is
// scanned, the translations included, since a translation is Liquid too.
//
// The API uses it to demand those keys from a transactional send, and to
// refuse a campaign on such a version; the sender uses it to refuse a
// delivery that carries no secret (ADR-0019).
func SecretKeys(v *store.MessageVersion) []string {
	if v == nil {
		return nil
	}
	srcs := []string{v.SubjectTpl, v.HTMLTpl, v.TextTpl}
	for _, msgs := range v.I18n.Locales {
		for _, m := range msgs {
			srcs = append(srcs, m)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, path := range ExtractVars("secret", srcs...) {
		top, _, _ := strings.Cut(path, ".")
		if top != "" && !seen[top] {
			seen[top] = true
			out = append(out, top)
		}
	}
	sort.Strings(out)
	return out
}
