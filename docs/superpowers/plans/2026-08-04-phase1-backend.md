# dnsaur Phase 1 Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A working headless blocking DNS resolver: UDP/TCP listeners, middleware pipeline (client-id → filter → local records → cache → upstream), blocklist engine, query log, and stats — configured via bootstrap YAML + DB-stored settings.

**Architecture:** Single Go binary. `miekg/dns` handles wire format; we own a middleware pipeline where each stage answers or passes on. All state flows through `internal/store` interfaces (SQLite default, Postgres optional, one `database/sql` implementation with dialect-aware migrations). Filtering uses an in-memory reversed-label trie rebuilt by a background refresher.

**Tech Stack:** Go 1.26, `github.com/miekg/dns`, `modernc.org/sqlite` (no CGO), `github.com/jackc/pgx/v5/stdlib`, `gopkg.in/yaml.v3`, `golang.org/x/sync/singleflight`, `github.com/google/uuid`, `github.com/testcontainers/testcontainers-go` (tests only).

## Global Constraints

- Module path: `github.com/aloks98/dnsaur` (adjust in Task 1 only if the GitHub username differs; nothing else hardcodes it).
- Go 1.26 (project baseline — go.mod and CI both pin 1.26); CGO disabled (`CGO_ENABLED=0`); binary must stay static.
- Logging via stdlib `log/slog` only.
- All timestamps stored as Unix milliseconds (`INTEGER`/`BIGINT`), never TEXT. Sanctioned exception: `stats_hourly.bucket` is hour-start Unix **seconds** (Phase 2 API code must not assume ms there).
- DNS names normalized lowercase without trailing dot internally; `dns.Fqdn()` only at wire boundaries.
- Guiding rule from spec: **DNS must not die** — storage/blocklist/upstream failures degrade features, never resolution.
- Conventional commit messages (`feat:`, `test:`, `chore:`).
- Every task: run `go test -race ./...` before committing.
- Defaults (from spec): blocking mode `null-ip` (0.0.0.0/::, TTL 30), list refresh 24h, log retention 90 days, cache max 10000 entries, serve-stale window 86400s, upstream strategy `race`, upstreams `1.1.1.1:53`, `1.0.0.1:53`, `9.9.9.9:53`.

## File Structure

```
cmd/dnsaur/main.go               — thin main calling internal/app (Task 15)
internal/app/app.go              — component wiring, lifecycle, hot-reload (Task 15)
internal/config/config.go        — bootstrap YAML + env (Task 2)
internal/store/store.go          — interfaces + types (Task 3)
internal/store/sql.go            — database/sql impl, both dialects (Tasks 3-4, 13-14)
internal/store/migrate.go        — embedded migration runner (Task 3)
internal/store/migrations/{sqlite,postgres}/0001_init.sql
internal/dnssrv/pipeline.go      — Handler/Middleware/Chain/Recover (Task 5)
internal/dnssrv/server.go        — UDP/TCP listeners (Task 6)
internal/clients/registry.go     — client-id stage (Task 7)
internal/filter/parse.go         — list format parsers (Task 8)
internal/filter/domainset.go     — reversed-label trie (Task 8)
internal/filter/ruleset.go       — compile + precedence (Task 8)
internal/filter/engine.go        — per-group state, middleware (Task 8)
internal/filter/refresh.go       — downloader + refresh job (Task 9)
internal/records/records.go      — local records stage (Task 10)
internal/cache/cache.go          — TTL cache, negative, serve-stale (Task 11)
internal/upstream/forwarder.go   — terminal handler (Task 12)
internal/qlog/qlog.go            — async query log + pruner (Task 13)
internal/stats/rollup.go         — hourly rollups (Task 14)
```

---

### Task 1: Scaffold, lint, CI

**Files:**
- Create: `go.mod`, `cmd/dnsaur/main.go`, `.golangci.yml`, `.github/workflows/ci.yml`, `.gitignore`

**Interfaces:**
- Produces: `main.run(ctx) error` skeleton later tasks extend; `Version` var set via ldflags.

- [ ] **Step 1: Init module and main skeleton**

```bash
go mod init github.com/aloks98/dnsaur
```

`cmd/dnsaur/main.go`:
```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	fmt.Println("dnsaur", Version)
	<-ctx.Done()
	return nil
}
```

`.gitignore`:
```
/dnsaur
/data/
*.db
```

- [ ] **Step 2: Lint config**

`.golangci.yml`:
```yaml
linters:
  enable: [govet, staticcheck, errcheck, ineffassign, unused, misspell]
run:
  timeout: 5m
```

- [ ] **Step 3: CI workflow**

`.github/workflows/ci.yml`:
```yaml
name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.26" }
      - uses: golangci/golangci-lint-action@v6
      - run: go test -race ./...
      - run: CGO_ENABLED=0 go build -ldflags "-X main.Version=${GITHUB_SHA::7}" ./cmd/dnsaur
```

- [ ] **Step 4: Verify build**

Run: `CGO_ENABLED=0 go build ./... && go vet ./...`
Expected: clean exit.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "chore: scaffold module, main skeleton, lint, CI"
```

---

### Task 2: Bootstrap config

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Load(path string) (*Config, error)`; `Config{DNSListen []string; HTTPListen string; DataDir string; LogLevel string; Storage struct{Driver, DSN string}}`. Defaults: DNSListen `[":53"]`, HTTPListen `":8080"`, DataDir `"./data"`, LogLevel `"info"`, Driver `"sqlite"`, DSN `"<DataDir>/dnsaur.db"`. Env overrides: `DNSAUR_DNS_LISTEN` (comma-sep), `DNSAUR_HTTP_LISTEN`, `DNSAUR_DATA_DIR`, `DNSAUR_LOG_LEVEL`, `DNSAUR_STORAGE_DRIVER`, `DNSAUR_STORAGE_DSN`. Env beats file beats defaults. Missing file is not an error; unknown driver is.

- [ ] **Step 1: Write failing tests**

`internal/config/config_test.go`:
```go
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
	os.WriteFile(p, []byte("http_listen: \":9999\"\nstorage:\n  driver: postgres\n  dsn: postgres://x\n"), 0o644)
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
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/config/ -v`
Expected: FAIL (package missing / `Load` undefined).

- [ ] **Step 3: Implement**

`internal/config/config.go`:
```go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DNSListen  []string `yaml:"dns_listen"`
	HTTPListen string   `yaml:"http_listen"`
	DataDir    string   `yaml:"data_dir"`
	LogLevel   string   `yaml:"log_level"`
	Storage    struct {
		Driver string `yaml:"driver"`
		DSN    string `yaml:"dsn"`
	} `yaml:"storage"`
}

func Load(path string) (*Config, error) {
	c := &Config{DNSListen: []string{":53"}, HTTPListen: ":8080", DataDir: "./data", LogLevel: "info"}
	c.Storage.Driver = "sqlite"
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if v := os.Getenv("DNSAUR_DNS_LISTEN"); v != "" {
		c.DNSListen = strings.Split(v, ",")
	}
	for _, e := range []struct {
		env string
		dst *string
	}{
		{"DNSAUR_HTTP_LISTEN", &c.HTTPListen}, {"DNSAUR_DATA_DIR", &c.DataDir},
		{"DNSAUR_LOG_LEVEL", &c.LogLevel}, {"DNSAUR_STORAGE_DRIVER", &c.Storage.Driver},
		{"DNSAUR_STORAGE_DSN", &c.Storage.DSN},
	} {
		if v := os.Getenv(e.env); v != "" {
			*e.dst = v
		}
	}
	if c.Storage.Driver != "sqlite" && c.Storage.Driver != "postgres" {
		return nil, fmt.Errorf("unknown storage driver %q", c.Storage.Driver)
	}
	if c.Storage.DSN == "" {
		if c.Storage.Driver == "postgres" {
			return nil, fmt.Errorf("storage.dsn required for postgres")
		}
		c.Storage.DSN = filepath.Join(c.DataDir, "dnsaur.db")
	}
	return c, nil
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/config/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config && git commit -m "feat: bootstrap config with YAML + env overrides"
```

---

### Task 3: Store interfaces, migrations, SQL implementation

**Files:**
- Create: `internal/store/store.go`, `internal/store/migrate.go`, `internal/store/sql.go`, `internal/store/migrations/sqlite/0001_init.sql`, `internal/store/migrations/postgres/0001_init.sql`, `internal/store/store_test.go`

**Interfaces:**
- Produces:
  - `store.Open(ctx context.Context, driver, dsn string) (Store, error)`
  - `type Store interface { Settings() SettingsStore; Clients() ClientStore; Filters() FilterStore; Records() RecordStore; QueryLog() QueryLogStore; Stats() StatsStore; Close() error }`
  - Types: `Group{ID int64; Name string; Enabled bool}`, `Client{ID int64; Name, Matcher string; GroupID int64}` (Matcher = IP or CIDR string), `List{ID int64; URL, Kind string; Enabled bool; LastRefreshed, EntryCount int64}` (Kind `block`|`allow`), `Rule{ID, GroupID int64; Action, Pattern string; IsRegex bool}` (Action `allow`|`block`), `LocalRecord{ID int64; Name, Type, Value string; TTL uint32}`, `QueryLogEntry{ID int64; At int64; InstanceID, ClientIP string; ClientID int64; QName, QType, Decision string; RuleID, ListID int64; Upstream, RCode string; DurationMs int64}`.
  - `ClientStore{ Groups(ctx) ([]Group, error); Clients(ctx) ([]Client, error); AddGroup(ctx, name string) (int64, error); AddClient(ctx, c Client) (int64, error) }`
  - `FilterStore{ Lists(ctx) ([]List, error); ListsForGroup(ctx, groupID int64) ([]List, error); Rules(ctx, groupID int64) ([]Rule, error); AddList(ctx, l List) (int64, error); AssignList(ctx, groupID, listID int64) error; AddRule(ctx, r Rule) (int64, error); TouchList(ctx, id, refreshedAt, entryCount int64) error }`
  - `RecordStore{ All(ctx) ([]LocalRecord, error); Add(ctx, r LocalRecord) (int64, error) }`
  - `QueryLogStore` and `StatsStore` are defined here but implemented in Tasks 13–14: declare `QueryLogStore{ InsertBatch(ctx, []QueryLogEntry) error; DeleteBefore(ctx, cutoffMs int64) (int64, error) }`, `StatsStore{ Rollup(ctx, fromID int64) (lastID int64, err error); Counter(ctx, bucketFromMs int64, metric string) (map[string]int64, error) }`.
- Migration files named `NNNN_name.sql`, applied in order inside a transaction, tracked in `schema_migrations(version INTEGER PRIMARY KEY)`.

- [ ] **Step 1: Write failing tests**

`internal/store/store_test.go`:
```go
package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func openSQLite(t *testing.T) Store {
	t.Helper()
	s, err := Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openPostgres(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("DNSAUR_TEST_POSTGRES_DSN") // set by TestMain via testcontainers when Docker present
	if dsn == "" {
		t.Skip("no postgres available")
	}
	s, err := Open(context.Background(), "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func forEachDriver(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, openSQLite(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, openPostgres(t)) })
}

func TestMigrateIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "t.db")
	ctx := context.Background()
	s, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, "sqlite", dsn) // re-open re-runs migrator; must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestClientAndFilterCRUD(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		gid, err := s.Clients().AddGroup(ctx, "kids")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Clients().AddClient(ctx, Client{Name: "tablet", Matcher: "10.0.0.5", GroupID: gid}); err != nil {
			t.Fatal(err)
		}
		lid, err := s.Filters().AddList(ctx, List{URL: "https://example.com/hosts", Kind: "block", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Filters().AssignList(ctx, gid, lid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Filters().AddRule(ctx, Rule{GroupID: gid, Action: "allow", Pattern: "ok.example.com"}); err != nil {
			t.Fatal(err)
		}
		ls, err := s.Filters().ListsForGroup(ctx, gid)
		if err != nil || len(ls) != 1 || ls[0].URL != "https://example.com/hosts" {
			t.Fatalf("lists: %v %v", ls, err)
		}
		rs, err := s.Filters().Rules(ctx, gid)
		if err != nil || len(rs) != 1 || rs[0].Action != "allow" {
			t.Fatalf("rules: %v %v", rs, err)
		}
	})
}

func TestLocalRecords(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if _, err := s.Records().Add(ctx, LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300}); err != nil {
			t.Fatal(err)
		}
		all, err := s.Records().All(ctx)
		if err != nil || len(all) != 1 || all[0].Value != "10.0.0.9" {
			t.Fatalf("records: %v %v", all, err)
		}
	})
}
```

Also create `internal/store/main_test.go` starting a Postgres testcontainer when Docker is available:
```go
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
		os.Setenv("DNSAUR_TEST_POSTGRES_DSN", fmt.Sprintf("postgres://postgres:t@%s:%s/dnsaur?sslmode=disable", host, port.Port()))
		defer pg.Terminate(ctx)
	}
	os.Exit(m.Run())
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/store/ -v` — Expected: FAIL (`Open` undefined).

- [ ] **Step 3: Write migrations**

`internal/store/migrations/sqlite/0001_init.sql`:
```sql
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE config_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL);
INSERT INTO config_version (id, version) VALUES (1, 1);
CREATE TABLE groups (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE clients (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  matcher TEXT NOT NULL UNIQUE,
  group_id INTEGER NOT NULL REFERENCES groups(id)
);
CREATE TABLE lists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  url TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL DEFAULT 'block',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_refreshed INTEGER NOT NULL DEFAULT 0,
  entry_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE group_lists (
  group_id INTEGER NOT NULL REFERENCES groups(id),
  list_id INTEGER NOT NULL REFERENCES lists(id),
  PRIMARY KEY (group_id, list_id)
);
CREATE TABLE rules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  group_id INTEGER NOT NULL REFERENCES groups(id),
  action TEXT NOT NULL,
  pattern TEXT NOT NULL,
  is_regex INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE local_records (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  value TEXT NOT NULL,
  ttl INTEGER NOT NULL DEFAULT 300
);
CREATE TABLE query_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at INTEGER NOT NULL,
  instance_id TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  client_id INTEGER NOT NULL DEFAULT 0,
  qname TEXT NOT NULL,
  qtype TEXT NOT NULL,
  decision TEXT NOT NULL,
  rule_id INTEGER NOT NULL DEFAULT 0,
  list_id INTEGER NOT NULL DEFAULT 0,
  upstream TEXT NOT NULL DEFAULT '',
  rcode TEXT NOT NULL,
  duration_ms INTEGER NOT NULL
);
CREATE INDEX idx_qlog_at ON query_log(at);
CREATE INDEX idx_qlog_qname ON query_log(qname);
CREATE TABLE stats_hourly (
  bucket INTEGER NOT NULL,
  metric TEXT NOT NULL,
  key TEXT NOT NULL,
  value INTEGER NOT NULL,
  PRIMARY KEY (bucket, metric, key)
);
```

`internal/store/migrations/postgres/0001_init.sql`: identical except:
```sql
-- same statements, with these substitutions:
--   INTEGER PRIMARY KEY AUTOINCREMENT  ->  BIGSERIAL PRIMARY KEY
--   all other INTEGER                  ->  BIGINT
--   enabled/is_regex INTEGER DEFAULT 1/0 -> BOOLEAN NOT NULL DEFAULT true/false
```
Write the postgres file out in full with those substitutions applied (it is the same 11 statements; no other changes).

