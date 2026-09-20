package render

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/osteele/liquid"
	lrender "github.com/osteele/liquid/render"

	"github.com/sendplane/sendplane/store"
)

// i18nBindingKey is where Render stashes the per-render i18n state so that the
// {% t %} tag can reach it. The NUL byte makes it unspellable in a template,
// so it cannot be read or shadowed from Liquid source.
const i18nBindingKey = "\x00sendplane.i18n"

// maxTranslationDepth bounds {% t %} inside a translation value inside a
// translation value…, which would otherwise recurse forever.
const maxTranslationDepth = 4

// WarningCode classifies a non-fatal render or publish problem.
type WarningCode string

const (
	// WarnMissingKey: no locale in the fallback chain defines the key, so the
	// key itself was rendered (architecture 6.2).
	WarnMissingKey WarningCode = "missing_key"
	// WarnBadTranslation: a translation value is not valid Liquid.
	WarnBadTranslation WarningCode = "bad_translation"
	// WarnUnusedKey: a bundle defines a key no part uses.
	WarnUnusedKey WarningCode = "unused_key"
	// WarnMissingTranslation: a non-default locale is missing a key the
	// default locale defines.
	WarnMissingTranslation WarningCode = "missing_translation"
	// WarnTranslationDepth: {% t %} nesting hit maxTranslationDepth.
	WarnTranslationDepth WarningCode = "translation_depth"
)

// Warning is a problem that does not stop a render or a publish. Warnings are
// returned to the caller rather than logged, so the API can surface them in a
// preview response and the sender can emit them as events.
type Warning struct {
	Code   WarningCode
	Key    string
	Locale string
	Detail string
}

func (w Warning) String() string {
	var b strings.Builder
	b.WriteString(string(w.Code))
	if w.Key != "" {
		fmt.Fprintf(&b, " key=%q", w.Key)
	}
	if w.Locale != "" {
		fmt.Fprintf(&b, " locale=%q", w.Locale)
	}
	if w.Detail != "" {
		fmt.Fprintf(&b, ": %s", w.Detail)
	}
	return b.String()
}

// Engine is a configured Liquid engine: standard filters and tags minus the
// ones that touch the filesystem, plus the sendplane i18n tag. It is safe for
// concurrent use, and templates it parses are safe to render concurrently.
//
// One Engine per process is enough; Renderer creates one if you do not pass it.
type Engine struct {
	lq      *liquid.Engine
	strict  bool
	trCache *lruCache[trKey, *liquid.Template]
}

type trKey struct {
	bundle string // bundle checksum
	locale string
	key    string
}

// EngineOption configures NewEngine.
type EngineOption func(*engineConfig)

type engineConfig struct {
	strict      bool
	trCacheSize int
}

// WithStrictVariables makes an undefined variable an error instead of an empty
// string. ADR-0004: on for previews, off for sending.
func WithStrictVariables() EngineOption {
	return func(c *engineConfig) { c.strict = true }
}

// WithTranslationCacheSize caps the number of parsed translation values kept
// in memory (default 4096).
func WithTranslationCacheSize(n int) EngineOption {
	return func(c *engineConfig) { c.trCacheSize = n }
}

// denyTemplateStore refuses every partial lookup. The include/render tags are
// unregistered below; this is the second layer, in case a future osteele
// release reaches the store some other way (architecture 16: Liquid sandbox,
// no file access).
type denyTemplateStore struct{}

func (denyTemplateStore) ReadTemplate(name string) ([]byte, error) {
	return nil, fmt.Errorf("render: template partials are disabled: %q: %w", name, fs.ErrNotExist)
}

// NewEngine returns an Engine with the sendplane sandbox applied.
func NewEngine(opts ...EngineOption) *Engine {
	cfg := engineConfig{trCacheSize: 4096}
	for _, o := range opts {
		o(&cfg)
	}

	lq := liquid.NewEngine()
	// ADR-0004 / architecture 6.1: no include, render or layout tags. osteele
	// ships include and render; layout does not exist.
	lq.UnregisterTag("include")
	lq.UnregisterTag("render")
	lq.RegisterTemplateStore(denyTemplateStore{})
	if cfg.strict {
		lq.StrictVariables()
	}

	e := &Engine{
		lq:      lq,
		strict:  cfg.strict,
		trCache: newLRU[trKey, *liquid.Template](cfg.trCacheSize),
	}
	lq.RegisterTag("t", e.tTag)
	// url_encode, url_decode and default are standard osteele filters, so
	// there is nothing to add here; see README.
	return e
}

// Parse compiles one template part. The `{{ "key" | t }}` filter form is
// normalised into the tag form first (see rewriteTFilter).
func (e *Engine) Parse(src string) (*liquid.Template, error) {
	rewritten, err := rewriteTFilter(src)
	if err != nil {
		return nil, err
	}
	tpl, serr := e.lq.ParseString(rewritten)
	if serr != nil {
		return nil, fmt.Errorf("render: parse: %w", serr)
	}
	return tpl, nil
}

// Validate reports whether src is a parseable template.
func (e *Engine) Validate(src string) error {
	_, err := e.Parse(src)
	return err
}

