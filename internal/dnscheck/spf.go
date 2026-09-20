package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// MaxSPFLookups is the RFC 7208 4.6.4 limit on DNS-querying mechanisms.
// Exceeding it is a permerror, which most receivers treat as "no SPF at all" —
// the single most common way a working SPF record rots as includes are added.
const MaxSPFLookups = 10

// termIgnored is the parsed name of a modifier RFC 7208 6 says to ignore. It
// is not a valid mechanism name, so it cannot collide with a real term.
const termIgnored = "-ignored-"

// maxSPFDepth bounds include/redirect recursion independently of the lookup
// budget, so a self-referencing record terminates even while the budget is
// being consumed by something else.
const maxSPFDepth = 12

// SPF evaluation results, as RFC 7208 names them.
const (
	spfPass      = "pass"
	spfFail      = "fail"
	spfSoftfail  = "softfail"
	spfNeutral   = "neutral"
	spfNone      = "none"
	spfPermerror = "permerror"
	spfTemperror = "temperror"
)

// SPF checks the sending domain's SPF record: that it exists and parses, that
// it stays inside the ten-lookup budget, and — when the probe observed the
// outbound IP — that the record actually authorizes it (architecture 11.3).
//
// observedIP may be nil, which is the "no probe has run yet" case: the record
// is then only checked for existence and syntax.
func (c *Checker) SPF(ctx context.Context, domain string, observedIP net.IP) Result {
	details := map[string]string{"domain": domain}
	if observedIP != nil {
		details["ip"] = observedIP.String()
	}

	e := &spfEval{r: c.r, seen: map[string]spfLookup{}}
	res, err := e.check(ctx, domain, observedIP, 0)
	details["lookups"] = strconv.Itoa(e.lookups)
	details["result"] = res
	if e.record != "" {
		details["record"] = e.record
	}
	if e.matched != "" {
		details["matched"] = e.matched
	}
	if err != nil {
		details["error"] = err.Error()
	}

	switch res {
	case spfNone:
		return result("spf", store.HealthRed, "SPF 레코드가 없습니다", details)
	case spfPermerror:
		if e.lookups > MaxSPFLookups {
			return result("spf", store.HealthRed,
				fmt.Sprintf("SPF DNS 조회가 %d회로 한도(%d)를 넘었습니다", e.lookups, MaxSPFLookups), details)
		}
		return result("spf", store.HealthRed, "SPF 레코드를 해석할 수 없습니다(permerror)", details)
	case spfTemperror:
		return result("spf", store.HealthYellow, "SPF 조회가 일시적으로 실패했습니다(temperror)", details)
	}

	if observedIP == nil {
		if !e.hasAll {
			return result("spf", store.HealthYellow,
				"SPF 레코드에 all 메커니즘이 없습니다", details)
		}
		return result("spf", store.HealthGreen,
			fmt.Sprintf("SPF 레코드 정상(조회 %d/%d). 관측된 발신 IP가 없어 인가 여부는 확인하지 않았습니다",
				e.lookups, MaxSPFLookups), details)
	}

	switch res {
	case spfPass:
		return result("spf", store.HealthGreen,
			fmt.Sprintf("%s는 SPF에 인가되어 있습니다(%s)", observedIP, e.matched), details)
	case spfSoftfail:
		return result("spf", store.HealthYellow,
			fmt.Sprintf("%s가 SPF에 없어 softfail(~all)입니다", observedIP), details)
	case spfNeutral:
		return result("spf", store.HealthYellow,
			fmt.Sprintf("%s에 대한 SPF 판정이 neutral입니다", observedIP), details)
	default: // spfFail
		return result("spf", store.HealthRed,
			fmt.Sprintf("IP %s가 SPF에 없습니다", observedIP), details)
	}
}

// spfLookup memoizes one name's TXT answer within a single evaluation: a
// record that includes the same provider twice must still only pay for it
// once in wall-clock terms, even though RFC 7208 charges it twice against the
// budget.
type spfLookup struct {
	records []string
	err     error
}

type spfEval struct {
	r       Resolver
	lookups int
	seen    map[string]spfLookup
	// record is the top-level record, kept for the details map.
	record string
	// matched names the mechanism that decided the result.
	matched string
	// hasAll reports whether the top-level record ended with all or redirect.
	hasAll bool
	depth  int
}

