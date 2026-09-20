package probe

import (
	"bytes"
	"errors"
	"net"
	"net/mail"
	"strings"
	"time"
)

// Field is one header, with its name as it was written.
type Field struct {
	Name  string
	Value string
}

// Headers is a message's header block in wire order. Order matters here in a
// way it does not for most mail code: the Received chain is read bottom-up,
// and "the first Authentication-Results" is a trust decision, so a map would
// throw away exactly the information the probe needs.
type Headers []Field

// Get returns the first value of a header, or "".
func (h Headers) Get(name string) string {
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			return f.Value
		}
	}
	return ""
}

// Values returns every value of a header, topmost first.
func (h Headers) Values(name string) []string {
	var out []string
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value)
		}
	}
	return out
}

// ParseHeaders reads the header block of an RFC 5322 message, unfolding
// continuation lines. Everything from the first empty line on is the body and
// is ignored; a message that is only headers parses too, which is what an IMAP
// BODY.PEEK[HEADER] fetch returns.
func ParseHeaders(raw []byte) Headers {
	var out Headers
	var name string
	var value strings.Builder
	flush := func() {
		if name != "" {
			out = append(out, Field{Name: name, Value: strings.TrimSpace(value.String())})
		}
		name, value = "", strings.Builder{}
	}

	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if name != "" {
				value.WriteByte(' ')
				value.Write(bytes.TrimSpace(line))
			}
			continue
		}
		i := bytes.IndexByte(line, ':')
		if i <= 0 {
			// Not a header line. A leading "From " envelope line from an mbox
			// is the usual cause; skip it rather than swallowing the rest.
			continue
		}
		flush()
		name = string(bytes.TrimSpace(line[:i]))
		value.Write(bytes.TrimSpace(line[i+1:]))
	}
	flush()
	return out
}

// --- Authentication-Results (RFC 8601) ---------------------------------

// AuthMethod is one method=result clause with its properties.
type AuthMethod struct {
	Method string `json:"method"`
	Result string `json:"result"`
	// Reason is the reason=... string, when the MTA gave one.
	Reason string `json:"reason,omitempty"`
	// Comment is the parenthesized text that followed the result. Gmail puts
	// the DMARC policy there ("dmarc=pass (p=NONE sp=NONE dis=NONE)"), which
	// is the only place the receiving side's view of p= is available.
	Comment string `json:"comment,omitempty"`
	// Props are the ptype.property=value pairs, keyed lowercase
	// ("header.d", "header.s", "smtp.mailfrom").
	Props map[string]string `json:"props,omitempty"`
}

// Prop returns a property value, or "".
func (m AuthMethod) Prop(name string) string { return m.Props[strings.ToLower(name)] }

// AuthResults is one parsed Authentication-Results header.
type AuthResults struct {
	AuthServID string       `json:"authserv_id"`
	Version    string       `json:"version,omitempty"`
	Methods    []AuthMethod `json:"methods,omitempty"`
}

// Method returns the named method's clause.
func (a AuthResults) Method(name string) (AuthMethod, bool) {
	for _, m := range a.Methods {
		if strings.EqualFold(m.Method, name) {
			return m, true
		}
	}
	return AuthMethod{}, false
}

// ErrNoAuthServID is returned for a header with no authserv-id, which cannot
// be trusted because there is nothing to match against the mailbox's
// configured value.
var ErrNoAuthServID = errors.New("probe: Authentication-Results has no authserv-id")

// ParseAuthResults parses one Authentication-Results header value.
func ParseAuthResults(value string) (AuthResults, error) {
	segs := splitClauses(value)
	if len(segs) == 0 {
		return AuthResults{}, ErrNoAuthServID
	}
	var out AuthResults
	head := strings.Fields(segs[0].text)
	if len(head) == 0 {
		return AuthResults{}, ErrNoAuthServID
	}
	rest := segs[1:]
	if isMethodClause(head[0]) {
		// Exchange Online omits the authserv-id and starts straight at the
		// first method. RFC 8601 requires one, but refusing to parse the
		// header would mean no verdict at all from Outlook mailboxes, which
		// are the second most important ones to probe. Such a mailbox is
		// configured with an empty ProbeMailbox.AuthServID, and the probe
		// token is what authenticates the message instead.
		rest = segs
	} else {
		out.AuthServID = strings.Trim(head[0], `"`)
		if len(head) > 1 && isDigits(head[1]) {
			out.Version = head[1]
		}
	}

	for _, seg := range rest {
		m, ok := parseAuthMethod(seg)
		if ok {
			out.Methods = append(out.Methods, m)
		}
	}
	return out, nil
}

