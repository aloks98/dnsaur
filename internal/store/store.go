package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Store is the top-level persistence handle providing access to all data stores.
type Store interface {
	Clients() ClientStore
	Filters() FilterStore
	Records() RecordStore
	Settings() SettingsStore
	QueryLog() QueryLogStore
	Stats() StatsStore
	Close() error
}

// Group is a client group: a named bucket of clients sharing filter lists
// and rules.
type Group struct {
	ID      int64
	Name    string
	Enabled bool
}

// Client identifies a requester by IP or CIDR (Matcher) and assigns it to a
// Group.
type Client struct {
	ID      int64
	Name    string
	Matcher string // IP or CIDR string
	GroupID int64
}

// List is a remote filter list (blocklist or allowlist).
type List struct {
	ID            int64
	URL           string
	Kind          string // "block" | "allow"
	Enabled       bool
	LastRefreshed int64
	EntryCount    int64
}

// Rule is a per-group allow/block override, either literal or regex.
type Rule struct {
	ID      int64
	GroupID int64
	Action  string // "allow" | "block"
	Pattern string
	IsRegex bool
}

// LocalRecord is a locally-defined DNS record served without upstream
// resolution.
type LocalRecord struct {
	ID    int64
	Name  string
	Type  string
	Value string
	TTL   uint32
}

// QueryLogEntry records a single resolved DNS query for auditing and stats.
type QueryLogEntry struct {
	ID         int64
	At         int64
	InstanceID string
	ClientIP   string
	ClientID   int64
	QName      string
	QType      string
	Decision   string
	RuleID     int64
	ListID     int64
	Upstream   string
	RCode      string
	DurationMs int64
}

// ClientStore manages client groups and clients.
type ClientStore interface {
	Groups(ctx context.Context) ([]Group, error)
	Clients(ctx context.Context) ([]Client, error)
	AddGroup(ctx context.Context, name string) (int64, error)
	AddClient(ctx context.Context, c Client) (int64, error)
}

// FilterStore manages filter lists, their group assignments, and rules.
type FilterStore interface {
	Lists(ctx context.Context) ([]List, error)
	ListsForGroup(ctx context.Context, groupID int64) ([]List, error)
	Rules(ctx context.Context, groupID int64) ([]Rule, error)
	AddList(ctx context.Context, l List) (int64, error)
	AssignList(ctx context.Context, groupID, listID int64) error
	AddRule(ctx context.Context, r Rule) (int64, error)
	TouchList(ctx context.Context, id, refreshedAt, entryCount int64) error
}

// RecordStore manages locally-defined DNS records.
type RecordStore interface {
	All(ctx context.Context) ([]LocalRecord, error)
	Add(ctx context.Context, r LocalRecord) (int64, error)
}

// QueryLogStore manages DNS query log entries.
type QueryLogStore interface {
	InsertBatch(ctx context.Context, entries []QueryLogEntry) error
	DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error)
}

// StatsStore manages query statistics and hourly aggregations.
type StatsStore interface {
	Rollup(ctx context.Context, afterID int64) (lastID int64, err error)
	Counter(ctx context.Context, bucketFromSec int64, metric string) (map[string]int64, error)
}

// SettingsStore manages persisted configuration key-value pairs with version
// tracking and change notifications. All operations are atomic. Get returns
// (value, found, error); GetInt parses the value and fails if not set or
// non-numeric. Set bumps config_version and publishes the new version on
// Changes(). SetInternal upserts without bumping version or notifying. SeedDefaults only inserts missing keys, without bumping version.
// Changes() returns a read-only channel subscribed to config version updates.
type SettingsStore interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error
	SetInternal(ctx context.Context, key, value string) error
	GetInt(ctx context.Context, key string) (int64, error)
	ConfigVersion(ctx context.Context) (int64, error)
	Changes() <-chan int64
	SeedDefaults(ctx context.Context, defaults map[string]string) error
}

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