- [ ] **Step 4: Implement migrator**

`internal/store/migrate.go`:
```go
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations
var migrationsFS embed.FS

func migrate(ctx context.Context, db *sql.DB, dialect string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations/" + dialect)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		ver, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %s: %w", name, err)
		}
		var n int
		if err := db.QueryRowContext(ctx, rebind(dialect, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`), ver).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + dialect + "/" + name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, rebind(dialect, `INSERT INTO schema_migrations (version) VALUES (?)`), ver); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// rebind converts ? placeholders to $1,$2… for postgres.
func rebind(dialect, q string) string {
	if dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
```

- [ ] **Step 5: Implement store**

`internal/store/store.go` — the interface + type declarations exactly as in the Interfaces block above, plus:
```go
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func Open(ctx context.Context, driver, dsn string) (Store, error) {
	var drvName string
	switch driver {
	case "sqlite":
		drvName = "sqlite"
		dsn = dsn + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	case "postgres":
		drvName = "pgx"
	default:
		return nil, fmt.Errorf("unknown driver %q", driver)
	}
	db, err := sql.Open(drvName, dsn)
	if err != nil {
		return nil, err
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, db, driver); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlStore{db: db, dialect: driver}, nil
}
```

`internal/store/sql.go`:
```go
package store

import (
	"context"
	"database/sql"
)

type sqlStore struct {
	db      *sql.DB
	dialect string
}

func (s *sqlStore) Close() error         { return s.db.Close() }
func (s *sqlStore) Clients() ClientStore { return &clientStore{s} }
func (s *sqlStore) Filters() FilterStore { return &filterStore{s} }
func (s *sqlStore) Records() RecordStore { return &recordStore{s} }
// Settings()/QueryLog()/Stats() accessors are added by Tasks 4, 13, 14.

func (s *sqlStore) q(q string) string { return rebind(s.dialect, q) }

// insert runs an INSERT and returns the new row id, handling the
// sqlite (LastInsertId) vs postgres (RETURNING) difference in one place.
func (s *sqlStore) insert(ctx context.Context, q string, args ...any) (int64, error) {
	if s.dialect == "postgres" {
		var id int64
		err := s.db.QueryRowContext(ctx, s.q(q+" RETURNING id"), args...).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, s.q(q), args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type clientStore struct{ s *sqlStore }

func (c *clientStore) AddGroup(ctx context.Context, name string) (int64, error) {
	return c.s.insert(ctx, `INSERT INTO groups (name) VALUES (?)`, name)
}

func (c *clientStore) AddClient(ctx context.Context, cl Client) (int64, error) {
	return c.s.insert(ctx, `INSERT INTO clients (name, matcher, group_id) VALUES (?, ?, ?)`, cl.Name, cl.Matcher, cl.GroupID)
}

func (c *clientStore) Groups(ctx context.Context) ([]Group, error) {
	rows, err := c.s.db.QueryContext(ctx, `SELECT id, name, enabled FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Enabled); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (c *clientStore) Clients(ctx context.Context) ([]Client, error) {
	rows, err := c.s.db.QueryContext(ctx, `SELECT id, name, matcher, group_id FROM clients ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		var cl Client
		if err := rows.Scan(&cl.ID, &cl.Name, &cl.Matcher, &cl.GroupID); err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

type filterStore struct{ s *sqlStore }

func (f *filterStore) AddList(ctx context.Context, l List) (int64, error) {
	return f.s.insert(ctx, `INSERT INTO lists (url, kind, enabled) VALUES (?, ?, ?)`, l.URL, l.Kind, l.Enabled)
}

func (f *filterStore) AssignList(ctx context.Context, groupID, listID int64) error {
	_, err := f.s.db.ExecContext(ctx, f.s.q(`INSERT INTO group_lists (group_id, list_id) VALUES (?, ?)`), groupID, listID)
	return err
}

func (f *filterStore) AddRule(ctx context.Context, r Rule) (int64, error) {
	return f.s.insert(ctx, `INSERT INTO rules (group_id, action, pattern, is_regex) VALUES (?, ?, ?, ?)`, r.GroupID, r.Action, r.Pattern, r.IsRegex)
}

func (f *filterStore) TouchList(ctx context.Context, id, refreshedAt, entryCount int64) error {
	_, err := f.s.db.ExecContext(ctx, f.s.q(`UPDATE lists SET last_refreshed = ?, entry_count = ? WHERE id = ?`), refreshedAt, entryCount, id)
	return err
}

func (f *filterStore) scanLists(rows *sql.Rows) ([]List, error) {
	defer rows.Close()
	var out []List
	for rows.Next() {
		var l List
		if err := rows.Scan(&l.ID, &l.URL, &l.Kind, &l.Enabled, &l.LastRefreshed, &l.EntryCount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (f *filterStore) Lists(ctx context.Context) ([]List, error) {
	rows, err := f.s.db.QueryContext(ctx, `SELECT id, url, kind, enabled, last_refreshed, entry_count FROM lists ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return f.scanLists(rows)
}

func (f *filterStore) ListsForGroup(ctx context.Context, groupID int64) ([]List, error) {
	rows, err := f.s.db.QueryContext(ctx, f.s.q(`SELECT l.id, l.url, l.kind, l.enabled, l.last_refreshed, l.entry_count
		FROM lists l JOIN group_lists gl ON gl.list_id = l.id WHERE gl.group_id = ? ORDER BY l.id`), groupID)
	if err != nil {
		return nil, err
	}
	return f.scanLists(rows)
}

func (f *filterStore) Rules(ctx context.Context, groupID int64) ([]Rule, error) {
	rows, err := f.s.db.QueryContext(ctx, f.s.q(`SELECT id, group_id, action, pattern, is_regex FROM rules WHERE group_id = ? ORDER BY id`), groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.GroupID, &r.Action, &r.Pattern, &r.IsRegex); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type recordStore struct{ s *sqlStore }

func (r *recordStore) Add(ctx context.Context, rec LocalRecord) (int64, error) {
	return r.s.insert(ctx, `INSERT INTO local_records (name, type, value, ttl) VALUES (?, ?, ?, ?)`, rec.Name, rec.Type, rec.Value, rec.TTL)
}

func (r *recordStore) All(ctx context.Context) ([]LocalRecord, error) {
	rows, err := r.s.db.QueryContext(ctx, `SELECT id, name, type, value, ttl FROM local_records ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalRecord
	for rows.Next() {
		var rec LocalRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.Value, &rec.TTL); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
```
The `Store` interface in this task contains only `Clients()`, `Filters()`, `Records()`, `Close()`. Tasks 4, 13, and 14 each add their accessor (`Settings()`, `QueryLog()`, `Stats()`) to the interface when they implement it — do not stub them earlier.

- [ ] **Step 6: Run tests, verify pass**

Run: `go test -race ./internal/store/ -v` — Expected: PASS (postgres subtests skip without Docker).

- [ ] **Step 7: Commit**

```bash
git add internal/store go.mod go.sum && git commit -m "feat: store layer with migrations for sqlite and postgres"
```

---

### Task 4: Settings store, defaults seeding, change notification

**Files:**
- Modify: `internal/store/store.go` (add `Settings()` to interface), `internal/store/sql.go`
- Create: `internal/store/settings.go`, `internal/store/settings_test.go`

**Interfaces:**
- Produces:
  - `SettingsStore{ Get(ctx, key string) (string, bool, error); Set(ctx, key, value string) error; ConfigVersion(ctx) (int64, error); Changes() <-chan int64; SeedDefaults(ctx, defaults map[string]string) error; GetInt(ctx, key string) (int64, error); }`
  - `Set` bumps `config_version` in the same transaction and (best-effort, non-blocking) publishes the new version on `Changes()`.
  - `SeedDefaults` inserts only missing keys, without bumping the version.
  - `GetInt` parses the stored string; returns error if missing or non-numeric (callers seed defaults first).
  - Canonical settings keys and defaults, seeded by `cmd/dnsaur` (Task 15): `instance.id` (uuid), `upstreams` (`1.1.1.1:53,1.0.0.1:53,9.9.9.9:53`), `upstream.strategy` (`race`), `blocking.mode` (`null-ip`), `blocking.ttl` (`30`), `cache.min_ttl` (`0`), `cache.max_ttl` (`86400`), `cache.max_entries` (`10000`), `cache.serve_stale_for` (`86400`), `lists.refresh_hours` (`24`), `qlog.retention_days` (`90`), `qlog.privacy` (`full`).

- [ ] **Step 1: Write failing tests**

`internal/store/settings_test.go`:
```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestSettingsSetBumpsVersionAndNotifies(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		v0, err := s.Settings().ConfigVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ch := s.Settings().Changes()
		if err := s.Settings().Set(ctx, "blocking.mode", "nxdomain"); err != nil {
			t.Fatal(err)
		}
		v1, _ := s.Settings().ConfigVersion(ctx)
		if v1 != v0+1 {
			t.Fatalf("version not bumped: %d -> %d", v0, v1)
		}
		select {
		case got := <-ch:
			if got != v1 {
				t.Fatalf("notified %d want %d", got, v1)
			}
		case <-time.After(time.Second):
			t.Fatal("no change notification")
		}
		val, ok, _ := s.Settings().Get(ctx, "blocking.mode")
		if !ok || val != "nxdomain" {
			t.Fatalf("get: %q %v", val, ok)
		}
	})
}

func TestSeedDefaultsOnlyFillsMissing(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		s.Settings().Set(ctx, "blocking.ttl", "60")
		v0, _ := s.Settings().ConfigVersion(ctx)
		if err := s.Settings().SeedDefaults(ctx, map[string]string{"blocking.ttl": "30", "blocking.mode": "null-ip"}); err != nil {
			t.Fatal(err)
		}
		ttl, _ := s.Settings().GetInt(ctx, "blocking.ttl")
		if ttl != 60 {
			t.Fatalf("seed overwrote existing value: %d", ttl)
		}
		mode, ok, _ := s.Settings().Get(ctx, "blocking.mode")
		if !ok || mode != "null-ip" {
			t.Fatalf("seed missed absent key: %q", mode)
		}
		if v1, _ := s.Settings().ConfigVersion(ctx); v1 != v0 {
			t.Fatal("seeding must not bump config version")
		}
	})
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/store/ -run TestSettings -v` — Expected: FAIL (`Settings` undefined).

- [ ] **Step 3: Implement**

`internal/store/settings.go`:
```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

type settingsStore struct {
	s *sqlStore
}

// one shared notification hub per sqlStore
type notifyHub struct {
	mu   sync.Mutex
	subs []chan int64
}

func (h *notifyHub) subscribe() <-chan int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan int64, 8)
	h.subs = append(h.subs, ch)
	return ch
}

func (h *notifyHub) publish(v int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- v:
		default: // never block a writer on a slow subscriber
		}
	}
}

func (st *settingsStore) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := st.s.db.QueryRowContext(ctx, st.s.q(`SELECT value FROM settings WHERE key = ?`), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (st *settingsStore) GetInt(ctx context.Context, key string) (int64, error) {
	v, ok, err := st.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("setting %q not set", key)
	}
	return strconv.ParseInt(v, 10, 64)
}

func (st *settingsStore) Set(ctx context.Context, key, value string) error {
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, st.s.q(upsert), key, value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE config_version SET version = version + 1 WHERE id = 1`); err != nil {
		return err
	}
	var v int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM config_version WHERE id = 1`).Scan(&v); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	st.s.hub.publish(v)
	return nil
}

func (st *settingsStore) SeedDefaults(ctx context.Context, defaults map[string]string) error {
	for k, v := range defaults {
		if _, err := st.s.db.ExecContext(ctx, st.s.q(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`), k, v); err != nil {
			return err
		}
	}
	return nil
}

func (st *settingsStore) ConfigVersion(ctx context.Context) (int64, error) {
	var v int64
	err := st.s.db.QueryRowContext(ctx, `SELECT version FROM config_version WHERE id = 1`).Scan(&v)
	return v, err
}

func (st *settingsStore) Changes() <-chan int64 { return st.s.hub.subscribe() }
```

In `sql.go`, add `hub notifyHub` field to `sqlStore`, add `func (s *sqlStore) Settings() SettingsStore { return &settingsStore{s: s} }`, and add `Settings() SettingsStore` to the `Store` interface in `store.go`.

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/store/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store && git commit -m "feat: settings store with version bump and change notification"
```

---

### Task 5: Pipeline core

**Files:**
- Create: `internal/dnssrv/pipeline.go`, `internal/dnssrv/pipeline_test.go`

**Interfaces:**
- Produces (every later task consumes these):
  - `type Decision string` with constants `DecisionAllowed("allowed")`, `DecisionBlocked("blocked")`, `DecisionLocal("local")`, `DecisionCached("cached")`, `DecisionStale("stale")`, `DecisionForwarded("forwarded")`, `DecisionError("error")`.
  - `type ClientInfo struct { ID int64; Name string; GroupID int64; GroupName string }`
  - `type Request struct { Msg *dns.Msg; ClientIP netip.Addr; Client ClientInfo }` with helpers `func (r *Request) QName() string` (lowercase, no trailing dot) and `func (r *Request) QType() uint16`.
  - `type Response struct { Msg *dns.Msg; Decision Decision; Upstream string; RuleID, ListID int64 }`
  - `type Handler interface { ServeDNS(ctx context.Context, req *Request) (*Response, error) }`, `type HandlerFunc func(...)` implementing it, `type Middleware func(next Handler) Handler`, `func Chain(terminal Handler, mws ...Middleware) Handler` (mws[0] is outermost).
  - `func Servfail(req *Request) *Response` — reply with rcode SERVFAIL, Decision `error`.
  - `func Recover() Middleware` — converts panics into `Servfail` + slog error.

- [ ] **Step 1: Write failing tests**

`internal/dnssrv/pipeline_test.go`:
```go
package dnssrv

import (
	"context"
	"testing"

	"github.com/miekg/dns"
)

func q(name string, qtype uint16) *Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &Request{Msg: m}
}

func TestChainOrderAndShortCircuit(t *testing.T) {
	var order []string
	mw := func(name string, answer bool) Middleware {
		return func(next Handler) Handler {
			return HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
				order = append(order, name)
				if answer {
					return &Response{Msg: new(dns.Msg), Decision: DecisionLocal}, nil
				}
				return next.ServeDNS(ctx, req)
			})
		}
	}
	terminal := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		order = append(order, "terminal")
		return &Response{Msg: new(dns.Msg), Decision: DecisionForwarded}, nil
	})
	resp, err := Chain(terminal, mw("a", false), mw("b", true)).ServeDNS(context.Background(), q("x.test", dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionLocal {
		t.Fatalf("decision %s", resp.Decision)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order %v", order)
	}
}

func TestQNameNormalized(t *testing.T) {
	req := q("WWW.Example.COM.", dns.TypeA)
	if req.QName() != "www.example.com" {
		t.Fatalf("got %q", req.QName())
	}
}

func TestRecoverConvertsPanic(t *testing.T) {
	h := Chain(HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		panic("boom")
	}), Recover())
	resp, err := h.ServeDNS(context.Background(), q("x.test", dns.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionError || resp.Msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("panic not converted: %+v", resp)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/dnssrv/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/dnssrv/pipeline.go`:
```go
package dnssrv

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

type Decision string

const (
	DecisionAllowed   Decision = "allowed"
	DecisionBlocked   Decision = "blocked"
	DecisionLocal     Decision = "local"
	DecisionCached    Decision = "cached"
	DecisionStale     Decision = "stale"
	DecisionForwarded Decision = "forwarded"
	DecisionError     Decision = "error"
)

type ClientInfo struct {
	ID        int64
	Name      string
	GroupID   int64
	GroupName string
}

type Request struct {
	Msg      *dns.Msg
	ClientIP netip.Addr
	Client   ClientInfo
}

func (r *Request) QName() string {
	if len(r.Msg.Question) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(r.Msg.Question[0].Name, "."))
}

func (r *Request) QType() uint16 {
	if len(r.Msg.Question) == 0 {
		return 0
	}
	return r.Msg.Question[0].Qtype
}

type Response struct {
	Msg      *dns.Msg
	Decision Decision
	Upstream string
	RuleID   int64
	ListID   int64
}

type Handler interface {
	ServeDNS(ctx context.Context, req *Request) (*Response, error)
}

type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

func (f HandlerFunc) ServeDNS(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

type Middleware func(next Handler) Handler

func Chain(terminal Handler, mws ...Middleware) Handler {
	h := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func Servfail(req *Request) *Response {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, dns.RcodeServerFailure)
	return &Response{Msg: m, Decision: DecisionError}
}

func Recover() Middleware {
	return func(next Handler) Handler {
		return HandlerFunc(func(ctx context.Context, req *Request) (resp *Response, err error) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("panic in dns pipeline", "qname", req.QName(), "panic", r)
					resp, err = Servfail(req), nil
				}
			}()
			return next.ServeDNS(ctx, req)
		})
	}
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/dnssrv/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dnssrv && git commit -m "feat: dns middleware pipeline core with panic recovery"
```

---

### Task 6: UDP/TCP listeners

**Files:**
- Create: `internal/dnssrv/server.go`, `internal/dnssrv/server_test.go`

**Interfaces:**
- Consumes: `Handler`, `Request`, `Servfail` from Task 5.
- Produces: `dnssrv.NewServer(addr string, h Handler) *Server`; `(*Server).Start() error` (starts UDP+TCP, non-blocking, returns first listen error); `(*Server).Shutdown(ctx) error`; `(*Server).Addr() string` (actual bound UDP address, for tests using `:0`... note: pass port `127.0.0.1:0`). Per-query timeout 5s. UDP responses truncated to the client's advertised EDNS0 size (512 default) via `msg.Truncate`.

- [ ] **Step 1: Write failing test**

`internal/dnssrv/server_test.go`:
```go
package dnssrv

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestServerServesPipeline(t *testing.T) {
	h := HandlerFunc(func(ctx context.Context, req *Request) (*Response, error) {
		if !req.ClientIP.IsValid() || !req.ClientIP.IsLoopback() {
			t.Errorf("client ip not set: %v", req.ClientIP)
		}
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 1.2.3.4")
		m.Answer = append(m.Answer, rr)
		return &Response{Msg: m, Decision: DecisionForwarded}, nil
	})
	s := NewServer("127.0.0.1:0", h)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())

	c := new(dns.Client)
	m := new(dns.Msg)
	m.SetQuestion("hello.test.", dns.TypeA)
	for _, net := range []string{"udp", "tcp"} {
		c.Net = net
		r, _, err := c.Exchange(m, s.Addr())
		if err != nil {
			t.Fatalf("%s exchange: %v", net, err)
		}
		if len(r.Answer) != 1 {
			t.Fatalf("%s: no answer", net)
		}
	}
	_ = netip.Addr{}
	_ = time.Second
}
```

- [ ] **Step 2: Run test, verify failure**

Run: `go test ./internal/dnssrv/ -run TestServer -v` — Expected: FAIL (`NewServer` undefined).

- [ ] **Step 3: Implement**

`internal/dnssrv/server.go`:
```go
package dnssrv

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

type Server struct {
	addr    string
	handler Handler
	udp     *dns.Server
	tcp     *dns.Server
	bound   string
}

func NewServer(addr string, h Handler) *Server {
	return &Server{addr: addr, handler: h}
}

func (s *Server) Start() error {
	pc, err := net.ListenPacket("udp", s.addr)
	if err != nil {
		return err
	}
	s.bound = pc.LocalAddr().String()
	ln, err := net.Listen("tcp", s.bound)
	if err != nil {
		pc.Close()
		return err
	}
	mux := dns.HandlerFunc(s.serve)
	s.udp = &dns.Server{PacketConn: pc, Handler: mux}
	s.tcp = &dns.Server{Listener: ln, Handler: mux}
	go s.udp.ActivateAndServe()
	go s.tcp.ActivateAndServe()
	return nil
}

func (s *Server) Addr() string { return s.bound }

func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		if srv != nil {
			if err := srv.ShutdownContext(ctx); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *Server) serve(w dns.ResponseWriter, m *dns.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ip netip.Addr
	switch a := w.RemoteAddr().(type) {
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	}
	req := &Request{Msg: m, ClientIP: ip.Unmap()}
	resp, err := s.handler.ServeDNS(ctx, req)
	if err != nil || resp == nil || resp.Msg == nil {
		resp = Servfail(req)
	}
	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		size := 512
		if opt := m.IsEdns0(); opt != nil {
			size = int(opt.UDPSize())
		}
		resp.Msg.Truncate(size)
	}
	w.WriteMsg(resp.Msg)
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/dnssrv/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dnssrv && git commit -m "feat: udp/tcp dns listeners feeding the pipeline"
```

---

### Task 7: Client registry + client-id middleware

**Files:**
- Create: `internal/clients/registry.go`, `internal/clients/registry_test.go`

**Interfaces:**
- Consumes: `store.ClientStore` (Task 3), `dnssrv` types (Task 5).
- Produces: `clients.NewRegistry(cs store.ClientStore) *Registry`; `(*Registry).Reload(ctx) error` (rebuilds snapshot from store); `(*Registry).Lookup(ip netip.Addr) dnssrv.ClientInfo`; `(*Registry).Middleware() dnssrv.Middleware` (sets `req.Client`). Matching precedence: exact IP > longest-prefix CIDR > default. Unknown clients get `ClientInfo{GroupID: 1, GroupName: "default"}` (group 1 is seeded as `default` in Task 15).

- [ ] **Step 1: Write failing tests**

`internal/clients/registry_test.go`:
```go
package clients

import (
	"context"
	"net/netip"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

type fakeClientStore struct {
	groups  []store.Group
	clients []store.Client
}

func (f *fakeClientStore) Groups(ctx context.Context) ([]store.Group, error)   { return f.groups, nil }
func (f *fakeClientStore) Clients(ctx context.Context) ([]store.Client, error) { return f.clients, nil }
func (f *fakeClientStore) AddGroup(ctx context.Context, name string) (int64, error) {
	return 0, nil
}
func (f *fakeClientStore) AddClient(ctx context.Context, c store.Client) (int64, error) {
	return 0, nil
}

func TestLookupPrecedence(t *testing.T) {
	fs := &fakeClientStore{
		groups: []store.Group{{ID: 1, Name: "default", Enabled: true}, {ID: 2, Name: "kids", Enabled: true}, {ID: 3, Name: "iot", Enabled: true}},
		clients: []store.Client{
			{ID: 10, Name: "tablet", Matcher: "10.0.0.5", GroupID: 2},
			{ID: 11, Name: "iot-net", Matcher: "10.0.0.0/24", GroupID: 3},
		},
	}
	r := NewRegistry(fs)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.5")); c.GroupName != "kids" {
		t.Fatalf("exact should beat cidr: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("10.0.0.77")); c.GroupName != "iot" {
		t.Fatalf("cidr match: %+v", c)
	}
	if c := r.Lookup(netip.MustParseAddr("192.168.1.1")); c.GroupName != "default" || c.GroupID != 1 {
		t.Fatalf("unknown -> default: %+v", c)
	}
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `go test ./internal/clients/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/clients/registry.go`:
```go
package clients

import (
	"context"
	"net/netip"
	"sort"
	"sync/atomic"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
)

type cidrEntry struct {
	prefix netip.Prefix
	info   dnssrv.ClientInfo
}

type snapshot struct {
	exact map[netip.Addr]dnssrv.ClientInfo
	cidrs []cidrEntry // sorted by prefix length, longest first
}

type Registry struct {
	cs   store.ClientStore
	snap atomic.Pointer[snapshot]
}

func NewRegistry(cs store.ClientStore) *Registry {
	r := &Registry{cs: cs}
	r.snap.Store(&snapshot{exact: map[netip.Addr]dnssrv.ClientInfo{}})
	return r
}

func (r *Registry) Reload(ctx context.Context) error {
	groups, err := r.cs.Groups(ctx)
	if err != nil {
		return err
	}
	gname := map[int64]string{}
	for _, g := range groups {
		gname[g.ID] = g.Name
	}
	cls, err := r.cs.Clients(ctx)
	if err != nil {
		return err
	}
	s := &snapshot{exact: map[netip.Addr]dnssrv.ClientInfo{}}
	for _, c := range cls {
		info := dnssrv.ClientInfo{ID: c.ID, Name: c.Name, GroupID: c.GroupID, GroupName: gname[c.GroupID]}
		if ip, err := netip.ParseAddr(c.Matcher); err == nil {
			s.exact[ip.Unmap()] = info
			continue
		}
		if p, err := netip.ParsePrefix(c.Matcher); err == nil {
			s.cidrs = append(s.cidrs, cidrEntry{prefix: p, info: info})
		}
	}
	sort.Slice(s.cidrs, func(i, j int) bool { return s.cidrs[i].prefix.Bits() > s.cidrs[j].prefix.Bits() })
	r.snap.Store(s)
	return nil
}

func (r *Registry) Lookup(ip netip.Addr) dnssrv.ClientInfo {
	s := r.snap.Load()
	if info, ok := s.exact[ip]; ok {
		return info
	}
	for _, e := range s.cidrs {
		if e.prefix.Contains(ip) {
			return e.info
		}
	}
	return dnssrv.ClientInfo{GroupID: 1, GroupName: "default"}
}

func (r *Registry) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			req.Client = r.Lookup(req.ClientIP)
			return next.ServeDNS(ctx, req)
		})
	}
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/clients/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/clients && git commit -m "feat: client registry with exact/cidr/default matching"
```

---

### Task 8: Filter engine — parsers, trie, ruleset, middleware

**Files:**
- Create: `internal/filter/parse.go`, `internal/filter/domainset.go`, `internal/filter/ruleset.go`, `internal/filter/engine.go`, and tests `parse_test.go`, `domainset_test.go`, `ruleset_test.go`, `engine_test.go`

**Interfaces:**
- Consumes: `store.Rule` (Task 3), `dnssrv` types (Task 5).
- Produces:
  - `filter.ParseList(r io.Reader) (ParseResult, error)`; `ParseResult{Block, Allow []string; Skipped int}`. Auto-detects per line: comments (`#`, `!`), hosts lines (`0.0.0.0 d`, `127.0.0.1 d`, `:: d`), ABP (`||d^` block, `@@||d^` allow), plain domains. Anything else counts as Skipped.
  - `filter.NewDomainSet() *DomainSet`; `(*DomainSet).Add(domain string)`; `(*DomainSet).Match(qname string) (matched string, ok bool)` — suffix match on label boundaries (entry `example.com` matches `example.com` and `a.example.com`, never `notexample.com`); `(*DomainSet).Len() int`.
  - `filter.CompiledList{ID int64; Kind string; Set *DomainSet}`
  - `filter.Compile(rules []store.Rule, lists []CompiledList) *Ruleset` (invalid regex rules are slog-warned and skipped); `(*Ruleset).Evaluate(qname string) Verdict`; `Verdict{Action string; RuleID, ListID int64; Matched string}` with Action `allow`|`block`|`none`. Precedence: manual allow > manual block > list allow > list block.
  - `filter.NewEngine() *Engine`; `(*Engine).SetGroups(map[int64]*Ruleset)`; `(*Engine).SetBlocking(mode string, ttl uint32)` (mode `null-ip`|`nxdomain`); `(*Engine).Pause(groupID int64, d time.Duration)` (groupID 0 = global); `(*Engine).Middleware() dnssrv.Middleware`.

- [ ] **Step 1: Write failing parser + trie tests**

`internal/filter/parse_test.go`:
```go
package filter

import (
	"strings"
	"testing"
)

func TestParseListAutoDetect(t *testing.T) {
	in := `# comment
! abp comment
0.0.0.0 ads.example.com
127.0.0.1 tracker.example.net
||abp-block.example^
@@||abp-allow.example^
plain.example.org
this is not a domain line
`
	res, err := ParseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	wantBlock := []string{"ads.example.com", "tracker.example.net", "abp-block.example", "plain.example.org"}
	if len(res.Block) != len(wantBlock) {
		t.Fatalf("block %v", res.Block)
	}
	for i, d := range wantBlock {
		if res.Block[i] != d {
			t.Fatalf("block[%d]=%q want %q", i, res.Block[i], d)
		}
	}
	if len(res.Allow) != 1 || res.Allow[0] != "abp-allow.example" {
		t.Fatalf("allow %v", res.Allow)
	}
	if res.Skipped != 1 {
		t.Fatalf("skipped %d", res.Skipped)
	}
}
```

`internal/filter/domainset_test.go`:
```go
package filter

import "testing"

func TestDomainSetLabelBoundaries(t *testing.T) {
	s := NewDomainSet()
	s.Add("example.com")
	s.Add("Ads.Example.NET") // must normalize case
	cases := []struct {
		q  string
		ok bool
	}{
		{"example.com", true},
		{"a.b.example.com", true},
		{"notexample.com", false},
		{"ads.example.net", true},
		{"sub.ads.example.net", true},
		{"example.net", false},
		{"com", false},
	}
	for _, c := range cases {
		if _, ok := s.Match(c.q); ok != c.ok {
			t.Errorf("Match(%q)=%v want %v", c.q, ok, c.ok)
		}
	}
	if m, _ := s.Match("deep.example.com"); m != "example.com" {
		t.Errorf("matched=%q", m)
	}
	if s.Len() != 2 {
		t.Errorf("len %d", s.Len())
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/filter/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement parser and trie**

`internal/filter/parse.go`:
```go
package filter

import (
	"bufio"
	"io"
	"strings"
)

type ParseResult struct {
	Block   []string
	Allow   []string
	Skipped int
}

func ParseList(r io.Reader) (ParseResult, error) {
	var res ParseResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case strings.HasPrefix(line, "@@||"):
			if d, ok := abpDomain(line[4:]); ok {
				res.Allow = append(res.Allow, d)
			} else {
				res.Skipped++
			}
		case strings.HasPrefix(line, "||"):
			if d, ok := abpDomain(line[2:]); ok {
				res.Block = append(res.Block, d)
			} else {
				res.Skipped++
			}
		default:
			fields := strings.Fields(line)
			switch {
			case len(fields) == 2 && (fields[0] == "0.0.0.0" || fields[0] == "127.0.0.1" || fields[0] == "::" || fields[0] == "::1"):
				if d, ok := validDomain(fields[1]); ok {
					res.Block = append(res.Block, d)
				} else {
					res.Skipped++
				}
			case len(fields) == 1:
				if d, ok := validDomain(fields[0]); ok {
					res.Block = append(res.Block, d)
				} else {
					res.Skipped++
				}
			default:
				res.Skipped++
			}
		}
	}
	return res, sc.Err()
}

func abpDomain(s string) (string, bool) {
	s = strings.TrimSuffix(s, "^")
	if strings.ContainsAny(s, "/^$*|") {
		return "", false // path/regex ABP rules unsupported
	}
	return validDomain(s)
}

func validDomain(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 || !strings.Contains(s, ".") {
		return "", false
	}
	for _, lbl := range strings.Split(s, ".") {
		if lbl == "" || len(lbl) > 63 {
			return "", false
		}
		for _, r := range lbl {
			if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				return "", false
			}
		}
	}
	return s, true
}
```

`internal/filter/domainset.go`:
```go
package filter

