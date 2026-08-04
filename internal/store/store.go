package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Sentinels for store operations
var (
	ErrInUse    = errors.New("resource in use")
	ErrNotFound = errors.New("not found")
)

// User represents an authenticated user.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	TOTPSecret   string `json:"-"`
	CreatedAt    int64  `json:"created_at"`
}

// AuthToken represents an API or session token.
type AuthToken struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Kind      string `json:"kind"` // "session" or "api"
	Name      string `json:"name"`
	TokenHash string `json:"-"`
	Scope     string `json:"scope"` // "read" or "write"
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	LastUsed  int64  `json:"last_used"`
}

// Store is the top-level persistence handle providing access to all data stores.
type Store interface {
	Clients() ClientStore
	Filters() FilterStore
	Records() RecordStore
	Settings() SettingsStore
	QueryLog() QueryLogStore
	Stats() StatsStore
	Users() UserStore
	Tokens() TokenStore
	Close() error
}

// Group is a client group: a named bucket of clients sharing filter lists
// and rules.
type Group struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Client identifies a requester by IP or CIDR (Matcher) and assigns it to a
// Group.
type Client struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Matcher string `json:"matcher"` // IP or CIDR string
	GroupID int64  `json:"group_id"`
}

// List is a remote filter list (blocklist or allowlist).
type List struct {
	ID            int64  `json:"id"`
	URL           string `json:"url"`
	Kind          string `json:"kind"` // "block" | "allow"
	Enabled       bool   `json:"enabled"`
	LastRefreshed int64  `json:"last_refreshed"`
	EntryCount    int64  `json:"entry_count"`
}

// Rule is a per-group allow/block override, either literal or regex.
type Rule struct {
	ID      int64  `json:"id"`
	GroupID int64  `json:"group_id"`
	Action  string `json:"action"` // "allow" | "block"
	Pattern string `json:"pattern"`
	IsRegex bool   `json:"is_regex"`
}

// LocalRecord is a locally-defined DNS record served without upstream
// resolution.
type LocalRecord struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   uint32 `json:"ttl"`
}

// QueryLogEntry records a single resolved DNS query for auditing and stats.
type QueryLogEntry struct {
	ID         int64  `json:"id"`
	At         int64  `json:"at"`
	InstanceID string `json:"instance_id"`
	ClientIP   string `json:"client_ip"`
	ClientID   int64  `json:"client_id"`
	QName      string `json:"q_name"`
	QType      string `json:"q_type"`
	Decision   string `json:"decision"`
	RuleID     int64  `json:"rule_id"`
	ListID     int64  `json:"list_id"`
	Upstream   string `json:"upstream"`
	RCode      string `json:"r_code"`
	DurationMs int64  `json:"duration_ms"`
}

// QueryLogFilter specifies optional filters for query log search.
// Zero values mean no constraint. Limit 0 defaults to 100, capped at 1000.
type QueryLogFilter struct {
	FromMs, ToMs                          int64
	ClientIP, QNameContains, Decision, QType string
	Limit, Offset                         int
}

// ClientStore manages client groups and clients.
type ClientStore interface {
	Groups(ctx context.Context) ([]Group, error)
	Clients(ctx context.Context) ([]Client, error)
	AddGroup(ctx context.Context, name string) (int64, error)
	AddClient(ctx context.Context, c Client) (int64, error)
	UpdateClient(ctx context.Context, c Client) error
	DeleteClient(ctx context.Context, id int64) error
	RenameGroup(ctx context.Context, id int64, name string) error
	SetGroupEnabled(ctx context.Context, id int64, enabled bool) error
	DeleteGroup(ctx context.Context, id int64) error
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
	SetListEnabled(ctx context.Context, id int64, enabled bool) error
	DeleteList(ctx context.Context, id int64) error
	UnassignList(ctx context.Context, groupID, listID int64) error
	DeleteRule(ctx context.Context, id int64) error
}

// RecordStore manages locally-defined DNS records.
type RecordStore interface {
	All(ctx context.Context) ([]LocalRecord, error)
	Add(ctx context.Context, r LocalRecord) (int64, error)
	Update(ctx context.Context, r LocalRecord) error
	Delete(ctx context.Context, id int64) error
}

// QueryLogStore manages DNS query log entries.
type QueryLogStore interface {
	InsertBatch(ctx context.Context, entries []QueryLogEntry) error
	DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error)
	Search(ctx context.Context, f QueryLogFilter) ([]QueryLogEntry, error)
}

// StatsStore manages query statistics and hourly aggregations.
type StatsStore interface {
	Rollup(ctx context.Context, afterID int64) (lastID int64, err error)
	Counter(ctx context.Context, bucketFromSec int64, metric string) (map[string]int64, error)
	Timeline(ctx context.Context, fromSec int64) (map[int64]map[string]int64, error)
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
	All(ctx context.Context) (map[string]string, error)
}

// UserStore manages user accounts.
type UserStore interface {
	Create(ctx context.Context, u User) (int64, error)
	// CreateIfNone atomically inserts u only if the users table is empty,
	// so two concurrent first-run setup requests can't both create an
	// admin account. created is true iff this call won the race.
	CreateIfNone(ctx context.Context, u User) (created bool, err error)
	ByUsername(ctx context.Context, name string) (User, bool, error)
	ByID(ctx context.Context, id int64) (User, bool, error)
	Count(ctx context.Context) (int64, error)
	SetTOTP(ctx context.Context, id int64, secret string) error
}

// TokenStore manages API and session tokens.
type TokenStore interface {
	Create(ctx context.Context, t AuthToken) (int64, error)
	ByHash(ctx context.Context, hash string) (AuthToken, bool, error)
	Delete(ctx context.Context, id int64) error
	ListAPI(ctx context.Context, userID int64) ([]AuthToken, error)
	Touch(ctx context.Context, id, ts int64) error
	DeleteExpired(ctx context.Context, nowMs int64) error
	SetExpiry(ctx context.Context, id, ts int64) error
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
