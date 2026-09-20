package render

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The link_no a rewriter sees at send time must index MessageVersion.Links,
// which was extracted at publish time (architecture 9.2).
func TestRewriteLinksMatchesPublishedOrder(t *testing.T) {
	v := publishFixture(t)
	p, err := NewRenderer().Prepare(v, "en")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	out, _, err := p.Render(context.Background(), fixtureBindings())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var seen []string
	rewritten, err := RewriteLinks(out.HTML, func(linkNo int, href string) string {
		seen = append(seen, href)
		return fmt.Sprintf("https://t.example.com/t/c/tok%d?u=%s", linkNo, href)
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(seen) != len(v.Links) {
		t.Fatalf("rewrote %d links, version has %d: %#v vs %#v", len(seen), len(v.Links), seen, v.Links)
	}
	// Link 0 is dynamic, so only its rendered form can be compared.
	if seen[0] != "https://example.com/orders/A-1001" {
		t.Errorf("link 0 = %q", seen[0])
	}
	if seen[1] != v.Links[1] {
		t.Errorf("link 1 = %q, want %q", seen[1], v.Links[1])
	}
	for i := range seen {
		if !strings.Contains(rewritten, fmt.Sprintf("t/c/tok%d", i)) {
			t.Errorf("link %d was not rewritten", i)
		}
	}
	// The unsubscribe link keeps its own URL: it carries data-sp-track="off".
	if !strings.Contains(rewritten, fixtureBindings().UnsubscribeURL) {
		t.Error("unsubscribe URL was rewritten")
	}
	if strings.Contains(rewritten, "u=mailto:") || strings.Contains(rewritten, "u=#top") {
		t.Error("a skipped link was rewritten")
	}
}

func TestExtractLinksSkipRules(t *testing.T) {
	src := `<html><body>
<a href="https://example.com/1">one</a>
<a href="HTTP://example.com/2">two</a>
<a href="mailto:a@example.com">mail</a>
<a href="tel:+1234">tel</a>
<a href="#anchor">anchor</a>
<a href="">empty</a>
<a name="bookmark">no href</a>
<a href="https://example.com/3" data-sp-track="off">opted out</a>
<a href="ftp://example.com/4">ftp</a>
<a href="{{ unsubscribe_url }}">unsub</a>
<a href="https://example.com/5">five</a>
</body></html>`
	got, err := ExtractLinks(src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want := []string{
		"https://example.com/1",
		"HTTP://example.com/2",
		"{{ unsubscribe_url }}",
		"https://example.com/5",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %#v, want %#v", got, want)
	}

	marked, err := markUnsubscribeAnchors(src)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err = ExtractLinks(marked)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	want = []string{"https://example.com/1", "HTTP://example.com/2", "https://example.com/5"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("after marking: got %#v, want %#v", got, want)
	}
}

// Anything that is not a rewritten anchor must come back byte for byte,
// including MSO conditional comments and mj-raw Liquid between table rows.
func TestMapAnchorsIsLossless(t *testing.T) {
	v := publishFixture(t)
	for _, src := range []string{
		v.HTMLTpl,
		`<!--[if mso]><table><tr><td>x</td></tr></table><![endif]--><p a=1 B='2'>t</p>`,
		"<table><tr><td>a</td></tr>{% if x %}<tr><td>b</td></tr>{% endif %}</table>",
		"plain text, no tags",
		"",
	} {
		got, err := RewriteLinks(src, func(_ int, href string) string { return href })
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if got != src {
			t.Errorf("not lossless at byte %d:\n got %q\nwant %q", firstDiff(got, src), excerpt(got, firstDiff(got, src)), excerpt(src, firstDiff(got, src)))
		}
	}
}

func TestRewriteLinksNilAndUnchanged(t *testing.T) {
	src := `<a href="https://example.com/">x</a>`
	got, err := RewriteLinks(src, nil)
	if err != nil || got != src {
		t.Errorf("nil rewriter: %q %v", got, err)
	}
	got, err = RewriteLinks(src, func(_ int, href string) string { return href })
	if err != nil || got != src {
		t.Errorf("identity rewriter: %q %v", got, err)
	}
}

func TestRewriteLinksKeepsOtherAttributes(t *testing.T) {
	src := `<a class="btn" href="https://example.com/" target="_blank">x</a>`
	got, err := RewriteLinks(src, func(int, string) string { return "https://t.example.com/t/c/x?u=y" })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="btn"`, `target="_blank"`, `href="https://t.example.com/t/c/x?u=y"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
}

func TestInsertPixel(t *testing.T) {
	const url = "https://t.example.com/t/o/tok?a=1&b=2"
	got := InsertPixel("<html><body><p>x</p></body></html>", url)
	if !strings.Contains(got, `<p>x</p><img src="https://t.example.com/t/o/tok?a=1&amp;b=2"`) {
		t.Errorf("pixel not inserted before </body>: %q", got)
	}
	if !strings.Contains(got, `width="1" height="1"`) || !strings.Contains(got, "display:none") {
		t.Errorf("pixel attributes: %q", got)
	}

	got = InsertPixel("<p>x</p>", url)
	if !strings.HasPrefix(got, "<p>x</p><img ") {
		t.Errorf("pixel not appended: %q", got)
	}
	got = InsertPixel("<html><BODY><p>x</p></BODY></html>", url)
	if !strings.Contains(got, "<img ") || !strings.HasSuffix(got, "</BODY></html>") {
		t.Errorf("uppercase body: %q", got)
	}
	if got := InsertPixel("<p>x</p>", ""); got != "<p>x</p>" {
		t.Errorf("empty pixel URL changed the document: %q", got)
	}
}

func TestHTMLToText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"paragraphs", "<p>one</p><p>two</p>", "one\n\ntwo"},
		{"br", "a<br>b", "a\nb"},
		{"link", `<p><a href="https://example.com/">click</a></p>`, "click ( https://example.com/ )"},
		{"link equals text", `<a href="https://example.com/">https://example.com/</a>`, "https://example.com/"},
		{"skips style and head", "<html><head><title>t</title><style>p{color:red}</style></head><body><p>x</p></body></html>", "x"},
		{"entities", "<p>a &amp; b &nbsp; c</p>", "a & b c"},
		{"collapses whitespace", "<p>a\n\n   b</p>", "a b"},
		{"comments dropped", "<!--[if mso]><p>ms</p><![endif]--><p>x</p>", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HTMLToText(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func excerpt(s string, at int) string {
	start := max(at-40, 0)
	end := min(at+40, len(s))
	return s[start:end]
}
