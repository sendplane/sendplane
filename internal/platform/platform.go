// Package platform resolves the shared ("platform") senders of
// store.PlatformCatalog into the concrete From identity one send uses.
//
// A platform sender's FromName, FromEmail and ReplyTo are Liquid templates
// over the tenant variables a request carried, so that one configuration entry
// serves every tenant:
//
//	from_email: "sender+{{ tenant.slug }}@mail.example.com"
//	from_name:  "{{ tenant.name }}"
//
// Two callers need the same answer and must not disagree about it. The API
// renders the templates at request time so that a missing variable is a 422
// naming the key, before anything is queued; the sender renders them per
// delivery, from what was stored. Doing it in one place is what keeps a
// campaign from being accepted and then failing every delivery.
//
// The templates are parsed once, at startup, so a syntax error in the config
// file is a refusal to start rather than a million failed deliveries.
package platform

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/osteele/liquid"

	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/store"
)

// TenantVarRoot is the Liquid binding a platform sender's templates read the
// tenant attributes under. It is the same name the message templates see
// (render.Bindings.TenantVars), so an operator writes `{{ tenant.slug }}` in
// both places.
const TenantVarRoot = "tenant"

// ErrMissingVars is returned when a template needs a tenant variable the
// request did not carry. MissingVarsError carries which ones.
var ErrMissingVars = errors.New("platform: tenant variables are missing")

// ErrInvalidFrom is returned when the templates rendered to something that is
// not a usable From address — an empty local part because a variable was
// blank, or a value with a line break in it.
var ErrInvalidFrom = errors.New("platform: the rendered From address is not usable")

// MissingVarsError names the tenant variables a send would need.
type MissingVarsError struct {
	SenderID string
	Keys     []string
}

func (e *MissingVarsError) Error() string {
	return fmt.Sprintf("%v: sender %s needs tenant_vars %s",
		ErrMissingVars, e.SenderID, strings.Join(e.Keys, ", "))
}

func (e *MissingVarsError) Unwrap() error { return ErrMissingVars }

// From is a resolved platform sender identity.
type From struct {
	Name    string
	Email   string
	ReplyTo string
}

// Spec is one platform sender, with its templates parsed and the tenant
// variables they read already extracted.
type Spec struct {
	ID   string
	Name string

	TransportID string
	DomainID    string

	// Uses is the effective use list (never empty for a sender in the
	// catalog).
	Uses []store.UseKind
	// RequiredVars are the tenant variable paths the three templates read,
	// sorted. A request that does not carry all of them is refused.
	RequiredVars []string
	// ProbeVars are the tenant variables the loopback probe renders with.
	ProbeVars map[string]any

	fromName  *tpl
	fromEmail *tpl
	replyTo   *tpl
}

// tpl is one parsed template plus its source, kept so that a constant
// template (no Liquid at all, which is the common case for reply_to) can skip
// rendering entirely.
type tpl struct {
	src      string
	parsed   *liquid.Template
	constant bool
}

// Resolver holds the parsed catalog.
type Resolver struct {
	specs map[string]*Spec
	cat   store.PlatformCatalog
}

