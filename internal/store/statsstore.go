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
