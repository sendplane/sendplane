package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/store"
)

// --- layouts -----------------------------------------------------------

func (s *server) ListLayouts(ctx context.Context, req ListLayoutsRequestObject) (ListLayoutsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Layouts().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Layout, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, layoutOut(&res.Items[i]))
	}
	return ListLayouts200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateLayout(ctx context.Context, req CreateLayoutRequestObject) (CreateLayoutResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	l := &store.Layout{}
	if err := s.applyLayout(t, l, LayoutUpdate{
		Name: req.Body.Name, Key: req.Body.Key, Shared: req.Body.Shared,
		Mode: req.Body.Mode, Body: req.Body.Body, I18n: req.Body.I18n,
	}); err != nil {
		return nil, err
	}
	if err := t.st.Layouts().Create(ctx, l); err != nil {
		return nil, err
	}
	return CreateLayout201JSONResponse(layoutOut(l)), nil
}

func (s *server) GetLayout(ctx context.Context, req GetLayoutRequestObject) (GetLayoutResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	l, err := t.st.Layouts().Get(ctx, req.LayoutId.String())
	if err != nil {
		return nil, err
	}
	return GetLayout200JSONResponse(layoutOut(l)), nil
}

func (s *server) UpdateLayout(ctx context.Context, req UpdateLayoutRequestObject) (UpdateLayoutResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	l, err := t.st.Layouts().Get(ctx, req.LayoutId.String())
	if err != nil {
		return nil, err
	}
	if err := refuseSharedWrite(t, l.TenantID, "layout", l.ID); err != nil {
		return nil, err
	}
	if err := s.applyLayout(t, l, *req.Body); err != nil {
		return nil, err
	}
	l.Version = req.Body.Version
	if err := t.st.Layouts().Update(ctx, l); err != nil {
		return nil, err
	}
	return UpdateLayout200JSONResponse(layoutOut(l)), nil
}

func (s *server) DeleteLayout(ctx context.Context, req DeleteLayoutRequestObject) (DeleteLayoutResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := t.st.Layouts().Delete(ctx, req.LayoutId.String()); err != nil {
		return nil, err
	}
	return DeleteLayout204Response{}, nil
}

func (s *server) applyLayout(t *tenant, l *store.Layout, in LayoutUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	key, shared, err := contentKeyAndShared(t, "layout", in.Key, in.Shared)
	if err != nil {
		return err
	}
	mode, err := contentModeIn(in.Mode)
	if err != nil {
		return err
	}
	if int64(len(in.Body)) > s.deps.Limits.MaxBodyBytes {
		return newErr(http.StatusRequestEntityTooLarge, ErrorCodePayloadTooLarge,
			"body exceeds the configured limit of %d bytes", s.deps.Limits.MaxBodyBytes)
	}
	l.Name, l.Mode, l.Body = in.Name, mode, in.Body
	l.Key, l.Shared = key, shared
	l.I18n = bundleIn(in.I18n)
	return nil
}

func layoutOut(v *store.Layout) Layout {
	return Layout{
		Id: uuidPtrOf(v.ID), Name: v.Name, Mode: ContentMode(v.Mode),
		Key: strPtr(v.Key), Shared: ptr(v.Shared), Overridden: ptr(v.Overridden),
		Body: strPtr(v.Body), I18n: bundleOut(v.I18n),
		Version: ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
}

// --- templates ---------------------------------------------------------

func (s *server) ListTemplates(ctx context.Context, req ListTemplatesRequestObject) (ListTemplatesResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := t.st.Templates().List(ctx, pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]Template, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, templateOut(&res.Items[i]))
	}
	return ListTemplates200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) CreateTemplate(ctx context.Context, req CreateTemplateRequestObject) (CreateTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	tpl := &store.Template{}
	if err := s.applyTemplate(ctx, t, tpl, TemplateUpdate{
		Name: req.Body.Name, Key: req.Body.Key, Shared: req.Body.Shared, Uses: req.Body.Uses,
		LayoutId: req.Body.LayoutId, Subject: req.Body.Subject,
		Preheader: req.Body.Preheader, Mode: req.Body.Mode, Body: req.Body.Body,
		Blocks: req.Body.Blocks, Text: req.Body.Text, I18n: req.Body.I18n,
		DefaultLocale: req.Body.DefaultLocale,
	}); err != nil {
		return nil, err
	}
	if err := t.st.Templates().Create(ctx, tpl); err != nil {
		return nil, err
	}
	return CreateTemplate201JSONResponse(templateOut(tpl)), nil
}

