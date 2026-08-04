package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMain(m *testing.M) {
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
		_ = os.Setenv("DNSAUR_TEST_POSTGRES_DSN", fmt.Sprintf("postgres://postgres:t@%s:%s/dnsaur?sslmode=disable", host, port.Port()))
		defer func() { _ = pg.Terminate(ctx) }()
	}
	os.Exit(m.Run())
}