// TrustedAuthResults returns the Authentication-Results headers whose
// authserv-id matches the mailbox's configured one. ADR-0012 is explicit about
// this: anyone upstream can add an Authentication-Results header, so a verdict
// built from an unauthenticated one is a verdict an attacker writes.
//
// An empty authServID means the mailbox has not been configured with one; all
// headers are then returned and the caller is expected to mark the result as
// less trustworthy.
func TrustedAuthResults(h Headers, authServID string) []AuthResults {
	var out []AuthResults
	for _, v := range h.Values("Authentication-Results") {
		ar, err := ParseAuthResults(v)
		if err != nil {
			continue
		}
		if authServID != "" && !strings.EqualFold(strings.TrimSpace(ar.AuthServID), authServID) {
			continue
		}
		out = append(out, ar)
	}
	return out
}

// clause is one ';'-separated piece of an Authentication-Results value, with
// its comments lifted out.
type clause struct {
	text     string
	comments []string
}

// splitClauses splits on ';' while honouring quoted strings and nested
// comments, and strips the comments out of the text. Splitting on ";" alone
// breaks on a perfectly ordinary Gmail header, whose SPF comment contains one.
func splitClauses(s string) []clause {
	var out []clause
	cur := clause{}
	var b strings.Builder
	inQuote := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if c == '"' {
				inQuote = false
			}
		case c == '"':
			inQuote = true
			b.WriteByte(c)
		case c == '(':
			text, n := readComment(s[i:])
			cur.comments = append(cur.comments, text)
			i += n - 1
			b.WriteByte(' ')
		case c == ';':
			cur.text = b.String()
			out = append(out, cur)
			cur, b = clause{}, strings.Builder{}
		default:
			b.WriteByte(c)
		}
	}
	cur.text = b.String()
	if strings.TrimSpace(cur.text) != "" || len(cur.comments) > 0 {
		out = append(out, cur)
	}
	return out
}

// readComment consumes a balanced parenthesized comment starting at s[0] and
// returns its inner text and how many bytes it spanned.
func readComment(s string) (string, int) {
	depth := 0
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		switch c {
		case '(':
			depth++
			if depth > 1 {
				b.WriteByte(c)
			}
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(b.String()), i + 1
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String()), len(s)
}

// quotedFields splits on whitespace, except inside a quoted string: a
// reason="key not found" is one token, not three.
func quotedFields(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(c)
		case !inQuote && (c == ' ' || c == '\t'):
			flush()
		default:
			b.WriteByte(c)
		}
	}
	flush()
	return out
}

func parseAuthMethod(c clause) (AuthMethod, bool) {
	fields := quotedFields(c.text)
	if len(fields) == 0 {
		return AuthMethod{}, false
	}
	name, result, ok := strings.Cut(fields[0], "=")
	if !ok {
		// "Authentication-Results: mx.example.com; none" — a valid header
		// saying no method ran.
		return AuthMethod{}, false
	}
	m := AuthMethod{
		Method: strings.ToLower(strings.TrimSpace(method(name))),
		Result: strings.ToLower(strings.Trim(result, `"`)),
		Props:  map[string]string{},
	}
	if len(c.comments) > 0 {
		m.Comment = strings.Join(c.comments, " ")
	}
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if k == "reason" {
			m.Reason = v
			continue
		}
		if !strings.Contains(k, ".") {
			continue
		}
		m.Props[k] = v
	}
	if len(m.Props) == 0 {
		m.Props = nil
	}
	return m, m.Method != ""
}

// knownMethods is the RFC 8601 method registry plus the non-standard ones
// receivers actually emit. It exists only to recognize a header whose
// authserv-id is missing; an unknown method inside a well-formed header is
// still parsed.
var knownMethods = map[string]bool{
	"auth": true, "arc": true, "bimi": true, "compauth": true,
	"dkim": true, "dkim-adsp": true, "dkim-atps": true, "dmarc": true,
	"domainkeys": true, "iprev": true, "rrvs": true, "sender-id": true,
	"smime": true, "spf": true, "vbr": true, "none": true,
}

// isMethodClause reports whether a token is "method=result" rather than an
// authserv-id.
func isMethodClause(s string) bool {
	name, _, ok := strings.Cut(s, "=")
	return ok && knownMethods[strings.ToLower(method(name))]
}

