package zones_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/storetest"
)

// This package's fixtures used to open sqlite and nothing else, which left
// every argument above the store — the reload, the two installs, the refresh
// scheduler — measured against exactly one connection pool. internal/store
// (store_test.go, forEachDriver) already runs its own cases on both drivers
// via a testcontainers postgres; the difference matters most *here*, because
// store.Open applies SetMaxOpenConns(1) only to sqlite (store.go). Under
// sqlite two concurrent Reloads have their reads serialised by that single
// connection whatever the resolver does; under postgres they genuinely
// overlap, so the window Resolver.rmu closes is wider than the one the fix
// was originally measured against.
//
// The container, the template and the per-test database are internal/
// storetest's, shared with the other two packages that need them.

// pgDSN is the maintenance DSN every per-test database is created from. It
// is set by TestMain when a container came up (or when the environment
// already provided one); empty means no postgres, which is a skip.
var pgDSN string

// pgTemplate is a database with the migrations already applied. Named
// distinctly from internal/app's template because CI may point both packages
// at one shared cluster.
const pgTemplate = "dnsaur_zones_tmpl"

func TestMain(m *testing.M) { os.Exit(run(m)) }

// run is TestMain's body in a function of its own so that its defers actually
// run: os.Exit skips them, and with the reaper disabled (see storetest.Start)
// this process is the only thing that will ever stop the container it starts.
func run(m *testing.M) int {
	ctx := context.Background()
	dsn, stop := storetest.Start(ctx, "zones")
	defer stop()
	if dsn != "" {
		if err := storetest.BuildTemplate(ctx, dsn, pgTemplate); err != nil {
			// Deliberately not fatal: a machine that can start a container
			// but cannot build the template is still allowed to run the
			// sqlite halves. pgDSN stays empty, so the postgres halves skip
			// and say so.
			fmt.Fprintf(os.Stderr, "zones tests: postgres unavailable, its halves will skip: %v\n", err)
		} else {
			pgDSN = dsn
		}
	}
	return m.Run()
}

// openPostgresStore returns a store over a database of this test's own,
// copied from the migrated template and dropped on cleanup.
//
// A database each, rather than internal/store's one shared database with
// unique row names, because Resolver.Reload reads *every* zone in the store:
// zones left behind by an earlier test would end up in a later test's served
// snapshot, and the fixtures here all build the same apex (transferApex), so
// the second test to run would fail on AddZone instead.
func openPostgresStore(t *testing.T) store.Store {
	t.Helper()
	// Registered before the store is opened, so the drop runs *after* the
	// store's own Close (cleanups are LIFO) and is not fighting a live pool.
	dsn := storetest.Database(t, pgDSN, pgTemplate, "zt")
	st, err := store.Open(context.Background(), "postgres", dsn)
	if err != nil {
		t.Fatalf("store.Open(postgres): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st
}

// openTestStoreOn is openTestStore with the driver named. sqlite is a file
// under t.TempDir(); postgres is a database of this test's own.
func openTestStoreOn(t *testing.T, driver string) store.Store {
	t.Helper()
	switch driver {
	case "sqlite":
		return openTestStore(t)
	case "postgres":
		return openPostgresStore(t)
	default:
		t.Fatalf("unknown driver %q", driver)
		return nil
	}
}

// forEachDriver runs fn as a subtest per driver, mirroring internal/store's
// helper of the same name. Only the cases where a driver difference could
// plausibly bite are wrapped in it — see task-13's report for which, and why
// the rest are deliberately sqlite-only.
func forEachDriver(t *testing.T, fn func(t *testing.T, driver string)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, "sqlite") })
	t.Run("postgres", func(t *testing.T) { fn(t, "postgres") })
}