import "strings"

type dsNode struct {
	children map[string]*dsNode
	terminal bool
}

type DomainSet struct {
	root *dsNode
	n    int
}

func NewDomainSet() *DomainSet {
	return &DomainSet{root: &dsNode{children: map[string]*dsNode{}}}
}

func (s *DomainSet) Add(domain string) {
	labels := splitRev(strings.ToLower(strings.TrimSuffix(domain, ".")))
	cur := s.root
	for _, lbl := range labels {
		next, ok := cur.children[lbl]
		if !ok {
			next = &dsNode{children: map[string]*dsNode{}}
			cur.children[lbl] = next
		}
		cur = next
	}
	if !cur.terminal {
		cur.terminal = true
		s.n++
	}
}

// Match returns the most-specific entry that is qname or a parent
// domain of qname, matching only on whole-label boundaries.
func (s *DomainSet) Match(qname string) (string, bool) {
	labels := splitRev(strings.ToLower(strings.TrimSuffix(qname, ".")))
	cur := s.root
	matched := ""
	found := false
	for i, lbl := range labels {
		next, ok := cur.children[lbl]
		if !ok {
			break
		}
		cur = next
		if cur.terminal {
			matched = strings.Join(reverse(labels[:i+1]), ".")
			found = true
		}
	}
	return matched, found
}