func (s *server) GetTemplate(ctx context.Context, req GetTemplateRequestObject) (GetTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	return GetTemplate200JSONResponse(templateOut(tpl)), nil
}

func (s *server) UpdateTemplate(ctx context.Context, req UpdateTemplateRequestObject) (UpdateTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBadRequest("a request body is required")
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	if err := refuseSharedWrite(t, tpl.TenantID, "template", tpl.ID); err != nil {
		return nil, err
	}
	if err := s.applyTemplate(ctx, t, tpl, *req.Body); err != nil {
		return nil, err
	}
	tpl.Version = req.Body.Version
	if err := t.st.Templates().Update(ctx, tpl); err != nil {
		return nil, err
	}
	return UpdateTemplate200JSONResponse(templateOut(tpl)), nil
}

func (s *server) DeleteTemplate(ctx context.Context, req DeleteTemplateRequestObject) (DeleteTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := t.st.Templates().Delete(ctx, req.TemplateId.String()); err != nil {
		return nil, err
	}
	return DeleteTemplate204Response{}, nil
}

func (s *server) applyTemplate(ctx context.Context, t *tenant, tpl *store.Template, in TemplateUpdate) error {
	if err := requireNonEmpty("name", in.Name); err != nil {
		return err
	}
	if err := requireNonEmpty("subject", in.Subject); err != nil {
		return err
	}
	// The subject becomes a header, so the CR/LF rule of architecture 16
	// applies to its template source, not only to the rendered value.
	if err := noCRLF("subject", in.Subject); err != nil {
		return err
	}
	mode, err := contentModeIn(in.Mode)
	if err != nil {
		return err
	}
	if int64(len(in.Body)) > s.deps.Limits.MaxBodyBytes {
		return newErr(http.StatusRequestEntityTooLarge, ErrorCodePayloadTooLarge,
			"body exceeds the configured limit of %d bytes", s.deps.Limits.MaxBodyBytes)
	}
	key, shared, err := contentKeyAndShared(t, "template", in.Key, in.Shared)
	if err != nil {
		return err
	}
	uses, err := templateUsesIn(in.Uses)
	if err != nil {
		return err
	}
	if layoutID := idPtrOf(in.LayoutId); layoutID != "" {
		l, err := t.st.Layouts().Get(ctx, layoutID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errInvalid("layout %s does not exist", layoutID)
			}
			return err
		}
		if err := sharedLayoutRule(shared, l); err != nil {
			return err
		}
	}
	tpl.Name = in.Name
	tpl.Key, tpl.Shared, tpl.Uses = key, shared, uses
	tpl.LayoutID = idPtrOf(in.LayoutId)
	tpl.Subject = in.Subject
	tpl.Preheader = deref(in.Preheader)
	tpl.Mode = mode
	tpl.Body = in.Body
	tpl.Text = deref(in.Text)
	tpl.DefaultLocale = deref(in.DefaultLocale)
	tpl.I18n = bundleIn(in.I18n)
	tpl.Blocks = nil
	if in.Blocks != nil {
		raw, err := json.Marshal(*in.Blocks)
		if err != nil {
			return errInvalid("blocks is not encodable as JSON: %v", err)
		}
		tpl.Blocks = raw
	}
	return nil
}

func templateOut(v *store.Template) Template {
	out := Template{
		Id: uuidPtrOf(v.ID), Name: v.Name, LayoutId: uuidPtrOf(v.LayoutID),
		Key: strPtr(v.Key), Shared: ptr(v.Shared), Overridden: ptr(v.Overridden),
		OverriddenFromVersionId: uuidPtrOf(v.OverriddenFromVersion),
		Subject:                 v.Subject, Preheader: strPtr(v.Preheader),
		Mode: ContentMode(v.Mode), Body: strPtr(v.Body), Text: strPtr(v.Text),
		I18n: bundleOut(v.I18n), DefaultLocale: strPtr(v.DefaultLocale),
		PublishedVersionId: uuidPtrOf(v.PublishedVersionID),
		Version:            ptr(v.Version), CreatedAt: timePtr(v.CreatedAt), UpdatedAt: timePtr(v.UpdatedAt),
	}
	if len(v.Blocks) > 0 {
		if m, err := jsonMap(v.Blocks); err == nil {
			out.Blocks = &m
		}
	}
	if len(v.Uses) > 0 {
		uses := make([]string, len(v.Uses))
		for i, u := range v.Uses {
			uses[i] = string(u)
		}
		out.Uses = &uses
	}
	if v.Overridden {
		// The shared original was published again after the copy was made.
		out.SharedUpdatedSinceOverride = ptr(v.SharedPublishedVersionID != v.OverriddenFromVersion)
	}
	return out
}

