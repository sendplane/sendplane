package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/postgres"
	"github.com/sendplane/sendplane/store/storetest"
)

// dsnEnv points at the database the conformance suite runs against. Without
// it the suite is skipped, so `go test ./...` stays green on a machine with no
// PostgreSQL.
const dsnEnv = "SENDPLANE_TEST_POSTGRES_DSN"

func TestPostgres(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s is not set; set it to a PostgreSQL DSN to run the store conformance suite", dsnEnv)
	}
	storetest.Run(t, func(t *testing.T) store.Provider {
		ctx := context.Background()
		p, err := postgres.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		// storetest.Run migrates as well: running it twice is the cheapest
		// check that the migration runner is idempotent.
		if err := p.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		return p
	})
}
