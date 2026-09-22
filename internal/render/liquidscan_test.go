package render

import (
	"slices"
	"testing"
)

// ExtractVars is what decides whether a request carries enough tenant
// variables for a shared sender's From templates (ADR-0017). Every case below
// is one an operator can actually write in a config file, and getting one
// wrong is either a 422 on a request that was fine or a mail from
// "sender+@example.com".

// The plain dotted form is the one the ADR's own example uses, so it has to
// come back with the root stripped.
func TestExtractVarsDottedPath(t *testing.T) {
	got := ExtractVars("tenant", "sender+{{ tenant.slug }}@mail.example.com")
	if !slices.Equal(got, []string{"slug"}) {
		t.Fatalf("ExtractVars = %q, want [slug]", got)
	}
}

// The bracket form names the same variable as the dotted one; a template that
// spells it this way must not look like it needs nothing.
func TestExtractVarsBracketPath(t *testing.T) {
	got := ExtractVars("tenant", `{{ tenant["slug"] }}`)
	if !slices.Equal(got, []string{"slug"}) {
		t.Fatalf("ExtractVars = %q, want [slug]", got)
	}
}

// A tag argument is a reference too: `{% if tenant.plan == "pro" %}` decides
// what the rendered address is, so the request has to supply `plan`.
func TestExtractVarsInsideATag(t *testing.T) {
	got := ExtractVars("tenant", `{% if tenant.plan == "pro" %}pro@x.test{% else %}std@x.test{% endif %}`)
	if !slices.Equal(got, []string{"plan"}) {
		t.Fatalf("ExtractVars = %q, want [plan]", got)
	}
}

// A nested path is reported whole, because that is the path the resolver walks
// when it checks whether the variable is there.
func TestExtractVarsNestedPath(t *testing.T) {
	got := ExtractVars("tenant", "{{ tenant.a.b }}")
	if !slices.Equal(got, []string{"a.b"}) {
		t.Fatalf("ExtractVars = %q, want [a.b]", got)
	}
}

// The root has to match a whole identifier. `subtenant` is a different
// variable, and treating it as a `tenant` reference would demand a variable
// the template never reads.
func TestExtractVarsIgnoresALongerIdentifier(t *testing.T) {
	if got := ExtractVars("tenant", "{{ subtenant.slug }}"); len(got) != 0 {
		t.Fatalf("ExtractVars = %q, want nothing", got)
	}
}

// A bare root names no path, so there is no key a request could be missing.
func TestExtractVarsIgnoresTheBareRoot(t *testing.T) {
	if got := ExtractVars("tenant", "{{ tenant }}"); len(got) != 0 {
		t.Fatalf("ExtractVars = %q, want nothing", got)
	}
}

// A path built at render time cannot be known by a scanner. Reporting a guess
// would refuse a request for a key that may not even be the one used, so it is
// skipped and the render decides.
func TestExtractVarsIgnoresAComputedIndex(t *testing.T) {
	if got := ExtractVars("tenant", "{{ tenant[key] }}"); len(got) != 0 {
		t.Fatalf("ExtractVars = %q, want nothing", got)
	}
}

// The result is the input of a 422's key list, so it has to be deduplicated
// and ordered: the same three templates must not produce a different message
// depending on map iteration.
func TestExtractVarsDedupesAndSorts(t *testing.T) {
	got := ExtractVars("tenant",
		"{{ tenant.name }}",
		`sender+{{ tenant.slug }}@dev.local`,
		`{{ tenant["slug"] }} {{ tenant.name }} {{ tenant.plan }}`,
	)
	if !slices.Equal(got, []string{"name", "plan", "slug"}) {
		t.Fatalf("ExtractVars = %q, want [name plan slug]", got)
	}
}