// tTag implements {% t "key" %} and {% t "key" name: expr, … %}.
func (e *Engine) tTag(ctx lrender.Context) (string, error) {
	key, _, kwargs, err := parseTagArgs(ctx.TagArgs())
	if err != nil {
		return "", ctx.WrapError(err)
	}
	if !isQuoted(strings.TrimSpace(ctx.TagArgs())) {
		// A dynamic key: evaluate it as an expression.
		v, err := ctx.EvaluateString(key)
		if err != nil {
			return "", ctx.WrapError(err)
		}
		key = toString(v)
	}

	args := make(map[string]any, len(kwargs))
	for _, kw := range kwargs {
		v, err := ctx.EvaluateString(kw.expr)
		if err != nil {
			return "", ctx.WrapError(err)
		}
		args[kw.name] = v
	}

	st, _ := ctx.Get(i18nBindingKey).(*i18nState)
	if st == nil {
		// No bundle bound (for example a bare Engine render): the key is the
		// only sensible output.
		return key, nil
	}
	return st.translate(e, ctx, key, args)
}

func isQuoted(s string) bool {
	return len(s) > 0 && (s[0] == '"' || s[0] == '\'')
}

func toString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	default:
		return fmt.Sprint(v)
	}
}

// i18nState is the per-render translation context. One is created per
// Prepared.Render call, so its mutable warning list is never shared between
// goroutines.
type i18nState struct {
	bundle   store.I18nBundle
	sum      string   // bundle checksum, part of the translation cache key
	chain    []string // locale fallback chain, most specific first
	depth    int
	warnings []Warning
	warned   map[Warning]struct{}
}

func newI18nState(bundle store.I18nBundle, sum string, chain []string) *i18nState {
	return &i18nState{bundle: bundle, sum: sum, chain: chain, warned: map[Warning]struct{}{}}
}

func (s *i18nState) warn(w Warning) {
	if _, dup := s.warned[w]; dup {
		return
	}
	s.warned[w] = struct{}{}
	s.warnings = append(s.warnings, w)
}

// lookup walks the fallback chain and returns the first locale that defines
// key, plus its value.
func (s *i18nState) lookup(key string) (locale, value string, ok bool) {
	for _, loc := range s.chain {
		if m := s.bundle.Locales[loc]; m != nil {
			if v, hit := m[key]; hit {
				return loc, v, true
			}
		}
	}
	return "", "", false
}

func (s *i18nState) translate(e *Engine, ctx lrender.Context, key string, args map[string]any) (string, error) {
	loc, value, ok := s.lookup(key)
	if !ok {
		s.warn(Warning{Code: WarnMissingKey, Key: key, Locale: s.requested()})
		return key, nil
	}
	if s.depth >= maxTranslationDepth {
		s.warn(Warning{Code: WarnTranslationDepth, Key: key, Locale: loc})
		return value, nil
	}

	ck := trKey{bundle: s.sum, locale: loc, key: key}
	tpl, hit := e.trCache.get(ck)
	if !hit {
		var err error
		tpl, err = e.Parse(value)
		if err != nil {
			s.warn(Warning{Code: WarnBadTranslation, Key: key, Locale: loc, Detail: err.Error()})
			return value, nil
		}
		e.trCache.put(ck, tpl)
	}

	// Translation values interpolate the surrounding bindings, plus the tag's
	// keyword arguments (architecture 6.1).
	bindings := make(map[string]any, len(ctx.Bindings())+len(args))
	for k, v := range ctx.Bindings() {
		bindings[k] = v
	}
	for k, v := range args {
		bindings[k] = v
	}

	s.depth++
	out, serr := tpl.RenderString(bindings)
	s.depth--
	if serr != nil {
		s.warn(Warning{Code: WarnBadTranslation, Key: key, Locale: loc, Detail: serr.Error()})
		return value, nil
	}
	return out, nil
}

func (s *i18nState) requested() string {
	if len(s.chain) == 0 {
		return ""
	}
	return s.chain[0]
}

// localeChain builds the fallback chain of architecture 6.2. Each requested
// locale contributes itself and its language subtag, in order, and the
// version's and bundle's default locales close the chain:
//
//	ko-KR -> ko -> (campaign default) -> ... -> version default -> its language
//
// What is left when nothing matches is the key itself, which translate
// handles.
func localeChain(requested []string, versionDefault, bundleDefault string) []string {
	var chain []string
	seen := map[string]struct{}{}
	add := func(loc string) {
		loc = strings.TrimSpace(loc)
		if loc == "" {
			return
		}
		if _, dup := seen[loc]; dup {
			return
		}
		seen[loc] = struct{}{}
		chain = append(chain, loc)
	}
	for _, loc := range append(append([]string{}, requested...), versionDefault, bundleDefault) {
		add(loc)
		add(languageOf(loc))
	}
	return chain
}

// languageOf returns the language subtag of a BCP 47 tag ("ko-KR" -> "ko").
func languageOf(tag string) string {
	if i := strings.IndexAny(tag, "-_"); i > 0 {
		return tag[:i]
	}
	return ""
}
