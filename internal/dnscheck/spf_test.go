package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/sendplane/sendplane/store"
)

// deepSPF builds a record whose include chain costs n lookups, which is how
// the RFC 7208 ten-lookup budget gets exceeded in the wild: one include per
// vendor, each of which includes two more.
func deepSPF(n int) zone {
	z := zone{txt: map[string][]string{}}
	record := "v=spf1"
	for i := range n {
		host := fmt.Sprintf("inc%d.example.com", i)
		record += " include:" + host
		z.txt[host] = []string{"v=spf1 ip4:198.51.100." + strconv.Itoa(i) + " -all"}
	}
	z.txt["example.com"] = []string{record + " -all"}
	return z
}

func TestSPF(t *testing.T) {
	t.Parallel()

	base := zone{
		txt: map[string][]string{
			"example.com":         {"v=spf1 ip4:203.0.113.0/24 include:_spf.relay.test a:mail.example.com mx -all"},
			"_spf.relay.test":     {"v=spf1 ip4:198.51.100.7 ip6:2001:db8::/32 -all"},
			"soft.example.com":    {"v=spf1 ip4:203.0.113.1 ~all"},
			"neutral.example.com": {"v=spf1 ip4:203.0.113.1 ?all"},
			"noall.example.com":   {"v=spf1 ip4:203.0.113.1"},
			"broken.example.com":  {"v=spf1 ip4:not-an-ip -all"},
			"double.example.com":  {"v=spf1 -all", "v=spf1 +all"},
			"nospf.example.com":   {"google-site-verification=abc"},
			"redir.example.com":   {"v=spf1 redirect=_spf.relay.test"},
		},
		a: map[string][]string{
			"mail.example.com": {"192.0.2.10"},
			"mx1.example.com":  {"192.0.2.20"},
		},
		mx: map[string][]MX{
			"example.com": {{Host: "mx1.example.com", Pref: 10}},
		},
	}

	tests := []struct {
		name   string
		zone   zone
		domain string
		ip     string
		want   store.HealthStatus
		result string
	}{
		{name: "ip4 authorized", zone: base, domain: "example.com", ip: "203.0.113.9", want: store.HealthGreen, result: spfPass},
		{name: "include authorized", zone: base, domain: "example.com", ip: "198.51.100.7", want: store.HealthGreen, result: spfPass},
		{name: "ip6 through include", zone: base, domain: "example.com", ip: "2001:db8::1", want: store.HealthGreen, result: spfPass},
		{name: "a mechanism", zone: base, domain: "example.com", ip: "192.0.2.10", want: store.HealthGreen, result: spfPass},
		{name: "mx mechanism", zone: base, domain: "example.com", ip: "192.0.2.20", want: store.HealthGreen, result: spfPass},
		{name: "redirect", zone: base, domain: "redir.example.com", ip: "198.51.100.7", want: store.HealthGreen, result: spfPass},
		{name: "no observed ip", zone: base, domain: "example.com", want: store.HealthGreen, result: spfFail},

		{name: "ip not authorized", zone: base, domain: "example.com", ip: "198.51.100.99", want: store.HealthRed, result: spfFail},
		{name: "softfail", zone: base, domain: "soft.example.com", ip: "198.51.100.99", want: store.HealthYellow, result: spfSoftfail},
		{name: "neutral", zone: base, domain: "neutral.example.com", ip: "198.51.100.99", want: store.HealthYellow, result: spfNeutral},
		{name: "no all mechanism", zone: base, domain: "noall.example.com", want: store.HealthYellow, result: spfNeutral},

		{name: "missing", zone: base, domain: "nothing.example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfNone},
		{name: "not an spf txt", zone: base, domain: "nospf.example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfNone},
		{name: "malformed", zone: base, domain: "broken.example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfPermerror},
		{name: "two records", zone: base, domain: "double.example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfPermerror},
		{name: "lookup limit exceeded", zone: deepSPF(11), domain: "example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfPermerror},
		{name: "lookup limit not exceeded", zone: deepSPF(10), domain: "example.com", ip: "203.0.113.9", want: store.HealthRed, result: spfFail},
		{
			name:   "servfail is temporary",
			zone:   zone{fail: map[string]error{"example.com": errors.New("SERVFAIL")}},
			domain: "example.com", ip: "203.0.113.9",
			want: store.HealthYellow, result: spfTemperror,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(newFake(tc.zone))
			var ip net.IP
			if tc.ip != "" {
				ip = net.ParseIP(tc.ip)
			}
			got := c.SPF(context.Background(), tc.domain, ip)
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s (%s) details=%v", got.Status, tc.want, got.Summary, got.Details)
			}
			if got.Details["result"] != tc.result {
				t.Errorf("result = %q, want %q", got.Details["result"], tc.result)
			}
			if got.Check != "spf" {
				t.Errorf("check = %q", got.Check)
			}
		})
	}
}

func TestSPFLookupBudget(t *testing.T) {
	t.Parallel()

	c := New(newFake(deepSPF(11)))
	got := c.SPF(context.Background(), "example.com", net.ParseIP("203.0.113.9"))
	if got.Status != store.HealthRed {
		t.Fatalf("status = %s, want red", got.Status)
	}
	// The evaluation must stop at the limit rather than walking all eleven
	// includes: a record that already exceeded the budget is a permerror
	// whatever the twelfth include says.
	if n, _ := strconv.Atoi(got.Details["lookups"]); n != MaxSPFLookups+1 {
		t.Fatalf("lookups = %s, want %d", got.Details["lookups"], MaxSPFLookups+1)
	}
}

func TestSPFRepeatedIncludeQueriesOnce(t *testing.T) {
	t.Parallel()

	f := newFake(zone{txt: map[string][]string{
		"example.com":     {"v=spf1 include:_spf.relay.test include:_spf.relay.test -all"},
		"_spf.relay.test": {"v=spf1 ip4:198.51.100.7 -all"},
	}})
	c := New(f)
	got := c.SPF(context.Background(), "example.com", net.ParseIP("198.51.100.7"))
	if got.Status != store.HealthGreen {
		t.Fatalf("status = %s, want green: %s", got.Status, got.Summary)
	}
	// Two TXT queries in total: the domain and the include, resolved once.
	if n := f.queries(); n != 2 {
		t.Fatalf("queries = %d, want 2 (%v)", n, f.calls)
	}
}

func TestParseSPFTerm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    string
		name  string
		value string
		qual  byte
		cidr4 int
		bad   bool
	}{
		{in: "ip4:203.0.113.0/24", name: "ip4", value: "203.0.113.0/24", qual: '+', cidr4: -1},
		{in: "-all", name: "all", qual: '-', cidr4: -1},
		{in: "~all", name: "all", qual: '~', cidr4: -1},
		{in: "include:_spf.example.com", name: "include", value: "_spf.example.com", qual: '+', cidr4: -1},
		{in: "a", name: "a", qual: '+', cidr4: -1},
		{in: "a/24", name: "a", qual: '+', cidr4: 24},
		{in: "a:mail.example.com/28", name: "a", value: "mail.example.com", qual: '+', cidr4: 28},
		{in: "mx:example.com//64", name: "mx", value: "example.com", qual: '+', cidr4: -1},
		{in: "redirect=_spf.example.com", name: "redirect", value: "_spf.example.com", qual: '+', cidr4: -1},
		{in: "exp=explain.example.com", name: "exp", value: "explain.example.com", qual: '+', cidr4: -1},
		{in: "unknown=1", name: termIgnored, qual: '+', cidr4: -1},
		{in: "ip4:nonsense", bad: true},
		{in: "include:", bad: true},
		{in: "bogus:x", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSPFTerm(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("parseSPFTerm(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSPFTerm(%q): %v", tc.in, err)
			}
			if got.name != tc.name || got.value != tc.value || got.qualifier != tc.qual || got.cidr4 != tc.cidr4 {
				t.Fatalf("parseSPFTerm(%q) = %+v, want {%s %s %c %d}",
					tc.in, got, tc.name, tc.value, tc.qual, tc.cidr4)
			}
		})
	}
}
