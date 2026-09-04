package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMain runs in run() rather than inline, because os.Exit skips deferred
// calls and the container's Terminate is one of them.
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()
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
		dsn := fmt.Sprintf("postgres://postgres:t@%s:%s/dnsaur?sslmode=disable", host, port.Port())
		defer func() { _ = pg.Terminate(ctx) }()
		// The port is listening before the server will accept a connection:
		// the image's entrypoint runs initdb against a *local* postmaster
		// first, and a connection arriving in that window is refused with
		// 57P03 "the database system is starting up". wait.ForListeningPort
		// cannot see the difference, so the readiness check is made here.
		//
		// It matters more here than in internal/zones, which skips its
		// postgres halves when the database never arrives: openPostgres
		// calls t.Fatal, so losing this race is a red run rather than a
		// visible skip.
		if err := waitReady(ctx, dsn); err != nil {
			fmt.Fprintf(os.Stderr, "store tests: postgres never became ready, its halves will skip: %v\n", err)
		} else {
			_ = os.Setenv("DNSAUR_TEST_POSTGRES_DSN", dsn)
		}
	}
	return m.Run()
}

// waitReady polls until the server answers, or gives up. See the call site
// for why a listening port is not the same thing as a ready server.
func waitReady(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
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
