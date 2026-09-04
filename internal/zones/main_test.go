package zones_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver this file opens the maintenance database with
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
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
// Same env var and same skip-when-absent behaviour as internal/store, so a
// machine without Docker still gets a green run with the postgres halves
// *reported as skipped* rather than quietly passing.

// pgDSN is the maintenance DSN every per-test database is created from. It
// is set by TestMain when a container came up (or when the environment
// already provided one); empty means no postgres, which is a skip.
var pgDSN string

// pgTemplate is a database with the migrations already applied. Each test's
// database is copied from it (CREATE DATABASE ... TEMPLATE), so a test pays
// for a file copy rather than for goose replaying all thirteen migrations —
// the difference between the postgres halves costing seconds and costing
// tens of seconds.
const pgTemplate = "dnsaur_zones_tmpl"

func TestMain(m *testing.M) { os.Exit(run(m)) }

// run is TestMain's body in a function of its own so that its defers actually
// run: os.Exit skips them, and with the reaper disabled (see below) this
// process is the only thing that will ever stop the container it starts.
func run(m *testing.M) int {
	ctx := context.Background()
	// An externally provided database wins: CI can point the suite at one it
	// already runs, and nothing then starts a container.
	dsn := os.Getenv("DNSAUR_TEST_POSTGRES_DSN")
	if dsn == "" {
		// Ryuk, testcontainers' reaper, is shared by every test binary in a
		// single `go test` invocation: the session ID is a hash of the parent
		// pid and its start time (internal/core/bootstrap.go), so a
		// `go test ./...` puts this package and internal/store behind *one*
		// reaper. That reaper prunes everything carrying the session's labels
		// as soon as a client disconnects — and internal/store finishes in
		// ~32s while this package runs for ~135s, so store exiting took this
		// package's postgres down with it. Every postgres half after that
		// point failed with "connection refused" against a mapped port that
		// no longer had a container behind it: thirteen red subtests, and red
		// only when the two packages were run together.
		//
		// Disabling the reaper for *this* process is what breaks the link.
		// core.DefaultLabels adds org.testcontainers.reap=true only when the
		// reaper is enabled, and that label is one of the filters the other
		// process's reaper prunes by, so a container started from here is no
		// longer matched by it. Nothing else about the other package changes.
		//
		// The cost is that nothing external cleans up after this process, so
		// the Terminate below has to actually run — hence run() rather than
		// os.Exit(m.Run()). A hard kill (SIGKILL, a panicking runtime) still
		// leaks one container.
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
		req := testcontainers.ContainerRequest{
			Image:        "postgres:17-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "t", "POSTGRES_DB": "dnsaur"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor:   wait.ForListeningPort("5432/tcp").WithStartupTimeout(60 * time.Second),
		}
		pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
		if err == nil {
			host, _ := pg.Host(ctx)
			port, _ := pg.MappedPort(ctx, "5432")
			dsn = fmt.Sprintf("postgres://postgres:t@%s:%s/dnsaur?sslmode=disable", host, port.Port())
			defer func() { _ = pg.Terminate(context.Background()) }()
		}
	}
	if dsn != "" {
		if err := buildPGTemplate(ctx, dsn); err != nil {
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

// buildPGTemplate creates pgTemplate and runs the migrations into it once.
func buildPGTemplate(ctx context.Context, dsn string) error {
	admin, err := maintenanceDB(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()
	// The port is listening before the server will accept a connection: the
	// image's entrypoint runs initdb against a *local* postmaster first, and
	// a connection that arrives in that window is refused with 57P03 "the
	// database system is starting up". wait.ForListeningPort cannot see the
	// difference, so the readiness check is made here, where it can.
	if err := waitReady(ctx, admin); err != nil {
		return err
	}
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+pgTemplate+`" WITH (FORCE)`); err != nil {
		return fmt.Errorf("dropping a stale template: %w", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+pgTemplate+`"`); err != nil {
		return fmt.Errorf("creating the template: %w", err)
	}
	// store.Open is what runs the migrator, so the template is migrated by
	// exactly the code every test would otherwise run itself.
	s, err := store.Open(ctx, "postgres", withDatabase(dsn, pgTemplate))
	if err != nil {
		return fmt.Errorf("migrating the template: %w", err)
	}
	// Closed before any copy is taken: CREATE DATABASE ... TEMPLATE refuses
	// while another session is connected to the source.
	return s.Close()
}

// waitReady pings until the server answers or the deadline passes.
func waitReady(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(60 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("postgres never became ready: %w", err)
}

// maintenanceDB opens the cluster's "postgres" database. Connecting there
// rather than to the application database is what lets CREATE DATABASE name
// the latter as a template.
func maintenanceDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", withDatabase(dsn, "postgres"))
	if err != nil {
		return nil, err
	}
	// One connection, closed with the handle: a lingering idle connection to
	// the maintenance database is harmless, but a pool of them is noise in
	// pg_stat_activity while a test is diagnosing something.
	db.SetMaxOpenConns(1)
	return db, nil
}

func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// pgSeq numbers the per-test databases. Names have to be unique within the
// cluster for the whole run, not merely within one test.
var pgSeq atomic.Int64

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
	if pgDSN == "" {
		t.Skip("no postgres available: set DNSAUR_TEST_POSTGRES_DSN, or run with Docker so TestMain can start one")
	}
	ctx := context.Background()
	admin, err := maintenanceDB(pgDSN)
	if err != nil {
		t.Fatalf("maintenance connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	name := fmt.Sprintf("zt_%d_%d", os.Getpid(), pgSeq.Add(1))
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`" TEMPLATE "`+pgTemplate+`"`); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	// Registered before the store is opened, so it runs *after* the store's
	// own Close (cleanups are LIFO) and the drop is not fighting a live pool.
	t.Cleanup(func() {
		db, err := maintenanceDB(pgDSN)
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		_, _ = db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})

	st, err := store.Open(ctx, "postgres", withDatabase(pgDSN, name))
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
