package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Shared templates and layouts with tenant overrides (ADR-0018).
//
// The store overlay does the reading: a tenant's Templates() and Layouts()
// include what the system tenant shares, read through and never copied. This
// file is the request-time half — who may set `shared`, how a send names a
// template by key, what the override endpoints copy, and the template-use
// policy.

// isSharedFor reports whether a row the tenant's store returned is the system
// tenant's shared one rather than the tenant's own.
func isSharedFor(t *tenant, rowTenant string) bool { return rowTenant != t.id }

// refuseSharedWrite answers a write to a shared template or layout from any
// tenant but the system one with 403 platform_read_only, before the handler
// validates anything: the caller asked to change something it can only read,
// and "override it" is the answer to that whatever the body said. The store
// refuses the write as well (store.ErrReadOnly); this makes the answer the
// same for every handler, including the ones that would fail on something
// else first.
func refuseSharedWrite(t *tenant, rowTenant, kind, id string) error {
	if !isSharedFor(t, rowTenant) {
		return nil
	}
	return &apiError{
		status: http.StatusForbidden, code: ErrorCodePlatformReadOnly,
		message: kind + " " + id + " is shared by the system tenant and is read-only here; " +
			"override it to make a copy this tenant can change",
	}
}

// contentKeyAndShared validates the `key` and `shared` fields of a template or
// layout write. Only the system tenant shares, and a shared object needs a
// key: the key is what a tenant's override and a send by key match on.
func contentKeyAndShared(t *tenant, kind string, key *ContentKey, shared *ContentShared) (string, bool, error) {
	k := deref(key)
	if err := store.ValidContentKey(k); err != nil {
		return "", false, errInvalid("key %q must be 1-64 characters of a-z, 0-9, '.', '_' or '-', "+
			"starting with a letter or digit", k)
	}
	sh := deref(shared)
	if sh && t.id != store.SystemTenantID {
		return "", false, errInvalid("only the system tenant can share a %s", kind)
	}
	if sh && k == "" {
		return "", false, errInvalid("a shared %s needs a key", kind)
	}
	return k, sh, nil
}

