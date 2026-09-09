package store

import (
	"context"
	"strconv"
)

type statsStore struct{ s *sqlStore }

// StatsWatermarkKey names the settings row recording how far into query_log
// the rollup has counted. It is written by Rollup itself, inside the same
// transaction as the counters it describes, and read by whoever drives the
// rollup to know where to resume — see Rollup.
const StatsWatermarkKey = "stats.watermark"

const upsertStat = ` ON CONFLICT (bucket, metric, key) DO UPDATE SET value = stats_hourly.value + excluded.value`

// Rollup folds every query_log row after afterID into the hourly counters
// and returns the id it counted up to.
//
// The counters are additive, so a row counted twice is counted wrong for as
// long as its bucket is kept. That makes "how far we got" part of the same
// write: it is stored under StatsWatermarkKey inside this transaction, so a
// batch is either counted and recorded or neither. Recording it afterwards,
// in a second statement, left a window where a committed batch had no
// record of itself and the next call added it again.
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
	upsertWatermark := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, st.s.q(upsertWatermark), StatsWatermarkKey, strconv.FormatInt(lastID, 10)); err != nil {
		return 0, err
	}
	return lastID, tx.Commit()
}

// PruneBefore deletes every hourly bucket that starts before
// bucketBeforeSec (a unix second, the unit stats_hourly.bucket is in).
//
// Nothing used to delete from this table: a row per hour per queried name
// and per client accumulates for as long as the instance runs, while the
// query log it is derived from is pruned on its own schedule. Chunked for
// the same reason the query-log prune is — see deleteInChunks.
func (st *statsStore) PruneBefore(ctx context.Context, bucketBeforeSec int64) (int64, error) {
	return deleteInChunks(ctx, pruneChunk, func(ctx context.Context, limit int64) (int64, error) {
		// By primary key, not by rowid or ctid: the table has no id column,
		// and (bucket, metric, key) is the one identifier both dialects
		// spell the same way.
		res, err := st.s.db.ExecContext(ctx, st.s.q(
			`DELETE FROM stats_hourly WHERE (bucket, metric, key) IN
			   (SELECT bucket, metric, key FROM stats_hourly WHERE bucket < ? LIMIT ?)`),
			bucketBeforeSec, limit)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
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
