package sender

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/store"
)

func poolFixture(t *testing.T, opts chaossmtp.Options, cfg PoolConfig) (*chaossmtp.Server, *Pool, *store.Transport) {
	t.Helper()
	srv, err := chaossmtp.Start(opts)
	if err != nil {
		t.Fatalf("chaossmtp: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	tr := &store.Transport{
		ID: "tr-1", Host: "127.0.0.1", Port: srv.Port(),
		TLS: store.TLSNone, MaxConns: 2, Version: 1,
		Username: opts.Username,
	}
	p := NewPool(cfg)
	t.Cleanup(p.Close)
	return srv, p, tr
}

func poolSend(t *testing.T, p *Pool, tr *store.Transport, password, rcpt string) {
	t.Helper()
	c, err := p.Get(context.Background(), tr, password)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data := []byte(fmt.Sprintf("From: <s@example.com>\r\nTo: <%s>\r\nSubject: x\r\n\r\nbody\r\n", rcpt))
	err = c.Send("s@example.com", []string{rcpt}, data, 5*time.Second)
	p.Put(c, Reusable(Classify(err)))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestPoolReusesConnections(t *testing.T) {
	srv, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{})
	for i := 0; i < 5; i++ {
		poolSend(t, p, tr, "", fmt.Sprintf("u%d@example.org", i))
	}
	if got := srv.Stats().Connections; got != 1 {
		t.Errorf("opened %d connections for 5 sequential messages, want 1", got)
	}
	if got := srv.Stats().Accepted; got != 5 {
		t.Errorf("accepted %d", got)
	}
}

func TestPoolReconnectsAfterMaxMessages(t *testing.T) {
	srv, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{MaxMsgsPerConn: 2})
	for i := 0; i < 5; i++ {
		poolSend(t, p, tr, "", fmt.Sprintf("u%d@example.org", i))
	}
	// 2 + 2 + 1 messages: three connections.
	if got := srv.Stats().Connections; got != 3 {
		t.Errorf("opened %d connections, want 3", got)
	}
}

func TestPoolClosesIdleConnections(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	srv, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{
		IdleTimeout: time.Second, Now: clock,
	})
	poolSend(t, p, tr, "", "a@example.org")
	now = now.Add(2 * time.Second)
	poolSend(t, p, tr, "", "b@example.org")
	if got := srv.Stats().Connections; got != 2 {
		t.Errorf("opened %d connections, want 2: the idle one had expired", got)
	}
}

func TestPoolBoundsConcurrentConnections(t *testing.T) {
	srv, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{})
	tr.MaxConns = 2

	a, err := p.Get(context.Background(), tr, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Get(context.Background(), tr, "")
	if err != nil {
		t.Fatal(err)
	}
	// The third checkout has to wait for one of the first two.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Get(ctx, tr, ""); err == nil {
		t.Fatal("the pool handed out more connections than MaxConns")
	}
	p.Put(a, true)
	c, err := p.Get(context.Background(), tr, "")
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	p.Put(b, true)
	p.Put(c, true)
	if got := srv.Stats().Connections; got != 2 {
		t.Errorf("opened %d connections, want 2", got)
	}
}

func TestPoolDropsConnectionsOfAnOlderTransportVersion(t *testing.T) {
	srv, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{})
	poolSend(t, p, tr, "", "a@example.org")

	// A rotated password bumps Version; the idle connection authenticated with
	// the old one must not be reused.
	tr.Version = 2
	poolSend(t, p, tr, "", "b@example.org")
	if got := srv.Stats().Connections; got != 2 {
		t.Errorf("opened %d connections, want 2", got)
	}
}

func TestPoolBrokenConnectionIsNotReused(t *testing.T) {
	// Every message is dropped mid-DATA, so the connection dies each time.
	srv, p, tr := poolFixture(t,
		chaossmtp.Options{Rates: chaossmtp.Rates{DropRate: 1}},
		PoolConfig{})
	for i := 0; i < 3; i++ {
		c, err := p.Get(context.Background(), tr, "")
		if err != nil {
			t.Fatal(err)
		}
		data := []byte("From: <s@example.com>\r\nSubject: x\r\n\r\nbody\r\n")
		err = c.Send("s@example.com", []string{"a@example.org"}, data, 5*time.Second)
		if err == nil {
			t.Fatal("expected the connection to drop")
		}
		f := Classify(err)
		if f.Class != store.ErrorClassTransient {
			t.Errorf("class = %s, want transient", f.Class)
		}
		p.Put(c, Reusable(f))
	}
	if got := srv.Stats().Connections; got != 3 {
		t.Errorf("opened %d connections, want 3: a dropped connection is never reused", got)
	}
}

func TestPoolAuthenticates(t *testing.T) {
	srv, p, tr := poolFixture(t,
		chaossmtp.Options{Username: "user", Password: "pass", RequireAuth: true},
		PoolConfig{})
	poolSend(t, p, tr, "pass", "a@example.org")
	if got := srv.Stats().Accepted; got != 1 {
		t.Fatalf("accepted %d", got)
	}
	// A pooled connection stays authenticated, so the wrong password is only
	// noticed on a fresh one.
	p2 := NewPool(PoolConfig{})
	defer p2.Close()
	c, err := p2.Get(context.Background(), tr, "wrong")
	if err == nil {
		p2.Put(c, true)
		t.Fatal("a wrong password was accepted")
	}
}

func TestPoolClosedRejectsGet(t *testing.T) {
	_, p, tr := poolFixture(t, chaossmtp.Options{}, PoolConfig{})
	p.Close()
	if _, err := p.Get(context.Background(), tr, ""); err != ErrPoolClosed {
		t.Fatalf("err = %v, want ErrPoolClosed", err)
	}
}
