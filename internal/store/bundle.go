package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// BundleFormat is the bundle document this build reads and writes. A replica
// refuses a format it does not know rather than applying the part of it it
// happens to understand (spec §10): an unknown format means the main is
// newer, and the answer is to upgrade the replica, not to guess.
const BundleFormat = 1

const (
	// instanceIDSetting identifies the box a bundle came from. It is a
	// local key (§4.3) — the query log is tagged with it — so it travels in
	// its own field rather than in Settings, where it would overwrite the
	// replica's own identity.
	instanceIDSetting = "instance.id"
	// syncKeySetting is the TSIG key the main designates for its replicas'
	// transfers (§6). Also local, and also needed on the other side: the
	// replica signs with the main's key, so it rides as Bundle.SyncKey.
	syncKeySetting = "sync.tsig_key_id"
	// zoneTypeInternal is the built-in (RFC 6303) zone type. Both boxes
	// seed their own from migration 7, so a zone of this type is in no
	// bundle and is not touched by an import (§4.2).
	zoneTypeInternal = "internal"
)

// localPrefixes are the settings namespaces §4.3 keeps on the instance: the
// box's identity, its listen addresses and certificate paths, and its own
// relationship to a main. Everything outside them describes the service both
// boxes provide and is synced.
var localPrefixes = []string{"instance.", "serve.", "sync."}

// LocalSettingKey reports whether key stays on the instance that holds it
// (§4.3). It is the single answer to that question: ExportBundle uses it to
// decide what leaves the main, and ImportBundle uses it both to decide what a
// bundle may overwrite and to protect a replica's own keys from being pruned
// as "absent from the bundle".
func LocalSettingKey(key string) bool {
	// Bookkeeping for this box's own rollup of this box's own query log.
	if key == StatsWatermarkKey {
		return true
	}
	for _, p := range localPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// BundleList is a filter list plus the groups it is assigned to, because the
// assignment lives in its own table and a bundle is one document.
type BundleList struct {
	List
	Groups []int64 `json:"groups"`
}

// Bundle is the whole of one instance's synced configuration (spec §4): what
// a replica needs to behave the same as its main, and nothing that describes
// either box.
//
// The ids are the main's, and ImportBundle keeps them — see §4.1 for why that
// is safe and what it buys. Zone records are deliberately absent: a replica
// gets those by AXFR, on the schedule the SOA gives.
type Bundle struct {
	Format        int               `json:"format"`
	ConfigVersion int64             `json:"config_version"`
	InstanceID    string            `json:"instance_id"`
	Settings      map[string]string `json:"settings"`
	// SyncKey is the main's sync.tsig_key_id; 0 when it has designated no
	// key. It is not written to the replica's settings — sync.* is local —
	// it is what the replica's derived secondaries sign with.
	SyncKey  int64        `json:"sync_key"`
	Groups   []Group      `json:"groups"`
	Clients  []Client     `json:"clients"`
	Lists    []BundleList `json:"lists"`
	Rules    []Rule       `json:"rules"`
	TSIGKeys []TSIGKey    `json:"tsig_keys"`
	// Zones are definitions only, and never of type internal.
	Zones []Zone `json:"zones"`
}

// ExportBundle reads this instance's synced configuration into one document.
//
// config_version is read first, so a bundle can only ever be stamped with a
// version at or before the rows it carries: a write landing mid-read makes
// the bundle look older than it is, which the replica catches on its next
// poll, where the reverse would make it skip a change forever.
func (s *sqlStore) ExportBundle(ctx context.Context) (Bundle, error) {
	b := Bundle{Format: BundleFormat}
	settings := s.Settings()

	var err error
	if b.ConfigVersion, err = settings.ConfigVersion(ctx); err != nil {
		return Bundle{}, err
	}
	all, err := settings.All(ctx)
	if err != nil {
		return Bundle{}, err
	}
	b.InstanceID = all[instanceIDSetting]
	// Unset or unparseable is 0, "no key designated" — the state every main
	// starts in, and not an error a bundle should fail on.
	b.SyncKey, _ = strconv.ParseInt(all[syncKeySetting], 10, 64)
	b.Settings = make(map[string]string, len(all))
	for k, v := range all {
		if !LocalSettingKey(k) {
			b.Settings[k] = v
		}
	}

	if b.Groups, err = s.Clients().Groups(ctx); err != nil {
		return Bundle{}, err
	}
	if b.Clients, err = s.Clients().Clients(ctx); err != nil {
		return Bundle{}, err
	}
	if b.TSIGKeys, err = s.TSIGKeys().List(ctx); err != nil {
		return Bundle{}, err
	}

	// Non-nil so an instance with no rules marshals `[]` rather than `null`,
	// like every other list this package hands out.
	b.Rules = []Rule{}
	for _, g := range b.Groups {
		rs, err := s.Filters().Rules(ctx, g.ID)
		if err != nil {
			return Bundle{}, err
		}
		b.Rules = append(b.Rules, rs...)
	}

	lists, err := s.Filters().Lists(ctx)
	if err != nil {
		return Bundle{}, err
	}
	assigned, err := s.groupsByList(ctx)
	if err != nil {
		return Bundle{}, err
	}
	b.Lists = make([]BundleList, len(lists))
	for i, l := range lists {
		b.Lists[i] = BundleList{List: l, Groups: assigned[l.ID]}
	}

	zs, err := s.Zones().Zones(ctx)
	if err != nil {
		return Bundle{}, err
	}
	b.Zones = make([]Zone, 0, len(zs))
	for _, z := range zs {
		if z.Type == zoneTypeInternal {
			continue // §4.2: both boxes seed their own built-ins.
		}
		b.Zones = append(b.Zones, z)
	}
	return b, nil
}

// groupsByList reads every list-to-group assignment in one query, keyed by
// list, which is the shape BundleList wants.
func (s *sqlStore) groupsByList(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT list_id, group_id FROM group_lists ORDER BY list_id, group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var listID, groupID int64
		if err := rows.Scan(&listID, &groupID); err != nil {
			return nil, err
		}
		out[listID] = append(out[listID], groupID)
	}
	return out, rows.Err()
}

