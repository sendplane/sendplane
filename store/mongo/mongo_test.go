package mongo_test

import (
	"context"
	"os"
	"testing"

	"github.com/sendplane/sendplane/store"
	mongostore "github.com/sendplane/sendplane/store/mongo"
	"github.com/sendplane/sendplane/store/platformtest"
	"github.com/sendplane/sendplane/store/storetest"
)

// testDB is the database the suite works in. Every subtest uses a fresh
// tenant, so nothing has to be cleaned up between runs and the suite can be
// re-run against the same database to prove Migrate is idempotent.
const testDB = "sendplane_test"

// TestMongo runs the conformance suite against the MongoDB the environment
// points at. Without SENDPLANE_TEST_MONGO_URI there is nothing to talk to and
// the test skips.
func TestMongo(t *testing.T) {
	uri := os.Getenv("SENDPLANE_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("SENDPLANE_TEST_MONGO_URI is not set")
	}
	storetest.Run(t, func(t *testing.T) store.Provider {
		p, err := mongostore.Open(context.Background(), uri, testDB)
		if err != nil {
			t.Fatalf("Open(%s): %v", uri, err)
		}
		t.Cleanup(func() {
			if err := p.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return p
	})
}

// TestMongoPlatformOverlay runs the platform overlay suite (ADR-0017) against
// MongoDB, for the same reason the Postgres one exists: the wrapper's logic is
// covered by memstore, the document mapping is not.
func TestMongoPlatformOverlay(t *testing.T) {
	uri := os.Getenv("SENDPLANE_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("SENDPLANE_TEST_MONGO_URI is not set")
	}
	platformtest.Run(t, func(t *testing.T) store.Provider {
		p, err := mongostore.Open(context.Background(), uri, testDB)
		if err != nil {
			t.Fatalf("Open(%s): %v", uri, err)
		}
		t.Cleanup(func() {
			if err := p.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return p
	})
}
