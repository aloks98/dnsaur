package store

import (
	"context"
	"database/sql"
	"errors"
)

// TSIGKey authenticates a zone transfer (RFC 8945). Unlike AuthToken, whose
// hash only ever needs comparing, Secret must be recoverable — the server
// signs outgoing and verifies incoming messages with it — so it is stored in
// the clear (see the 0008 migration's comment). It is never marshalled to
// JSON by the store package itself; callers that expose it over the API are
// responsible for deciding what, if anything, leaves the server.
type TSIGKey struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`      // owner name of the TSIG RR, canonical form (lowercase, trailing dot)
	Algorithm string `json:"algorithm"` // e.g. "hmac-sha256." — miekg's own constants, trailing dot included
	Secret    string `json:"secret"`    // base64
	CreatedAt int64  `json:"created_at"`
}

// TSIGKeyStore manages TSIG keys used to authenticate zone transfers.
type TSIGKeyStore interface {
	List(ctx context.Context) ([]TSIGKey, error)
	Get(ctx context.Context, id int64) (TSIGKey, bool, error)
	// ByName looks a key up by its TSIG RR owner name — the lookup a signed
	// message actually arrives with.
	ByName(ctx context.Context, name string) (TSIGKey, bool, error)
	Create(ctx context.Context, k TSIGKey) (int64, error)
	Update(ctx context.Context, k TSIGKey) error
	Delete(ctx context.Context, id int64) error
}

type tsigKeyStore struct{ s *sqlStore }

const tsigKeyColumns = `id, name, algorithm, secret, created_at`

func scanTSIGKey(row interface{ Scan(...any) error }, k *TSIGKey) error {
	return row.Scan(&k.ID, &k.Name, &k.Algorithm, &k.Secret, &k.CreatedAt)
}

func (t *tsigKeyStore) List(ctx context.Context) ([]TSIGKey, error) {
	rows, err := t.s.db.QueryContext(ctx, `SELECT `+tsigKeyColumns+` FROM tsig_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil (not `var out []TSIGKey`) so zero keys marshals to JSON `[]`,
	// not `null` — matches every other list endpoint (see sql.go's
	// Groups/Clients/Lists/Rules/records.All, and ListAPI in tokenstore.go).
	// A brand-new instance starts with no TSIG keys, so this is the default
	// state, not an edge case.
	out := []TSIGKey{}
	for rows.Next() {
		var k TSIGKey
		if err := scanTSIGKey(rows, &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (t *tsigKeyStore) Get(ctx context.Context, id int64) (TSIGKey, bool, error) {
	var k TSIGKey
	row := t.s.db.QueryRowContext(ctx, t.s.q(`SELECT `+tsigKeyColumns+` FROM tsig_keys WHERE id = ?`), id)
	err := scanTSIGKey(row, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return TSIGKey{}, false, nil
	}
	return k, err == nil, err
}

func (t *tsigKeyStore) ByName(ctx context.Context, name string) (TSIGKey, bool, error) {
	var k TSIGKey
	row := t.s.db.QueryRowContext(ctx, t.s.q(`SELECT `+tsigKeyColumns+` FROM tsig_keys WHERE name = ?`), name)
	err := scanTSIGKey(row, &k)
	if errors.Is(err, sql.ErrNoRows) {
		return TSIGKey{}, false, nil
	}
	return k, err == nil, err
}

func (t *tsigKeyStore) Create(ctx context.Context, k TSIGKey) (int64, error) {
	return t.s.insert(ctx, `INSERT INTO tsig_keys (name, algorithm, secret, created_at) VALUES (?, ?, ?, ?)`,
		k.Name, k.Algorithm, k.Secret, k.CreatedAt)
}

func (t *tsigKeyStore) Update(ctx context.Context, k TSIGKey) error {
	return t.s.execOne(ctx, `UPDATE tsig_keys SET name = ?, algorithm = ?, secret = ? WHERE id = ?`,
		k.Name, k.Algorithm, k.Secret, k.ID)
}

func (t *tsigKeyStore) Delete(ctx context.Context, id int64) error {
	return t.s.execOne(ctx, `DELETE FROM tsig_keys WHERE id = ?`, id)
}
