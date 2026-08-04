package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsWhenNoFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Driver != "sqlite" || c.HTTPListen != ":8080" || len(c.DNSListen) != 1 || c.DNSListen[0] != ":53" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.Storage.DSN != filepath.Join("./data", "dnsaur.db") {
		t.Fatalf("bad default dsn: %s", c.Storage.DSN)
	}
}

func TestFileAndEnvPrecedence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("http_listen: \":9999\"\nstorage:\n  driver: postgres\n  dsn: postgres://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DNSAUR_HTTP_LISTEN", ":7777")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPListen != ":7777" {
		t.Fatalf("env should beat file, got %s", c.HTTPListen)
	}
	if c.Storage.Driver != "postgres" || c.Storage.DSN != "postgres://x" {
		t.Fatalf("file values lost: %+v", c.Storage)
	}
}

func TestInvalidDriver(t *testing.T) {
	t.Setenv("DNSAUR_STORAGE_DRIVER", "mysql")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestPostgresRequiresDSN(t *testing.T) {
	t.Setenv("DNSAUR_STORAGE_DRIVER", "postgres")
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected error for postgres without dsn")
	}
}
