package render

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// This file holds the HTML post-processing hooks of architecture 9.2. They run
// *after* Liquid has been rendered; signing the tracking tokens is the
// caller's job, which is why RewriteLinks takes a rewriter function.

// TrackOffAttr on an <a> opts the link out of click tracking.
const TrackOffAttr = "data-sp-track"

// mapAnchors walks src with the HTML tokenizer and lets fn replace the raw
// text of every <a> start tag. Everything outside those tags is copied
// verbatim, so the document is not re-serialised: MSO conditional comments,
// whitespace and attribute spelling all survive, which matters for email HTML.
//
// The tokenizer (not the tree builder) is deliberate: parsing MJML output into
// a tree and printing it back moves text nodes that sit between table rows out
// of the table ("foster parenting"), which would reorder the Liquid control
// flow that the <mj-raw> convention puts there (ADR-0009).
func mapAnchors(src string, fn func(attrs []html.Attribute, raw string) (string, bool)) (string, error) {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	b.Grow(len(src) + 64)
	pos := 0
	for {
		tt := z.Next()
		n := len(z.Raw()) // must be read before Token(), which unescapes in place
		start := pos
		pos += n
		if tt == html.ErrorToken {
			if err := z.Err(); err != nil && !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("render: html scan: %w", err)
			}
			break
		}
		if pos > len(src) {
			return "", fmt.Errorf("render: html scan: offset %d past input %d", pos, len(src))
		}
		if tt != html.StartTagToken {
			b.WriteString(src[start:pos])
			continue
		}
		raw := src[start:pos]
		if !isAnchorTag(raw) {
			b.WriteString(raw)
			continue
		}
		// Token() consumes the tokenizer's attribute state, so it is called
		// once, only for anchors, and only after the cheap prefilter above.
		tok := z.Token()
		if tok.Data != "a" {
			b.WriteString(raw)
			continue
		}
		replacement, replaced := fn(tok.Attr, raw)
		if replaced {
			b.WriteString(replacement)
		} else {
			b.WriteString(raw)
		}
	}
	if pos < len(src) {
		b.WriteString(src[pos:])
	}
	return b.String(), nil
}

// isAnchorTag reports whether a raw start tag is <a …>, without parsing it.
func isAnchorTag(raw string) bool {
	if len(raw) < 3 || raw[0] != '<' {
		return false
	}
	if raw[1] != 'a' && raw[1] != 'A' {
		return false
	}
	switch raw[2] {
	case ' ', '\t', '\n', '\r', '\f', '/', '>':
		return true
	}
	return false
}

