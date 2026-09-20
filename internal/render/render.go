// Package render turns a sendplane template into the subject, HTML and text
// parts of one message.
//
// It has two entry points, matching the two halves of the pipeline described
// in architecture 6.3:
//
//	Publish   template (+layout) -> merged, MJML compiled, links extracted
//	          -> store.MessageVersion. Runs once, in the control plane.
//	Renderer  MessageVersion + locale -> Prepared (parsed once, cached)
//	          -> Render(bindings) -> Output. Runs once per recipient, in the
//	          sender, and must be cheap and concurrency-safe.
//
// After rendering, RewriteLinks and InsertPixel apply the tracking transforms
// of architecture 9.2. Signing the tracking tokens is the caller's job.
package render

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/osteele/liquid"

	"github.com/sendplane/sendplane/store"
)

// Render errors.
var (
	// ErrNoVersion is returned when Prepare is called without a version.
	ErrNoVersion = errors.New("render: no message version")
	// ErrOutputTooLarge is returned when a rendered part exceeds the
	// configured cap (architecture 16).
	ErrOutputTooLarge = errors.New("render: output too large")
)

// DefaultMaxOutputBytes caps each rendered part.
const DefaultMaxOutputBytes = 2 << 20 // 2 MiB

// DefaultCacheSize is the number of (version, locale) parse results kept.
const DefaultCacheSize = 256

// Recipient is the per-recipient half of the bindings.
type Recipient struct {
	Email  string
	Name   string
	Locale string
	Vars   map[string]any
}

// Campaign identifies the campaign a message belongs to. Transactional sends
// leave it zero.
type Campaign struct {
	ID   string
	Name string
}

// Bindings is everything a template can see. Recipient.Vars override Vars of
// the same name.
type Bindings struct {
	Recipient      Recipient
	Vars           map[string]any
	Campaign       Campaign
	UnsubscribeURL string
	// Locale overrides the locale exposed as {{ locale }}; it defaults to the
	// locale the Prepared was built for.
	Locale string
}

// Output is one rendered message.
type Output struct {
	Subject string
	HTML    string
	Text    string
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithEngine reuses an existing Engine instead of creating one.
func WithEngine(e *Engine) Option {
	return func(r *Renderer) { r.eng = e }
}

// WithCacheSize caps the number of prepared (version, locale) pairs kept in
// memory (default DefaultCacheSize).
func WithCacheSize(n int) Option {
	return func(r *Renderer) { r.cache = newLRU[string, *Prepared](n) }
}

// WithMaxOutputBytes caps each rendered part (default
// DefaultMaxOutputBytes). Zero removes the cap.
func WithMaxOutputBytes(n int) Option {
	return func(r *Renderer) { r.maxOutput = n }
}

// Renderer renders published message versions. One Renderer serves every
// sender goroutine: it is safe for concurrent use, and so is everything it
// returns.
type Renderer struct {
	eng       *Engine
	cache     *lruCache[string, *Prepared]
	maxOutput int
}

// NewRenderer returns a Renderer with the default engine, cache size and
// output cap.
func NewRenderer(opts ...Option) *Renderer {
	r := &Renderer{maxOutput: DefaultMaxOutputBytes}
	for _, o := range opts {
		o(r)
	}
	if r.eng == nil {
		r.eng = NewEngine()
	}
	if r.cache == nil {
		r.cache = newLRU[string, *Prepared](DefaultCacheSize)
	}
	return r
}

// Prepared is a message version parsed for one locale. Render may be called on
// it concurrently.
type Prepared struct {
	versionID string
	locale    string
	chain     []string
	bundle    store.I18nBundle
	bundleSum string

	eng       *Engine
	maxOutput int

	subject *liquid.Template
	html    *liquid.Template
	text    *liquid.Template
}

// Locale is the locale this Prepared resolves translations for.
func (p *Prepared) Locale() string { return p.locale }

// LocaleChain is the fallback chain used for translations, most specific
// first (architecture 6.2).
func (p *Prepared) LocaleChain() []string {
	out := make([]string, len(p.chain))
	copy(out, p.chain)
	return out
}

// Prepare parses a version's three parts for one locale and caches the result
// under (version ID, locale).
//
// The rest of the fallback chain - the language subtag, the version default
// and finally the key itself - is applied during rendering. A campaign default
// locale sits between the recipient's language and the version default, so a
// campaign send calls PrepareChain instead.
func (r *Renderer) Prepare(v *store.MessageVersion, locale string) (*Prepared, error) {
	return r.PrepareChain(v, locale)
}

// PrepareChain is Prepare with the full preference list of architecture 6.2,
// most specific first: the recipient locale, then the campaign default locale.
// Each entry also contributes its language subtag.
func (r *Renderer) PrepareChain(v *store.MessageVersion, locales ...string) (*Prepared, error) {
	if v == nil {
		return nil, ErrNoVersion
	}
	locale := ""
	for _, l := range locales {
		if strings.TrimSpace(l) != "" {
			locale = l
			break
		}
	}
	key := v.ID + "\x00" + strings.Join(locales, ",")
	if p, ok := r.cache.get(key); ok {
		return p, nil
	}

	p := &Prepared{
		versionID: v.ID,
		locale:    locale,
		chain:     localeChain(locales, v.DefaultLocale, v.I18n.DefaultLocale),
		bundle:    v.I18n,
		bundleSum: bundleChecksum(v.I18n),
		eng:       r.eng,
		maxOutput: r.maxOutput,
	}
	var err error
	if p.subject, err = r.eng.Parse(v.SubjectTpl); err != nil {
		return nil, fmt.Errorf("render: subject: %w", err)
	}
	if p.html, err = r.eng.Parse(v.HTMLTpl); err != nil {
		return nil, fmt.Errorf("render: html: %w", err)
	}
	if p.text, err = r.eng.Parse(v.TextTpl); err != nil {
		return nil, fmt.Errorf("render: text: %w", err)
	}

	r.cache.put(key, p)
	return p, nil
}

// CacheLen reports how many prepared versions are cached; it exists for tests
// and metrics.
func (r *Renderer) CacheLen() int { return r.cache.len() }

// Render produces the three parts for one recipient.
//
// The HTML part is rendered with HTML-escaped bindings; subject and text are
// not escaped (ADR-0004). The subject is rejected if it renders to something
// containing CR or LF (architecture 16). Rendering stops with ctx.Err() when
// the context is done, and with ErrOutputTooLarge when a part exceeds the
// configured cap.
func (p *Prepared) Render(ctx context.Context, b Bindings) (Output, []Warning, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, nil, err
	}

	locale := b.Locale
	if locale == "" {
		locale = b.Recipient.Locale
	}
	if locale == "" {
		locale = p.locale
	}
	base := p.baseBindings(b, locale)

	type result struct {
		out      Output
		warnings []Warning
		err      error
	}
	done := make(chan result, 1)
	go func() {
		var res result
		res.out, res.warnings, res.err = p.renderAll(ctx, base)
		done <- res
	}()

	select {
	case <-ctx.Done():
		// The goroutine observes the same context through its writers and
		// exits on its next write.
		return Output{}, nil, ctx.Err()
	case res := <-done:
		return res.out, res.warnings, res.err
	}
}