// --- i18n --------------------------------------------------------------

func bundleIn(in *I18nBundle) store.I18nBundle {
	out := store.I18nBundle{Locales: map[string]map[string]string{}}
	if in == nil {
		return out
	}
	out.DefaultLocale = deref(in.DefaultLocale)
	if in.Locales != nil {
		for loc, kv := range *in.Locales {
			cp := make(map[string]string, len(kv))
			for k, v := range kv {
				cp[k] = v
			}
			out.Locales[loc] = cp
		}
	}
	return out
}

func bundleOut(b store.I18nBundle) *I18nBundle {
	locales := map[string]map[string]string{}
	for loc, kv := range b.Locales {
		cp := make(map[string]string, len(kv))
		for k, v := range kv {
			cp[k] = v
		}
		locales[loc] = cp
	}
	return &I18nBundle{DefaultLocale: strPtr(b.DefaultLocale), Locales: &locales}
}

// GetTemplateI18n serves the bundle as JSON or as the YAML interchange format
// of architecture 6.2. `format` wins over Accept, as the spec says.
func (s *server) GetTemplateI18n(ctx context.Context, req GetTemplateI18nRequestObject) (GetTemplateI18nResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	if !wantsYAML(ctx, req.Params.Format) {
		return GetTemplateI18n200JSONResponse(*bundleOut(tpl.I18n)), nil
	}
	b := tpl.I18n
	if b.DefaultLocale == "" {
		b.DefaultLocale = tpl.DefaultLocale
	}
	data, err := render.ExportYAML(b)
	if err != nil {
		return nil, err
	}
	return GetTemplateI18n200ApplicationxYamlResponse{
		Body: bytes.NewReader(data), ContentLength: int64(len(data)),
	}, nil
}

// wantsYAML applies the content negotiation the spec describes: the explicit
// `format` parameter first, then the Accept header, JSON otherwise.
func wantsYAML(ctx context.Context, format *GetTemplateI18nParamsFormat) bool {
	if format != nil {
		return *format == Yaml
	}
	accept, _ := ctx.Value(ctxKeyAccept).(string)
	accept = strings.ToLower(accept)
	if !strings.Contains(accept, "yaml") {
		return false
	}
	// "application/json, */*" asks for JSON even though */* would also match
	// YAML; only an explicit yaml media type switches the representation.
	return !strings.Contains(accept, "application/json")
}