// method strips the "/version" an RFC 8601 method may carry
// ("dkim/1=pass").
func method(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// DMARCPolicy digs the p= the receiver applied out of a dmarc clause. It is
// reported in the comment ("(p=NONE sp=QUARANTINE dis=NONE)") by Gmail and
// Outlook and as a property by some others.
func DMARCPolicy(m AuthMethod) string {
	if v := m.Prop("header.p"); v != "" {
		return strings.ToLower(v)
	}
	for _, tok := range strings.FieldsFunc(m.Comment, func(r rune) bool {
		return r == ' ' || r == ',' || r == ';'
	}) {
		if k, v, ok := strings.Cut(tok, "="); ok && strings.EqualFold(k, "p") {
			return strings.ToLower(v)
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// --- Received chain ----------------------------------------------------

// Hop is one parsed Received header.
type Hop struct {
	// From is the HELO/EHLO name the sender announced.
	From string `json:"from,omitempty"`
	// RDNS is the reverse name the receiver looked up, when it recorded one.
	RDNS string `json:"rdns,omitempty"`
	// IP is the address the receiver saw the connection come from.
	IP net.IP `json:"ip,omitempty"`
	// By is the receiving host.
	By string `json:"by,omitempty"`
	// TLS reports whether the hop announced a TLS-protected protocol
	// (ESMTPS/SMTPS) or recorded a cipher.
	TLS bool      `json:"tls"`
	At  time.Time `json:"at,omitempty"`
	Raw string    `json:"-"`
}

// ParseReceived parses one Received header value.
func ParseReceived(value string) Hop {
	h := Hop{Raw: value}

	// The timestamp is what follows the last top-level ';'.
	clauses := splitClauses(value)
	if n := len(clauses); n > 1 {
		if t, err := parseDate(clauses[n-1].text); err == nil {
			h.At = t
		}
	}

	// The address the receiver recorded is the only part of a Received header
	// written from what it observed rather than from what the client claimed.
	h.IP, h.RDNS = observedAddr(value)

	fields := strings.Fields(stripComments(value))
	for i, f := range fields {
		switch strings.ToLower(f) {
		case "from":
			if h.From == "" && i+1 < len(fields) {
				h.From = strings.Trim(fields[i+1], "<>")
			}
		case "by":
			if h.By == "" && i+1 < len(fields) {
				h.By = strings.TrimSuffix(fields[i+1], ".")
			}
		case "with":
			if i+1 < len(fields) && isTLSProtocol(fields[i+1]) {
				h.TLS = true
			}
		}
	}
	if !h.TLS {
		h.TLS = mentionsTLS(value)
	}
	return h
}

// ReceivedChain parses every Received header, oldest first — the reverse of
// the order they appear in, since each MTA prepends its own.
func ReceivedChain(h Headers) []Hop {
	values := h.Values("Received")
	out := make([]Hop, 0, len(values))
	for i := len(values) - 1; i >= 0; i-- {
		out = append(out, ParseReceived(values[i]))
	}
	return out
}

// FirstExternalHop is the hop where the mail crossed into the receiving
// organization: the oldest Received that recorded a public source address.
// That is the one carrying sendplane's own outbound IP, its PTR and whether
// the connection was encrypted (architecture 11.2).
//
// Hops above it are internal relays, whose addresses belong to the receiver.
func FirstExternalHop(h Headers) (Hop, bool) {
	chain := ReceivedChain(h)
	for _, hop := range chain {
		if hop.IP != nil && isPublicIP(hop.IP) {
			return hop, true
		}
	}
	// No public address anywhere: a test MTA on a private network. Fall back
	// to the oldest hop that recorded any address at all.
	for _, hop := range chain {
		if hop.IP != nil {
			return hop, true
		}
	}
	return Hop{}, false
}

// observedAddr finds the source address a receiver recorded. Two shapes cover
// everything in the wild: the bracketed "(rdns [ip])" of Postfix, Gmail and
// Sendmail, and the bare parenthesized "(ip)" of Exchange Online.
func observedAddr(s string) (net.IP, string) {
	if ip, rdns := bracketedAddr(s); ip != nil {
		return ip, rdns
	}
	return commentAddr(s)
}

// commentAddr scans the parenthesized comments for a bare address, taking the
// first one: Exchange writes "from helo (203.0.113.11) by host (2603:...)",
// and the source is the one in front.
func commentAddr(s string) (net.IP, string) {
	for i := 0; i < len(s); i++ {
		if s[i] != '(' {
			continue
		}
		text, n := readComment(s[i:])
		i += n - 1
		var ip net.IP
		var name string
		for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ';'
		}) {
			tok = strings.Trim(tok, "[]")
			if got := net.ParseIP(tok); got != nil && ip == nil {
				ip = got
				continue
			}
			if name == "" && strings.Contains(tok, ".") && !strings.ContainsAny(tok, "=<>@") {
				name = strings.TrimSuffix(tok, ".")
			}
		}
		if ip != nil {
			if strings.EqualFold(name, "unknown") {
				name = ""
			}
			return ip, name
		}
	}
	return nil, ""
}

// bracketedAddr finds the first "[addr]" and the token in front of it, which
// is where a receiver writes "(rdns [ip])".
func bracketedAddr(s string) (net.IP, string) {
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		j := strings.IndexByte(s[i:], ']')
		if j < 0 {
			return nil, ""
		}
		inner := s[i+1 : i+j]
		// Postfix and Exchange write IPv6 as "[IPv6:2001:db8::1]".
		inner = strings.TrimPrefix(inner, "IPv6:")
		inner = strings.TrimPrefix(inner, "ipv6:")
		if ip := net.ParseIP(strings.TrimSpace(inner)); ip != nil {
			return ip, rdnsBefore(s[:i])
		}
		i += j
	}
	return nil, ""
}

// rdnsBefore reads the hostname a receiver wrote just before the bracketed
// address. "unknown" is Postfix's way of saying there was no PTR, and is not
// a hostname.
func rdnsBefore(s string) string {
	s = strings.TrimRight(s, " \t")
	if s == "" || s[len(s)-1] == '(' {
		// "from helo ([203.0.113.5])": the receiver recorded no name at all.
		return ""
	}
	i := strings.LastIndexAny(s, " \t(")
	name := strings.TrimSpace(s[i+1:])
	name = strings.Trim(name, "()")
	name = strings.TrimSuffix(name, ".")
	if name == "" || strings.EqualFold(name, "unknown") || strings.EqualFold(name, "from") {
		return ""
	}
	if strings.ContainsAny(name, "<>@") {
		return ""
	}
	return name
}

func stripComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '(' {
			_, n := readComment(s[i:])
			i += n - 1
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isTLSProtocol reports whether a "with <protocol>" token means the hop was
// encrypted. ESMTPS and its authenticated variants are the ones RFC 3848
// defines; SMTPS and HTTPS show up from Exchange and web submission.
func isTLSProtocol(s string) bool {
	switch strings.ToUpper(strings.TrimSuffix(s, ";")) {
	case "ESMTPS", "ESMTPSA", "ESMTPTLS", "SMTPS", "HTTPS", "ESMTPTLSA":
		return true
	}
	return false
}

// mentionsTLS catches the receivers that record the cipher instead of using a
// TLS-flavoured protocol token (Postfix "(using TLSv1.3 with cipher ...)",
// Exchange "(version=TLS1_2, cipher=...)").
func mentionsTLS(s string) bool {
	l := strings.ToLower(s)
	for _, needle := range []string{"using tls", "version=tls", "with tls", "(tls1", "cipher="} {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// isPublicIP reports whether an address could be sendplane's own outbound
// address as the outside world sees it.
func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

// parseDate parses a date-time, tolerating the trailing "(PDT)" style comment
// most MTAs append.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := mail.ParseDate(s); err == nil {
		return t, nil
	}
	return mail.ParseDate(strings.TrimSpace(stripComments(s)))
}

// --- latency -----------------------------------------------------------

// Latency is how long the mail took to arrive: from when it was submitted
// (the Date header, or, failing that, the oldest Received) to when the probe
// mailbox holds it.
//
// receivedAt is the mailbox's own timestamp (IMAP INTERNALDATE); a zero value
// falls back to the newest Received, which is the receiving MTA's clock.
// Clock skew between two organizations can make that negative, and a negative
// latency is not information, so it is reported as zero.
func Latency(h Headers, receivedAt time.Time) (time.Duration, bool) {
	chain := ReceivedChain(h)

	var sent time.Time
	if t, err := parseDate(h.Get("Date")); err == nil {
		sent = t
	} else if len(chain) > 0 {
		sent = chain[0].At
	}
	if sent.IsZero() {
		return 0, false
	}

	got := receivedAt
	if got.IsZero() {
		for i := len(chain) - 1; i >= 0; i-- {
			if !chain[i].At.IsZero() {
				got = chain[i].At
				break
			}
		}
	}
	if got.IsZero() {
		return 0, false
	}
	d := got.Sub(sent)
	if d < 0 {
		d = 0
	}
	return d, true
}