func (p *Prepared) renderAll(ctx context.Context, base map[string]any) (out Output, warnings []Warning, err error) {
	// A template is tenant-authored input running in the sender process. The
	// Liquid evaluator turns most failures into errors, but re-panics an
	// unexpected one from a filter; that must not take the sender down
	// (architecture 16).
	defer func() {
		if r := recover(); r != nil {
			out, err = Output{}, fmt.Errorf("render: panic: %v", r)
		}
	}()

	// One i18n state for the whole message, so a key missing from all three
	// parts is reported once.
	st := newI18nState(p.bundle, p.bundleSum, p.chain)
	rawVars := rawBindings(base).(map[string]any)
	escVars := escapeBindings(base).(map[string]any)

	subject, err := p.renderPart(ctx, p.subject, rawVars, st)
	if err != nil {
		return Output{}, st.warnings, fmt.Errorf("render: subject: %w", err)
	}
	if strings.ContainsAny(subject, "\r\n") {
		return Output{}, st.warnings, ErrHeaderInjection
	}
	out.Subject = subject

	if out.HTML, err = p.renderPart(ctx, p.html, escVars, st); err != nil {
		return Output{}, st.warnings, fmt.Errorf("render: html: %w", err)
	}
	if out.Text, err = p.renderPart(ctx, p.text, rawVars, st); err != nil {
		return Output{}, st.warnings, fmt.Errorf("render: text: %w", err)
	}
	return out, st.warnings, nil
}

// renderPart renders one template part into a size- and deadline-capped
// writer.
func (p *Prepared) renderPart(ctx context.Context, tpl *liquid.Template, vars map[string]any, st *i18nState) (string, error) {
	bindings := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		bindings[k] = v
	}
	bindings[i18nBindingKey] = st

	w := &capWriter{ctx: ctx, limit: p.maxOutput}
	if err := tpl.FRender(w, bindings); err != nil {
		if w.err != nil {
			return "", w.err
		}
		return "", err
	}
	return w.buf.String(), nil
}

// baseBindings assembles the variables of architecture 6.1.
func (p *Prepared) baseBindings(b Bindings, locale string) map[string]any {
	vars := make(map[string]any, len(b.Vars)+len(b.Recipient.Vars))
	for k, v := range b.Vars {
		vars[k] = v
	}
	// Per-recipient variables win over campaign-level ones.
	for k, v := range b.Recipient.Vars {
		vars[k] = v
	}

	recipient := map[string]any{
		"email":  b.Recipient.Email,
		"name":   b.Recipient.Name,
		"locale": locale,
		"vars":   b.Recipient.Vars,
	}
	if recipient["vars"] == nil {
		recipient["vars"] = map[string]any{}
	}

	return map[string]any{
		"recipient":       recipient,
		"vars":            vars,
		"campaign":        map[string]any{"id": b.Campaign.ID, "name": b.Campaign.Name},
		"unsubscribe_url": b.UnsubscribeURL,
		"locale":          locale,
	}
}

// capWriter enforces the output cap and the render deadline. Liquid has no
// context support, so cancellation is cooperative: the writer is the only
// thing the renderer calls often enough to check.
type capWriter struct {
	ctx    context.Context
	limit  int
	buf    strings.Builder
	writes int
	err    error
}

var _ io.Writer = (*capWriter)(nil)

func (w *capWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes%64 == 0 {
		if err := w.ctx.Err(); err != nil {
			w.err = err
			return 0, err
		}
	}
	if w.limit > 0 && w.buf.Len()+len(p) > w.limit {
		w.err = fmt.Errorf("%w: over %d bytes", ErrOutputTooLarge, w.limit)
		return 0, w.err
	}
	return w.buf.Write(p)
}
