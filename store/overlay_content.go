package store

import (
	"context"
	"errors"
	"fmt"
)

// Shared templates and layouts (ADR-0018).
//
// The system tenant may mark a template or a layout Shared. Every other
// tenant then reads it *through* this overlay — it is never copied into the
// tenant — with these rules:
//
//   - Get falls through to the system tenant when the tenant has no row with
//     that ID and the system tenant's row is shared.
//   - GetByKey is own first, then shared: a tenant's own template with a
//     shared template's key *overrides* it.
//   - List is the shared rows the tenant has not overridden, followed by the
//     tenant's own. An own row that shadows a shared one is marked Overridden
//     (and, for a template, carries the shared original's current
//     PublishedVersionID) — computed here, never stored.
//   - Any write that names a shared row's ID is ErrReadOnly: the only way to
//     change a shared template in a tenant is to override it, which is an
//     explicit copy the caller asks for.
//
// Message versions follow the one rule that keeps sending simple: a tenant's
// Versions().Get falls through to the system tenant for *any* version ID.
// Versions are immutable and their IDs are UUIDv7s nobody can enumerate, and
// the sender has to keep reading the version an in-flight delivery was queued
// with even if the template was unshared since. Versions are never *listed*
// across the boundary, except ListByTemplate of a template that is shared
// right now. An API that hands a version ID from a caller to the store checks
// the template's visibility itself (internal/api).
//
// The system tenant sees its own rows as they are; it is where shared
// content is authored.

func (s *platformStore) Templates() TemplateRepo {
	if s.system {
		return s.Store.Templates()
	}
	return &sharedTemplates{s: s, inner: s.Store.Templates()}
}

func (s *platformStore) Layouts() LayoutRepo {
	if s.system {
		return s.Store.Layouts()
	}
	return &sharedLayouts{s: s, inner: s.Store.Layouts()}
}

func (s *platformStore) Versions() MessageVersionRepo {
	if s.system {
		return s.Store.Versions()
	}
	return &sharedVersions{s: s, inner: s.Store.Versions()}
}

// sharedReadOnly is the refusal a tenant's write to a shared row gets.
func sharedReadOnly(kind, id string) error {
	return fmt.Errorf("%w: %s %s is shared by the system tenant; override it to change it",
		ErrReadOnly, kind, id)
}

// errSharingOutsideSystem refuses a Shared flag on a tenant's own row: only
// the system tenant shares.
func errSharingOutsideSystem(kind string) error {
	return fmt.Errorf("%w: only the system tenant can share a %s", ErrInvalid, kind)
}

// allShared walks a system-tenant listing and keeps the shared rows. The
// system tenant holds the operator's handful of shared templates, so reading
// it whole per tenant listing is the simple thing; a deployment that shares
// thousands would want a dedicated query (ADR-0018).
func allShared[T any](ctx context.Context, list func(context.Context, Page) (Result[T], error),
	shared func(*T) bool) ([]T, error) {
	var out []T
	page := Page{Limit: MaxPageLimit}
	for {
		res, err := list(ctx, page)
		if err != nil {
			return nil, err
		}
		for i := range res.Items {
			if shared(&res.Items[i]) {
				out = append(out, res.Items[i])
			}
		}
		if res.NextCursor == "" {
			return out, nil
		}
		page.Cursor = res.NextCursor
	}
}

// --- templates ---------------------------------------------------------

type sharedTemplates struct {
	s     *platformStore
	inner TemplateRepo
}

func (r *sharedTemplates) system(ctx context.Context) (TemplateRepo, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	return sys.Templates(), nil
}

// sharedByID returns the system tenant's template id when it is shared, and
// ErrNotFound otherwise.
func (r *sharedTemplates) sharedByID(ctx context.Context, id string) (*Template, error) {
	sys, err := r.system(ctx)
	if err != nil {
		return nil, err
	}
	v, err := sys.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !v.Shared {
		return nil, fmt.Errorf("%w: template %s", ErrNotFound, id)
	}
	return v, nil
}

