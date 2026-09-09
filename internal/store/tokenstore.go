package store

import (
	"context"
	"database/sql"
	"errors"
)

type tokenStore struct{ s *sqlStore }

func (t *tokenStore) Create(ctx context.Context, tok AuthToken) (int64, error) {
	return t.s.insert(ctx, `INSERT INTO auth_tokens (user_id, kind, name, token_hash, scope, created_at, expires_at, last_used) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tok.UserID, tok.Kind, tok.Name, tok.TokenHash, tok.Scope, tok.CreatedAt, tok.ExpiresAt, tok.LastUsed)
}

func (t *tokenStore) ByHash(ctx context.Context, hash string) (AuthToken, bool, error) {
	var tok AuthToken
	err := t.s.db.QueryRowContext(ctx, t.s.q(`SELECT id, user_id, kind, name, token_hash, scope, created_at, expires_at, last_used FROM auth_tokens WHERE token_hash = ?`), hash).
		Scan(&tok.ID, &tok.UserID, &tok.Kind, &tok.Name, &tok.TokenHash, &tok.Scope, &tok.CreatedAt, &tok.ExpiresAt, &tok.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthToken{}, false, nil
	}
	return tok, err == nil, err
}

func (t *tokenStore) Delete(ctx context.Context, id int64) error {
	_, err := t.s.db.ExecContext(ctx, t.s.q(`DELETE FROM auth_tokens WHERE id = ?`), id)
	return err
}

func (t *tokenStore) ListAPI(ctx context.Context, userID int64) ([]AuthToken, error) {
	rows, err := t.s.db.QueryContext(ctx, t.s.q(`SELECT id, user_id, kind, name, token_hash, scope, created_at, expires_at, last_used FROM auth_tokens WHERE user_id = ? AND kind = 'api' ORDER BY id`), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Non-nil (not `var out []AuthToken`) so zero tokens marshals to JSON
	// `[]`, not `null` — matches every other list endpoint (see
	// sql.go's Groups/Clients/Lists/Rules/records.All for the same fix).
	// Every account starts with zero API tokens, so this is the default
	// state, not an edge case.
	out := []AuthToken{}
	for rows.Next() {
		var tok AuthToken
		if err := rows.Scan(&tok.ID, &tok.UserID, &tok.Kind, &tok.Name, &tok.TokenHash, &tok.Scope, &tok.CreatedAt, &tok.ExpiresAt, &tok.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

func (t *tokenStore) Touch(ctx context.Context, id, ts int64) error {
	_, err := t.s.db.ExecContext(ctx, t.s.q(`UPDATE auth_tokens SET last_used = ? WHERE id = ?`), ts, id)
	return err
}

func (t *tokenStore) DeleteExpired(ctx context.Context, nowMs int64) error {
	// Use strict < (not <=) intentionally: a token expiring exactly at nowMs survives until the next pass.
	_, err := t.s.db.ExecContext(ctx, t.s.q(`DELETE FROM auth_tokens WHERE expires_at > 0 AND expires_at < ?`), nowMs)
	return err
}

func (t *tokenStore) SetExpiry(ctx context.Context, id, ts int64) error {
	return t.s.execOne(ctx, `UPDATE auth_tokens SET expires_at = ? WHERE id = ?`, ts, id)
}

func (t *tokenStore) DeleteSessions(ctx context.Context, userID, exceptID int64) error {
	// Not execOne: deleting nothing is the ordinary case (an account with
	// one browser open has no other session to revoke), not a missing row.
	_, err := t.s.db.ExecContext(ctx,
		t.s.q(`DELETE FROM auth_tokens WHERE user_id = ? AND kind = 'session' AND id <> ?`), userID, exceptID)
	return err
}