// ReplaceTemplateI18n imports a bundle and reports how it lines up with the
// keys the template actually uses.
func (s *server) ReplaceTemplateI18n(ctx context.Context, req ReplaceTemplateI18nRequestObject) (ReplaceTemplateI18nResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	if err := refuseSharedWrite(t, tpl.TenantID, "template", tpl.ID); err != nil {
		return nil, err
	}

	var bundle store.I18nBundle
	switch {
	case req.JSONBody != nil:
		bundle = bundleIn(req.JSONBody)
	case req.Body != nil:
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		b, _, err := render.ImportYAML(raw)
		if err != nil {
			return nil, err
		}
		bundle = b
	default:
		return nil, errBadRequest("a request body is required")
	}
	if bundle.DefaultLocale == "" {
		bundle.DefaultLocale = tpl.DefaultLocale
	}

	tpl.I18n = bundle
	if err := t.st.Templates().Update(ctx, tpl); err != nil {
		return nil, err
	}

	merged, err := s.mergedBundle(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	usage, err := s.keyUsage(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	keys := map[string]struct{}{}
	for _, kv := range bundle.Locales {
		for k := range kv {
			keys[k] = struct{}{}
		}
	}
	out := I18nImportResult{Locales: clampInt32(len(bundle.Locales)), Keys: clampInt32(len(keys))}
	missing, unused := coverage(usage, merged, keys)
	if len(missing) > 0 {
		out.MissingKeys = &missing
	}
	if len(unused) > 0 {
		out.UnusedKeys = &unused
	}
	return ReplaceTemplateI18n200JSONResponse(out), nil
}

func (s *server) ListTemplateI18nKeys(ctx context.Context, req ListTemplateI18nKeysRequestObject) (ListTemplateI18nKeysResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	usage, err := s.keyUsage(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	merged, err := s.mergedBundle(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	defaultLocale, err := s.effectiveDefaultLocale(ctx, t, merged.DefaultLocale)
	if err != nil {
		return nil, err
	}

	// The locale set is the merged bundle's locales plus the effective
	// default: a brand-new template has an empty bundle and would otherwise
	// list zero locales, which made every key look "fully translated"
	// (nothing to be missing from) instead of missing from the one locale a
	// send would actually use.
	localeSet := map[string]struct{}{defaultLocale: {}}
	for loc := range merged.Locales {
		localeSet[loc] = struct{}{}
	}
	locales := sortedMapKeys(localeSet)

	items := make([]I18nKeyUsage, 0, len(usage))
	for _, key := range sortedKeys(usage) {
		u := I18nKeyUsage{Key: key}
		where := usage[key]
		if len(where) > 0 {
			u.UsedIn = &where
		}
		var missing []string
		for _, loc := range locales {
			// A locale only counts as covered when the fallback chain a real
			// render would use (render.LocaleChain) resolves the key, so a
			// value inherited from the default locale is not reported as
			// missing here either.
			if !resolvesKey(merged, render.LocaleChain([]string{loc}, "", defaultLocale), key) {
				missing = append(missing, loc)
			}
		}
		if len(missing) > 0 {
			u.MissingLocales = &missing
		}
		items = append(items, u)
	}
	return ListTemplateI18nKeys200JSONResponse(I18nKeyList{
		DefaultLocale: strPtr(defaultLocale),
		Locales:       &locales,
		Items:         items,
	}), nil
}

// effectiveDefaultLocale is the locale a send actually falls back to when
// nothing more specific is set: the merged bundle's own default, then the
// template's (mergedBundle already tries both), then the tenant's configured
// default, then "en".
func (s *server) effectiveDefaultLocale(ctx context.Context, t *tenant, mergedDefault string) (string, error) {
	if mergedDefault != "" {
		return mergedDefault, nil
	}
	settings, err := store.LoadTenantSettings(ctx, t.st, t.id, s.now())
	if err != nil {
		return "", err
	}
	if settings.DefaultLocale != "" {
		return settings.DefaultLocale, nil
	}
	return "en", nil
}

// resolvesKey reports whether key has a value in bundle under any locale in
// chain, most specific first.
func resolvesKey(bundle store.I18nBundle, chain []string, key string) bool {
	for _, loc := range chain {
		if _, ok := bundle.Locales[loc][key]; ok {
			return true
		}
	}
	return false
}

// keyUsage extracts the i18n keys per template part, which is what the editor
// highlights. render.ExtractKeys takes three sources at a time, so each part
// is scanned on its own to know where a key came from.
func (s *server) keyUsage(ctx context.Context, t *tenant, tpl *store.Template) (map[string][]I18nKeyUsageUsedIn, error) {
	layout, err := s.layoutOf(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	parts := []struct {
		where I18nKeyUsageUsedIn
		src   string
	}{
		{I18nKeyUsageUsedInSubject, tpl.Subject},
		{I18nKeyUsageUsedInPreheader, tpl.Preheader},
		{I18nKeyUsageUsedInHtml, tpl.Body},
		{I18nKeyUsageUsedInText, tpl.Text},
	}
	if layout != nil {
		parts = append(parts, struct {
			where I18nKeyUsageUsedIn
			src   string
		}{I18nKeyUsageUsedInLayout, layout.Body})
	}
	usage := map[string][]I18nKeyUsageUsedIn{}
	for _, p := range parts {
		if strings.TrimSpace(p.src) == "" {
			continue
		}
		keys, err := render.ExtractKeys(p.src, "", "")
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			usage[k] = append(usage[k], p.where)
		}
	}
	return usage, nil
}

// coverage splits the merged bundle into the keys a part uses but no locale
// defines, and the keys a locale defines that nothing uses.
func coverage(usage map[string][]I18nKeyUsageUsedIn, merged store.I18nBundle, stored map[string]struct{}) ([]I18nKeyIssue, []string) {
	locales := make([]string, 0, len(merged.Locales))
	for loc := range merged.Locales {
		locales = append(locales, loc)
	}
	sort.Strings(locales)

	var missing []I18nKeyIssue
	for _, key := range sortedKeys(usage) {
		for _, loc := range locales {
			if _, ok := merged.Locales[loc][key]; !ok {
				missing = append(missing, I18nKeyIssue{Key: key, Locale: loc})
			}
		}
		if len(locales) == 0 {
			missing = append(missing, I18nKeyIssue{Key: key, Locale: merged.DefaultLocale})
		}
	}
	var unused []string
	for _, k := range sortedMapKeys(stored) {
		if _, used := usage[k]; !used {
			unused = append(unused, k)
		}
	}
	return missing, unused
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedMapKeys(m map[string]struct{}) []string { return sortedKeys(m) }

// mergedBundle is the layout bundle overlaid by the template's, which is what
// publishing and rendering actually see (internal/render mergeBundles).
func (s *server) mergedBundle(ctx context.Context, t *tenant, tpl *store.Template) (store.I18nBundle, error) {
	layout, err := s.layoutOf(ctx, t, tpl)
	if err != nil {
		return store.I18nBundle{}, err
	}
	out := store.I18nBundle{Locales: map[string]map[string]string{}}
	add := func(b store.I18nBundle) {
		for loc, kv := range b.Locales {
			if out.Locales[loc] == nil {
				out.Locales[loc] = map[string]string{}
			}
			for k, v := range kv {
				out.Locales[loc][k] = v
			}
		}
	}
	if layout != nil {
		add(layout.I18n)
	}
	add(tpl.I18n)
	out.DefaultLocale = tpl.I18n.DefaultLocale
	if out.DefaultLocale == "" {
		out.DefaultLocale = tpl.DefaultLocale
	}
	return out, nil
}

// layoutOf loads the layout a template is rendered with.
//
// The reference is an ID, but a layout that has a key resolves *by key*, the
// tenant's own layout first and the shared one second (ADR-0018): that is how
// a tenant's override of a shared layout takes effect for every template of
// the tenant that names the shared one, including its override of a shared
// template, whose copied layout_id is the shared layout's ID. A shared
// template viewed from a tenant is the exception: it renders with exactly the
// layout the system tenant published it with, so its preview matches what it
// sends.
func (s *server) layoutOf(ctx context.Context, t *tenant, tpl *store.Template) (*store.Layout, error) {
	if tpl.LayoutID == "" {
		return nil, nil
	}
	l, err := t.st.Layouts().Get(ctx, tpl.LayoutID)
	if errors.Is(err, store.ErrNotFound) {
		// The layout was deleted under the template. Publishing without it is
		// still better than a 500, and the missing {{ content }} slot would be
		// the only thing it contributed.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.Key == "" || tpl.TenantID != t.id {
		return l, nil
	}
	byKey, err := t.st.Layouts().GetByKey(ctx, l.Key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return l, nil
	case err != nil:
		return nil, err
	}
	return byKey, nil
}

// --- preview and publish -----------------------------------------------

// PreviewTemplate renders the current draft through the real pipeline: a
// throwaway Publish of the template and its layout, then the same
// PrepareChain/Render the sender runs. That is what makes the preview
// authoritative instead of an approximation (architecture 6.3).
func (s *server) PreviewTemplate(ctx context.Context, req PreviewTemplateRequestObject) (PreviewTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	layout, err := s.layoutOf(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	body := PreviewRequest{}
	if req.Body != nil {
		body = *req.Body
	}

	// A draft is allowed to have missing keys; they come back as warnings and
	// missing_keys instead of failing the preview the way a publish would.
	version, warnings, err := render.Publish(ctx, tpl, layout, render.PublishOptions{
		AllowMissingKeys: true, Now: s.deps.Clock, NewID: store.NewID,
	})
	if err != nil {
		return nil, err
	}

	var rcp PreviewRecipient
	if body.Recipient != nil {
		rcp = *body.Recipient
	}
	locales := []string{deref(rcp.Locale), deref(body.Locale), tpl.DefaultLocale}
	prepared, err := s.deps.Renderer.PrepareChain(version, locales...)
	if err != nil {
		return nil, err
	}
	// The preview sees the same `tenant` binding a real send would, so a
	// template that reads {{ tenant.name }} can be checked before it is
	// published. The hook runs here too: a preview must not be a way to see
	// what a template renders with tenant attributes the host would refuse
	// (ADR-0017).
	tenantVars, err := s.tenantVars(ctx, t, body.TenantVars)
	if err != nil {
		return nil, err
	}
	out, renderWarnings, err := prepared.Render(ctx, render.Bindings{
		Recipient: render.Recipient{
			Email:  string(deref(rcp.Email)),
			Name:   deref(rcp.Name),
			Locale: deref(rcp.Locale),
			Vars:   varsOf(rcp.Vars),
		},
		Vars:           varsOf(body.Vars),
		TenantVars:     tenantVars,
		UnsubscribeURL: deref(rcp.UnsubscribeUrl),
	})
	if err != nil {
		return nil, err
	}

	warnings = append(warnings, renderWarnings...)
	res := PreviewResult{
		Locale: prepared.Locale(), Subject: out.Subject, Html: out.HTML, Text: strPtr(out.Text),
	}
	var msgs []string
	var missing []I18nKeyIssue
	for _, w := range warnings {
		msgs = append(msgs, w.String())
		if w.Code == render.WarnMissingKey {
			missing = append(missing, I18nKeyIssue{Key: w.Key, Locale: w.Locale})
		}
	}
	if len(msgs) > 0 {
		res.Warnings = &msgs
	}
	if len(missing) > 0 {
		res.MissingKeys = &missing
	}
	return PreviewTemplate200JSONResponse(res), nil
}

func (s *server) PublishTemplate(ctx context.Context, req PublishTemplateRequestObject) (PublishTemplateResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := t.st.Templates().Get(ctx, req.TemplateId.String())
	if err != nil {
		return nil, err
	}
	// A shared template is published in the system tenant only; a tenant
	// publishes its override instead.
	if err := refuseSharedWrite(t, tpl.TenantID, "template", tpl.ID); err != nil {
		return nil, err
	}
	layout, err := s.layoutOf(ctx, t, tpl)
	if err != nil {
		return nil, err
	}
	if layout != nil {
		if err := sharedLayoutRule(tpl.Shared, layout); err != nil {
			return nil, err
		}
	}
	allowMissing := false
	if req.Body != nil && req.Body.AllowMissingI18nKeys != nil {
		allowMissing = *req.Body.AllowMissingI18nKeys
	}

	version, _, err := render.Publish(ctx, tpl, layout, render.PublishOptions{
		AllowMissingKeys: allowMissing, Now: s.deps.Clock, NewID: store.NewID,
	})
	if err != nil {
		return nil, err
	}
	if err := t.st.Versions().Create(ctx, version); err != nil {
		return nil, err
	}
	tpl.PublishedVersionID = version.ID
	if err := t.st.Templates().Update(ctx, tpl); err != nil {
		return nil, err
	}
	return PublishTemplate201JSONResponse(versionOut(version)), nil
}

// --- message versions --------------------------------------------------

func (s *server) ListMessageVersions(ctx context.Context, req ListMessageVersionsRequestObject) (ListMessageVersionsResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := t.st.Templates().Get(ctx, req.TemplateId.String()); err != nil {
		return nil, err
	}
	res, err := t.st.Versions().ListByTemplate(ctx, req.TemplateId.String(),
		pageOf(req.Params.Limit, req.Params.Cursor))
	if err != nil {
		return nil, err
	}
	items := make([]MessageVersion, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, versionOut(&res.Items[i]))
	}
	return ListMessageVersions200JSONResponse{Items: items, NextCursor: nextCursor(res.NextCursor)}, nil
}

func (s *server) GetMessageVersion(ctx context.Context, req GetMessageVersionRequestObject) (GetMessageVersionResponseObject, error) {
	t, err := tenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := s.visibleVersion(ctx, t, req.VersionId.String())
	if err != nil {
		return nil, err
	}
	return GetMessageVersion200JSONResponse(versionOut(v)), nil
}

func versionOut(v *store.MessageVersion) MessageVersion {
	out := MessageVersion{
		Id: uuidOf(v.ID), TemplateId: uuidOf(v.TemplateID), LayoutId: uuidPtrOf(v.LayoutID),
		SubjectTpl: v.SubjectTpl, HtmlTpl: v.HTMLTpl, TextTpl: strPtr(v.TextTpl),
		I18n: bundleOut(v.I18n), DefaultLocale: strPtr(v.DefaultLocale),
		Checksum: strPtr(v.Checksum), CreatedAt: timePtr(v.CreatedAt),
	}
	if len(v.Links) > 0 {
		links := v.Links
		out.Links = &links
	}
	return out
}
