package app

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/storetest"
)

// internal/store has run every case on both drivers for a long time
// (store_test.go, forEachDriver) and internal/zones now does for the cases
// where a driver difference could bite (its own main_test.go). This package
// was the last significant gap, and it is the one the difference matters most
// in: it is the only place that wires the resolver, the refresh scheduler and
// the conditional routing table together, and its central operation —
// App.ReloadZones — is a concurrent whole-store reload feeding a routing
// table, which is exactly the shape sqlite flatters.
//
// store.Open applies SetMaxOpenConns(1) only in the sqlite branch
// (store.go), so under sqlite the two QueryContext reads inside
// Resolver.Reload are serialised by that single connection whatever the code
// above does. Under postgres they genuinely overlap, and every measurement
// behind this milestone's concurrency work — routeMu, the install ordering,
// the cache purge — was taken on the driver where the window is narrowest.
//
// The container, the template and the per-test database are internal/
// storetest's, shared with the other two packages that need them.

// pgDSN is the maintenance DSN every per-test database is created from. It is
// set by TestMain when a container came up (or when the environment already
// provided one); empty means no postgres, which is a skip.
var pgDSN string

// pgTemplate is a database with the migrations already applied. Named
// distinctly from internal/zones' template because CI may point both packages
// at one shared cluster.
const pgTemplate = "dnsaur_app_tmpl"

func TestMain(m *testing.M) { os.Exit(run(m)) }

// run is TestMain's body in a function of its own so that its defers actually
// run: os.Exit skips them, and with the reaper disabled (see storetest.Start)
// this process is the only thing that will ever stop the container it starts.
func run(m *testing.M) int {
	ctx := context.Background()
	dsn, stop := storetest.Start(ctx, "app")
	defer stop()
	if dsn != "" {
		if err := storetest.BuildTemplate(ctx, dsn, pgTemplate); err != nil {
			// Deliberately not fatal: a machine that can start a container
			// but cannot build the template is still allowed to run the
			// sqlite halves. pgDSN stays empty, so the postgres halves skip
			// and say so.
			fmt.Fprintf(os.Stderr, "app tests: postgres unavailable, its halves will skip: %v\n", err)
		} else {
			pgDSN = dsn
		}
	}
	return m.Run()
}

// postgresConfig returns a config pointing at a database of this test's own,
// copied from the migrated template and dropped on cleanup.
func postgresConfig(t *testing.T) *config.Config {
	t.Helper()
	// Registered before the caller builds its App, so the drop runs after
	// the App's own Shutdown — see storetest.Database.
	dsn := storetest.Database(t, pgDSN, pgTemplate, "at")

	// DataDir still points at a temp directory: App.New makes it, and the
	// blocklist refresher caches downloads under it. Only the store moves.
	dir := t.TempDir()
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "postgres"
	cfg.Storage.DSN = dsn
	return cfg
}

// testConfigOn is testConfig with the driver named: sqlite is a file under
// t.TempDir(), postgres is a database of this test's own.
func testConfigOn(t *testing.T, driver string) *config.Config {
	t.Helper()
	switch driver {
	case "sqlite":
		return testConfig(t.TempDir())
	case "postgres":
		return postgresConfig(t)
	default:
		t.Fatalf("unknown driver %q", driver)
		return nil
	}
}

// forEachDriver runs fn as a subtest per driver, mirroring internal/store's
// and internal/zones' helpers of the same name. Only the cases where a driver
// difference could plausibly bite are wrapped in it — see task-14's report for
// which, and why the rest are deliberately sqlite-only.
func forEachDriver(t *testing.T, fn func(t *testing.T, driver string)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, "sqlite") })
	t.Run("postgres", func(t *testing.T) { fn(t, "postgres") })
}