// New parses every platform sender's templates. A parse failure is an error:
// the caller is startup validation, and a template that cannot be parsed can
// only ever fail at send time.
func New(cat store.PlatformCatalog) (*Resolver, error) {
	cat = cat.Normalize()
	eng := render.NewEngine()
	r := &Resolver{specs: make(map[string]*Spec, len(cat.Senders)), cat: cat}
	var errs []string
	for _, s := range cat.Senders {
		spec := &Spec{
			ID: s.ID, Name: s.Name,
			TransportID: s.TransportID, DomainID: s.DomainID,
			Uses:      cat.SenderUses(s.ID),
			ProbeVars: maps.Clone(s.ProbeVars),
		}
		for _, f := range []struct {
			field string
			src   string
			dst   **tpl
		}{
			{"from_name", s.FromName, &spec.fromName},
			{"from_email", s.FromEmail, &spec.fromEmail},
			{"reply_to", s.ReplyTo, &spec.replyTo},
		} {
			t, err := parse(eng, f.src)
			if err != nil {
				errs = append(errs, fmt.Sprintf("platform.senders[%s].%s: %v",
					strings.TrimPrefix(s.ID, store.PlatformIDPrefix), f.field, err))
				continue
			}
			*f.dst = t
		}
		spec.RequiredVars = render.ExtractVars(TenantVarRoot, s.FromName, s.FromEmail, s.ReplyTo)
		r.specs[s.ID] = spec
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("invalid platform sender templates:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return r, nil
}

func parse(eng *render.Engine, src string) (*tpl, error) {
	if !strings.Contains(src, "{{") && !strings.Contains(src, "{%") {
		return &tpl{src: src, constant: true}, nil
	}
	p, err := eng.Parse(src)
	if err != nil {
		return nil, err
	}
	return &tpl{src: src, parsed: p}, nil
}

// Catalog is the normalized catalog this resolver was built from.
func (r *Resolver) Catalog() store.PlatformCatalog { return r.cat }

// Spec returns the platform sender with this virtual ID.
func (r *Resolver) Spec(senderID string) (*Spec, bool) {
	s, ok := r.specs[senderID]
	return s, ok
}

// Missing reports which of a sender's required tenant variables the given map
// does not supply, sorted. A variable that is present but empty counts as
// missing: an empty slug produces "sender+@example.com", which is worse than
// a refusal.
//
// It returns nil for a sender that is not a platform sender, so a caller can
// check unconditionally.
func (r *Resolver) Missing(senderID string, vars map[string]any) []string {
	s, ok := r.specs[senderID]
	if !ok {
		return nil
	}
	var out []string
	for _, key := range s.RequiredVars {
		if !hasVar(vars, key) {
			out = append(out, key)
		}
	}
	return out
}

// hasVar reports whether the dotted path resolves to a non-empty value.
func hasVar(vars map[string]any, path string) bool {
	cur := any(vars)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[part]
		if !ok {
			return false
		}
	}
	switch v := cur.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	default:
		return true
	}
}

// Resolve renders a platform sender's identity with the given tenant
// variables.
//
// It reports a MissingVarsError before rendering anything, so the caller can
// answer 422 with the keys, and ErrInvalidFrom when the rendered address is
// not one sendplane will put on the wire.
func (r *Resolver) Resolve(senderID string, vars map[string]any) (From, error) {
	s, ok := r.specs[senderID]
	if !ok {
		return From{}, fmt.Errorf("platform: %s is not a configured platform sender", senderID)
	}
	if missing := r.Missing(senderID, vars); len(missing) > 0 {
		return From{}, &MissingVarsError{SenderID: senderID, Keys: missing}
	}
	bindings := map[string]any{TenantVarRoot: varsOrEmpty(vars)}

	name, err := s.fromName.render(bindings)
	if err != nil {
		return From{}, fmt.Errorf("platform: sender %s from_name: %w", senderID, err)
	}
	email, err := s.fromEmail.render(bindings)
	if err != nil {
		return From{}, fmt.Errorf("platform: sender %s from_email: %w", senderID, err)
	}
	reply, err := s.replyTo.render(bindings)
	if err != nil {
		return From{}, fmt.Errorf("platform: sender %s reply_to: %w", senderID, err)
	}

	norm, err := store.NormalizeEmail(email)
	if err != nil {
		return From{}, fmt.Errorf("%w: sender %s rendered from_email %q: %v",
			ErrInvalidFrom, senderID, email, err)
	}
	if reply != "" {
		if _, err := store.NormalizeEmail(reply); err != nil {
			return From{}, fmt.Errorf("%w: sender %s rendered reply_to %q: %v",
				ErrInvalidFrom, senderID, reply, err)
		}
	}
	if strings.ContainsAny(name, "\r\n") {
		return From{}, fmt.Errorf("%w: sender %s rendered from_name contains a line break",
			ErrInvalidFrom, senderID)
	}
	return From{Name: name, Email: norm, ReplyTo: reply}, nil
}

func varsOrEmpty(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func (t *tpl) render(bindings map[string]any) (string, error) {
	if t == nil {
		return "", nil
	}
	if t.constant {
		return strings.TrimSpace(t.src), nil
	}
	out, err := t.parsed.Render(bindings)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
