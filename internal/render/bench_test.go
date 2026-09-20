package render

import (
	"context"
	"testing"
)

// BenchmarkMJMLCompile measures the publish-time cost that ADR-0009 moved off
// the per-recipient path. The doc's target is under 500 ms per compile.
func BenchmarkMJMLCompile(b *testing.B) {
	tpl, layout := fixture(b)
	merged, err := mergeLayout(layout.Body, tpl.Body)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := compileMJML(ctx, merged, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRender is the per-recipient cost: Liquid only, no MJML.
func BenchmarkRender(b *testing.B) {

	v := publishFixture(b)
	p, err := NewRenderer().Prepare(v, "ko-KR")
	if err != nil {
		b.Fatal(err)
	}
	bindings := fixtureBindings()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := p.Render(ctx, bindings); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrepareCached(b *testing.B) {

	v := publishFixture(b)
	r := NewRenderer()
	if _, err := r.Prepare(v, "ko-KR"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Prepare(v, "ko-KR"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteLinks(b *testing.B) {

	v := publishFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RewriteLinks(v.HTMLTpl, func(_ int, href string) string { return "https://t/" + href }); err != nil {
			b.Fatal(err)
		}
	}
}
