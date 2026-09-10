package store

import (
	"context"
	"strings"
	"time"
)

type queryLogStore struct{ s *sqlStore }

// pruneChunk is how many rows one retention DELETE removes, and pruneYield
// how long the loop waits before the next one.
//
// SQLite runs on a single connection (see Open), so a DELETE is not merely
// slow, it is exclusive: dropping a 90-day query log to 7 days is millions
// of rows in one statement, tens of seconds during which every query-log
// flush waits on the same connection and is discarded when its 5s timeout
// expires. Bounded deletes with a pause between them give those writers the
// connection back; the prune is a daily background job, so trading a little
// wall-clock for that is free.
//
// pruneChunk is a var only so tests can exercise the loop with a handful of
// rows rather than a hundred thousand.
var pruneChunk int64 = 10000

const pruneYield = 10 * time.Millisecond

// deleteInChunks calls deleteOne — one bounded DELETE — until a pass comes
// back short, which means there was nothing left for it to find. It returns
// what every completed pass deleted, including when a later one fails: those
// rows really are gone, and reporting 0 would put a wrong number in the log.
func deleteInChunks(ctx context.Context, chunk int64, deleteOne func(context.Context, int64) (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := deleteOne(ctx, chunk)
		total += n
		if err != nil {
			return total, err
		}
		if n < chunk {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(pruneYield):
		}
	}
}

func (q *queryLogStore) InsertBatch(ctx context.Context, batch []QueryLogEntry) error {
	if len(batch) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO query_log (at, instance_id, client_ip, client_id, qname, qtype, decision, rule_id, list_id, upstream, rcode, duration_ms, matched) VALUES `)
	args := make([]any, 0, len(batch)*13)
	for i, e := range batch {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, e.At, e.InstanceID, e.ClientIP, e.ClientID, e.QName, e.QType, e.Decision, e.RuleID, e.ListID, e.Upstream, e.RCode, e.DurationMs, e.Matched)
	}
	_, err := q.s.db.ExecContext(ctx, q.s.q(sb.String()), args...)
	return err
}

func (q *queryLogStore) DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	return deleteInChunks(ctx, pruneChunk, func(ctx context.Context, limit int64) (int64, error) {
		res, err := q.s.db.ExecContext(ctx,
			q.s.q(`DELETE FROM query_log WHERE id IN (SELECT id FROM query_log WHERE at < ? LIMIT ?)`),
			cutoffMs, limit)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}
