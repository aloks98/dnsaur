package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	return st.SetMany(ctx, map[string]string{key: value})
}

const settingUpsert = `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`

// SetMany writes every pair in one transaction and bumps the config version
// once, so a caller changing several dependent settings reconfigures the
// running server once rather than once per key. Set is this with one pair.
//
// The keys are written in sorted order. Nothing observes the order — the
// transaction commits as a unit — but two concurrent multi-key writes that
// touched the same rows in opposite orders could deadlock on postgres,
// which takes a row lock per UPDATE.
func (st *settingsStore) SetMany(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, err := tx.ExecContext(ctx, st.s.q(settingUpsert), key, values[key]); err != nil {
			return err
		}
	}
	v, err := bumpVersionTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	st.s.hub.publish(v)
	return nil
}

// bumpVersionTx advances config_version inside tx and returns the new value.
// It is the one statement pair every version move shares — settings writes,
// a bundle import, and every synced-table write through configWrite — so
// that "what counts as a configuration change" is answered in one place
// rather than in a dozen copies of two SQL statements.
//
// The caller publishes on the hub only after its commit: a subscriber that
// reconfigured itself from a transaction that then rolled back would be
// serving a config no box holds.
func bumpVersionTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE config_version SET version = version + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var v int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM config_version WHERE id = 1`).Scan(&v)
	return v, err
}

func (st *settingsStore) SetInternal(ctx context.Context, key, value string) error {
	_, err := st.s.db.ExecContext(ctx, st.s.q(settingUpsert), key, value)
	return err
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
