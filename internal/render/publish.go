package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	mjml "github.com/Boostport/mjml-go"

	"github.com/sendplane/sendplane/store"
)

// Publish errors.
var (
	// ErrNoTemplate is returned when Publish is called without a template.
	ErrNoTemplate = errors.New("render: no template")
	// ErrNoContentSlot is returned when a layout body has no {{ content }}.
	ErrNoContentSlot = errors.New("render: layout has no {{ content }} slot")
	// ErrModeMismatch is returned when a layout and a template disagree on
	// MJML vs HTML.
	ErrModeMismatch = errors.New("render: layout and template content modes differ")
	// ErrUnknownMode is returned for a content mode that is not blocks, mjml
	// or html.
	ErrUnknownMode = errors.New("render: unknown content mode")
	// ErrMissingKeys is returned when a part uses an i18n key that the merged
	// bundle does not define for the default locale.
	ErrMissingKeys = errors.New("render: missing i18n keys")
	// ErrHeaderInjection is returned when a subject contains CR or LF
	// (architecture 16).
	ErrHeaderInjection = errors.New("render: subject contains a line break")
)

// PublishOptions tunes Publish. The zero value is usable.
type PublishOptions struct {
	// Engine parses the template parts. Publish creates one if nil.
	Engine *Engine
	// AllowMissingKeys downgrades unknown i18n keys from an error to a
	// warning (architecture 6.2: "tenant setting may relax this").
	AllowMissingKeys bool
	// MinifyHTML minifies the compiled MJML output.
	MinifyHTML bool
	// Now and NewID are injectable for deterministic tests.
	Now   func() time.Time
	NewID func() string
}

func (o PublishOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}

func (o PublishOptions) newID() string {
	if o.NewID != nil {
		return o.NewID()
	}
	return store.NewID()
}

// Publish freezes a template and its layout into an immutable MessageVersion:
// layout slot merged, i18n bundles merged, MJML compiled once, text derived,
// trackable links extracted (architecture 6.3, ADR-0009).
//
// The returned MessageVersion has no CreatedAt-dependent state beyond the
// timestamp; storing it is the caller's job.
func Publish(ctx context.Context, tpl *store.Template, layout *store.Layout, opts PublishOptions) (*store.MessageVersion, []Warning, error) {
	if tpl == nil {
		return nil, nil, ErrNoTemplate
	}
	eng := opts.Engine
	if eng == nil {
		eng = NewEngine()
	}
	var warnings []Warning

	mode, err := effectiveMode(tpl.Mode)
	if err != nil {
		return nil, nil, err
	}

	body := tpl.Body
	layoutID := tpl.LayoutID
	if layout != nil {
		layoutMode, err := effectiveMode(layout.Mode)
		if err != nil {
			return nil, nil, fmt.Errorf("layout: %w", err)
		}
		if layoutMode != mode {
			return nil, nil, fmt.Errorf("%w: layout %q, template %q", ErrModeMismatch, layout.Mode, tpl.Mode)
		}
		body, err = mergeLayout(layout.Body, tpl.Body)
		if err != nil {
			return nil, nil, err
		}
		layoutID = layout.ID
	}

	bundle := mergeBundles(layout, tpl)

	htmlOut := body
	if mode == store.ContentMJML {
		htmlOut, err = compileMJML(ctx, body, opts.MinifyHTML)
		if err != nil {
			return nil, nil, err
		}
	}

	// Mark unsubscribe anchors before extracting links so that link_no stays
	// stable once the href has rendered into a real URL (architecture 9.2).
	htmlOut, err = markUnsubscribeAnchors(htmlOut)
	if err != nil {
		return nil, nil, err
	}

	text := tpl.Text
	if strings.TrimSpace(text) == "" {
		text = HTMLToText(htmlOut)
	}

	subject := tpl.Subject
	if strings.ContainsAny(subject, "\r\n") {
		return nil, nil, ErrHeaderInjection
	}

	for _, part := range []struct{ name, src string }{
		{"subject", subject}, {"html", htmlOut}, {"text", text},
	} {
		if err := eng.Validate(part.src); err != nil {
			return nil, nil, fmt.Errorf("render: %s: %w", part.name, err)
		}
	}

	links, err := ExtractLinks(htmlOut)
	if err != nil {
		return nil, nil, err
	}

	keyWarnings, missing, err := checkKeys(eng, bundle, subject, htmlOut, text)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, keyWarnings...)
	if len(missing) > 0 && !opts.AllowMissingKeys {
		return nil, nil, fmt.Errorf("%w: %s (locale %q)", ErrMissingKeys, strings.Join(missing, ", "), bundle.DefaultLocale)
	}

	v := &store.MessageVersion{
		ID:            opts.newID(),
		TenantID:      tpl.TenantID,
		TemplateID:    tpl.ID,
		LayoutID:      layoutID,
		SubjectTpl:    subject,
		HTMLTpl:       htmlOut,
		TextTpl:       text,
		I18n:          bundle,
		DefaultLocale: bundle.DefaultLocale,
		Links:         links,
		CreatedAt:     opts.now(),
	}
	v.Checksum = versionChecksum(v)
	return v, warnings, nil
}