func (s *DomainSet) Len() int { return s.n }

func splitRev(d string) []string { return reverse(strings.Split(d, ".")) }

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
```

- [ ] **Step 4: Run parser/trie tests, verify pass; commit**

Run: `go test -race ./internal/filter/ -v` — Expected: PASS.

```bash
git add internal/filter && git commit -m "feat: blocklist parsers and domain trie"
```

- [ ] **Step 5: Write failing ruleset tests**

`internal/filter/ruleset_test.go`:
```go
package filter

import (
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

func compiledList(id int64, kind string, domains ...string) CompiledList {
	s := NewDomainSet()
	for _, d := range domains {
		s.Add(d)
	}
	return CompiledList{ID: id, Kind: kind, Set: s}
}

func TestPrecedence(t *testing.T) {
	rs := Compile(
		[]store.Rule{
			{ID: 1, Action: "allow", Pattern: "ok.blocked.example"},
			{ID: 2, Action: "block", Pattern: "manual-block.example"},
			{ID: 3, Action: "block", Pattern: `^ad[0-9]+\.regex\.example$`, IsRegex: true},
		},
		[]CompiledList{
			compiledList(10, "block", "blocked.example", "manual-block.example", "listallow.example"),
			compiledList(11, "allow", "listallow.example"),
		},
	)
	cases := []struct {
		q      string
		action string
		ruleID int64
		listID int64
	}{
		{"sub.blocked.example", "block", 0, 10},
		{"ok.blocked.example", "allow", 1, 0},   // manual allow beats list block
		{"manual-block.example", "block", 2, 0}, // manual block beats list block attribution
		{"listallow.example", "allow", 0, 11},   // list allow beats list block
		{"ad42.regex.example", "block", 3, 0},
		{"clean.example", "none", 0, 0},
	}
	for _, c := range cases {
		v := rs.Evaluate(c.q)
		if v.Action != c.action || v.RuleID != c.ruleID || v.ListID != c.listID {
			t.Errorf("Evaluate(%q) = %+v, want action=%s rule=%d list=%d", c.q, v, c.action, c.ruleID, c.listID)
		}
	}
}
```

- [ ] **Step 6: Run, verify failure, implement ruleset**

Run: `go test ./internal/filter/ -run TestPrecedence -v` — Expected: FAIL.

`internal/filter/ruleset.go`:
```go
package filter

import (
	"log/slog"
	"regexp"

	"github.com/aloks98/dnsaur/internal/store"
)

type Verdict struct {
	Action  string // allow | block | none
	RuleID  int64
	ListID  int64
	Matched string
}

type CompiledList struct {
	ID   int64
	Kind string // block | allow
	Set  *DomainSet
}

type regexRule struct {
	id     int64
	action string
	re     *regexp.Regexp
}

type Ruleset struct {
	manualAllow   *DomainSet
	manualBlock   *DomainSet
	manualAllowID map[string]int64 // matched pattern -> rule id
	manualBlockID map[string]int64
	regexes       []regexRule
	lists         []CompiledList
}

func Compile(rules []store.Rule, lists []CompiledList) *Ruleset {
	rs := &Ruleset{
		manualAllow: NewDomainSet(), manualBlock: NewDomainSet(),
		manualAllowID: map[string]int64{}, manualBlockID: map[string]int64{},
		lists: lists,
	}
	for _, r := range rules {
		if r.IsRegex {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				slog.Warn("skipping invalid regex rule", "id", r.ID, "pattern", r.Pattern, "err", err)
				continue
			}
			rs.regexes = append(rs.regexes, regexRule{id: r.ID, action: r.Action, re: re})
			continue
		}
		if r.Action == "allow" {
			rs.manualAllow.Add(r.Pattern)
			rs.manualAllowID[normalize(r.Pattern)] = r.ID
		} else {
			rs.manualBlock.Add(r.Pattern)
			rs.manualBlockID[normalize(r.Pattern)] = r.ID
		}
	}
	return rs
}

func normalize(d string) string { m, _ := validDomain(d); return m }

func (rs *Ruleset) Evaluate(qname string) Verdict {
	if m, ok := rs.manualAllow.Match(qname); ok {
		return Verdict{Action: "allow", RuleID: rs.manualAllowID[m], Matched: m}
	}
	for _, rr := range rs.regexes {
		if rr.action == "allow" && rr.re.MatchString(qname) {
			return Verdict{Action: "allow", RuleID: rr.id, Matched: rr.re.String()}
		}
	}
	if m, ok := rs.manualBlock.Match(qname); ok {
		return Verdict{Action: "block", RuleID: rs.manualBlockID[m], Matched: m}
	}
	for _, rr := range rs.regexes {
		if rr.action == "block" && rr.re.MatchString(qname) {
			return Verdict{Action: "block", RuleID: rr.id, Matched: rr.re.String()}
		}
	}
	for _, l := range rs.lists {
		if l.Kind != "allow" {
			continue
		}
		if m, ok := l.Set.Match(qname); ok {
			return Verdict{Action: "allow", ListID: l.ID, Matched: m}
		}
	}
	for _, l := range rs.lists {
		if l.Kind != "block" {
			continue
		}
		if m, ok := l.Set.Match(qname); ok {
			return Verdict{Action: "block", ListID: l.ID, Matched: m}
		}
	}
	return Verdict{Action: "none"}
}
```

Run: `go test -race ./internal/filter/ -v` — Expected: PASS.

```bash
git add internal/filter && git commit -m "feat: filter ruleset compile and precedence evaluation"
```

- [ ] **Step 7: Write failing engine/middleware tests**

`internal/filter/engine_test.go`:
```go
package filter

import (
	"context"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

func testReq(name string, qtype uint16, groupID int64) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return &dnssrv.Request{Msg: m, Client: dnssrv.ClientInfo{GroupID: groupID}}
}

func passthrough(t *testing.T) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func engineWith(t *testing.T, mode string) *Engine {
	e := NewEngine()
	e.SetBlocking(mode, 30)
	e.SetGroups(map[int64]*Ruleset{
		1: Compile(nil, []CompiledList{compiledList(10, "block", "ads.example")}),
	})
	return e
}

func TestBlockNullIP(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	resp, err := h.ServeDNS(context.Background(), testReq("x.ads.example", dns.TypeA, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionBlocked || resp.ListID != 10 {
		t.Fatalf("%+v", resp)
	}
	a, ok := resp.Msg.Answer[0].(*dns.A)
	if !ok || a.A.String() != "0.0.0.0" || a.Hdr.Ttl != 30 {
		t.Fatalf("answer %v", resp.Msg.Answer)
	}
}

func TestBlockNXDOMAIN(t *testing.T) {
	e := engineWith(t, "nxdomain")
	h := e.Middleware()(passthrough(t))
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1))
	if resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode %d", resp.Msg.Rcode)
	}
}

func TestPauseDisablesBlocking(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	e.Pause(0, time.Minute)
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 1))
	if resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("paused engine still blocked: %+v", resp)
	}
}

func TestUnknownGroupPassesThrough(t *testing.T) {
	e := engineWith(t, "null-ip")
	h := e.Middleware()(passthrough(t))
	resp, _ := h.ServeDNS(context.Background(), testReq("ads.example", dns.TypeA, 99))
	if resp.Decision != dnssrv.DecisionForwarded {
		t.Fatalf("%+v", resp)
	}
}
```

- [ ] **Step 8: Run, verify failure, implement engine**

Run: `go test ./internal/filter/ -run 'TestBlock|TestPause|TestUnknown' -v` — Expected: FAIL.

`internal/filter/engine.go`:
```go
package filter

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

type blockingCfg struct {
	mode string
	ttl  uint32
}

type Engine struct {
	groups   atomic.Pointer[map[int64]*Ruleset]
	blocking atomic.Pointer[blockingCfg]

	mu     sync.Mutex
	paused map[int64]time.Time // groupID -> until; 0 = global
	now    func() time.Time
}

func NewEngine() *Engine {
	e := &Engine{paused: map[int64]time.Time{}, now: time.Now}
	empty := map[int64]*Ruleset{}
	e.groups.Store(&empty)
	e.blocking.Store(&blockingCfg{mode: "null-ip", ttl: 30})
	return e
}

func (e *Engine) SetGroups(g map[int64]*Ruleset)      { e.groups.Store(&g) }
func (e *Engine) SetBlocking(mode string, ttl uint32) { e.blocking.Store(&blockingCfg{mode, ttl}) }

func (e *Engine) Pause(groupID int64, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.paused[groupID] = e.now().Add(d)
}

func (e *Engine) isPaused(groupID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	return e.paused[0].After(now) || e.paused[groupID].After(now)
}

func (e *Engine) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			rs, ok := (*e.groups.Load())[req.Client.GroupID]
			if !ok || e.isPaused(req.Client.GroupID) {
				return next.ServeDNS(ctx, req)
			}
			v := rs.Evaluate(req.QName())
			if v.Action != "block" {
				return next.ServeDNS(ctx, req)
			}
			cfg := e.blocking.Load()
			m := new(dns.Msg)
			if cfg.mode == "nxdomain" {
				m.SetRcode(req.Msg, dns.RcodeNameError)
			} else {
				m.SetReply(req.Msg)
				hdr := dns.RR_Header{Name: req.Msg.Question[0].Name, Class: dns.ClassINET, Ttl: cfg.ttl}
				switch req.QType() {
				case dns.TypeA:
					hdr.Rrtype = dns.TypeA
					m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: net.IPv4zero}}
				case dns.TypeAAAA:
					hdr.Rrtype = dns.TypeAAAA
					m.Answer = []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: net.IPv6zero}}
				}
			}
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked, RuleID: v.RuleID, ListID: v.ListID}, nil
		})
	}
}
```

Run: `go test -race ./internal/filter/ -v` — Expected: PASS.

```bash
git add internal/filter && git commit -m "feat: filter engine middleware with pause and block modes"
```

---

### Task 9: Blocklist refresher

**Files:**
- Create: `internal/filter/refresh.go`, `internal/filter/refresh_test.go`

**Interfaces:**
- Consumes: `store.FilterStore`, `store.ClientStore` (Task 3), `Engine`/`ParseList`/`Compile` (Task 8).
- Produces: `filter.NewRefresher(fs store.FilterStore, cs store.ClientStore, eng *Engine, dataDir string) *Refresher`; `(*Refresher).RefreshAll(ctx) error` (downloads enabled lists with ETag caching to `<dataDir>/lists/<id>.txt` + `<id>.etag`, falls back to the cached file on any download failure, compiles every group's Ruleset from lists + rules, calls `eng.SetGroups`, `TouchList` per refreshed list); `(*Refresher).Run(ctx, every time.Duration)` (ticker loop calling RefreshAll, logging errors, never exiting before ctx cancel).

- [ ] **Step 1: Write failing tests**

`internal/filter/refresh_test.go`:
```go
package filter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aloks98/dnsaur/internal/store"
)

type fakeFilterStore struct {
	lists   []store.List
	touched atomic.Int64
}

func (f *fakeFilterStore) Lists(ctx context.Context) ([]store.List, error) { return f.lists, nil }
func (f *fakeFilterStore) ListsForGroup(ctx context.Context, g int64) ([]store.List, error) {
	return f.lists, nil
}
func (f *fakeFilterStore) Rules(ctx context.Context, g int64) ([]store.Rule, error) {
	return nil, nil
}
func (f *fakeFilterStore) AddList(ctx context.Context, l store.List) (int64, error)  { return 0, nil }
func (f *fakeFilterStore) AssignList(ctx context.Context, g, l int64) error          { return nil }
func (f *fakeFilterStore) AddRule(ctx context.Context, r store.Rule) (int64, error)  { return 0, nil }
func (f *fakeFilterStore) TouchList(ctx context.Context, id, at, n int64) error {
	f.touched.Add(1)
	return nil
}

func TestRefreshDownloadsCompilesAndKeepsOldOnFailure(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer srv.Close()

	fs := &fakeFilterStore{lists: []store.List{{ID: 1, URL: srv.URL, Kind: "block", Enabled: true}}}
	cs := &fakeClientStore{groups: []store.Group{{ID: 1, Name: "default", Enabled: true}}}
	eng := NewEngine()
	ref := NewRefresher(fs, cs, eng, t.TempDir())

	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("not compiled: %+v", v)
	}

	failing.Store(true) // server breaks; cached file must keep the block working
	if err := ref.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := (*eng.groups.Load())[1].Evaluate("ads.example.com"); v.Action != "block" {
		t.Fatalf("lost list on failed refresh: %+v", v)
	}
}
```

Note: `fakeClientStore` here duplicates the one in Task 7's test but lives in package `filter` — define it in this file with the same shape (Groups/Clients/AddGroup/AddClient methods over struct fields `groups []store.Group; clients []store.Client`).

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/filter/ -run TestRefresh -v` — Expected: FAIL (`NewRefresher` undefined).

- [ ] **Step 3: Implement**

