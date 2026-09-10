package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/aloks98/dnsaur/internal/storetest"
)

// TestMain lives in the external test package because storetest imports
// store, which an in-package test file could not import back. It runs in
// run() rather than inline, because os.Exit skips deferred calls and the
// container's stop is one of them.
//
// Unlike internal/zones and internal/app there is no template database:
// openPostgres (store_test.go) shares one database across the postgres
// halves and isolates them by row name instead, so the DSN goes into the
// environment for it to find.
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dsn, stop := storetest.Start(context.Background(), "store")
	defer stop()
	if dsn != "" {
		_ = os.Setenv("DNSAUR_TEST_POSTGRES_DSN", dsn)
	}
	return m.Run()
}