// annotate marks an own template that shadows a shared one.
func (r *sharedTemplates) annotate(ctx context.Context, v *Template) error {
	v.Overridden, v.SharedPublishedVersionID = false, ""
	if v.Key == "" {
		return nil
	}
	sys, err := r.system(ctx)
	if err != nil {
		return err
	}
	orig, err := sys.GetByKey(ctx, v.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if orig.Shared {
		v.Overridden, v.SharedPublishedVersionID = true, orig.PublishedVersionID
	}
	return nil
}

func (r *sharedTemplates) Create(ctx context.Context, t *Template) error {
	if t != nil && t.Shared {
		return errSharingOutsideSystem("template")
	}
	return r.inner.Create(ctx, t)
}

func (r *sharedTemplates) Get(ctx context.Context, id string) (*Template, error) {
	v, err := r.inner.Get(ctx, id)
	if err == nil {
		return v, r.annotate(ctx, v)
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return r.sharedByID(ctx, id)
}

func (r *sharedTemplates) GetByKey(ctx context.Context, key string) (*Template, error) {
	v, err := r.inner.GetByKey(ctx, key)
	if err == nil {
		return v, r.annotate(ctx, v)
	}
	if !errors.Is(err, ErrNotFound) || key == "" {
		return nil, err
	}
	sys, err := r.system(ctx)
	if err != nil {
		return nil, err
	}
	v, err = sys.GetByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if !v.Shared {
		return nil, fmt.Errorf("%w: template with key %q", ErrNotFound, key)
	}
	return v, nil
}

func (r *sharedTemplates) Update(ctx context.Context, t *Template) error {
	if t == nil {
		return r.inner.Update(ctx, t)
	}
	if t.Shared {
		if _, err := r.sharedByID(ctx, t.ID); err == nil {
			return sharedReadOnly("template", t.ID)
		}
		return errSharingOutsideSystem("template")
	}
	err := r.inner.Update(ctx, t)
	if errors.Is(err, ErrNotFound) {
		if _, serr := r.sharedByID(ctx, t.ID); serr == nil {
			return sharedReadOnly("template", t.ID)
		}
	}
	return err
}

func (r *sharedTemplates) Delete(ctx context.Context, id string) error {
	err := r.inner.Delete(ctx, id)
	if errors.Is(err, ErrNotFound) {
		if _, serr := r.sharedByID(ctx, id); serr == nil {
			return sharedReadOnly("template", id)
		}
	}
	return err
}

func (r *sharedTemplates) List(ctx context.Context, p Page) (Result[Template], error) {
	sys, err := r.system(ctx)
	if err != nil {
		return Result[Template]{}, err
	}
	shared, err := allShared(ctx, sys.List, func(v *Template) bool { return v.Shared })
	if err != nil {
		return Result[Template]{}, err
	}
	byKey := make(map[string]*Template, len(shared))
	virt := make([]Template, 0, len(shared))
	for i := range shared {
		v := &shared[i]
		if v.Key != "" {
			byKey[v.Key] = v
			// A shared template the tenant has overridden is not listed: its
			// own copy stands in for it, marked Overridden.
			if _, err := r.inner.GetByKey(ctx, v.Key); err == nil {
				continue
			} else if !errors.Is(err, ErrNotFound) {
				return Result[Template]{}, err
			}
		}
		virt = append(virt, *v)
	}
	own := func(ctx context.Context, pg Page) (Result[Template], error) {
		res, err := r.inner.List(ctx, pg)
		if err != nil {
			return res, err
		}
		for i := range res.Items {
			v := &res.Items[i]
			if orig, ok := byKey[v.Key]; ok && v.Key != "" {
				v.Overridden, v.SharedPublishedVersionID = true, orig.PublishedVersionID
			}
		}
		return res, nil
	}
	return overlayList(ctx, virt, p, func(v *Template) string { return v.ID }, own)
}

// --- layouts -----------------------------------------------------------

type sharedLayouts struct {
	s     *platformStore
	inner LayoutRepo
}

func (r *sharedLayouts) system(ctx context.Context) (LayoutRepo, error) {
	sys, err := r.s.o.shadowStore(ctx)
	if err != nil {
		return nil, err
	}
	return sys.Layouts(), nil
}

func (r *sharedLayouts) sharedByID(ctx context.Context, id string) (*Layout, error) {
	sys, err := r.system(ctx)
	if err != nil {
		return nil, err
	}
	v, err := sys.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !v.Shared {
		return nil, fmt.Errorf("%w: layout %s", ErrNotFound, id)
	}
	return v, nil
}

func (r *sharedLayouts) annotate(ctx context.Context, v *Layout) error {
	v.Overridden = false
	if v.Key == "" {
		return nil
	}
	sys, err := r.system(ctx)
	if err != nil {
		return err
	}
	orig, err := sys.GetByKey(ctx, v.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	v.Overridden = orig.Shared
	return nil
}

func (r *sharedLayouts) Create(ctx context.Context, l *Layout) error {
	if l != nil && l.Shared {
		return errSharingOutsideSystem("layout")
	}
	return r.inner.Create(ctx, l)
}

func (r *sharedLayouts) Get(ctx context.Context, id string) (*Layout, error) {
	v, err := r.inner.Get(ctx, id)
	if err == nil {
		return v, r.annotate(ctx, v)
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return r.sharedByID(ctx, id)
}

func (r *sharedLayouts) GetByKey(ctx context.Context, key string) (*Layout, error) {
	v, err := r.inner.GetByKey(ctx, key)
	if err == nil {
		return v, r.annotate(ctx, v)
	}
	if !errors.Is(err, ErrNotFound) || key == "" {
		return nil, err
	}
	sys, err := r.system(ctx)
	if err != nil {
		return nil, err
	}
	v, err = sys.GetByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if !v.Shared {
		return nil, fmt.Errorf("%w: layout with key %q", ErrNotFound, key)
	}
	return v, nil
}

func (r *sharedLayouts) Update(ctx context.Context, l *Layout) error {
	if l == nil {
		return r.inner.Update(ctx, l)
	}
	if l.Shared {
		if _, err := r.sharedByID(ctx, l.ID); err == nil {
			return sharedReadOnly("layout", l.ID)
		}
		return errSharingOutsideSystem("layout")
	}
	err := r.inner.Update(ctx, l)
	if errors.Is(err, ErrNotFound) {
		if _, serr := r.sharedByID(ctx, l.ID); serr == nil {
			return sharedReadOnly("layout", l.ID)
		}
	}
	return err
}

func (r *sharedLayouts) Delete(ctx context.Context, id string) error {
	err := r.inner.Delete(ctx, id)
	if errors.Is(err, ErrNotFound) {
		if _, serr := r.sharedByID(ctx, id); serr == nil {
			return sharedReadOnly("layout", id)
		}
	}
	return err
}

func (r *sharedLayouts) List(ctx context.Context, p Page) (Result[Layout], error) {
	sys, err := r.system(ctx)
	if err != nil {
		return Result[Layout]{}, err
	}
	shared, err := allShared(ctx, sys.List, func(v *Layout) bool { return v.Shared })
	if err != nil {
		return Result[Layout]{}, err
	}
	keys := make(map[string]bool, len(shared))
	virt := make([]Layout, 0, len(shared))
	for i := range shared {
		v := &shared[i]
		if v.Key != "" {
			keys[v.Key] = true
			if _, err := r.inner.GetByKey(ctx, v.Key); err == nil {
				continue
			} else if !errors.Is(err, ErrNotFound) {
				return Result[Layout]{}, err
			}
		}
		virt = append(virt, *v)
	}
	own := func(ctx context.Context, pg Page) (Result[Layout], error) {
		res, err := r.inner.List(ctx, pg)
		if err != nil {
			return res, err
		}
		for i := range res.Items {
			res.Items[i].Overridden = res.Items[i].Key != "" && keys[res.Items[i].Key]
		}
		return res, nil
	}
	return overlayList(ctx, virt, p, func(v *Layout) string { return v.ID }, own)
}

// --- message versions --------------------------------------------------

type sharedVersions struct {
	s     *platformStore
	inner MessageVersionRepo
}

func (r *sharedVersions) system(ctx context.Context) (Store, error) {
	return r.s.o.shadowStore(ctx)
}

// sharedTemplate reports whether templateID names a template the system
// tenant shares right now.
func (r *sharedVersions) sharedTemplate(ctx context.Context, templateID string) (bool, error) {
	if templateID == "" {
		return false, nil
	}
	sys, err := r.system(ctx)
	if err != nil {
		return false, err
	}
	v, err := sys.Templates().Get(ctx, templateID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return v.Shared, nil
}

// Create refuses a version filed under a shared template: publishing one is
// the system tenant's, and a tenant publishes its override instead.
func (r *sharedVersions) Create(ctx context.Context, v *MessageVersion) error {
	if v != nil {
		shared, err := r.sharedTemplate(ctx, v.TemplateID)
		if err != nil {
			return err
		}
		if shared {
			return sharedReadOnly("template", v.TemplateID)
		}
	}
	return r.inner.Create(ctx, v)
}

// Get falls through to the system tenant for any version ID; see the rule at
// the top of this file.
func (r *sharedVersions) Get(ctx context.Context, id string) (*MessageVersion, error) {
	v, err := r.inner.Get(ctx, id)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return v, err
	}
	sys, serr := r.system(ctx)
	if serr != nil {
		return nil, serr
	}
	return sys.Versions().Get(ctx, id)
}

func (r *sharedVersions) ListByTemplate(ctx context.Context, templateID string, p Page) (Result[MessageVersion], error) {
	shared, err := r.sharedTemplate(ctx, templateID)
	if err != nil {
		return Result[MessageVersion]{}, err
	}
	if !shared {
		return r.inner.ListByTemplate(ctx, templateID, p)
	}
	sys, err := r.system(ctx)
	if err != nil {
		return Result[MessageVersion]{}, err
	}
	return sys.Versions().ListByTemplate(ctx, templateID, p)
}
