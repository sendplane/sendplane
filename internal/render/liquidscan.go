package render

import (
	"fmt"
	"sort"
	"strings"
)

// This file holds the small, quote-aware scanner that lets us look at Liquid
// source without rendering it: finding `{{ … }}` / `{% … %}` spans, extracting
// `t` keys, and rewriting the `{{ "key" | t }}` filter form into the
// `{% t "key" %}` tag form (see rewriteTFilter for why).

type spanKind int

const (
	spanObject spanKind = iota // {{ … }}
	spanTag                    // {% … %}
)

// span is one Liquid construct in a source string. start/end are byte offsets
// covering the whole construct including its delimiters; inner is the text
// between them with trim markers ("-") removed.
type span struct {
	kind       spanKind
	start, end int
	inner      string
	trimLeft   bool
	trimRight  bool
}

// scanLiquidSpans returns every `{{ … }}` and `{% … %}` construct in src, in
// order. Quoted strings inside a construct may contain the closing delimiter.
// Unterminated constructs at the end of src are ignored; the Liquid parser
// reports them properly later.
func scanLiquidSpans(src string) []span {
	var out []span
	for i := 0; i+1 < len(src); i++ {
		if src[i] != '{' {
			continue
		}
		var kind spanKind
		var closing string
		switch src[i+1] {
		case '{':
			kind, closing = spanObject, "}}"
		case '%':
			kind, closing = spanTag, "%}"
		default:
			continue
		}
		end, ok := scanToClose(src, i+2, closing)
		if !ok {
			continue
		}
		inner := src[i+2 : end-2]
		sp := span{kind: kind, start: i, end: end}
		if strings.HasPrefix(inner, "-") {
			sp.trimLeft = true
			inner = inner[1:]
		}
		if strings.HasSuffix(inner, "-") {
			sp.trimRight = true
			inner = inner[:len(inner)-1]
		}
		sp.inner = strings.TrimSpace(inner)
		out = append(out, sp)
		i = end - 1
	}
	return out
}

// scanToClose returns the offset just past closing, starting the search at i
// and skipping over single- and double-quoted strings.
func scanToClose(src string, i int, closing string) (int, bool) {
	for i < len(src) {
		switch c := src[i]; c {
		case '\'', '"':
			j := strings.IndexByte(src[i+1:], c)
			if j < 0 {
				return 0, false
			}
			i += j + 2
		default:
			if strings.HasPrefix(src[i:], closing) {
				return i + 2, true
			}
			i++
		}
	}
	return 0, false
}

// splitTopLevel splits s on sep, ignoring separators inside quoted strings.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				i = len(s)
			} else {
				i += j + 1
			}
		case sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// filterChain splits an object expression into its receiver and filter
// segments: `"a" | t: x: 1 | upcase` -> `"a"`, [`t: x: 1`, `upcase`].
func filterChain(inner string) (receiver string, filters []string) {
	parts := splitTopLevel(inner, '|')
	receiver = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		filters = append(filters, strings.TrimSpace(p))
	}
	return receiver, filters
}

// filterName returns the name of a filter segment ("t: x: 1" -> "t").
func filterName(f string) string {
	if i := strings.IndexByte(f, ':'); i >= 0 {
		return strings.TrimSpace(f[:i])
	}
	return strings.TrimSpace(f)
}

// filterArgs returns the argument text of a filter segment, or "".
func filterArgs(f string) string {
	if i := strings.IndexByte(f, ':'); i >= 0 {
		return strings.TrimSpace(f[i+1:])
	}
	return ""
}

// ErrTFilterChain is returned when `t` is used as a non-final filter, which
// rewriteTFilter cannot express as a tag.
var ErrTFilterChain = fmt.Errorf("render: the %q filter must be the last filter in an expression", "t")

// rewriteTFilter turns `{{ expr | t }}` / `{{ expr | t: name: x }}` into
// `{% t expr, name: x %}`.
//
// osteele/liquid filters are plain functions: they receive argument values and
// have no access to the rendering context, so a filter implementation cannot
// see the current locale or bindings and could not render a translation that
// itself contains Liquid. Custom *tags* do get the context. Both spellings are
// in the spec (architecture 6.1), so we normalise the filter form to the tag
// form before parsing. The rewrite is purely syntactic and keeps trim markers.
func rewriteTFilter(src string) (string, error) {
	spans := scanLiquidSpans(src)
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		if sp.kind != spanObject {
			continue
		}
		recv, filters := filterChain(sp.inner)
		idx := -1
		for i, f := range filters {
			if filterName(f) == "t" {
				idx = i
			}
		}
		if idx < 0 {
			continue
		}
		if idx != len(filters)-1 {
			return "", ErrTFilterChain
		}
		b.WriteString(src[last:sp.start])
		b.WriteString("{%")
		if sp.trimLeft {
			b.WriteString("-")
		}
		b.WriteString(" t ")
		b.WriteString(recv)
		if args := filterArgs(filters[idx]); args != "" {
			b.WriteString(", ")
			b.WriteString(args)
		}
		b.WriteString(" ")
		if sp.trimRight {
			b.WriteString("-")
		}
		b.WriteString("%}")
		last = sp.end
	}
	if last == 0 {
		return src, nil
	}
	b.WriteString(src[last:])
	return b.String(), nil
}

type kwarg struct {
	name string
	expr string
}

