package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestUserLifecycle(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		n, err := s.Users().Count(ctx)
		if err != nil || n != 0 {
			t.Fatalf("initial count %d err %v", n, err)
		}
		id, err := s.Users().Create(ctx, User{Username: "admin", PasswordHash: "$argon2id$x", CreatedAt: 1000})
		if err != nil {
			t.Fatal(err)
		}
		u, ok, err := s.Users().ByUsername(ctx, "admin")
		if err != nil || !ok || u.ID != id || u.PasswordHash != "$argon2id$x" {
			t.Fatalf("byusername: %+v %v %v", u, ok, err)
		}
		u2, ok, err := s.Users().ByID(ctx, id)
		if err != nil || !ok || u2.Username != "admin" {
			t.Fatalf("byid: %+v %v %v", u2, ok, err)
		}
		if _, ok, err := s.Users().ByID(ctx, 999999); ok || err != nil {
			t.Fatalf("byid absent: %v %v", ok, err)
		}
		if err := s.Users().SetTOTP(ctx, id, "SECRET"); err != nil {
			t.Fatal(err)
		}
		u, _, _ = s.Users().ByUsername(ctx, "admin")
		if u.TOTPSecret != "SECRET" {
			t.Fatalf("totp not set: %+v", u)
		}
		if _, _, err := s.Users().ByUsername(ctx, "absent"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestCreateIfNoneAtomic guards against the setup TOCTOU: a plain
// Count()-then-Create() lets two concurrent first-run setup requests both
// observe an empty users table and both insert. CreateIfNone's single
// atomic INSERT ... SELECT ... WHERE NOT EXISTS must let exactly one
// concurrent caller win, no matter how many race for it.
func TestCreateIfNoneAtomic(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		ss, ok := s.(*sqlStore)
		if !ok {
			t.Fatal("not a *sqlStore")
		}
		// Unlike sqlite (a fresh tempdir DB per test), the postgres store
		// here always points at the same test-container database across
		// every test in this package, so the "table starts empty" this
		// race needs isn't guaranteed by test order alone. Force it and
		// restore it afterward so this test is self-contained regardless
		// of what ran before or after it.
		clearUsers := func() {
			_, _ = ss.db.ExecContext(ctx, `DELETE FROM auth_tokens`)
			_, _ = ss.db.ExecContext(ctx, `DELETE FROM users`)
		}
		clearUsers()
		t.Cleanup(clearUsers)

		const n = 20
		results := make([]bool, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = s.Users().CreateIfNone(ctx, User{
					Username: "admin", PasswordHash: "$argon2id$x", CreatedAt: int64(i),
				})
			}(i)
		}
		wg.Wait()

		var created int
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("CreateIfNone[%d]: %v", i, errs[i])
			}
			if results[i] {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("expected exactly 1 winner, got %d", created)
		}
		n2, err := s.Users().Count(ctx)
		if err != nil || n2 != 1 {
			t.Fatalf("final user count = %d, err = %v", n2, err)
		}

		// A subsequent call against the now-populated table must also
		// report created=false without error.
		created2, err := s.Users().CreateIfNone(ctx, User{Username: "another", PasswordHash: "h", CreatedAt: 999})
		if err != nil || created2 {
			t.Fatalf("post-race CreateIfNone: created=%v err=%v", created2, err)
		}
	})
}

func TestTokenLifecycle(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		uid, _ := s.Users().Create(ctx, User{Username: "u2", PasswordHash: "h", CreatedAt: 1})
		// uid is a freshly minted, never-before-used user id, so this is a
		// safe zero-row check even against the shared postgres DB other
		// tests in this package also write to: nothing could have created
		// an API token for this exact uid yet. Regression coverage for
		// Task 14's finding — ListAPI used to declare `var out []AuthToken`,
		// which marshals to JSON `null` (not `[]`) on zero rows, crashing
		// the Account › API Tokens page for every account that hasn't
		// created a token yet (the default state).
		noTokens, err := s.Tokens().ListAPI(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		mustMarshalArray(t, noTokens)

		id, err := s.Tokens().Create(ctx, AuthToken{UserID: uid, Kind: "session", TokenHash: "hash1", Scope: "write", CreatedAt: 1000, ExpiresAt: 5000})
		if err != nil {
			t.Fatal(err)
		}
		tok, ok, err := s.Tokens().ByHash(ctx, "hash1")
		if err != nil || !ok || tok.ID != id || tok.Kind != "session" || tok.Scope != "write" {
			t.Fatalf("byhash: %+v %v %v", tok, ok, err)
		}
		if err := s.Tokens().Touch(ctx, id, 2000); err != nil {
			t.Fatal(err)
		}
		tok, _, _ = s.Tokens().ByHash(ctx, "hash1")
		if tok.LastUsed != 2000 {
			t.Fatalf("touch: %+v", tok)
		}
		if err := s.Tokens().SetExpiry(ctx, id, 9000); err != nil {
			t.Fatal(err)
		}
		tok, _, _ = s.Tokens().ByHash(ctx, "hash1")
		if tok.ExpiresAt != 9000 {
			t.Fatalf("setexpiry: %+v", tok)
		}
		if err := s.Tokens().SetExpiry(ctx, id, 5000); err != nil {
			t.Fatal(err)
		}
		if err := s.Tokens().SetExpiry(ctx, 999999, 1000); !errors.Is(err, ErrNotFound) {
			t.Fatalf("setexpiry missing row: %v", err)
		}
		if _, err := s.Tokens().Create(ctx, AuthToken{UserID: uid, Kind: "api", Name: "ha", TokenHash: "hash2", Scope: "read", CreatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
		api, err := s.Tokens().ListAPI(ctx, uid)
		if err != nil || len(api) != 1 || api[0].Name != "ha" || api[0].Scope != "read" {
			t.Fatalf("listapi: %v %v", api, err)
		}
		if err := s.Tokens().DeleteExpired(ctx, 6000); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Tokens().ByHash(ctx, "hash1"); ok {
			t.Fatal("expired session not deleted")
		}
		if _, ok, _ := s.Tokens().ByHash(ctx, "hash2"); !ok {
			t.Fatal("never-expiring api token wrongly deleted")
		}
		if err := s.Tokens().Delete(ctx, api[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Tokens().ByHash(ctx, "hash2"); ok {
			t.Fatal("deleted token still present")
		}
	})
}
