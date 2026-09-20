package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// ErrNoRecord is what every lookup returns for NXDOMAIN and for a NOERROR
// answer that carries no record of the requested type. The two are the same
// thing to a health check ("the record is not there"), and keeping them apart
// would only push the distinction into every caller.
var ErrNoRecord = errors.New("dnscheck: no record")

// MX is one MX record.
type MX struct {
	Host string
	Pref uint16
}

// Resolver is the DNS surface the checks need. It is an interface so tests run
// against a table instead of the network (there is no other reason: the
// production implementation is the only one that ships).
//
// Every method returns ErrNoRecord when the name exists but has no record of
// that type, and when it does not exist at all. Any other error is a transport
// or server failure, which the checks report as yellow rather than red: a
// SERVFAIL says nothing about the record.
type Resolver interface {
	TXT(ctx context.Context, name string) ([]string, error)
	A(ctx context.Context, name string) ([]net.IP, error)
	AAAA(ctx context.Context, name string) ([]net.IP, error)
	MX(ctx context.Context, name string) ([]MX, error)
	PTR(ctx context.Context, name string) ([]string, error)
}

// DefaultTimeout is the per-query deadline when none is given.
const DefaultTimeout = 5 * time.Second

// resolver queries the configured nameservers directly with miekg/dns rather
// than through the host's stub resolver (architecture 11.3): a probe that
// wants to know what the world sees must not be answered from a local cache or
// an /etc/hosts entry.
type resolver struct {
	client  *dns.Client
	tcp     *dns.Client
	servers []string
}

// NewResolver queries the given nameservers in order. Each entry is host or
// host:port; port 53 is assumed.
func NewResolver(servers []string, timeout time.Duration) (Resolver, error) {
	if len(servers) == 0 {
		return nil, errors.New("dnscheck: no nameserver configured")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	addrs := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		addrs = append(addrs, s)
	}
	if len(addrs) == 0 {
		return nil, errors.New("dnscheck: no nameserver configured")
	}
	return &resolver{
		client:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:     &dns.Client{Net: "tcp", Timeout: timeout},
		servers: addrs,
	}, nil
}

// SystemResolver reads the system resolvers from /etc/resolv.conf. It is the
// default: an operator who has not named a nameserver gets the one the machine
// already trusts.
func SystemResolver(timeout time.Duration) (Resolver, error) {
	return ResolverFromFile("/etc/resolv.conf", timeout)
}

// ResolverFromFile reads nameservers from a resolv.conf-shaped file.
func ResolverFromFile(path string, timeout time.Duration) (Resolver, error) {
	cfg, err := dns.ClientConfigFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("dnscheck: %w", err)
	}
	if timeout <= 0 {
		if cfg.Timeout > 0 {
			timeout = time.Duration(cfg.Timeout) * time.Second
		} else {
			timeout = DefaultTimeout
		}
	}
	servers := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		servers = append(servers, net.JoinHostPort(s, cfg.Port))
	}
	return NewResolver(servers, timeout)
}

// exchange asks every configured server in turn and returns the first usable
// answer. A refusal or a server failure moves on to the next server; NXDOMAIN
// is an answer and stops the walk.
func (r *resolver) exchange(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	fqdn := dns.Fqdn(name)
	m := new(dns.Msg)
	m.SetQuestion(fqdn, qtype)
	m.RecursionDesired = true

	var lastErr error
	for _, srv := range r.servers {
		resp, _, err := r.client.ExchangeContext(ctx, m, srv)
		if err == nil && resp != nil && resp.Truncated {
			resp, _, err = r.tcp.ExchangeContext(ctx, m, srv)
		}
		if err != nil {
			lastErr = err
			continue
		}
		switch resp.Rcode {
		case dns.RcodeSuccess:
			return resp, nil
		case dns.RcodeNameError:
			return nil, ErrNoRecord
		default:
			lastErr = fmt.Errorf("dnscheck: %s %s: %s",
				dns.TypeToString[qtype], name, dns.RcodeToString[resp.Rcode])
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("dnscheck: %s %s: no answer", dns.TypeToString[qtype], name)
	}
	return nil, lastErr
}

func (r *resolver) TXT(ctx context.Context, name string) ([]string, error) {
	resp, err := r.exchange(ctx, name, dns.TypeTXT)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rr := range resp.Answer {
		if t, ok := rr.(*dns.TXT); ok {
			// RFC 7208 3.3: the character-strings of one TXT record are
			// concatenated without separators. Splitting a 300-byte SPF record
			// at the 255-byte boundary is a normal, valid way to publish it.
			out = append(out, strings.Join(t.Txt, ""))
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecord
	}
	return out, nil
}

func (r *resolver) A(ctx context.Context, name string) ([]net.IP, error) {
	return r.addrs(ctx, name, dns.TypeA)
}

func (r *resolver) AAAA(ctx context.Context, name string) ([]net.IP, error) {
	return r.addrs(ctx, name, dns.TypeAAAA)
}

func (r *resolver) addrs(ctx context.Context, name string, qtype uint16) ([]net.IP, error) {
	resp, err := r.exchange(ctx, name, qtype)
	if err != nil {
		return nil, err
	}
	var out []net.IP
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			out = append(out, v.A)
		case *dns.AAAA:
			out = append(out, v.AAAA)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecord
	}
	return out, nil
}

func (r *resolver) MX(ctx context.Context, name string) ([]MX, error) {
	resp, err := r.exchange(ctx, name, dns.TypeMX)
	if err != nil {
		return nil, err
	}
	var out []MX
	for _, rr := range resp.Answer {
		if v, ok := rr.(*dns.MX); ok {
			out = append(out, MX{Host: strings.TrimSuffix(v.Mx, "."), Pref: v.Preference})
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecord
	}
	return out, nil
}

func (r *resolver) PTR(ctx context.Context, name string) ([]string, error) {
	resp, err := r.exchange(ctx, name, dns.TypePTR)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rr := range resp.Answer {
		if v, ok := rr.(*dns.PTR); ok {
			out = append(out, strings.TrimSuffix(v.Ptr, "."))
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecord
	}
	return out, nil
}

// ReverseName is the in-addr.arpa / ip6.arpa name of an address.
func ReverseName(ip net.IP) (string, error) {
	name, err := dns.ReverseAddr(ip.String())
	if err != nil {
		return "", fmt.Errorf("dnscheck: %w", err)
	}
	return strings.TrimSuffix(name, "."), nil
}