// The upserts below write b's rows under b's ids (§4.1). Each names its
// columns explicitly in DO UPDATE rather than relying on a whole-row write,
// for the reason zones.go's const block gives: a column that belongs to this
// box and not to the main must not be reachable from here at all.
const (
	upsertGroupSQL = `INSERT INTO groups (id, name, enabled) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, enabled = excluded.enabled`
	upsertClientSQL = `INSERT INTO clients (id, name, matcher, group_id) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, matcher = excluded.matcher, group_id = excluded.group_id`
	// The refresh-state columns (last_refreshed, entry_count, last_status,
	// last_error, last_attempt) are deliberately absent: they describe a
	// download this box made, and importing the main's would have the
	// replica reporting entries it has never fetched. A new row defaults to
	// pending/0, which is the truth until its own refresher runs.
	upsertListSQL = `INSERT INTO lists (id, url, name, kind, enabled) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET url = excluded.url, name = excluded.name, kind = excluded.kind, enabled = excluded.enabled`
	upsertRuleSQL = `INSERT INTO rules (id, group_id, action, pattern, is_regex) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET group_id = excluded.group_id, action = excluded.action, pattern = excluded.pattern, is_regex = excluded.is_regex`
	upsertTSIGKeySQL = `INSERT INTO tsig_keys (id, name, algorithm, secret, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, algorithm = excluded.algorithm, secret = excluded.secret, created_at = excluded.created_at`
	// Definition columns only. soa_serial, refreshed_at, expires_at and the
	// last_* pairs are transfer state: what this box pulled and when, which
	// no config pull may revise — a serving secondary must not be made to
	// forget what it transferred (§5).
	//
	// So DO UPDATE names none of them. The INSERT names soa_serial (with
	// created_at, and the definition columns), because a row being created
	// here has no transfer state to keep and the serial is where its first
	// comparison starts; refreshed_at, expires_at and the last_* columns are
	// in neither, left to default for a new row exactly as AddZone leaves
	// them to the transfer path and NoteTransferAttempt.
	upsertZoneSQL = `INSERT INTO zones (id, name, type, enabled, soa_ns, soa_mbox, soa_serial, soa_refresh, soa_retry, soa_expire, soa_minimum, soa_ttl, primaries, tsig_key_id, allow_transfer, notify_to, forward_to, created_at, modified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, type = excluded.type, enabled = excluded.enabled, soa_ns = excluded.soa_ns, soa_mbox = excluded.soa_mbox, soa_refresh = excluded.soa_refresh, soa_retry = excluded.soa_retry, soa_expire = excluded.soa_expire, soa_minimum = excluded.soa_minimum, soa_ttl = excluded.soa_ttl, primaries = excluded.primaries, tsig_key_id = excluded.tsig_key_id, allow_transfer = excluded.allow_transfer, notify_to = excluded.notify_to, forward_to = excluded.forward_to, modified_at = excluded.modified_at`
	assignListSQL = `INSERT INTO group_lists (group_id, list_id) VALUES (?, ?)`
)