func attrValue(attrs []html.Attribute, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(attrs []html.Attribute, key, val string) []html.Attribute {
	out := make([]html.Attribute, len(attrs))
	copy(out, attrs)
	for i := range out {
		if out[i].Key == key {
			out[i].Val = val
			return out
		}
	}
	return append(out, html.Attribute{Key: key, Val: val})
}

// renderStartTag prints an <a> tag from its attributes. Only anchors we
// actually change go through here.
func renderStartTag(name string, attrs []html.Attribute) string {
	var b strings.Builder
	b.WriteString("<")
	b.WriteString(name)
	for _, a := range attrs {
		b.WriteString(" ")
		if a.Namespace != "" {
			b.WriteString(a.Namespace)
			b.WriteString(":")
		}
		b.WriteString(a.Key)
		b.WriteString(`="`)
		b.WriteString(html.EscapeString(a.Val))
		b.WriteString(`"`)
	}
	b.WriteString(">")
	return b.String()
}

// isTrackable implements the single skip rule shared by link extraction at
// publish time and link rewriting at send time, so that the index a rewriter
// sees is the index into MessageVersion.Links (architecture 9.2).
//
// Skipped: empty hrefs, fragments, mailto:/tel: (any non-http scheme), and
// anything carrying data-sp-track="off". A href that still contains Liquid is
// assumed to render to http(s); see README for that caveat.
func isTrackable(attrs []html.Attribute) bool {
	if strings.EqualFold(strings.TrimSpace(attrValue(attrs, TrackOffAttr)), "off") {
		return false
	}
	href := strings.TrimSpace(attrValue(attrs, "href"))
	if href == "" || strings.HasPrefix(href, "#") {
		return false
	}
	lower := strings.ToLower(href)
	for _, scheme := range []string{"mailto:", "tel:", "sms:", "ftp:", "javascript:", "data:"} {
		if strings.HasPrefix(lower, scheme) {
			return false
		}
	}
	if strings.Contains(href, "{{") || strings.Contains(href, "{%") {
		return true
	}
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// ExtractLinks returns the trackable hrefs of an HTML document in document
// order. The index of a href is its link_no (architecture 9.2).
func ExtractLinks(htmlSrc string) ([]string, error) {
	var links []string
	_, err := mapAnchors(htmlSrc, func(attrs []html.Attribute, _ string) (string, bool) {
		if isTrackable(attrs) {
			links = append(links, strings.TrimSpace(attrValue(attrs, "href")))
		}
		return "", false
	})
	if err != nil {
		return nil, err
	}
	return links, nil
}

// RewriteLinks replaces the href of every trackable anchor with the result of
// rewrite(linkNo, href). linkNo counts trackable anchors in document order and
// therefore lines up with MessageVersion.Links. Returning the href unchanged
// leaves the anchor alone.
//
// It runs on rendered HTML, after Liquid; the caller signs the tracking token
// (architecture 9.1) and builds the redirect URL.
func RewriteLinks(htmlSrc string, rewrite func(linkNo int, href string) string) (string, error) {
	if rewrite == nil {
		return htmlSrc, nil
	}
	n := 0
	return mapAnchors(htmlSrc, func(attrs []html.Attribute, _ string) (string, bool) {
		if !isTrackable(attrs) {
			return "", false
		}
		href := strings.TrimSpace(attrValue(attrs, "href"))
		linkNo := n
		n++
		next := rewrite(linkNo, href)
		if next == href {
			return "", false
		}
		return renderStartTag("a", setAttr(attrs, "href", next)), true
	})
}

// markUnsubscribeAnchors tags every anchor whose href interpolates
// unsubscribe_url with data-sp-track="off". Publish does this so that the
// unsubscribe link is excluded from Links *and* is still excluded at send
// time, when the href has already rendered into an ordinary https URL and
// would otherwise shift every link_no after it (architecture 9.2).
func markUnsubscribeAnchors(htmlSrc string) (string, error) {
	return mapAnchors(htmlSrc, func(attrs []html.Attribute, _ string) (string, bool) {
		href := attrValue(attrs, "href")
		if !strings.Contains(href, "unsubscribe_url") {
			return "", false
		}
		if strings.EqualFold(attrValue(attrs, TrackOffAttr), "off") {
			return "", false
		}
		return renderStartTag("a", setAttr(attrs, TrackOffAttr, "off")), true
	})
}

// InsertPixel inserts the open-tracking pixel before </body>, or appends it if
// the document has no body close tag (architecture 9.2). An empty pixelURL
// returns the document unchanged.
func InsertPixel(htmlSrc, pixelURL string) string {
	if pixelURL == "" {
		return htmlSrc
	}
	img := `<img src="` + html.EscapeString(pixelURL) +
		`" width="1" height="1" alt="" border="0" style="display:none;width:1px;height:1px;border:0;" />`
	if i := lastIndexFold(htmlSrc, "</body>"); i >= 0 {
		return htmlSrc[:i] + img + htmlSrc[i:]
	}
	return htmlSrc + img
}

// lastIndexFold finds the last case-insensitive occurrence of sub. sub must
// start with a byte that has no case of its own; the only caller passes
// "</body>".
func lastIndexFold(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i] != sub[0] {
			continue
		}
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}