`internal/filter/refresh.go`:
```go
package filter

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

type Refresher struct {
	fs      store.FilterStore
	cs      store.ClientStore
	eng     *Engine
	dataDir string
	hc      *http.Client
	now     func() time.Time
}

func NewRefresher(fs store.FilterStore, cs store.ClientStore, eng *Engine, dataDir string) *Refresher {
	return &Refresher{fs: fs, cs: cs, eng: eng, dataDir: dataDir,
		hc: &http.Client{Timeout: 60 * time.Second}, now: time.Now}
}

func (r *Refresher) cachePath(id int64) string {
	return filepath.Join(r.dataDir, "lists", fmt.Sprintf("%d.txt", id))
}

// fetch downloads url honoring a cached ETag; on failure or 304 it falls
// back to the cache file. Returns an open reader or an error if neither
// network nor cache is available.
func (r *Refresher) fetch(ctx context.Context, l store.List) (io.ReadCloser, error) {
	etagPath := r.cachePath(l.ID) + ".etag"
	req, err := http.NewRequestWithContext(ctx, "GET", l.URL, nil)
	if err != nil {
		return nil, err
	}
	if etag, err := os.ReadFile(etagPath); err == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	resp, err := r.hc.Do(req)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		os.MkdirAll(filepath.Dir(r.cachePath(l.ID)), 0o755)
		tmp := r.cachePath(l.ID) + ".tmp"
		f, ferr := os.Create(tmp)
		if ferr != nil {
			return nil, ferr
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		if err := os.Rename(tmp, r.cachePath(l.ID)); err != nil {
			return nil, err
		}
		if et := resp.Header.Get("ETag"); et != "" {
			os.WriteFile(etagPath, []byte(et), 0o644)
		}
	} else if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotModified {
			slog.Warn("list download failed, using cache", "url", l.URL, "status", resp.StatusCode)
		}
	} else {
		slog.Warn("list download failed, using cache", "url", l.URL, "err", err)
	}
	return os.Open(r.cachePath(l.ID))
}

func (r *Refresher) RefreshAll(ctx context.Context) error {
	lists, err := r.fs.Lists(ctx)
	if err != nil {
		return err
	}
	compiled := map[int64]CompiledList{}
	for _, l := range lists {
		if !l.Enabled {
			continue
		}
		body, err := r.fetch(ctx, l)
		if err != nil {
			slog.Error("list unavailable (no cache)", "url", l.URL, "err", err)
			continue
		}
		res, err := ParseList(body)
		body.Close()
		if err != nil {
			slog.Error("list parse failed", "url", l.URL, "err", err)
			continue
		}
		set := NewDomainSet()
		entries := res.Block
		if l.Kind == "allow" {
			entries = res.Allow
			// a plain domains file subscribed as an allowlist parses into Block; accept both
			entries = append(entries, res.Block...)
		}
		for _, d := range entries {
			set.Add(d)
		}
		compiled[l.ID] = CompiledList{ID: l.ID, Kind: l.Kind, Set: set}
		r.fs.TouchList(ctx, l.ID, r.now().UnixMilli(), int64(set.Len()))
	}
	groups, err := r.cs.Groups(ctx)
	if err != nil {
		return err
	}
	out := map[int64]*Ruleset{}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		gl, err := r.fs.ListsForGroup(ctx, g.ID)
		if err != nil {
			return err
		}
		var cls []CompiledList
		for _, l := range gl {
			if c, ok := compiled[l.ID]; ok {
				cls = append(cls, c)
			}
		}
		rules, err := r.fs.Rules(ctx, g.ID)
		if err != nil {
			return err
		}
		out[g.ID] = Compile(rules, cls)
	}
	r.eng.SetGroups(out)
	return nil
}

func (r *Refresher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RefreshAll(ctx); err != nil {
				slog.Error("blocklist refresh failed", "err", err)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/filter/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/filter && git commit -m "feat: blocklist refresher with etag caching and stale-list fallback"
```

---

### Task 10: Local records stage

**Files:**
- Create: `internal/records/records.go`, `internal/records/records_test.go`

**Interfaces:**
- Consumes: `store.RecordStore` (Task 3), `dnssrv` types (Task 5).
- Produces: `records.NewResolver(rs store.RecordStore) *Resolver`; `(*Resolver).Reload(ctx) error`; `(*Resolver).Middleware() dnssrv.Middleware`. Behavior: supports record types `A`, `AAAA`, `CNAME`, `TXT`. Exact-name match first, then wildcard (`*.parent` entries match any name below `parent`, walking up label by label). If the name exists locally but has no records of the asked type → NODATA (NOERROR, empty answer, Decision `local`). CNAME: answer the CNAME RR; if the target is also local, append its records of the asked qtype (chase up to 8 links, loop-safe); if the target is not local, pass the *target* query to `next` and merge its answers. Names with no local data at all → pass through untouched.

- [ ] **Step 1: Write failing tests**

`internal/records/records_test.go`:
```go
package records

import (
	"context"
	"testing"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type fakeRecordStore struct{ recs []store.LocalRecord }

func (f *fakeRecordStore) All(ctx context.Context) ([]store.LocalRecord, error) { return f.recs, nil }
func (f *fakeRecordStore) Add(ctx context.Context, r store.LocalRecord) (int64, error) {
	return 0, nil
}

func resolver(t *testing.T, recs ...store.LocalRecord) *Resolver {
	r := NewResolver(&fakeRecordStore{recs: recs})
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func ask(t *testing.T, h dnssrv.Handler, name string, qtype uint16) *dnssrv.Response {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	resp, err := h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func nextCounter(n *int) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		*n++
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func TestExactAndWildcardAndPassthrough(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300},
		store.LocalRecord{Name: "*.apps.home.lan", Type: "A", Value: "10.0.0.10", TTL: 60},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "nas.home.lan", dns.TypeA)
	if resp.Decision != dnssrv.DecisionLocal || len(resp.Msg.Answer) != 1 {
		t.Fatalf("%+v", resp)
	}
	if a := resp.Msg.Answer[0].(*dns.A); a.A.String() != "10.0.0.9" || a.Hdr.Ttl != 300 {
		t.Fatalf("%v", resp.Msg.Answer)
	}

	resp = ask(t, h, "grafana.apps.home.lan", dns.TypeA)
	if len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].(*dns.A).A.String() != "10.0.0.10" {
		t.Fatalf("wildcard: %v", resp.Msg.Answer)
	}

	// NODATA: name exists, wrong type
	resp = ask(t, h, "nas.home.lan", dns.TypeAAAA)
	if resp.Decision != dnssrv.DecisionLocal || len(resp.Msg.Answer) != 0 || resp.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("nodata: %+v", resp)
	}

	if calls != 0 {
		t.Fatalf("next called %d times", calls)
	}
	ask(t, h, "example.com", dns.TypeA)
	if calls != 1 {
		t.Fatalf("passthrough missing, calls=%d", calls)
	}
}

func TestCNAMEChaseLocalAndRemote(t *testing.T) {
	r := resolver(t,
		store.LocalRecord{Name: "www.home.lan", Type: "CNAME", Value: "nas.home.lan", TTL: 300},
		store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300},
		store.LocalRecord{Name: "ext.home.lan", Type: "CNAME", Value: "external.example.com", TTL: 300},
	)
	calls := 0
	h := r.Middleware()(nextCounter(&calls))

	resp := ask(t, h, "www.home.lan", dns.TypeA)
	if len(resp.Msg.Answer) != 2 {
		t.Fatalf("cname+target expected: %v", resp.Msg.Answer)
	}

	resp = ask(t, h, "ext.home.lan", dns.TypeA)
	if calls != 1 {
		t.Fatalf("remote target should hit next, calls=%d", calls)
	}
	if _, ok := resp.Msg.Answer[0].(*dns.CNAME); !ok {
		t.Fatalf("first answer must be the CNAME: %v", resp.Msg.Answer)
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/records/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/records/records.go`:
```go
package records

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type snapshot struct {
	byName map[string][]store.LocalRecord // lowercase name -> records (wildcards keyed as "*.parent")
}

type Resolver struct {
	rs   store.RecordStore
	snap atomic.Pointer[snapshot]
}

func NewResolver(rs store.RecordStore) *Resolver {
	r := &Resolver{rs: rs}
	r.snap.Store(&snapshot{byName: map[string][]store.LocalRecord{}})
	return r
}

func (r *Resolver) Reload(ctx context.Context) error {
	all, err := r.rs.All(ctx)
	if err != nil {
		return err
	}
	s := &snapshot{byName: map[string][]store.LocalRecord{}}
	for _, rec := range all {
		k := strings.ToLower(strings.TrimSuffix(rec.Name, "."))
		s.byName[k] = append(s.byName[k], rec)
	}
	r.snap.Store(s)
	return nil
}

// lookup finds records for name: exact first, then wildcard walking up.
func (s *snapshot) lookup(name string) ([]store.LocalRecord, bool) {
	if recs, ok := s.byName[name]; ok {
		return recs, true
	}
	labels := strings.Split(name, ".")
	for i := 1; i < len(labels); i++ {
		if recs, ok := s.byName["*."+strings.Join(labels[i:], ".")]; ok {
			return recs, true
		}
	}
	return nil, false
}

func toRR(qname string, rec store.LocalRecord) (dns.RR, error) {
	return dns.NewRR(fmt.Sprintf("%s %d IN %s %s", dns.Fqdn(qname), rec.TTL, rec.Type, rec.Value))
}

func (r *Resolver) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			s := r.snap.Load()
			qname := req.QName()
			qtypeStr := dns.TypeToString[req.QType()]
			recs, ok := s.lookup(qname)
			if !ok {
				return next.ServeDNS(ctx, req)
			}
			m := new(dns.Msg)
			m.SetReply(req.Msg)
			m.Authoritative = true
			name := qname
			for hop := 0; hop < 8; hop++ {
				cur, ok := s.lookup(name)
				if !ok {
					// target is remote: resolve via the rest of the pipeline and merge
					sub := new(dns.Msg)
					sub.SetQuestion(dns.Fqdn(name), req.QType())
					subResp, err := next.ServeDNS(ctx, &dnssrv.Request{Msg: sub, ClientIP: req.ClientIP, Client: req.Client})
					if err == nil && subResp.Msg != nil {
						m.Answer = append(m.Answer, subResp.Msg.Answer...)
					}
					break
				}
				var cname *store.LocalRecord
				matched := false
				for i := range cur {
					rec := cur[i]
					if strings.EqualFold(rec.Type, qtypeStr) {
						if rr, err := toRR(name, rec); err == nil {
							m.Answer = append(m.Answer, rr)
							matched = true
						}
					}
					if strings.EqualFold(rec.Type, "CNAME") {
						cname = &cur[i]
					}
				}
				if matched || cname == nil || req.QType() == dns.TypeCNAME {
					break
				}
				if rr, err := toRR(name, *cname); err == nil {
					m.Answer = append(m.Answer, rr)
				}
				name = strings.ToLower(strings.TrimSuffix(cname.Value, "."))
			}
			_ = recs
			return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionLocal}, nil
		})
	}
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/records/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/records && git commit -m "feat: local records stage with wildcard and cname chasing"
```

---

### Task 11: DNS cache

**Files:**
- Create: `internal/cache/cache.go`, `internal/cache/cache_test.go`

**Interfaces:**
- Consumes: `dnssrv` types (Task 5).
- Produces: `cache.New(o Options) *Cache`; `Options{MinTTL, MaxTTL, NegTTL, ServeStaleFor time.Duration; MaxEntries int; Now func() time.Time}` (zero-value fields default to: MinTTL 0, MaxTTL 24h, NegTTL 30s, ServeStaleFor 24h, MaxEntries 10000, Now time.Now); `(*Cache).Middleware() dnssrv.Middleware`; `(*Cache).Len() int`. Behavior:
  - Key: (qname, qtype). Only NOERROR and NXDOMAIN responses with Decision `forwarded` are stored.
  - Positive TTL = min TTL across answer RRs, clamped to [MinTTL, MaxTTL]. Negative (NXDOMAIN or empty NOERROR) TTL = SOA MINIMUM from the authority section clamped to [MinTTL, MaxTTL], defaulting to NegTTL when no SOA is present (RFC 2308).
  - Hits return a copy with `Id` matching the request and TTLs reduced by age; Decision `cached`.
  - Expired entries are kept for ServeStaleFor beyond expiry; if `next` errors or returns SERVFAIL and a stale entry exists, serve it with all TTLs set to 30 and Decision `stale` (RFC 8767).
  - Concurrent misses for the same key collapse into one upstream call (`golang.org/x/sync/singleflight`), each caller getting its own copy.
  - LRU eviction beyond MaxEntries.

- [ ] **Step 1: Write failing tests**