// parseTagArgs parses the argument text of `{% t … %}`. The first term is the
// key: a quoted literal, or a bare expression (a variable path). Remaining
// terms are `name: expression` pairs separated by commas; the comma after the
// key is optional, so both spellings from architecture 6.1 work:
//
//	{% t "greeting" name: recipient.name %}
//	{% t "greeting", name: recipient.name, count: 3 %}
func parseTagArgs(args string) (key string, literal bool, kwargs []kwarg, err error) {
	s := strings.TrimSpace(args)
	if s == "" {
		return "", false, nil, fmt.Errorf("render: t tag requires a key")
	}
	if s[0] == '"' || s[0] == '\'' {
		j := strings.IndexByte(s[1:], s[0])
		if j < 0 {
			return "", false, nil, fmt.Errorf("render: t tag has an unterminated string key")
		}
		key, literal = s[1:j+1], true
		s = s[j+2:]
	} else {
		j := strings.IndexAny(s, " \t,")
		if j < 0 {
			j = len(s)
		}
		key = s[:j]
		s = s[j:]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ",")
	s = strings.TrimSpace(s)
	if s == "" {
		return key, literal, nil, nil
	}
	for _, part := range splitTopLevel(s, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, ':')
		if i < 0 {
			return "", false, nil, fmt.Errorf("render: t tag argument %q is not name: value", part)
		}
		name := strings.TrimSpace(part[:i])
		expr := strings.TrimSpace(part[i+1:])
		if name == "" || expr == "" {
			return "", false, nil, fmt.Errorf("render: t tag argument %q is not name: value", part)
		}
		kwargs = append(kwargs, kwarg{name: name, expr: expr})
	}
	return key, literal, kwargs, nil
}

// ExtractKeys returns the i18n keys used by the given parts, sorted and
// deduplicated. Both the `{% t "key" %}` tag form and the `{{ "key" | t }}`
// filter form are recognised. Keys built from expressions (`{% t page.key %}`)
// cannot be known statically and are skipped.
func ExtractKeys(subject, body, text string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, src := range []string{subject, body, text} {
		for _, sp := range scanLiquidSpans(src) {
			var args string
			switch sp.kind {
			case spanTag:
				name, rest, _ := strings.Cut(sp.inner, " ")
				if strings.TrimSpace(name) != "t" {
					continue
				}
				args = rest
			case spanObject:
				// Any position in the chain counts: a draft that puts `t` in
				// the middle still tells the editor which key it uses, even
				// though rewriteTFilter will reject it at publish time.
				recv, filters := filterChain(sp.inner)
				idx := -1
				for i, f := range filters {
					if filterName(f) == "t" {
						idx = i
					}
				}
				if idx < 0 {
					continue
				}
				args = recv
				if a := filterArgs(filters[idx]); a != "" {
					args += ", " + a
				}
			}
			key, literal, _, err := parseTagArgs(args)
			if err != nil {
				return nil, err
			}
			if literal {
				seen[key] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// ExtractVars returns the variable paths under root that the given Liquid
// sources read, sorted and deduplicated and with the root stripped:
// ExtractVars("tenant", `{{ tenant.slug }}`) is ["slug"].
//
// It is a scanner, not an evaluator, so it sees what a template *mentions*
// rather than what a render would actually need. That is the useful direction
// for the one caller: a platform sender's From templates are checked against a
// request's tenant variables before anything is queued, and a missing
// variable has to be a 422 naming the key rather than a mail from
// "sender+@example.com" (ADR-0017).
//
// Recognised forms are a bare `{{ root.path }}`, the same inside a filter
// chain or a tag argument (`{% if tenant.plan == "pro" %}`), and the bracket
// form `{{ root["slug"] }}`. A path built at render time
// (`{{ tenant[key] }}`) cannot be known statically and is skipped, as is a
// bare `{{ root }}` with no path at all.
func ExtractVars(root string, srcs ...string) []string {
	seen := map[string]struct{}{}
	for _, src := range srcs {
		for _, sp := range scanLiquidSpans(src) {
			collectVars(root, sp.inner, seen)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectVars finds every `root.<path>` and `root["path"]` reference in one
// Liquid construct's inner text.
func collectVars(root, inner string, seen map[string]struct{}) {
	for i := 0; i+len(root) <= len(inner); {
		j := strings.Index(inner[i:], root)
		if j < 0 {
			return
		}
		at := i + j
		i = at + len(root)
		// The match has to be a whole identifier: `subtenant.slug` is not a
		// reference to `tenant`.
		if at > 0 && isIdentByte(inner[at-1]) {
			continue
		}
		if path, n := scanVarPath(inner[i:]); path != "" {
			seen[path] = struct{}{}
			i += n
		}
	}
}

// scanVarPath reads the `.a.b` / `["a"]` suffix at the start of s and returns
// the dotted path it names plus how many bytes it consumed.
func scanVarPath(s string) (string, int) {
	var parts []string
	i := 0
	for i < len(s) {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			if j == i+1 {
				return join(parts), i
			}
			parts = append(parts, s[i+1:j])
			i = j
		case '[':
			if i+1 >= len(s) || (s[i+1] != '"' && s[i+1] != '\'') {
				// A computed index: nothing static to record, and whatever
				// follows it is not a path this scanner can continue.
				return join(parts), i
			}
			q := s[i+1]
			k := strings.IndexByte(s[i+2:], q)
			if k < 0 {
				return join(parts), i
			}
			key := s[i+2 : i+2+k]
			end := i + 2 + k + 1
			if end >= len(s) || s[end] != ']' {
				return join(parts), i
			}
			if key == "" {
				return join(parts), i
			}
			parts = append(parts, key)
			i = end + 1
		default:
			return join(parts), i
		}
	}
	return join(parts), i
}

func join(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ".")
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
