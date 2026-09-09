package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noConfigFile puts the test in a directory with no dnsaur.yaml in it, so
// Load(DefaultPath) exercises the "no config file at all" setup rather than
// picking up whatever happens to sit in the working directory.
func noConfigFile(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	return DefaultPath
}

func TestDefaultsWhenNoFile(t *testing.T) {
	c, err := Load(noConfigFile(t))
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

// TestTrustedProxies covers the one bootstrap key whose value decides
// whether a forwarded header is believed at all: written either way (YAML
// list or env), a bare address accepted as a single host, and a value that
// is not an address refused at startup rather than silently ignored — a
// typo'd CIDR that loaded as "trust nothing" would ship the session cookie
// without Secure and say nothing.
func TestTrustedProxies(t *testing.T) {
	if c, err := Load(noConfigFile(t)); err != nil || len(c.TrustedProxies) != 0 {
		t.Fatalf("default should trust nothing: %v %v", c.TrustedProxies, err)
	}

	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("trusted_proxies:\n  - 10.0.0.0/8\n  - 192.168.1.5\n  - \"::1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(c.TrustedProxies))
	for _, pfx := range c.TrustedProxies {
		got = append(got, pfx.String())
	}
	want := "10.0.0.0/8 192.168.1.5/32 ::1/128"
	if strings.Join(got, " ") != want {
		t.Fatalf("trusted_proxies = %v, want %s", got, want)
	}

	t.Setenv("DNSAUR_TRUSTED_PROXIES", "172.16.0.0/12, 10.1.2.3")
	c, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 || c.TrustedProxies[0].String() != "172.16.0.0/12" || c.TrustedProxies[1].String() != "10.1.2.3/32" {
		t.Fatalf("env should beat file: %v", c.TrustedProxies)
	}

	t.Setenv("DNSAUR_TRUSTED_PROXIES", "10.0.0.0/8, not-an-address")
	if _, err := Load(p); err == nil {
		t.Fatal("a trusted_proxies entry that is not an address or CIDR must be refused")
	}
}

func TestInvalidDriver(t *testing.T) {
	t.Setenv("DNSAUR_STORAGE_DRIVER", "mysql")
	if _, err := Load(noConfigFile(t)); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestPostgresRequiresDSN(t *testing.T) {
	t.Setenv("DNSAUR_STORAGE_DRIVER", "postgres")
	if _, err := Load(noConfigFile(t)); err == nil {
		t.Fatal("expected error for postgres without dsn")
	}
}

// A named config file that is not there is a typo, not a deployment running
// without one: starting on defaults hides the typo behind a server that
// answers but resolves nothing the operator configured.
func TestExplicitMissingFileRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dnsuar.yaml") // the classic transposition
	_, err := Load(p)
	if err == nil {
		t.Fatal("a config path that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error should name the path it could not find, got %v", err)
	}
}

func TestMalformedYAMLRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("dns_listen: [\":53\"\nhttp_listen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

// dns_listen written as a scalar is the natural thing to type for a single
// address, and koanf's Strings returns nothing for it. Unchecked, that bound
// no DNS listener at all and the server ran answering no queries.
func TestScalarDNSListen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("dns_listen: \":5353\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.DNSListen) != 1 || c.DNSListen[0] != ":5353" {
		t.Fatalf("scalar dns_listen = %#v, want [\":5353\"]", c.DNSListen)
	}
}

func TestEmptyDNSListenRejected(t *testing.T) {
	t.Run("empty list in the file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte("dns_listen: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("an empty dns_listen was accepted; the server would bind no DNS listener")
		}
	})
	t.Run("empty string in the file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte("dns_listen: \"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("an empty scalar dns_listen was accepted")
		}
	})
	t.Run("env naming nothing", func(t *testing.T) {
		t.Setenv("DNSAUR_DNS_LISTEN", ",")
		if _, err := Load(noConfigFile(t)); err == nil {
			t.Fatal("DNSAUR_DNS_LISTEN=\",\" was accepted")
		}
	})
}

// The env list is hand-written, so it arrives with the spacing a human
// types. Every entry goes to net.Listen verbatim, and " :5353" does not
// bind.
func TestEnvDNSListenParsing(t *testing.T) {
	t.Setenv("DNSAUR_DNS_LISTEN", " :53, 127.0.0.1:5353 ,")
	c, err := Load(noConfigFile(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{":53", "127.0.0.1:5353"}
	if len(c.DNSListen) != len(want) {
		t.Fatalf("DNSListen = %#v, want %#v", c.DNSListen, want)
	}
	for i := range want {
		if c.DNSListen[i] != want[i] {
			t.Fatalf("DNSListen = %#v, want %#v", c.DNSListen, want)
		}
	}
}

// A file list and an env list naming the same addresses have to produce the
// same value: they are the same configuration written two ways, and only one
// of them was being trimmed.
func TestFileAndEnvDNSListenAgree(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("dns_listen:\n  - \" :53\"\n  - \"127.0.0.1:5353 \"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fromFile, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DNSAUR_DNS_LISTEN", " :53, 127.0.0.1:5353")
	fromEnv, err := Load(noConfigFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fromFile.DNSListen, "|") != strings.Join(fromEnv.DNSListen, "|") {
		t.Fatalf("file %#v and env %#v disagree", fromFile.DNSListen, fromEnv.DNSListen)
	}
}

// An unparseable log_level used to be silently INFO, so an operator asking
// for debug output got none and nothing pointing at why.
func TestInvalidLogLevelRejected(t *testing.T) {
	t.Setenv("DNSAUR_LOG_LEVEL", "verbose")
	_, err := Load(noConfigFile(t))
	if err == nil {
		t.Fatal("log_level \"verbose\" was accepted")
	}
	for _, want := range []string{"debug", "info", "warn", "error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the accepted levels, missing %q: %v", want, err)
		}
	}
}

func TestValidLogLevelsAccepted(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "DEBUG"} {
		t.Setenv("DNSAUR_LOG_LEVEL", lvl)
		if _, err := Load(noConfigFile(t)); err != nil {
			t.Errorf("log_level %q rejected: %v", lvl, err)
		}
	}
}