`internal/cache/cache_test.go`:
```go
package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

func answer(name string, ttl uint32, ip string) dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		rr, _ := dns.NewRR(dns.Fqdn(name) + " 300 IN A " + ip)
		rr.Header().Ttl = ttl
		m.Answer = []dns.RR{rr}
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
}

func req(name string) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return &dnssrv.Request{Msg: m}
}

func TestHitDecrementsTTLAndSkipsUpstream(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	counted := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		return answer("x.test", 300, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(counted)

	if resp, _ := h.ServeDNS(context.Background(), req("x.test")); resp.Decision != dnssrv.DecisionForwarded {
		t.Fatal("first query should forward")
	}
	clk.t = clk.t.Add(100 * time.Second)
	resp, _ := h.ServeDNS(context.Background(), req("x.test"))
	if resp.Decision != dnssrv.DecisionCached {
		t.Fatalf("want cached, got %s", resp.Decision)
	}
	if ttl := resp.Msg.Answer[0].Header().Ttl; ttl != 200 {
		t.Fatalf("ttl %d want 200", ttl)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls %d", calls.Load())
	}
}

func TestExpiryRefetches(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	counted := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		return answer("x.test", 60, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(counted)
	h.ServeDNS(context.Background(), req("x.test"))
	clk.t = clk.t.Add(61 * time.Second)
	h.ServeDNS(context.Background(), req("x.test"))
	if calls.Load() != 2 {
		t.Fatalf("expired entry not refetched, calls=%d", calls.Load())
	}
}

func TestServeStaleOnUpstreamFailure(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var fail atomic.Bool
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		if fail.Load() {
			return nil, errors.New("upstream down")
		}
		return answer("x.test", 60, "1.2.3.4").ServeDNS(ctx, r)
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(up)
	h.ServeDNS(context.Background(), req("x.test"))
	fail.Store(true)
	clk.t = clk.t.Add(2 * time.Hour) // expired but within 24h stale window
	resp, err := h.ServeDNS(context.Background(), req("x.test"))
	if err != nil || resp.Decision != dnssrv.DecisionStale {
		t.Fatalf("want stale, got %v %v", resp, err)
	}
	if resp.Msg.Answer[0].Header().Ttl != 30 {
		t.Fatalf("stale ttl %d", resp.Msg.Answer[0].Header().Ttl)
	}
}

func TestNegativeCaching(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var calls atomic.Int64
	up := dnssrv.HandlerFunc(func(ctx context.Context, r *dnssrv.Request) (*dnssrv.Response, error) {
		calls.Add(1)
		m := new(dns.Msg)
		m.SetRcode(r.Msg, dns.RcodeNameError)
		soa, _ := dns.NewRR("test. 3600 IN SOA a.test. b.test. 1 1 1 1 600")
		m.Ns = []dns.RR{soa}
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	})
	c := New(Options{Now: clk.Now})
	h := c.Middleware()(up)
	h.ServeDNS(context.Background(), req("nope.test"))
	clk.t = clk.t.Add(30 * time.Second)
	resp, _ := h.ServeDNS(context.Background(), req("nope.test"))
	if resp.Decision != dnssrv.DecisionCached || resp.Msg.Rcode != dns.RcodeNameError {
		t.Fatalf("%+v", resp)
	}
	if calls.Load() != 1 {
		t.Fatalf("negative response not cached, calls=%d", calls.Load())
	}
}

func TestEviction(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.Now, MaxEntries: 2})
	h := c.Middleware()(answer("a.test", 300, "1.1.1.1"))
	for _, n := range []string{"a.test", "b.test", "c.test"} {
		h.ServeDNS(context.Background(), req(n))
	}
	if c.Len() != 2 {
		t.Fatalf("len %d want 2", c.Len())
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/cache/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/cache/cache.go`:
```go
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
	"golang.org/x/sync/singleflight"
)

type Options struct {
	MinTTL        time.Duration
	MaxTTL        time.Duration
	NegTTL        time.Duration
	ServeStaleFor time.Duration
	MaxEntries    int
	Now           func() time.Time
}

type ckey struct {
	name  string
	qtype uint16
}

type entry struct {
	msg      *dns.Msg
	storedAt time.Time
	ttl      time.Duration
	elem     *list.Element
}

type Cache struct {
	mu      sync.Mutex
	entries map[ckey]*entry
	lru     *list.List // front = most recent; values are ckey
	o       Options
	sf      singleflight.Group
}

func New(o Options) *Cache {
	if o.MaxTTL == 0 {
		o.MaxTTL = 24 * time.Hour
	}
	if o.NegTTL == 0 {
		o.NegTTL = 30 * time.Second
	}
	if o.ServeStaleFor == 0 {
		o.ServeStaleFor = 24 * time.Hour
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = 10000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Cache{entries: map[ckey]*entry{}, lru: list.New(), o: o}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) get(k ckey) (fresh *entry, stale *entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return nil, nil
	}
	age := c.o.Now().Sub(e.storedAt)
	c.lru.MoveToFront(e.elem)
	if age < e.ttl {
		return e, nil
	}
	if age < e.ttl+c.o.ServeStaleFor {
		return nil, e
	}
	c.removeLocked(k)
	return nil, nil
}

func (c *Cache) removeLocked(k ckey) {
	if e, ok := c.entries[k]; ok {
		c.lru.Remove(e.elem)
		delete(c.entries, k)
	}
}

func (c *Cache) put(k ckey, msg *dns.Msg, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(k)
	e := &entry{msg: msg.Copy(), storedAt: c.o.Now(), ttl: ttl}
	e.elem = c.lru.PushFront(k)
	c.entries[k] = e
	for len(c.entries) > c.o.MaxEntries {
		back := c.lru.Back()
		c.removeLocked(back.Value.(ckey))
	}
}

func respTTL(o Options, m *dns.Msg) (time.Duration, bool) {
	if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
		return 0, false
	}
	if len(m.Answer) == 0 { // negative: NXDOMAIN or NODATA, RFC 2308
		ttl := o.NegTTL
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				ttl = time.Duration(min(soa.Minttl, uint32(soa.Hdr.Ttl))) * time.Second
				break
			}
		}
		return clamp(ttl, o.MinTTL, o.MaxTTL), true
	}
	minTTL := uint32(1<<32 - 1)
	for _, rr := range m.Answer {
		if rr.Header().Ttl < minTTL {
			minTTL = rr.Header().Ttl
		}
	}
	return clamp(time.Duration(minTTL)*time.Second, o.MinTTL, o.MaxTTL), true
}

func clamp(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

func withTTLs(m *dns.Msg, set func(cur uint32) uint32) *dns.Msg {
	out := m.Copy()
	for _, sec := range [][]dns.RR{out.Answer, out.Ns, out.Extra} {
		for _, rr := range sec {
			if rr.Header().Rrtype != dns.TypeOPT {
				rr.Header().Ttl = set(rr.Header().Ttl)
			}
		}
	}
	return out
}

func (c *Cache) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			if len(req.Msg.Question) != 1 || req.Msg.Question[0].Qclass != dns.ClassINET {
				return next.ServeDNS(ctx, req)
			}
			k := ckey{name: req.QName(), qtype: req.QType()}
			if fresh, _ := c.get(k); fresh != nil {
				age := uint32(c.o.Now().Sub(fresh.storedAt) / time.Second)
				m := withTTLs(fresh.msg, func(cur uint32) uint32 {
					if cur <= age {
						return 1
					}
					return cur - age
				})
				m.Id = req.Msg.Id
				return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionCached}, nil
			}
			v, err, _ := c.sf.Do(k.name+"|"+dns.TypeToString[k.qtype], func() (any, error) {
				resp, err := next.ServeDNS(ctx, req)
				if err == nil && resp != nil && resp.Msg != nil && resp.Decision == dnssrv.DecisionForwarded {
					if ttl, ok := respTTL(c.o, resp.Msg); ok {
						c.put(k, resp.Msg, ttl)
					}
				}
				return resp, err
			})
			resp, _ := v.(*dnssrv.Response)
			failed := err != nil || resp == nil || resp.Msg == nil || resp.Msg.Rcode == dns.RcodeServerFailure
			if failed {
				if _, stale := c.get(k); stale != nil {
					m := withTTLs(stale.msg, func(uint32) uint32 { return 30 })
					m.Id = req.Msg.Id
					return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionStale}, nil
				}
				return resp, err
			}
			out := *resp
			out.Msg = resp.Msg.Copy()
			out.Msg.Id = req.Msg.Id
			return &out, nil
		})
	}
}
```
(Remove the stray placeholder lines in the test's `answer` helper — build the RR with `dns.NewRR(name + " 300 IN A " + ip)` then override `Header().Ttl`; the final test file must contain only the `rr2` version.)

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/cache/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cache go.mod go.sum && git commit -m "feat: dns cache with negative caching, serve-stale, lru"
```

---

### Task 12: Upstream forwarder

**Files:**
- Create: `internal/upstream/forwarder.go`, `internal/upstream/forwarder_test.go`

**Interfaces:**
- Consumes: `dnssrv` types (Task 5), `filter.DomainSet` (Task 8) for conditional suffix matching.
- Produces: `upstream.New(cfg Config) (*Forwarder, error)`; `Config{Upstreams []string; Strategy string; Timeout time.Duration; Conditional map[string][]string}` (Strategy `failover`|`fastest`|`race`, default `race`; Timeout default 2s; Conditional maps domain suffix → upstream addrs); `(*Forwarder).Handler() dnssrv.Handler` (terminal). Behavior:
  - Per-upstream health: 3 consecutive failures → down for 15s (skipped while down; retried after).
  - `failover`: try in configured order. `fastest`: order by EWMA latency. `race`: query all healthy in parallel, first success wins.
  - 0x20: query names are sent with randomized case; a response whose question doesn't echo it exactly is discarded as spoofed; response names are normalized back before returning.
  - Truncated UDP responses are retried over TCP.
  - RFC 9520: when all upstreams fail for a (qname, qtype), SERVFAIL is cached for 30s and returned without retrying upstreams.
  - Success → `Response{Decision: forwarded, Upstream: <addr>}`.

- [ ] **Step 1: Write failing tests**

`internal/upstream/forwarder_test.go`:
```go
package upstream

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/miekg/dns"
)

// mockUpstream runs a real DNS server on 127.0.0.1:0.
func mockUpstream(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	u := &dns.Server{PacketConn: pc, Handler: handler}
	s := &dns.Server{Listener: ln, Handler: handler}
	go u.ActivateAndServe()
	go s.ActivateAndServe()
	t.Cleanup(func() { u.Shutdown(); s.Shutdown() })
	return pc.LocalAddr().String()
}

func answerA(ip string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A " + ip)
		r.Answer = []dns.RR{rr}
		w.WriteMsg(r)
	}
}

func req(name string) *dnssrv.Request {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return &dnssrv.Request{Msg: m}
}

func TestForwardSuccessAndCaseRestore(t *testing.T) {
	addr := mockUpstream(t, answerA("5.6.7.8"))
	f, err := New(Config{Upstreams: []string{addr}, Strategy: "failover"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.Handler().ServeDNS(context.Background(), req("MiXeD.Example.COM"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != dnssrv.DecisionForwarded || resp.Upstream != addr {
		t.Fatalf("%+v", resp)
	}
	if resp.Msg.Question[0].Name != "MiXeD.Example.COM." {
		t.Fatalf("question case not restored: %s", resp.Msg.Question[0].Name)
	}
}

func TestFailoverSkipsDeadUpstream(t *testing.T) {
	good := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{"127.0.0.1:1", good}, Strategy: "failover", Timeout: 200 * time.Millisecond})
	resp, err := f.Handler().ServeDNS(context.Background(), req("x.test"))
	if err != nil || resp.Upstream != good {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestTruncationFallsBackToTCP(t *testing.T) {
	addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
			r.Truncated = true
		} else {
			rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 5.6.7.8")
			r.Answer = []dns.RR{rr}
		}
		w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{addr}, Strategy: "failover"})
	resp, err := f.Handler().ServeDNS(context.Background(), req("x.test"))
	if err != nil || len(resp.Msg.Answer) != 1 {
		t.Fatalf("tcp fallback failed: %+v %v", resp, err)
	}
}

func TestSpoofedCaseRejected(t *testing.T) {
	addr := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(m)
		r.Question[0].Name = "spoofed.example.com." // fails 0x20 echo check
		w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{addr}, Strategy: "failover", Timeout: 300 * time.Millisecond})
	if _, err := f.Handler().ServeDNS(context.Background(), req("real.example.com")); err == nil {
		t.Fatal("spoofed response accepted")
	}
}

func TestConditionalRouting(t *testing.T) {
	corp := mockUpstream(t, answerA("10.0.0.1"))
	pub := mockUpstream(t, answerA("5.6.7.8"))
	f, _ := New(Config{Upstreams: []string{pub}, Strategy: "failover", Conditional: map[string][]string{"corp.example": {corp}}})
	resp, _ := f.Handler().ServeDNS(context.Background(), req("vpn.corp.example"))
	if resp.Upstream != corp {
		t.Fatalf("conditional missed: %+v", resp)
	}
	resp, _ = f.Handler().ServeDNS(context.Background(), req("other.example"))
	if resp.Upstream != pub {
		t.Fatalf("default missed: %+v", resp)
	}
}

func TestFailureCacheShortCircuits(t *testing.T) {
	var hits atomic.Int64
	dead := mockUpstream(t, func(w dns.ResponseWriter, m *dns.Msg) {
		hits.Add(1) // count then never answer usefully
		r := new(dns.Msg)
		r.SetRcode(m, dns.RcodeServerFailure)
		w.WriteMsg(r)
	})
	f, _ := New(Config{Upstreams: []string{dead}, Strategy: "failover", Timeout: 200 * time.Millisecond})
	h := f.Handler()
	h.ServeDNS(context.Background(), req("down.test"))
	before := hits.Load()
	h.ServeDNS(context.Background(), req("down.test")) // within 30s failure cache
	if hits.Load() != before {
		t.Fatalf("failure cache did not short-circuit: %d -> %d", before, hits.Load())
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/upstream/ -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/upstream/forwarder.go`:
```go
package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/miekg/dns"
)

type Config struct {
	Upstreams   []string
	Strategy    string
	Timeout     time.Duration
	Conditional map[string][]string
}

type up struct {
	addr      string
	udp, tcp  *dns.Client
	ewmaMicro atomic.Int64
	fails     atomic.Int32
	downUntil atomic.Int64 // unix nano
}

func newUp(addr string, timeout time.Duration) *up {
	return &up{
		addr: addr,
		udp:  &dns.Client{Net: "udp", Timeout: timeout},
		tcp:  &dns.Client{Net: "tcp", Timeout: timeout},
	}
}

func (u *up) healthy(now time.Time) bool { return now.UnixNano() >= u.downUntil.Load() }

func (u *up) markResult(ok bool, latency time.Duration, now time.Time) {
	if ok {
		u.fails.Store(0)
		old := u.ewmaMicro.Load()
		u.ewmaMicro.Store((old*7 + latency.Microseconds()) / 8)
		return
	}
	if u.fails.Add(1) >= 3 {
		u.downUntil.Store(now.Add(15 * time.Second).UnixNano())
		u.fails.Store(0)
	}
}

type condRoute struct {
	set *filter.DomainSet
	ups []*up
}

type failKey struct {
	name  string
	qtype uint16
}

type Forwarder struct {
	def      []*up
	cond     []condRoute
	strategy string
	now      func() time.Time

	fmu       sync.Mutex
	failCache map[failKey]time.Time
}

func New(cfg Config) (*Forwarder, error) {
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("no upstreams configured")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Strategy == "" {
		cfg.Strategy = "race"
	}
	f := &Forwarder{strategy: cfg.Strategy, now: time.Now, failCache: map[failKey]time.Time{}}
	for _, a := range cfg.Upstreams {
		f.def = append(f.def, newUp(a, cfg.Timeout))
	}
	for suffix, addrs := range cfg.Conditional {
		set := filter.NewDomainSet()
		set.Add(suffix)
		var ups []*up
		for _, a := range addrs {
			ups = append(ups, newUp(a, cfg.Timeout))
		}
		f.cond = append(f.cond, condRoute{set: set, ups: ups})
	}
	return f, nil
}

func scramble(name string, rnd *rand.Rand) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'a' && c <= 'z' && rnd.IntN(2) == 1 {
			b[i] = c - 32
		}
	}
	return string(b)
}

// exchange sends m to u with 0x20 case randomization and TCP fallback.
func (f *Forwarder) exchange(ctx context.Context, m *dns.Msg, u *up) (*dns.Msg, error) {
	orig := m.Question[0].Name
	sent := m.Copy()
	rnd := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	sent.Question[0].Name = scramble(strings.ToLower(orig), rnd)
	start := f.now()
	r, _, err := u.udp.ExchangeContext(ctx, sent, u.addr)
	if err == nil && r.Truncated {
		r, _, err = u.tcp.ExchangeContext(ctx, sent, u.addr)
	}
	ok := err == nil && r != nil
	if ok && (len(r.Question) != 1 || r.Question[0].Name != sent.Question[0].Name) {
		ok = false
		err = fmt.Errorf("upstream %s: 0x20 case check failed", u.addr)
	}
	u.markResult(ok && r.Rcode != dns.RcodeServerFailure, f.now().Sub(start), f.now())
	if !ok {
		return nil, err
	}
	// restore original case everywhere it echoes
	r.Question[0].Name = orig
	for _, sec := range [][]dns.RR{r.Answer, r.Ns, r.Extra} {
		for _, rr := range sec {
			if strings.EqualFold(rr.Header().Name, orig) {
				rr.Header().Name = orig
			}
		}
	}
	return r, nil
}

func (f *Forwarder) pick(qname string) []*up {
	for _, c := range f.cond {
		if _, ok := c.set.Match(qname); ok {
			return c.ups
		}
	}
	return f.def
}

