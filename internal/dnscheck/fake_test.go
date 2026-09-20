package dnscheck

import (
	"context"
	"net"
	"strings"
	"sync"
)

// zone is a hand-written DNS zone. Every check in this package is table
// driven against it: the point of the Resolver interface is that no test in
// here ever touches the network.
type zone struct {
	txt  map[string][]string
	a    map[string][]string
	aaaa map[string][]string
	mx   map[string][]MX
	ptr  map[string][]string
	// fail forces a server failure for a name, which is how the temperror and
	// "could not look up" paths get exercised.
	fail map[string]error
}

type fakeResolver struct {
	z zone

	mu    sync.Mutex
	calls map[string]int
}

func newFake(z zone) *fakeResolver {
	return &fakeResolver{z: z, calls: map[string]int{}}
}

func (f *fakeResolver) count(kind, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[kind+" "+strings.ToLower(name)]++
}

func (f *fakeResolver) queries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeResolver) check(kind, name string) error {
	f.count(kind, name)
	if err, ok := f.z.fail[strings.ToLower(name)]; ok {
		return err
	}
	return nil
}

func (f *fakeResolver) TXT(_ context.Context, name string) ([]string, error) {
	if err := f.check("TXT", name); err != nil {
		return nil, err
	}
	v, ok := f.z.txt[strings.ToLower(name)]
	if !ok || len(v) == 0 {
		return nil, ErrNoRecord
	}
	return v, nil
}

func (f *fakeResolver) A(_ context.Context, name string) ([]net.IP, error) {
	if err := f.check("A", name); err != nil {
		return nil, err
	}
	return ips(f.z.a[strings.ToLower(name)])
}

func (f *fakeResolver) AAAA(_ context.Context, name string) ([]net.IP, error) {
	if err := f.check("AAAA", name); err != nil {
		return nil, err
	}
	return ips(f.z.aaaa[strings.ToLower(name)])
}

func (f *fakeResolver) MX(_ context.Context, name string) ([]MX, error) {
	if err := f.check("MX", name); err != nil {
		return nil, err
	}
	v, ok := f.z.mx[strings.ToLower(name)]
	if !ok || len(v) == 0 {
		return nil, ErrNoRecord
	}
	return v, nil
}

func (f *fakeResolver) PTR(_ context.Context, name string) ([]string, error) {
	if err := f.check("PTR", name); err != nil {
		return nil, err
	}
	v, ok := f.z.ptr[strings.ToLower(name)]
	if !ok || len(v) == 0 {
		return nil, ErrNoRecord
	}
	return v, nil
}

func ips(ss []string) ([]net.IP, error) {
	if len(ss) == 0 {
		return nil, ErrNoRecord
	}
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		out = append(out, net.ParseIP(s))
	}
	return out, nil
}