// effectiveMode maps the authoring mode to the compile mode: blocks are stored
// as the MJML the editor exported (store.ContentBlocks).
func effectiveMode(m store.ContentMode) (store.ContentMode, error) {
	switch m {
	case store.ContentBlocks, store.ContentMJML:
		return store.ContentMJML, nil
	case store.ContentHTML:
		return store.ContentHTML, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMode, m)
	}
}

// mergeLayout substitutes the template body for the layout's {{ content }}
// slot. For an MJML layout the slot sits inside the MJML tree, so a textual
// substitution inserts the body at the right place in the tree.
func mergeLayout(layoutBody, templateBody string) (string, error) {
	var b strings.Builder
	last := 0
	found := false
	for _, sp := range scanLiquidSpans(layoutBody) {
		if sp.kind != spanObject || strings.TrimSpace(sp.inner) != "content" {
			continue
		}
		b.WriteString(layoutBody[last:sp.start])
		b.WriteString(templateBody)
		last = sp.end
		found = true
	}
	if !found {
		return "", ErrNoContentSlot
	}
	b.WriteString(layoutBody[last:])
	return b.String(), nil
}

// mergeBundles merges the layout bundle into the template bundle. Template
// keys win, and the default locale is the template's: a layout is shared, so
// it does not get to pick the message language (architecture 6.2).
func mergeBundles(layout *store.Layout, tpl *store.Template) store.I18nBundle {
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
	return out
}

func compileMJML(ctx context.Context, src string, minify bool) (string, error) {
	opts := []mjml.ToHTMLOption{mjml.WithValidationLevel(mjml.Strict)}
	if minify {
		opts = append(opts, mjml.WithMinify(true))
	}
	out, err := mjml.ToHTML(ctx, src, opts...)
	if err != nil {
		return "", fmt.Errorf("render: mjml: %w", err)
	}
	return out, nil
}

// checkKeys validates i18n key usage against the merged bundle and reports
// keys that are missing from the default locale, translations that do not
// parse, translations missing from a secondary locale, and unused keys.
func checkKeys(eng *Engine, b store.I18nBundle, subject, htmlSrc, text string) (warnings []Warning, missing []string, err error) {
	used, err := ExtractKeys(subject, htmlSrc, text)
	if err != nil {
		return nil, nil, err
	}
	def := b.Locales[b.DefaultLocale]
	usedSet := make(map[string]struct{}, len(used))
	for _, k := range used {
		usedSet[k] = struct{}{}
		if _, ok := def[k]; !ok {
			missing = append(missing, k)
			warnings = append(warnings, Warning{Code: WarnMissingKey, Key: k, Locale: b.DefaultLocale})
		}
	}

	locales := make([]string, 0, len(b.Locales))
	for loc := range b.Locales {
		locales = append(locales, loc)
	}
	sort.Strings(locales)
	for _, loc := range locales {
		kv := b.Locales[loc]
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := eng.Validate(kv[k]); err != nil {
				warnings = append(warnings, Warning{Code: WarnBadTranslation, Key: k, Locale: loc, Detail: err.Error()})
			}
			if _, ok := usedSet[k]; !ok {
				warnings = append(warnings, Warning{Code: WarnUnusedKey, Key: k, Locale: loc})
			}
		}
		if loc == b.DefaultLocale || len(kv) == 0 {
			continue
		}
		for _, k := range used {
			if _, ok := kv[k]; !ok {
				warnings = append(warnings, Warning{Code: WarnMissingTranslation, Key: k, Locale: loc})
			}
		}
	}
	return warnings, missing, nil
}

// versionChecksum hashes the parts that decide whether two publishes produce
// the same message: subject, html, text, default locale and the merged bundle.
// Links and IDs are derived from those, so they are not hashed.
func versionChecksum(v *store.MessageVersion) string {
	h := sha256.New()
	writeField(h, "subject", v.SubjectTpl)
	writeField(h, "html", v.HTMLTpl)
	writeField(h, "text", v.TextTpl)
	writeField(h, "default_locale", v.DefaultLocale)
	writeBundle(h, v.I18n)
	return hex.EncodeToString(h.Sum(nil))
}

// bundleChecksum identifies a bundle, and is part of the translation parse
// cache key.
func bundleChecksum(b store.I18nBundle) string {
	h := sha256.New()
	writeBundle(h, b)
	return hex.EncodeToString(h.Sum(nil))
}

func writeBundle(h io.Writer, b store.I18nBundle) {
	writeField(h, "bundle_default", b.DefaultLocale)
	locales := make([]string, 0, len(b.Locales))
	for loc := range b.Locales {
		locales = append(locales, loc)
	}
	sort.Strings(locales)
	for _, loc := range locales {
		writeField(h, "locale", loc)
		kv := b.Locales[loc]
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeField(h, k, kv[k])
		}
	}
}

// writeField writes a length-prefixed field so that concatenation cannot be
// ambiguous.
func writeField(h io.Writer, name, value string) {
	_, _ = fmt.Fprintf(h, "%d:%s=%d:%s\n", len(name), name, len(value), value)
}