func (f *Forwarder) Handler() dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		k := failKey{req.QName(), req.QType()}
		f.fmu.Lock()
		until, failed := f.failCache[k]
		f.fmu.Unlock()
		if failed && f.now().Before(until) {
			return dnssrv.Servfail(req), nil // RFC 9520
		}
		candidates := f.pick(req.QName())
		var healthy []*up
		for _, u := range candidates {
			if u.healthy(f.now()) {
				healthy = append(healthy, u)
			}
		}
		if len(healthy) == 0 {
			healthy = candidates // all down: try anyway rather than refusing
		}
		var r *dns.Msg
		var lastErr error
		var winner *up
		switch f.strategy {
		case "race":
			r, winner, lastErr = f.race(ctx, req.Msg, healthy)
		default: // failover, fastest
			ordered := healthy
			if f.strategy == "fastest" {
				ordered = append([]*up{}, healthy...)
				sort.Slice(ordered, func(i, j int) bool {
					return ordered[i].ewmaMicro.Load() < ordered[j].ewmaMicro.Load()
				})
			}
			for _, u := range ordered {
				if r, lastErr = f.exchange(ctx, req.Msg, u); lastErr == nil && r.Rcode != dns.RcodeServerFailure {
					winner = u
					break
				}
				r = nil
			}
		}
		if r == nil {
			f.fmu.Lock()
			f.failCache[k] = f.now().Add(30 * time.Second)
			f.fmu.Unlock()
			if lastErr == nil {
				lastErr = errors.New("all upstreams failed")
			}
			return nil, lastErr
		}
		return &dnssrv.Response{Msg: r, Decision: dnssrv.DecisionForwarded, Upstream: winner.addr}, nil
	})
}

func (f *Forwarder) race(ctx context.Context, m *dns.Msg, ups []*up) (*dns.Msg, *up, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		r   *dns.Msg
		u   *up
		err error
	}
	ch := make(chan result, len(ups))
	for _, u := range ups {
		go func(u *up) {
			r, err := f.exchange(ctx, m, u)
			ch <- result{r, u, err}
		}(u)
	}
	var lastErr error
	for range ups {
		res := <-ch
		if res.err == nil && res.r.Rcode != dns.RcodeServerFailure {
			return res.r, res.u, nil
		}
		lastErr = res.err
	}
	return nil, nil, lastErr
}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test -race ./internal/upstream/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/upstream && git commit -m "feat: upstream forwarder with 0x20, strategies, failure caching"
```

---

### Task 13: Query log

**Files:**
- Modify: `internal/store/store.go` (add `QueryLog()` + `QueryLogStore` to interface), `internal/store/sql.go` (accessor)
- Create: `internal/store/qlogstore.go`, `internal/qlog/qlog.go`, `internal/qlog/qlog_test.go`, `internal/store/qlogstore_test.go`

**Interfaces:**
- Consumes: `dnssrv` types, `store` types.
- Produces:
  - `store.QueryLogStore{ InsertBatch(ctx, []QueryLogEntry) error; DeleteBefore(ctx, cutoffMs int64) (int64, error) }` implemented for both dialects.
  - `qlog.New(qs store.QueryLogStore, o Options) *Logger`; `Options{BatchSize int; FlushEvery time.Duration; Buffer int; Privacy string; InstanceID string; Now func() time.Time}` (defaults 1000, 1s, 10000, `full`); `(*Logger).Middleware() dnssrv.Middleware` (outermost stage: measures duration, records decision/rcode/upstream/rule/list; on pipeline error records Decision `error`, rcode SERVFAIL); `(*Logger).Run(ctx)` (flush loop; drains buffer on shutdown); `(*Logger).Dropped() int64`. Privacy: `none` → skip logging entirely; `anon` → IPv4 last octet zeroed, IPv6 truncated to /48; `full` → verbatim.
  - `qlog.NewPruner(qs store.QueryLogStore, retentionDays func() int64) *Pruner`; `(*Pruner).Run(ctx)` — daily `DeleteBefore(now - retention)`.

- [ ] **Step 1: Write failing tests**

`internal/store/qlogstore_test.go`:
```go
package store

import (
	"context"
	"testing"
)

func TestQueryLogInsertAndPrune(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		batch := []QueryLogEntry{
			{At: 1000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "old.example", QType: "A", Decision: "forwarded", RCode: "NOERROR", DurationMs: 12},
			{At: 2000, InstanceID: "i1", ClientIP: "10.0.0.5", QName: "new.example", QType: "A", Decision: "blocked", RCode: "NOERROR", DurationMs: 1},
		}
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		n, err := s.QueryLog().DeleteBefore(ctx, 1500)
		if err != nil || n != 1 {
			t.Fatalf("deleted %d err %v", n, err)
		}
	})
}
```

`internal/qlog/qlog_test.go`:
```go
package qlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type fakeQLStore struct {
	mu      sync.Mutex
	batches [][]store.QueryLogEntry
	cutoff  int64
}

func (f *fakeQLStore) InsertBatch(ctx context.Context, b []store.QueryLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]store.QueryLogEntry{}, b...)
	f.batches = append(f.batches, cp)
	return nil
}

func (f *fakeQLStore) DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoff = cutoffMs
	return 0, nil
}

func (f *fakeQLStore) entries() []store.QueryLogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.QueryLogEntry
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func blockedHandler() dnssrv.Handler {
	return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionBlocked, ListID: 7}, nil
	})
}

