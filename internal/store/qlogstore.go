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
