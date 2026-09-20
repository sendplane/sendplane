package render

import "html"

// HTMLMarkerKey marks a binding value as trusted HTML that must not be
// escaped: {"$html": "<b>bold</b>"}. It is the only way raw markup can reach
// the HTML part of a message (ADR-0004).
const HTMLMarkerKey = "$html"

// escapeBindings walks a binding value and HTML-escapes every string it
// contains. Maps, slices and their nested combinations are rebuilt rather than
// mutated, so the caller's data is untouched and two parts of the same message
// can be rendered with different escaping.
//
// A map whose only entry is HTMLMarkerKey with a string value is replaced by
// that string, unescaped.
func escapeBindings(v any) any { return walkBindings(v, true) }

// rawBindings performs the same walk without escaping: it only unwraps the
// {"$html": …} marker. Subject and text parts use it (ADR-0004).
func rawBindings(v any) any { return walkBindings(v, false) }

func walkBindings(v any, escape bool) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if escape {
			return html.EscapeString(t)
		}
		return t
	case map[string]any:
		if raw, ok := htmlMarker(t); ok {
			return raw
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = walkBindings(val, escape)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = walkBindings(val, escape)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = walkBindings(val, escape)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = walkBindings(val, escape)
		}
		return out
	default:
		// Numbers, bools, times and host-provided structs pass through: they
		// have no markup to escape, and a struct is rendered field by field by
		// Liquid, which we cannot rewrite here. Hosts must pass user-supplied
		// text as strings, maps or slices.
		return v
	}
}

// htmlMarker reports whether m is exactly {"$html": "…"} and returns the
// trusted markup. Any other shape (extra keys, non-string value) is treated as
// an ordinary map so that a hostile binding cannot smuggle markup through.
func htmlMarker(m map[string]any) (string, bool) {
	if len(m) != 1 {
		return "", false
	}
	raw, ok := m[HTMLMarkerKey].(string)
	return raw, ok
}