func TestMiddlewareEmitsAndFlushes(t *testing.T) {
	fs := &fakeQLStore{}
	l := New(fs, Options{BatchSize: 1, FlushEvery: 10 * time.Millisecond, InstanceID: "i1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	h := l.Middleware()(blockedHandler())
	m := new(dns.Msg)
	m.SetQuestion("ads.example.", dns.TypeA)
	h.ServeDNS(context.Background(), &dnssrv.Request{Msg: m})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if es := fs.entries(); len(es) == 1 {
			e := es[0]
			if e.QName != "ads.example" || e.Decision != "blocked" || e.ListID != 7 || e.InstanceID != "i1" {
				t.Fatalf("%+v", e)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("entry never flushed")
}

func TestAnonPrivacy(t *testing.T) {
	if got := anonymize("10.1.2.3"); got != "10.1.2.0" {
		t.Fatalf("v4 anon: %s", got)
	}
	if got := anonymize("2001:db8:abcd:1234::1"); got != "2001:db8:abcd::" {
		t.Fatalf("v6 anon: %s", got)
	}
}

func TestPrunerUsesRetention(t *testing.T) {
	fs := &fakeQLStore{}
	p := NewPruner(fs, func() int64 { return 90 })
	p.pruneOnce(context.Background(), time.UnixMilli(100*24*3600*1000))
	want := int64((100 - 90) * 24 * 3600 * 1000)
	if fs.cutoff != want {
		t.Fatalf("cutoff %d want %d", fs.cutoff, want)
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/qlog/ ./internal/store/ -run 'TestQueryLog|TestMiddleware|TestAnon|TestPruner' -v` — Expected: FAIL.

- [ ] **Step 3: Implement store side**

`internal/store/qlogstore.go`:
```go
package store

import (
	"context"
	"strings"
)

type queryLogStore struct{ s *sqlStore }

func (q *queryLogStore) InsertBatch(ctx context.Context, batch []QueryLogEntry) error {
	if len(batch) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO query_log (at, instance_id, client_ip, client_id, qname, qtype, decision, rule_id, list_id, upstream, rcode, duration_ms) VALUES `)
	args := make([]any, 0, len(batch)*12)
	for i, e := range batch {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, e.At, e.InstanceID, e.ClientIP, e.ClientID, e.QName, e.QType, e.Decision, e.RuleID, e.ListID, e.Upstream, e.RCode, e.DurationMs)
	}
	_, err := q.s.db.ExecContext(ctx, q.s.q(sb.String()), args...)
	return err
}

func (q *queryLogStore) DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := q.s.db.ExecContext(ctx, q.s.q(`DELETE FROM query_log WHERE at < ?`), cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```
Add `QueryLog() QueryLogStore` accessor on `sqlStore` and to the `Store` interface.

- [ ] **Step 4: Implement logger + pruner**

`internal/qlog/qlog.go`:
```go
package qlog

import (
	"context"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

type Options struct {
	BatchSize  int
	FlushEvery time.Duration
	Buffer     int
	Privacy    string
	InstanceID string
	Now        func() time.Time
}

type Logger struct {
	qs      store.QueryLogStore
	o       Options
	ch      chan store.QueryLogEntry
	dropped atomic.Int64
}

func New(qs store.QueryLogStore, o Options) *Logger {
	if o.BatchSize == 0 {
		o.BatchSize = 1000
	}
	if o.FlushEvery == 0 {
		o.FlushEvery = time.Second
	}
	if o.Buffer == 0 {
		o.Buffer = 10000
	}
	if o.Privacy == "" {
		o.Privacy = "full"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Logger{qs: qs, o: o, ch: make(chan store.QueryLogEntry, o.Buffer)}
}

func (l *Logger) Dropped() int64 { return l.dropped.Load() }

func anonymize(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a.Is4() {
		b := a.As4()
		b[3] = 0
		return netip.AddrFrom4(b).String()
	}
	b := a.As16()
	for i := 6; i < 16; i++ {
		b[i] = 0
	}
	return netip.AddrFrom16(b).String()
}

func (l *Logger) emit(e store.QueryLogEntry) {
	for {
		select {
		case l.ch <- e:
			return
		default: // full: drop oldest, keep newest
			select {
			case <-l.ch:
				l.dropped.Add(1)
			default:
			}
		}
	}
}

func (l *Logger) Middleware() dnssrv.Middleware {
	return func(next dnssrv.Handler) dnssrv.Handler {
		return dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
			start := l.o.Now()
			resp, err := next.ServeDNS(ctx, req)
			if l.o.Privacy == "none" {
				return resp, err
			}
			e := store.QueryLogEntry{
				At: start.UnixMilli(), InstanceID: l.o.InstanceID,
				ClientIP: req.ClientIP.String(), ClientID: req.Client.ID,
				QName: req.QName(), QType: dns.TypeToString[req.QType()],
				DurationMs: l.o.Now().Sub(start).Milliseconds(),
			}
			if l.o.Privacy == "anon" {
				e.ClientIP = anonymize(e.ClientIP)
			}
			if err != nil || resp == nil {
				e.Decision, e.RCode = string(dnssrv.DecisionError), "SERVFAIL"
			} else {
				e.Decision = string(resp.Decision)
				e.RuleID, e.ListID, e.Upstream = resp.RuleID, resp.ListID, resp.Upstream
				if resp.Msg != nil {
					e.RCode = dns.RcodeToString[resp.Msg.Rcode]
				}
			}
			l.emit(e)
			return resp, err
		})
	}
}

func (l *Logger) Run(ctx context.Context) {
	t := time.NewTicker(l.o.FlushEvery)
	defer t.Stop()
	batch := make([]store.QueryLogEntry, 0, l.o.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := l.qs.InsertBatch(fctx, batch); err != nil {
			slog.Warn("query log flush failed", "n", len(batch), "err", err)
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			for { // drain what's buffered, then final flush
				select {
				case e := <-l.ch:
					batch = append(batch, e)
					if len(batch) >= l.o.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-l.ch:
			batch = append(batch, e)
			if len(batch) >= l.o.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

type Pruner struct {
	qs            store.QueryLogStore
	retentionDays func() int64
}

func NewPruner(qs store.QueryLogStore, retentionDays func() int64) *Pruner {
	return &Pruner{qs: qs, retentionDays: retentionDays}
}

func (p *Pruner) pruneOnce(ctx context.Context, now time.Time) {
	cutoff := now.UnixMilli() - p.retentionDays()*24*3600*1000
	if n, err := p.qs.DeleteBefore(ctx, cutoff); err != nil {
		slog.Warn("query log prune failed", "err", err)
	} else if n > 0 {
		slog.Info("query log pruned", "rows", n)
	}
}

func (p *Pruner) Run(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	p.pruneOnce(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pruneOnce(ctx, time.Now())
		}
	}
}
```

- [ ] **Step 5: Run tests, verify pass; commit**

Run: `go test -race ./internal/qlog/ ./internal/store/ -v` — Expected: PASS.

```bash
git add internal/qlog internal/store && git commit -m "feat: async query log with batching, privacy modes, pruner"
```

---

### Task 14: Stats rollups

**Files:**
- Modify: `internal/store/store.go` (add `Stats()` + `StatsStore` to interface), `internal/store/sql.go` (accessor)
- Create: `internal/store/statsstore.go`, `internal/stats/rollup.go`, `internal/store/statsstore_test.go`

**Interfaces:**
- Produces:
  - `store.StatsStore{ Rollup(ctx, afterID int64) (lastID int64, err error); Counter(ctx, bucketFromSec int64, metric string) (map[string]int64, error) }`. `Rollup` aggregates `query_log` rows with `id > afterID` into `stats_hourly` (bucket = hour-start unix seconds) in one transaction and returns the max id processed (or `afterID` when none). Metrics written: `decision` (key = decision value), `domain` (key = qname, only decisions `forwarded`/`cached`/`stale`), `blocked_domain` (key = qname where decision `blocked`), `client` (key = client_ip).
  - `stats.NewRunner(ss store.StatsStore, settings store.SettingsStore, every time.Duration) *Runner`; `(*Runner).Run(ctx)` — loop: read watermark from settings key `stats.watermark` (missing → 0), call `Rollup`, write the watermark back. Because `Set` bumps the config version (and would trigger a reload every minute), this task also adds `SettingsStore.SetInternal(ctx, key, value string) error` — an upsert with no version bump and no notification — used for the watermark (and by tests that stage settings without triggering reloads).
- [ ] **Step 1: Write failing tests**

`internal/store/statsstore_test.go`:
```go
package store

import (
	"context"
	"testing"
)

func TestRollupAndCounter(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		hourMs := int64(3600 * 1000)
		batch := []QueryLogEntry{
			{At: hourMs + 1, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
			{At: hourMs + 2, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "cached", RCode: "NOERROR"},
			{At: hourMs + 3, InstanceID: "i", ClientIP: "10.0.0.6", QName: "ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR"},
		}
		if err := s.QueryLog().InsertBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		last, err := s.Stats().Rollup(ctx, 0)
		if err != nil || last == 0 {
			t.Fatalf("rollup: last=%d err=%v", last, err)
		}
		dec, err := s.Stats().Counter(ctx, 0, "decision")
		if err != nil {
			t.Fatal(err)
		}
		if dec["forwarded"] != 1 || dec["cached"] != 1 || dec["blocked"] != 1 {
			t.Fatalf("decisions: %v", dec)
		}
		dom, _ := s.Stats().Counter(ctx, 0, "domain")
		if dom["a.example"] != 2 || dom["ads.example"] != 0 {
			t.Fatalf("domains: %v", dom)
		}
		bd, _ := s.Stats().Counter(ctx, 0, "blocked_domain")
		if bd["ads.example"] != 1 {
			t.Fatalf("blocked domains: %v", bd)
		}
		// idempotent: second rollup from watermark adds nothing
		if _, err := s.Stats().Rollup(ctx, last); err != nil {
			t.Fatal(err)
		}
		dec2, _ := s.Stats().Counter(ctx, 0, "decision")
		if dec2["forwarded"] != 1 {
			t.Fatalf("double counted: %v", dec2)
		}
	})
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/store/ -run TestRollup -v` — Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/store/statsstore.go`:
```go
package store

import "context"

type statsStore struct{ s *sqlStore }

const upsertStat = ` ON CONFLICT (bucket, metric, key) DO UPDATE SET value = stats_hourly.value + excluded.value`

func (st *statsStore) Rollup(ctx context.Context, afterID int64) (int64, error) {
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var lastID int64
	err = tx.QueryRowContext(ctx, st.s.q(`SELECT COALESCE(MAX(id), ?) FROM query_log WHERE id > ?`), afterID, afterID).Scan(&lastID)
	if err != nil {
		return 0, err
	}
	if lastID == afterID {
		return afterID, tx.Commit()
	}
	stmts := []string{
		`INSERT INTO stats_hourly (bucket, metric, key, value)
		   SELECT (at/3600000)*3600, 'decision', decision, COUNT(*) FROM query_log
		   WHERE id > ? AND id <= ? GROUP BY (at/3600000)*3600, decision` + upsertStat,
		`INSERT INTO stats_hourly (bucket, metric, key, value)
		   SELECT (at/3600000)*3600, 'domain', qname, COUNT(*) FROM query_log
		   WHERE id > ? AND id <= ? AND decision IN ('forwarded','cached','stale')
		   GROUP BY (at/3600000)*3600, qname` + upsertStat,
		`INSERT INTO stats_hourly (bucket, metric, key, value)
		   SELECT (at/3600000)*3600, 'blocked_domain', qname, COUNT(*) FROM query_log
		   WHERE id > ? AND id <= ? AND decision = 'blocked'
		   GROUP BY (at/3600000)*3600, qname` + upsertStat,
		`INSERT INTO stats_hourly (bucket, metric, key, value)
		   SELECT (at/3600000)*3600, 'client', client_ip, COUNT(*) FROM query_log
		   WHERE id > ? AND id <= ? GROUP BY (at/3600000)*3600, client_ip` + upsertStat,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, st.s.q(q), afterID, lastID); err != nil {
			return 0, err
		}
	}
	return lastID, tx.Commit()
}

func (st *statsStore) Counter(ctx context.Context, bucketFromSec int64, metric string) (map[string]int64, error) {
	rows, err := st.s.db.QueryContext(ctx, st.s.q(`SELECT key, SUM(value) FROM stats_hourly WHERE metric = ? AND bucket >= ? GROUP BY key`), metric, bucketFromSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
```

Add `SetInternal` to `SettingsStore` (upsert only, no version bump, no notify — same upsert SQL as `Set` without the version statements), plus a one-case test in `settings_test.go` asserting `SetInternal` does not change `ConfigVersion`.

`internal/stats/rollup.go`:
```go
package stats

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

type Runner struct {
	ss       store.StatsStore
	settings store.SettingsStore
	every    time.Duration
}

func NewRunner(ss store.StatsStore, settings store.SettingsStore, every time.Duration) *Runner {
	return &Runner{ss: ss, settings: settings, every: every}
}

func (r *Runner) once(ctx context.Context) {
	var after int64
	if v, ok, _ := r.settings.Get(ctx, "stats.watermark"); ok {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	last, err := r.ss.Rollup(ctx, after)
	if err != nil {
		slog.Warn("stats rollup failed", "err", err)
		return
	}
	if last != after {
		r.settings.SetInternal(ctx, "stats.watermark", strconv.FormatInt(last, 10))
	}
}

func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.once(ctx)
		}
	}
}
```

- [ ] **Step 4: Run tests, verify pass; commit**

Run: `go test -race ./internal/store/ ./internal/stats/ -v` — Expected: PASS.

```bash
git add internal/store internal/stats && git commit -m "feat: hourly stats rollups with watermark"
```

---

### Task 15: App wiring, hot-reload, end-to-end test

**Files:**
- Create: `internal/app/app.go`, `internal/app/app_e2e_test.go`
- Modify: `cmd/dnsaur/main.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `app.New(ctx, cfg *config.Config) (*App, error)` (opens store, seeds defaults + `default` group id 1, builds all components); `(*App).Start(ctx) error` (starts DNS servers, background jobs, reload watcher; initial async `RefreshAll`); `(*App).Shutdown(ctx) error` (stops listeners, drains query log); `(*App).Store() store.Store` (for tests and later the API server); `(*App).DNSAddr() string` (first bound listener). Pipeline order (outermost first): `qlog → Recover → client-id → filter → local records → cache → forwarder`. Hot-reload: on `Settings().Changes()`, re-read settings, `registry.Reload`, `records.Reload`, `engine.SetBlocking`, rebuild forwarder, and re-run `RefreshAll` — all errors logged, never fatal.
- Seeded defaults: the settings map from Task 4 (with `instance.id` = `uuid.NewString()` only when missing) and group `default` when no groups exist.

- [ ] **Step 1: Write failing e2e test**

`internal/app/app_e2e_test.go`:
```go
package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

func mockUpstream(t *testing.T, counter *atomic.Int64) string {
	t.Helper()
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		counter.Add(1)
		r := new(dns.Msg)
		r.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 9.9.9.9")
		r.Answer = []dns.RR{rr}
		w.WriteMsg(r)
	})}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestEndToEnd(t *testing.T) {
	ctx := context.Background()
	var upstreamHits atomic.Int64
	upAddr := mockUpstream(t, &upstreamHits)
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0.0.0.0 ads.example.com\n"))
	}))
	defer listSrv.Close()

	dir := t.TempDir()
	cfg := &config.Config{DNSListen: []string{"127.0.0.1:0"}, HTTPListen: ":0", DataDir: dir, LogLevel: "error"}
	cfg.Storage.Driver = "sqlite"
	cfg.Storage.DSN = dir + "/t.db"

	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := a.Store()
	s.Settings().SetInternal(ctx, "upstreams", upAddr)
	lid, _ := s.Filters().AddList(ctx, store.List{URL: listSrv.URL, Kind: "block", Enabled: true})
	s.Filters().AssignList(ctx, 1, lid)
	s.Records().Add(ctx, store.LocalRecord{Name: "nas.home.lan", Type: "A", Value: "10.0.0.9", TTL: 300})

	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown(context.Background())
	a.WaitReady(5 * time.Second) // blocks until initial RefreshAll + reloads done

	c := new(dns.Client)
	ask := func(name string) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		r, _, err := c.Exchange(m, a.DNSAddr())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := ask("ads.example.com"); r.Answer[0].(*dns.A).A.String() != "0.0.0.0" {
		t.Fatalf("not blocked: %v", r.Answer)
	}
	if r := ask("nas.home.lan"); r.Answer[0].(*dns.A).A.String() != "10.0.0.9" {
		t.Fatalf("local record: %v", r.Answer)
	}
	if r := ask("clean.example.org"); r.Answer[0].(*dns.A).A.String() != "9.9.9.9" {
		t.Fatalf("forward: %v", r.Answer)
	}
	before := upstreamHits.Load()
	ask("clean.example.org") // cached now
	if upstreamHits.Load() != before {
		t.Fatal("cache miss on repeat query")
	}
}
```

- [ ] **Step 2: Run, verify failure**

Run: `go test ./internal/app/ -v` — Expected: FAIL (`app.New` undefined).

- [ ] **Step 3: Implement app**

`internal/app/app.go`:
```go
package app

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/cache"
	"github.com/aloks98/dnsaur/internal/clients"
	"github.com/aloks98/dnsaur/internal/config"
	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/records"
	"github.com/aloks98/dnsaur/internal/stats"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/aloks98/dnsaur/internal/upstream"
	"github.com/google/uuid"
)

func defaultSettings() map[string]string {
	return map[string]string{
		"instance.id":          uuid.NewString(),
		"upstreams":            "1.1.1.1:53,1.0.0.1:53,9.9.9.9:53",
		"upstream.strategy":    "race",
		"blocking.mode":        "null-ip",
		"blocking.ttl":         "30",
		"cache.min_ttl":        "0",
		"cache.max_ttl":        "86400",
		"cache.max_entries":    "10000",
		"cache.serve_stale_for": "86400",
		"lists.refresh_hours":  "24",
		"qlog.retention_days":  "90",
		"qlog.privacy":         "full",
	}
}

// swappable lets us rebuild the tail of the pipeline (forwarder) on
// settings changes without restarting listeners.
type swappable struct {
	mu sync.RWMutex
	h  dnssrv.Handler
}

func (s *swappable) set(h dnssrv.Handler) { s.mu.Lock(); s.h = h; s.mu.Unlock() }
func (s *swappable) ServeDNS(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
	s.mu.RLock()
	h := s.h
	s.mu.RUnlock()
	return h.ServeDNS(ctx, req)
}

type App struct {
	cfg      *config.Config
	st       store.Store
	registry *clients.Registry
	engine   *filter.Engine
	resolver *records.Resolver
	refresher *filter.Refresher
	logger   *qlog.Logger
	fwd      *swappable
	servers  []*dnssrv.Server
	bg       []func(context.Context)
	cancel   context.CancelFunc
	ready    chan struct{}
	wg       sync.WaitGroup
}

func New(ctx context.Context, cfg *config.Config) (*App, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, cfg.Storage.Driver, cfg.Storage.DSN)
	if err != nil {
		return nil, err
	}
	if err := st.Settings().SeedDefaults(ctx, defaultSettings()); err != nil {
		return nil, err
	}
	if gs, err := st.Clients().Groups(ctx); err != nil {
		return nil, err
	} else if len(gs) == 0 {
		if _, err := st.Clients().AddGroup(ctx, "default"); err != nil {
			return nil, err
		}
	}
	a := &App{
		cfg:      cfg,
		st:       st,
		registry: clients.NewRegistry(st.Clients()),
		engine:   filter.NewEngine(),
		resolver: records.NewResolver(st.Records()),
		fwd:      &swappable{},
		ready:    make(chan struct{}),
	}
	a.refresher = filter.NewRefresher(st.Filters(), st.Clients(), a.engine, cfg.DataDir)
	return a, nil
}

func (a *App) Store() store.Store { return a.st }

func (a *App) getSetting(ctx context.Context, key string) string {
	v, _, _ := a.st.Settings().Get(ctx, key)
	return v
}

func (a *App) getInt(ctx context.Context, key string, fallback int64) int64 {
	v, err := a.st.Settings().GetInt(ctx, key)
	if err != nil {
		return fallback
	}
	return v
}

// applySettings re-reads DB settings into live components.
func (a *App) applySettings(ctx context.Context) {
	mode := a.getSetting(ctx, "blocking.mode")
	a.engine.SetBlocking(mode, uint32(a.getInt(ctx, "blocking.ttl", 30)))
	if err := a.registry.Reload(ctx); err != nil {
		slog.Error("client reload failed", "err", err)
	}
	if err := a.resolver.Reload(ctx); err != nil {
		slog.Error("records reload failed", "err", err)
	}
	fwd, err := upstream.New(upstream.Config{
		Upstreams: strings.Split(a.getSetting(ctx, "upstreams"), ","),
		Strategy:  a.getSetting(ctx, "upstream.strategy"),
	})
	if err != nil {
		slog.Error("keeping previous upstream config", "err", err)
		return
	}
	a.fwd.set(fwd.Handler())
}

func (a *App) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	a.applySettings(ctx)

	instanceID := a.getSetting(ctx, "instance.id")
	a.logger = qlog.New(a.st.QueryLog(), qlog.Options{
		Privacy: a.getSetting(ctx, "qlog.privacy"), InstanceID: instanceID,
	})
	dnsCache := cache.New(cache.Options{
		MinTTL:        time.Duration(a.getInt(ctx, "cache.min_ttl", 0)) * time.Second,
		MaxTTL:        time.Duration(a.getInt(ctx, "cache.max_ttl", 86400)) * time.Second,
		ServeStaleFor: time.Duration(a.getInt(ctx, "cache.serve_stale_for", 86400)) * time.Second,
		MaxEntries:    int(a.getInt(ctx, "cache.max_entries", 10000)),
	})
	handler := dnssrv.Chain(a.fwd,
		a.logger.Middleware(),
		dnssrv.Recover(),
		a.registry.Middleware(),
		a.engine.Middleware(),
		a.resolver.Middleware(),
		dnsCache.Middleware(),
	)
	for _, addr := range a.cfg.DNSListen {
		s := dnssrv.NewServer(addr, handler)
		if err := s.Start(); err != nil {
			return err
		}
		a.servers = append(a.servers, s)
	}

	pruner := qlog.NewPruner(a.st.QueryLog(), func() int64 { return a.getInt(ctx, "qlog.retention_days", 90) })
	rollups := stats.NewRunner(a.st.Stats(), a.st.Settings(), time.Minute)
	refreshEvery := time.Duration(a.getInt(ctx, "lists.refresh_hours", 24)) * time.Hour
	changes := a.st.Settings().Changes()

	a.bg = []func(context.Context){
		a.logger.Run,
		pruner.Run,
		rollups.Run,
		func(c context.Context) { a.refresher.Run(c, refreshEvery) },
		func(c context.Context) {
			for {
				select {
				case <-c.Done():
					return
				case <-changes:
					a.applySettings(c)
					if err := a.refresher.RefreshAll(c); err != nil {
						slog.Error("refresh after settings change failed", "err", err)
					}
				}
			}
		},
	}
	for _, f := range a.bg {
		a.wg.Add(1)
		go func(f func(context.Context)) { defer a.wg.Done(); f(runCtx) }(f)
	}
	go func() { // initial list load; readiness gate for tests
		if err := a.refresher.RefreshAll(runCtx); err != nil {
			slog.Error("initial blocklist refresh failed", "err", err)
		}
		close(a.ready)
	}()
	return nil
}

func (a *App) WaitReady(d time.Duration) {
	select {
	case <-a.ready:
	case <-time.After(d):
	}
}

func (a *App) DNSAddr() string {
	if len(a.servers) == 0 {
		return ""
	}
	return a.servers[0].Addr()
}

func (a *App) Shutdown(ctx context.Context) error {
	for _, s := range a.servers {
		s.Shutdown(ctx)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait() // qlog drains its buffer on ctx cancel before returning
	return a.st.Close()
}
```

`cmd/dnsaur/main.go` — replace `run`:
```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aloks98/dnsaur/internal/app"
	"github.com/aloks98/dnsaur/internal/config"
)

var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfgPath := flag.String("config", "dnsaur.yaml", "path to bootstrap config")
	flag.Parse()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var lvl slog.Level
	lvl.UnmarshalText([]byte(cfg.LogLevel))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))
	slog.Info("dnsaur starting", "version", Version)

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	if err := a.Start(ctx); err != nil {
		return err
	}
	slog.Info("dns listening", "addr", a.DNSAddr())
	<-ctx.Done()
	slog.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.Shutdown(shCtx)
}
```

- [ ] **Step 4: Run all tests, verify pass**

Run: `go test -race ./...`
Expected: PASS across all packages, including the e2e test.

- [ ] **Step 5: Manual smoke test**

```bash
go build ./cmd/dnsaur && DNSAUR_DNS_LISTEN=127.0.0.1:5353 ./dnsaur &
sleep 2
dig @127.0.0.1 -p 5353 example.com +short   # expect a real answer
kill %1
```

- [ ] **Step 6: Commit**

```bash
git add internal/app cmd/dnsaur && git commit -m "feat: app wiring with hot-reload and end-to-end tests"
```

