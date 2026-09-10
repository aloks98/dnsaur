// Package storetest starts the Postgres a package's postgres halves run
// against, and hands out a migrated database per test.
//
// It takes a *testing.T and imports "testing" despite not being a _test.go
// file, both unusual for a non-test package — the same deliberate exception
// internal/certtest makes, and for the same reason: this package exists only
// to be called from tests, and t.Fatalf on a failure is what every caller
// would otherwise write by hand around an error return.
//
// It was three copies before it was a package. internal/store, internal/zones
// and internal/app each grew their own container start, readiness poll,
// template build and per-test database, byte-identical apart from a log
// prefix and a database-name prefix.
package storetest

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
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver this package opens the maintenance database with
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start brings up the Postgres the calling package's tests use and returns
// the maintenance DSN it is reached on, plus the stop that has to run before
// the process exits.
//
// An externally provided database wins: DNSAUR_TEST_POSTGRES_DSN lets CI
// point the suite at one it already runs, and nothing then starts a
// container.
//
// An empty DSN means no postgres, which callers turn into a skip rather than
// a failure — a machine without Docker still gets a green run with the
// postgres halves *reported as skipped* rather than quietly passing. pkg
// names the caller in that diagnostic.
//
// The reaper is disabled deliberately. testcontainers derives its session ID
// from the parent pid and its start time (internal/core/bootstrap.go), so
// every test binary in one `go test ./...` shares a single Ryuk — and Ryuk
// prunes everything carrying that session's labels as soon as a client
// disconnects. internal/store finishes in ~32s while internal/zones and
// internal/app run for minutes, so store exiting took the others' postgres
// down with it: "connection refused" against a mapped port with no container
// behind it, red only when the packages were run together, which is what
// `go test ./...` and CI do. core.DefaultLabels adds
// org.testcontainers.reap=true only when the reaper is enabled, and that
// label is one of the filters a reaper prunes by, so a container started
// from here is matched by nobody's.
//
// The cost is that nothing external cleans up after the process, so the
// returned stop has to actually run — hence a TestMain whose body is a
// run() function, since os.Exit skips defers. A hard kill (SIGKILL, a
// panicking runtime) still leaks one container.
func Start(ctx context.Context, pkg string) (string, func()) {
	stop := func() {}
	dsn := os.Getenv("DNSAUR_TEST_POSTGRES_DSN")
	if dsn == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
		req := testcontainers.ContainerRequest{
			Image:        "postgres:17-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "t", "POSTGRES_DB": "dnsaur"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor:   wait.ForListeningPort("5432/tcp").WithStartupTimeout(60 * time.Second),
		}
		pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s tests: no postgres, its halves will skip: %v\n", pkg, err)
			return "", stop
		}
		host, _ := pg.Host(ctx)
		port, _ := pg.MappedPort(ctx, "5432")
		dsn = fmt.Sprintf("postgres://postgres:t@%s:%s/dnsaur?sslmode=disable", host, port.Port())
		stop = func() { _ = pg.Terminate(context.Background()) }
	}
	// The port is listening before the server will accept a connection: the
	// image's entrypoint runs initdb against a *local* postmaster first, and
	// a connection that arrives in that window is refused with 57P03 "the
	// database system is starting up". wait.ForListeningPort cannot see the
	// difference, so the readiness check is made here, where it can.
	if err := waitReady(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "%s tests: postgres never became ready, its halves will skip: %v\n", pkg, err)
		stop()
		return "", func() {}
	}
	return dsn, stop
}

// BuildTemplate creates the database called name and runs the migrations
// into it once. Each test's own database is then copied from it (CREATE
// DATABASE ... TEMPLATE), so a test pays for a file copy rather than for
// goose replaying every migration.
//
// Callers name their template distinctly from each other's, because CI may
// point several packages at one shared cluster.
func BuildTemplate(ctx context.Context, dsn, name string) error {
	admin, err := maintenanceDB(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		return fmt.Errorf("dropping a stale template: %w", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return fmt.Errorf("creating the template: %w", err)
	}
	// store.Open is what runs the migrator, so the template is migrated by
	// exactly the code every test would otherwise run itself.
	s, err := store.Open(ctx, "postgres", WithDatabase(dsn, name))
	if err != nil {
		return fmt.Errorf("migrating the template: %w", err)
	}
	// Closed before any copy is taken: CREATE DATABASE ... TEMPLATE refuses
	// while another session is connected to the source.
	return s.Close()
}

// Database copies template into a database of this test's own and returns
// the DSN for it, dropping it on cleanup. An empty dsn is a skip: no
// postgres came up.
//
// A database each, rather than one shared database with unique row names,
// because the callers' operations read *every* zone in the store: rows left
// behind by an earlier test would end up in a later test's served snapshot.
//
// The drop is registered here, before the caller opens its store or builds
// its App, so it runs *after* whatever closes that store — cleanups are
// LIFO. Dropping while the pool is live would need FORCE to succeed and
// would race the owner's own goroutines.
func Database(t *testing.T, dsn, template, prefix string) string {
	t.Helper()
	if dsn == "" {
		t.Skip("no postgres available: set DNSAUR_TEST_POSTGRES_DSN, or run with Docker so TestMain can start one")
	}
	ctx := context.Background()
	admin, err := maintenanceDB(dsn)
	if err != nil {
		t.Fatalf("maintenance connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	// Unique within the cluster for the whole run, not merely within one
	// test: the seq alone would collide with a second test binary's.
	name := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), seq.Add(1))
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`" TEMPLATE "`+template+`"`); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		db, err := maintenanceDB(dsn)
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		_, _ = db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})
	return WithDatabase(dsn, name)
}

// WithDatabase points dsn at the database called name.
func WithDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// seq numbers the per-test databases.
var seq atomic.Int64

// waitReady pings until the server answers or the deadline passes. See
// Start for why a listening port is not the same thing as a ready server.
func waitReady(ctx context.Context, dsn string) error {
	db, err := maintenanceDB(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(60 * time.Second)
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
	db, err := sql.Open("pgx", WithDatabase(dsn, "postgres"))
	if err != nil {
		return nil, err
	}
	// One connection, closed with the handle: a lingering idle connection to
	// the maintenance database is harmless, but a pool of them is noise in
	// pg_stat_activity while a test is diagnosing something.
	db.SetMaxOpenConns(1)
	return db, nil
}