// templateUsesIn validates `uses`: campaign and transactional, each at most
// once. Nil and empty both mean "unrestricted".
func templateUsesIn(in *TemplateUses) ([]store.UseKind, error) {
	if in == nil || len(*in) == 0 {
		return nil, nil
	}
	allowed := store.TemplateUses()
	seen := map[store.UseKind]bool{}
	out := make([]store.UseKind, 0, len(*in))
	for _, u := range *in {
		k := store.UseKind(u)
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return nil, errInvalid("uses: %q is not campaign or transactional", u)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}

// sharedLayoutRule is the one constraint between a shared template and its
// layout: the layout must be shared too, or no tenant could render the
// template's preview or its override.
func sharedLayoutRule(templateShared bool, l *store.Layout) error {
	if !templateShared || l.Shared {
		return nil
	}
	return errInvalid("layout %s is not shared: a shared template's layout must be a shared layout", l.ID)
}

// visibleVersion reads a message version a caller named by ID.
//
// The store lets a tenant read any system-tenant version by ID, because the
// sender must keep rendering what an in-flight delivery was queued with
// (store/overlay_content.go). A caller handing an ID in is held to more: a
// system version is visible only while its template is shared, so an unshared
// template's versions are not reachable by guessing IDs.
func (s *server) visibleVersion(ctx context.Context, t *tenant, id string) (*store.MessageVersion, error) {
	v, err := t.st.Versions().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !isSharedFor(t, v.TenantID) {
		return v, nil
	}
	if tpl, err := t.st.Templates().Get(ctx, v.TemplateID); err == nil && tpl.Shared {
		return v, nil
	}
	return nil, errNotFound("message version", id)
}

// templateByRef resolves the template a send names by `template_id` or by
// `template_key`, which exclude each other. By key the tenant's own template
// wins, then the shared one (the store overlay's GetByKey). A nil result with
// a nil error means neither was given.
func (s *server) templateByRef(ctx context.Context, t *tenant, id *UUID, key *ContentKey) (*store.Template, error) {
	switch {
	case id != nil && key != nil:
		return nil, errInvalid("template_id and template_key exclude each other; name the template once")
	case id != nil:
		tpl, err := t.st.Templates().Get(ctx, idOf(*id))
		if errors.Is(err, store.ErrNotFound) {
			return nil, errInvalid("template %s does not exist", *id)
		}
		return tpl, err
	case key != nil:
		if *key == "" {
			return nil, errInvalid("template_key is empty")
		}
		tpl, err := t.st.Templates().GetByKey(ctx, *key)
		if errors.Is(err, store.ErrNotFound) {
			return nil, errInvalid("no template has the key %q", *key)
		}
		return tpl, err
	}
	return nil, nil
}

// templateOfVersion is the template a pinned version was published from, for
// the template-use policy. Nil when it is gone: a tenant's own version outlives
// its template, and an unrestricted own template is exactly what the policy
// would have seen.
func (s *server) templateOfVersion(ctx context.Context, t *tenant, v *store.MessageVersion) (*store.Template, error) {
	tpl, err := t.st.Templates().Get(ctx, v.TemplateID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return tpl, err
}

// checkTemplateUse runs the template-use policy (Hooks.TemplatePolicy, default
// host.DefaultTemplatePolicy). Any error is 403 template_use_denied carrying
// the policy's own message.
func (s *server) checkTemplateUse(
	ctx context.Context, t *tenant, tpl *store.Template, kind store.UseKind, vars map[string]any,
) error {
	if tpl == nil {
		return nil
	}
	policy := s.deps.Hooks.TemplatePolicy
	if policy == nil {
		policy = host.DefaultTemplatePolicy
	}
	use := host.TemplateUse{
		TenantID: t.id, Principal: t.p,
		TemplateID: tpl.ID, TemplateKey: tpl.Key,
		Shared: isSharedFor(t, tpl.TenantID) && tpl.Shared,
		Uses:   append([]store.UseKind(nil), tpl.Uses...),
		Kind:   kind, TenantVars: vars,
	}
	if err := policy(ctx, use); err != nil {
		return &apiError{
			status: http.StatusForbidden, code: ErrorCodeTemplateUseDenied,
			message: err.Error(), cause: err,
		}
	}
	return nil
}

// checkCampaignTemplateUse re-runs the template policy for a campaign about to
// start, against the template it names or the one its pinned version came
// from. Like the sender check next to it, it defers a missing campaign or
// template to StartCampaign, which reports those properly.
func (s *server) checkCampaignTemplateUse(ctx context.Context, t *tenant, c *store.Campaign) error {
	var tpl *store.Template
	switch {
	case c.VersionID != "":
		v, err := t.st.Versions().Get(ctx, c.VersionID)
		if err != nil {
			return nil //nolint:nilerr // StartCampaign reports a missing version
		}
		if tpl, err = s.templateOfVersion(ctx, t, v); err != nil {
			return err
		}
	case c.TemplateID != "":
		got, err := t.st.Templates().Get(ctx, c.TemplateID)
		if err != nil {
			return nil //nolint:nilerr // StartCampaign turns a missing template into ErrNoVersion
		}
		tpl = got
	}
	return s.checkTemplateUse(ctx, t, tpl, store.UseCampaign, campaignTenantVars(c))
}

// --- the override endpoints --------------------------------------------

// OverrideTemplate copies a shared template into the tenant as a complete
// template of its own (ADR-0018). It is the one place shared content is
// copied, and only because the caller asked for a copy to edit.
//
// The copy starts out published with the shared template's current version,
// by reference: versions are read through, so the tenant keeps sending the
// same thing until it publishes its own, and nothing is duplicated.
func (s *server) OverrideTemplate(ctx context.Context, req OverrideTemplateRequestObject) (OverrideTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if t.id == store.SystemTenantID {
		return nil, errInvalid("the system tenant owns the shared templates; there is nothing to override")
	}
	src, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	if !isSharedFor(t, src.TenantID) || !src.Shared || src.Key == "" {
		return nil, errInvalid("template %s is not a shared template", src.ID)
	}
	if own, err := t.st.Templates().GetByKey(ctx, src.Key); err == nil && !isSharedFor(t, own.TenantID) {
		return nil, errConflict(ErrorCodeDuplicate,
			"this tenant already has template %s with the key %q", own.ID, src.Key)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	cp := &store.Template{
		Name: src.Name, Key: src.Key, LayoutID: src.LayoutID,
		Subject: src.Subject, Preheader: src.Preheader, Mode: src.Mode, Body: src.Body,
		Blocks: append([]byte(nil), src.Blocks...), Text: src.Text,
		I18n:          cloneBundle(src.I18n),
		DefaultLocale: src.DefaultLocale,
		Uses:          append([]store.UseKind(nil), src.Uses...),

		PublishedVersionID:    src.PublishedVersionID,
		OverriddenFromVersion: src.PublishedVersionID,
	}
	if len(cp.Blocks) == 0 {
		cp.Blocks = nil
	}
	if err := t.st.Templates().Create(ctx, cp); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, errConflict(ErrorCodeDuplicate,
				"this tenant already has a template with the key %q", src.Key)
		}
		return nil, err
	}
	// Read it back through the overlay so the answer carries `overridden`.
	out, err := t.st.Templates().Get(ctx, cp.ID)
	if err != nil {
		return nil, err
	}
	return OverrideTemplate201JSONResponse(templateOut(out)), nil
}

// OverrideLayout is OverrideTemplate for layouts. A layout has no versions:
// the tenant's templates pick the copy up at their next publish, because a
// layout reference resolves by key, own first (layoutOf).
func (s *server) OverrideLayout(ctx context.Context, req OverrideLayoutRequestObject) (OverrideLayoutResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if t.id == store.SystemTenantID {
		return nil, errInvalid("the system tenant owns the shared layouts; there is nothing to override")
	}
	src, err := t.st.Layouts().Get(ctx, req.LayoutId.String())
	if err != nil {
		return nil, err
	}
	if !isSharedFor(t, src.TenantID) || !src.Shared || src.Key == "" {
		return nil, errInvalid("layout %s is not a shared layout", src.ID)
	}
	if own, err := t.st.Layouts().GetByKey(ctx, src.Key); err == nil && !isSharedFor(t, own.TenantID) {
		return nil, errConflict(ErrorCodeDuplicate,
			"this tenant already has layout %s with the key %q", own.ID, src.Key)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	cp := &store.Layout{
		Name: src.Name, Key: src.Key, Mode: src.Mode, Body: src.Body,
		I18n: cloneBundle(src.I18n),
	}
	if err := t.st.Layouts().Create(ctx, cp); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, errConflict(ErrorCodeDuplicate,
				"this tenant already has a layout with the key %q", src.Key)
		}
		return nil, err
	}
	out, err := t.st.Layouts().Get(ctx, cp.ID)
	if err != nil {
		return nil, err
	}
	return OverrideLayout201JSONResponse(layoutOut(out)), nil
}

// cloneBundle deep-copies an i18n bundle, so the copy shares no map with the
// row it came from.
func cloneBundle(b store.I18nBundle) store.I18nBundle {
	out := store.I18nBundle{DefaultLocale: b.DefaultLocale}
	if b.Locales != nil {
		out.Locales = make(map[string]map[string]string, len(b.Locales))
		for loc, kv := range b.Locales {
			cp := make(map[string]string, len(kv))
			for k, v := range kv {
				cp[k] = v
			}
			out.Locales[loc] = cp
		}
	}
	return out
}
