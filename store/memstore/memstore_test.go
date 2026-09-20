package memstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/memstore"
	"github.com/sendplane/sendplane/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Provider {
		p := memstore.New()
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}

func TestWithClock(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	p := memstore.New(memstore.WithClock(func() time.Time { return now }))
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	s, err := p.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("ForTenant: %v", err)
	}
	tr := &store.Transport{Name: "smtp", Host: "h", Port: 25}
	if err := s.Transports().Create(ctx, tr); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !tr.CreatedAt.Equal(now) || !tr.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %v/%v, want %v", tr.CreatedAt, tr.UpdatedAt, now)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Transports().Get(ctx, tr.ID); err == nil {
		t.Fatal("Get after Close: want an error")
	}
}