// syncedTables are the tables whose ids come from the main, in the order the
// sequences behind them have to be advanced on postgres.
var syncedTables = []string{"groups", "clients", "lists", "rules", "tsig_keys", "zones"}

// parkedKeys are the unique natural keys a bundle rewrites, and parking them
// is what stops two of them *swapping* from wedging a replica.
//
// A swap is not a delete-and-recreate, so pruning first does not help: a
// matcher moved from one client to another while both rows survive leaves the
// row-by-row upsert hitting the unique index halfway through. The transaction
// rolls back, and — this is the part that matters — every later poll of the
// same bundle fails in exactly the same place, so the replica never applies
// that config again.
//
// Each of these columns is therefore emptied of real values before any upsert
// runs: every surviving row is parked on parkedKey(nonce, id), one statement
// per table. Rows that survive the prune are exactly the bundle's, so each
// parked value is overwritten before the commit — except the built-in zones,
// which the prune exempts and this must too, or an import would rename
// localhost.
//
// The nonce is why it is generated per import rather than being a constant
// like "import:". Parked values cannot collide with each other, because ids
// do not, but they *can* collide with a value the bundle is about to write: a
// group named "import:2" beside a group of id 2 is a legal configuration, and
// upserting the first one would land on the second one's sentinel before the
// second one is reached. Three of these columns could not hold such a value,
// but groups.name is free text and the mechanism must not rest on that — so
// the sentinel is made impossible to type instead, by carrying 128 bits of
// randomness drawn this instant.
var parkedKeys = []struct{ table, column, where string }{
	{table: "groups", column: "name"},
	{table: "clients", column: "matcher"},
	{table: "lists", column: "url"},
	{table: "tsig_keys", column: "name"},
	{table: "zones", column: "name", where: ` WHERE type <> '` + zoneTypeInternal + `'`},
}

