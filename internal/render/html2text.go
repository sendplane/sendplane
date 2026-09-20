package render

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// blockTags end the current text line when they open or close.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"body": true, "center": true, "dd": true, "div": true, "dl": true,
	"dt": true, "fieldset": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hr": true, "li": true, "main": true, "nav": true,
	"ol": true, "p": true, "pre": true, "section": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"tr": true, "ul": true,
}

// skipTags and their contents never appear in the text part.
var skipTags = map[string]bool{
	"head": true, "script": true, "style": true, "title": true,
	"noscript": true, "template": true, "mj-style": true,
}

// HTMLToText derives the plain-text part of a message from its HTML part. It
// runs at publish time on HTML that still contains Liquid, so it must preserve
// Liquid tags verbatim and in document order.
//
// It is written against the HTML tokenizer rather than a tree, and rather than
// an off-the-shelf converter, because tree-based converters foster-parent the
// text that the <mj-raw> convention leaves between table rows: they move
// `{% if %}` and `{% endif %}` out of the table and away from the content they
// guard, which silently breaks the rendered text part. See README.
func HTMLToText(htmlSrc string) string {
	t := &textifier{}
	z := html.NewTokenizer(strings.NewReader(htmlSrc))
	skip := 0
	var hrefs []string
	for {
		switch z.Next() {
		case html.ErrorToken:
			return t.String()
		case html.TextToken:
			if skip == 0 {
				t.text(string(z.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			name := tok.Data
			if skipTags[name] {
				if tok.Type == html.StartTagToken {
					skip++
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case name == "br":
				t.endLine()
			case name == "a":
				href := strings.TrimSpace(attrValue(tok.Attr, "href"))
				if tok.Type == html.StartTagToken {
					hrefs = append(hrefs, href)
				}
			case blockTags[name]:
				t.endLine()
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipTags[tag] {
				if skip > 0 {
					skip--
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case tag == "a":
				if n := len(hrefs); n > 0 {
					href := hrefs[n-1]
					hrefs = hrefs[:n-1]
					t.link(href)
				}
			case blockTags[tag]:
				t.endLine()
			}
		}
	}
}

// controlTags are the Liquid tags that produce no output of their own, so a
// line containing only one of them is layout scaffolding rather than content.
var controlTags = map[string]bool{
	"if": true, "elsif": true, "else": true, "endif": true,
	"unless": true, "endunless": true, "for": true, "endfor": true,
	"case": true, "when": true, "endcase": true, "tablerow": true,
	"endtablerow": true, "assign": true, "capture": true, "endcapture": true,
	"break": true, "continue": true, "comment": true, "endcomment": true,
	"cycle": true,
}

func isControlLine(line string) bool {
	if !strings.HasPrefix(line, "{%") || !strings.HasSuffix(line, "%}") {
		return false
	}
	inner := strings.TrimSpace(strings.Trim(strings.TrimSuffix(strings.TrimPrefix(line, "{%"), "%}"), "-"))
	name, _, _ := strings.Cut(inner, " ")
	return controlTags[strings.TrimSpace(name)]
}

// textifier accumulates lines, collapsing runs of whitespace and of blank
// lines the way a mail client would.
type textifier struct {
	out  strings.Builder
	line strings.Builder
	// blanks counts consecutive empty lines already written.
	blanks int
	// wrote is true once any line has been emitted.
	wrote bool
	// lastWasControl is true when the previous line was a bare Liquid tag.
	lastWasControl bool
}

func (t *textifier) text(s string) {
	var b strings.Builder
	b.Grow(len(s))
	space := strings.HasSuffix(t.line.String(), " ") || t.line.Len() == 0
	for _, r := range s {
		if r == ' ' {
			r = ' '
		}
		if unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
				space = true
			}
			continue
		}
		space = false
		b.WriteRune(r)
	}
	t.line.WriteString(b.String())
}

func (t *textifier) link(href string) {
	if href == "" || strings.HasPrefix(href, "#") {
		return
	}
	cur := strings.TrimSpace(t.line.String())
	if strings.HasSuffix(cur, href) {
		return
	}
	if !strings.HasSuffix(t.line.String(), " ") && t.line.Len() > 0 {
		t.line.WriteString(" ")
	}
	t.line.WriteString("( " + href + " )")
}

func (t *textifier) endLine() {
	line := strings.TrimSpace(t.line.String())
	t.line.Reset()
	if line == "" {
		if t.wrote {
			t.blanks++
		}
		return
	}
	// A line that is nothing but a Liquid tag is control flow lifted out of
	// <mj-raw>. Keeping it tight against the content it guards stops the
	// rendered text from growing a run of blank lines where the tag used to
	// be.
	control := isControlLine(line)
	if t.wrote {
		t.out.WriteString("\n")
		if t.blanks > 0 && !control && !t.lastWasControl {
			t.out.WriteString("\n")
		}
	}
	t.blanks = 0
	t.lastWasControl = control
	t.out.WriteString(line)
	t.wrote = true
}

func (t *textifier) String() string {
	t.endLine()
	return t.out.String()
}