// check is RFC 7208's check_host() reduced to what a diagnostic needs: no
// macro expansion (sendplane does not publish macros and a record that uses
// them is reported as evaluated best-effort), and ptr treated as never
// matching.
func (e *spfEval) check(ctx context.Context, domain string, ip net.IP, depth int) (string, error) {
	if depth > maxSPFDepth {
		return spfPermerror, errors.New("include/redirect가 너무 깊습니다")
	}
	txts, err := e.txt(ctx, domain)
	if err != nil {
		if errors.Is(err, ErrNoRecord) {
			return spfNone, nil
		}
		return spfTemperror, err
	}
	var records []string
	for _, t := range txts {
		if isSPFRecord(t) {
			records = append(records, t)
		}
	}
	switch len(records) {
	case 0:
		return spfNone, nil
	case 1:
	default:
		return spfPermerror, fmt.Errorf("SPF 레코드가 %d개입니다", len(records))
	}
	record := records[0]
	if depth == 0 {
		e.record = record
	}

	terms, err := parseSPF(record)
	if err != nil {
		return spfPermerror, err
	}

	var redirect string
	for _, t := range terms {
		switch t.name {
		case "redirect":
			redirect = t.value
			continue
		case "exp", termIgnored:
			continue
		}
		if depth == 0 && t.name == "all" {
			e.hasAll = true
		}

		matched, res, err := e.match(ctx, t, domain, ip, depth)
		if err != nil {
			return res, err
		}
		if res != "" && !matched {
			// An include that hit a hard error decides the whole evaluation.
			return res, nil
		}
		if !matched {
			continue
		}
		if depth == 0 || e.matched == "" {
			e.matched = t.String()
		}
		return qualifierResult(t.qualifier), nil
	}

	if redirect != "" {
		if depth == 0 {
			e.hasAll = true
		}
		if !e.charge() {
			return spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		res, err := e.check(ctx, redirect, ip, depth+1)
		if res == spfNone {
			// RFC 7208 6.1: a redirect to a domain with no record is a
			// permerror, not "no policy".
			return spfPermerror, fmt.Errorf("redirect 대상 %s에 SPF 레코드가 없습니다", redirect)
		}
		if e.matched == "" {
			e.matched = "redirect:" + redirect
		}
		return res, err
	}
	return spfNeutral, nil
}

// match evaluates one mechanism. It returns (matched, hardResult, err): a
// non-empty hardResult with matched=false is an include that produced
// permerror or temperror and ends the evaluation.
func (e *spfEval) match(ctx context.Context, t spfTerm, domain string, ip net.IP, depth int) (bool, string, error) {
	switch t.name {
	case "all":
		return true, "", nil

	case "ip4", "ip6":
		if ip == nil {
			return false, "", nil
		}
		ok, err := matchCIDR(t.value, ip)
		if err != nil {
			return false, spfPermerror, err
		}
		return ok, "", nil

	case "a":
		if !e.charge() {
			return false, spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		target := t.target(domain)
		if ip == nil {
			return false, "", nil
		}
		ips, err := e.addrs(ctx, target, ip)
		if err != nil {
			return false, "", nil
		}
		return containsIP(ips, ip, t.prefix(ip)), "", nil

	case "mx":
		if !e.charge() {
			return false, spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		target := t.target(domain)
		if ip == nil {
			return false, "", nil
		}
		mxs, err := e.r.MX(ctx, target)
		if err != nil {
			return false, "", nil
		}
		// RFC 7208 4.6.4: the MX hosts themselves are capped at ten and do not
		// each cost a lookup from the budget.
		if len(mxs) > MaxSPFLookups {
			mxs = mxs[:MaxSPFLookups]
		}
		for _, mx := range mxs {
			ips, err := e.addrs(ctx, mx.Host, ip)
			if err != nil {
				continue
			}
			if containsIP(ips, ip, t.prefix(ip)) {
				return true, "", nil
			}
		}
		return false, "", nil

	case "include":
		if !e.charge() {
			return false, spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		res, err := e.check(ctx, t.value, ip, depth+1)
		switch res {
		case spfPass:
			return true, "", nil
		case spfTemperror:
			return false, spfTemperror, err
		case spfPermerror, spfNone:
			return false, spfPermerror, fmt.Errorf("include:%s → %s", t.value, res)
		default:
			return false, "", nil
		}

	case "exists":
		if !e.charge() {
			return false, spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		if _, err := e.r.A(ctx, t.value); err != nil {
			return false, "", nil
		}
		return true, "", nil

	case "ptr":
		// Deprecated by RFC 7208 5.5 and ignored by many receivers. It still
		// costs a lookup, which is the part that matters for the budget.
		if !e.charge() {
			return false, spfPermerror, fmt.Errorf("DNS 조회 %d회 > %d", e.lookups, MaxSPFLookups)
		}
		return false, "", nil
	}
	return false, spfPermerror, fmt.Errorf("알 수 없는 메커니즘 %q", t.name)
}

// charge consumes one lookup from the RFC 7208 budget and reports whether the
// evaluation may continue.
func (e *spfEval) charge() bool {
	e.lookups++
	return e.lookups <= MaxSPFLookups
}

func (e *spfEval) txt(ctx context.Context, name string) ([]string, error) {
	key := strings.ToLower(name)
	if got, ok := e.seen[key]; ok {
		return got.records, got.err
	}
	records, err := e.r.TXT(ctx, name)
	e.seen[key] = spfLookup{records: records, err: err}
	return records, err
}

// addrs resolves the address family of ip only: an a mechanism matched against
// an IPv4 connection never needs the AAAA set.
func (e *spfEval) addrs(ctx context.Context, name string, ip net.IP) ([]net.IP, error) {
	if ip.To4() != nil {
		return e.r.A(ctx, name)
	}
	return e.r.AAAA(ctx, name)
}

// spfTerm is one parsed mechanism or modifier.
type spfTerm struct {
	qualifier byte
	name      string
	value     string
	// cidr4 and cidr6 are -1 when the term carried no prefix length.
	cidr4 int
	cidr6 int
}

func (t spfTerm) String() string {
	s := t.name
	if t.value != "" && t.name != "a" && t.name != "mx" {
		s += ":" + t.value
	}
	if t.qualifier != '+' {
		s = string(t.qualifier) + s
	}
	return s
}

// target is the domain an a or mx mechanism applies to: its own argument, or
// the domain being evaluated.
func (t spfTerm) target(domain string) string {
	if t.value != "" {
		return t.value
	}
	return domain
}

// prefix is the CIDR length to compare with, defaulting to a host match.
func (t spfTerm) prefix(ip net.IP) int {
	if ip.To4() != nil {
		if t.cidr4 >= 0 {
			return t.cidr4
		}
		return 32
	}
	if t.cidr6 >= 0 {
		return t.cidr6
	}
	return 128
}

func isSPFRecord(s string) bool {
	if len(s) < 6 {
		return false
	}
	if !strings.EqualFold(s[:6], "v=spf1") {
		return false
	}
	return len(s) == 6 || s[6] == ' ' || s[6] == '\t'
}

// parseSPF splits a record into terms. It is deliberately strict: a record
// this cannot parse is a record a receiver may also reject, and reporting
// permerror is the whole point of the check.
func parseSPF(record string) ([]spfTerm, error) {
	fields := strings.Fields(record)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "v=spf1") {
		return nil, errors.New("v=spf1로 시작하지 않습니다")
	}
	var out []spfTerm
	for _, f := range fields[1:] {
		t, err := parseSPFTerm(f)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func parseSPFTerm(f string) (spfTerm, error) {
	t := spfTerm{qualifier: '+', cidr4: -1, cidr6: -1}

	// Modifiers are name=value and carry no qualifier.
	if i := strings.IndexByte(f, '='); i > 0 && !strings.ContainsAny(f[:i], ":/") {
		t.name = strings.ToLower(f[:i])
		t.value = f[i+1:]
		switch t.name {
		case "redirect", "exp":
			if t.value == "" {
				return t, fmt.Errorf("%s= 값이 비어 있습니다", t.name)
			}
			return t, nil
		default:
			// Unknown modifiers must be ignored (RFC 7208 6).
			t.name = termIgnored
			t.value = ""
			return t, nil
		}
	}

	switch f[0] {
	case '+', '-', '~', '?':
		t.qualifier = f[0]
		f = f[1:]
	}
	if f == "" {
		return t, errors.New("빈 메커니즘")
	}

	// The name/value split is on the first colon, so that an IPv6 argument's
	// own colons stay in the value.
	name, rest := f, ""
	if i := strings.IndexByte(f, ':'); i >= 0 {
		name, rest = f[:i], f[i+1:]
	}
	// a, mx and ptr may carry a prefix length with no argument at all ("a/24"),
	// which would otherwise be read as a mechanism named "a/24".
	cidrPart := ""
	if rest == "" {
		if i := strings.IndexByte(name, '/'); i >= 0 {
			cidrPart, name = name[i:], name[:i]
		}
	}
	t.name = strings.ToLower(name)
	if cidrPart != "" && t.name != "a" && t.name != "mx" && t.name != "ptr" {
		return t, fmt.Errorf("%s는 프리픽스 길이를 받지 않습니다", t.name)
	}

	switch t.name {
	case "ip4", "ip6":
		if rest == "" {
			return t, fmt.Errorf("%s: 주소가 없습니다", t.name)
		}
		// Validated here rather than at match time: a malformed address is a
		// permerror even for the pass where no IP was observed.
		if _, err := matchCIDR(rest, net.IPv4zero); err != nil {
			return t, err
		}
		t.value = rest
		return t, nil
	case "all":
		if rest != "" {
			return t, errors.New("all은 인자를 받지 않습니다")
		}
		return t, nil
	case "include", "exists":
		if rest == "" {
			return t, fmt.Errorf("%s: 도메인이 없습니다", t.name)
		}
		t.value = rest
		return t, nil
	case "a", "mx", "ptr":
		v := rest
		if i := strings.IndexByte(v, '/'); i >= 0 {
			cidrPart, v = v[i:], v[:i]
		}
		if cidrPart != "" {
			cidrs, err := parseDualCIDR(cidrPart)
			if err != nil {
				return t, err
			}
			t.cidr4, t.cidr6 = cidrs[0], cidrs[1]
		}
		t.value = v
		return t, nil
	}
	return t, fmt.Errorf("알 수 없는 메커니즘 %q", t.name)
}

// parseDualCIDR reads "/24", "//64" or "/24//64".
func parseDualCIDR(s string) ([2]int, error) {
	out := [2]int{-1, -1}
	if i := strings.Index(s, "//"); i >= 0 {
		v6, err := strconv.Atoi(s[i+2:])
		if err != nil || v6 < 0 || v6 > 128 {
			return out, fmt.Errorf("잘못된 IPv6 프리픽스 %q", s)
		}
		out[1] = v6
		s = s[:i]
	}
	if strings.HasPrefix(s, "/") && len(s) > 1 {
		v4, err := strconv.Atoi(s[1:])
		if err != nil || v4 < 0 || v4 > 32 {
			return out, fmt.Errorf("잘못된 IPv4 프리픽스 %q", s)
		}
		out[0] = v4
	}
	return out, nil
}

// matchCIDR compares ip against an ip4:/ip6: argument, with or without a
// prefix length.
func matchCIDR(spec string, ip net.IP) (bool, error) {
	if strings.Contains(spec, "/") {
		_, n, err := net.ParseCIDR(spec)
		if err != nil {
			return false, fmt.Errorf("잘못된 CIDR %q", spec)
		}
		return n.Contains(ip), nil
	}
	got := net.ParseIP(spec)
	if got == nil {
		return false, fmt.Errorf("잘못된 IP %q", spec)
	}
	return got.Equal(ip), nil
}

func containsIP(list []net.IP, ip net.IP, prefix int) bool {
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	mask := net.CIDRMask(prefix, bits)
	for _, got := range list {
		a, b := got, ip
		if bits == 32 {
			a, b = a.To4(), b.To4()
		} else {
			a, b = a.To16(), b.To16()
		}
		if a == nil || b == nil {
			continue
		}
		if a.Mask(mask).Equal(b.Mask(mask)) {
			return true
		}
	}
	return false
}

func qualifierResult(q byte) string {
	switch q {
	case '-':
		return spfFail
	case '~':
		return spfSoftfail
	case '?':
		return spfNeutral
	default:
		return spfPass
	}
}