// ImportBundle replaces every synced table with b's rows, keeping b's ids, in
// one transaction, and bumps config_version once. Partial application is not
// a state a replica can be left in (§5): either the box matches the bundle or
// it matches what it had before.
//
// What it does not touch: zone records (they arrive by AXFR), a zone's
// transfer state, a list's refresh state, and every local setting of §4.3 —
// users, tokens, the query log and the stats are not synced tables at all.
func (s *sqlStore) ImportBundle(ctx context.Context, b Bundle) error {
	if b.Format != BundleFormat {
		return fmt.Errorf("bundle format %d: this instance reads %d", b.Format, BundleFormat)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	zoneIDs := bundleIDs(b.Zones, func(z Zone) int64 { return z.ID })

	// Prune first, children before parents: none of these foreign keys
	// carries ON DELETE CASCADE, so a group the main dropped can only go
	// once the clients and rules naming it have. Pruning before the upserts
	// is also what keeps a row the bundle no longer carries from colliding,
	// by name or URL or matcher, with one it does.
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_lists`); err != nil {
		return err
	}
	for _, prune := range []struct {
		table string
		keep  []int64
		where string
	}{
		{table: "clients", keep: bundleIDs(b.Clients, func(c Client) int64 { return c.ID })},
		{table: "rules", keep: bundleIDs(b.Rules, func(r Rule) int64 { return r.ID })},
		// A built-in zone is in no bundle, so "absent from the bundle"
		// never means it (§4.2).
		{table: "zones", keep: zoneIDs, where: `type <> '` + zoneTypeInternal + `'`},
		{table: "lists", keep: bundleIDs(b.Lists, func(l BundleList) int64 { return l.ID })},
		{table: "groups", keep: bundleIDs(b.Groups, func(g Group) int64 { return g.ID })},
		{table: "tsig_keys", keep: bundleIDs(b.TSIGKeys, func(k TSIGKey) int64 { return k.ID })},
	} {
		if err := s.pruneMissing(ctx, tx, prune.table, prune.keep, prune.where); err != nil {
			return fmt.Errorf("pruning %s: %w", prune.table, err)
		}
	}

	// The prune exempts built-in zones, so the upsert has to as well: an id
	// in the bundle that lands on one would quietly rewrite an RFC 6303 zone
	// into a synced one. Skipping the row instead would leave the two boxes
	// disagreeing with nothing to say so, so it is refused.
	if err := s.refuseBuiltinCollision(ctx, tx, zoneIDs); err != nil {
		return err
	}

	// Every surviving row's unique natural key goes out of the way before
	// anything is written back into it — see parkedKeys.
	nonce, err := importNonce()
	if err != nil {
		return err
	}
	for _, park := range parkedKeys {
		if _, err := tx.ExecContext(ctx, `UPDATE `+park.table+` SET `+park.column+` = '`+nonce+`:' || id`+park.where); err != nil {
			return fmt.Errorf("parking %s.%s: %w", park.table, park.column, err)
		}
	}

	// Upserts run parents before children, so a client, rule or assignment
	// lands only once the row it names exists.
	for _, g := range b.Groups {
		if err := s.execTx(ctx, tx, upsertGroupSQL, g.ID, g.Name, g.Enabled); err != nil {
			return err
		}
	}
	for _, l := range b.Lists {
		// l.Name is never blank on the way out of Lists() — a blank one is
		// resolved through DeriveListName — so this stores the label the
		// main renders rather than the blank it may hold. Same name, and
		// the replica does not render lists of its own anyway.
		if err := s.execTx(ctx, tx, upsertListSQL, l.ID, l.URL, l.Name, l.Kind, l.Enabled); err != nil {
			return err
		}
	}
	for _, k := range b.TSIGKeys {
		if err := s.execTx(ctx, tx, upsertTSIGKeySQL, k.ID, k.Name, k.Algorithm, k.Secret, k.CreatedAt); err != nil {
			return err
		}
	}
	for _, c := range b.Clients {
		if err := s.execTx(ctx, tx, upsertClientSQL, c.ID, c.Name, c.Matcher, c.GroupID); err != nil {
			return err
		}
	}
	for _, r := range b.Rules {
		if err := s.execTx(ctx, tx, upsertRuleSQL, r.ID, r.GroupID, r.Action, r.Pattern, r.IsRegex); err != nil {
			return err
		}
	}
	for _, z := range b.Zones {
		if err := s.execTx(ctx, tx, upsertZoneSQL, z.ID, z.Name, z.Type, z.Enabled, z.SOANS, z.SOAMbox, z.SOASerial,
			z.SOARefresh, z.SOARetry, z.SOAExpire, z.SOAMinimum, z.SOATTL, z.Primaries, z.TSIGKeyID,
			z.AllowTransfer, z.NotifyTo, z.ForwardTo, z.CreatedAt, z.ModifiedAt); err != nil {
			return err
		}
	}
	for _, l := range b.Lists {
		for _, groupID := range l.Groups {
			if err := s.execTx(ctx, tx, assignListSQL, groupID, l.ID); err != nil {
				return err
			}
		}
	}

	if err := s.importSettings(ctx, tx, b.Settings); err != nil {
		return err
	}

	// postgres hands out ids from a sequence that an explicit insert does
	// not advance, so without this the first row created after a promotion
	// collides with one the main already used (§4.1). sqlite's AUTOINCREMENT
	// tracks an explicit id itself, in sqlite_sequence, and needs nothing.
	if s.dialect == "postgres" {
		for _, table := range syncedTables {
			if _, err := tx.ExecContext(ctx, resetSequenceSQL(table)); err != nil {
				return fmt.Errorf("advancing the %s sequence: %w", table, err)
			}
		}
	}

	// One bump for the whole apply, by the statement SetMany uses, and the
	// hub is told only after the commit — a subscriber that reconfigured
	// itself from a transaction that then rolled back would be serving a
	// config no box holds.
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
	s.hub.publish(v)
	return nil
}

// importSettings writes every synced key the bundle carries and removes every
// synced key it does not. Local keys (§4.3) are skipped in both directions:
// they are this box's own, so a main must neither set them nor clear them.
//
// The writes are sorted for the reason SetMany sorts its own: two multi-key
// writes touching the same rows in opposite orders can deadlock on postgres.
func (s *sqlStore) importSettings(ctx context.Context, tx *sql.Tx, values map[string]string) error {
	stale, err := staleSettings(ctx, tx, values)
	if err != nil {
		return err
	}
	for _, key := range stale {
		if err := s.execTx(ctx, tx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if LocalSettingKey(key) {
			continue
		}
		if err := s.execTx(ctx, tx, settingUpsert, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}

// staleSettings is the synced keys this box holds that keep does not, sorted,
// read in full before anything else runs on tx — a transaction has one
// connection, so the rows have to be closed before the deletes begin.
func staleSettings(ctx context.Context, tx *sql.Tx, keep map[string]string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if _, ok := keep[key]; !ok && !LocalSettingKey(key) {
			out = append(out, key)
		}
	}
	return out, rows.Err()
}

// refuseBuiltinCollision fails the import when a bundle zone id names one of
// this box's built-in zones. See the call site.
func (s *sqlStore) refuseBuiltinCollision(ctx context.Context, tx *sql.Tx, zoneIDs []int64) error {
	if len(zoneIDs) == 0 {
		return nil
	}
	args := append([]any{zoneTypeInternal}, anyIDs(zoneIDs)...)
	var name string
	err := tx.QueryRowContext(ctx, s.q(`SELECT name FROM zones WHERE type = ? AND id IN (`+placeholders(len(zoneIDs))+`)`), args...).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("bundle zone id collides with the built-in zone %q", name)
}

// pruneMissing deletes every row of table whose id the bundle does not carry.
// where is an extra predicate ANDed in, for the one table that has an
// exemption. An empty keep is not a no-op: a bundle with no rows of a kind
// means the main has none.
func (s *sqlStore) pruneMissing(ctx context.Context, tx *sql.Tx, table string, keep []int64, where string) error {
	var conds []string
	if where != "" {
		conds = append(conds, where)
	}
	if len(keep) > 0 {
		conds = append(conds, `id NOT IN (`+placeholders(len(keep))+`)`)
	}
	q := `DELETE FROM ` + table
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	return s.execTx(ctx, tx, q, anyIDs(keep)...)
}

// execTx is ExecContext on a transaction with the dialect's placeholders and
// wrapDBErr, so a bundle naming a parent row that does not exist comes back
// as ErrReference rather than as a raw driver error.
func (s *sqlStore) execTx(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	_, err := tx.ExecContext(ctx, s.q(q), args...)
	return wrapDBErr(err)
}

// importNonce is one import's share of randomness, 128 bits as hex. See
// parkedKeys for what it is for.
//
// Hex is the point of the encoding, not decoration: the nonce goes into the
// parking statement as a literal (it is concatenated with the row's id in
// SQL, and a bound parameter beside `|| id` has no unambiguous type on
// postgres), so it has to be drawn from an alphabet that cannot close a
// string. [0-9a-f] is.
func importNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("import nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// resetSequenceSQL sets table's id sequence to the largest id in it. The
// third setval argument is is_called: false for an empty table, so the next
// id is 1 — and because a sequence may not be set to 0 in the first place.
func resetSequenceSQL(table string) string {
	return `SELECT setval(pg_get_serial_sequence('` + table + `', 'id'),
		GREATEST(COALESCE((SELECT MAX(id) FROM ` + table + `), 0), 1),
		COALESCE((SELECT MAX(id) FROM ` + table + `), 0) > 0)`
}

func bundleIDs[T any](rows []T, id func(T) int64) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = id(r)
	}
	return out
}

func anyIDs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// placeholders is the "?, ?, ?" of an IN list of n values, before s.q
// rewrites them for the dialect.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
