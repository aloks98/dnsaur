package store

import (
	"context"
	"strings"
)

func (q *queryLogStore) Search(ctx context.Context, f QueryLogFilter) ([]QueryLogEntry, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, at, instance_id, client_ip, client_id, qname, qtype, decision, rule_id, list_id, upstream, rcode, duration_ms FROM query_log WHERE 1=1`)
	var args []any
	if f.FromMs > 0 {
		sb.WriteString(` AND at >= ?`)
		args = append(args, f.FromMs)
	}
	if f.ToMs > 0 {
		sb.WriteString(` AND at <= ?`)
		args = append(args, f.ToMs)
	}
	if f.ClientIP != "" {
		sb.WriteString(` AND client_ip = ?`)
		args = append(args, f.ClientIP)
	}
	if f.Decision != "" {
		sb.WriteString(` AND decision = ?`)
		args = append(args, f.Decision)
	}
	if f.QType != "" {
		sb.WriteString(` AND qtype = ?`)
		args = append(args, f.QType)
	}
	if f.QNameContains != "" {
		sb.WriteString(` AND qname LIKE ?`)
		args = append(args, "%"+escapeLike(f.QNameContains)+"%")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	sb.WriteString(` ORDER BY id DESC LIMIT ? OFFSET ?`)
	args = append(args, limit, f.Offset)
	rows, err := q.s.db.QueryContext(ctx, q.s.q(sb.String()), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryLogEntry
	for rows.Next() {
		var e QueryLogEntry
		if err := rows.Scan(&e.ID, &e.At, &e.InstanceID, &e.ClientIP, &e.ClientID, &e.QName, &e.QType, &e.Decision, &e.RuleID, &e.ListID, &e.Upstream, &e.RCode, &e.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE wildcards; both dialects accept backslash escaping
// with an explicit ESCAPE clause omitted since \ is default in sqlite and
// postgres treats \ specially only in older modes — to stay portable we
// strip the wildcards instead of escaping them.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "%", "")
	return strings.ReplaceAll(s, "_", "")
}

func (st *statsStore) Timeline(ctx context.Context, fromSec int64) (map[int64]map[string]int64, error) {
	rows, err := st.s.db.QueryContext(ctx, st.s.q(`SELECT bucket, key, value FROM stats_hourly WHERE metric = 'decision' AND bucket >= ?`), fromSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int64{}
	for rows.Next() {
		var bucket, val int64
		var key string
		if err := rows.Scan(&bucket, &key, &val); err != nil {
			return nil, err
		}
		if out[bucket] == nil {
			out[bucket] = map[string]int64{}
		}
		out[bucket][key] += val
	}
	return out, rows.Err()
}

func (st *settingsStore) All(ctx context.Context) (map[string]string, error) {
	rows, err := st.s.db.QueryContext(ctx, st.s.q(`SELECT key, value FROM settings`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
