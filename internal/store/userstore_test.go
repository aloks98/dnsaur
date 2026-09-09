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

// TestClaimTOTPStepRefusesReuse covers the column 0014 adds: a TOTP time
// step can be claimed once and never again, and re-enrolling (SetTOTP)
// clears the mark so the new secret's first code isn't refused as a replay
// of the old secret's last one.
//
// The concurrent half is the reason this lives at the store layer rather
// than only in internal/auth: two logins presenting the same captured code
// at the same instant must not both find a lower stored step and both
// proceed, and only the real UPDATE ... WHERE decides that. Under sqlite
// SetMaxOpenConns(1) would make almost any implementation pass, so it runs
// against postgres too via forEachDriver.
func TestClaimTOTPStepRefusesReuse(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		id, err := s.Users().Create(ctx, User{Username: "totp-step", PasswordHash: "h", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Users().ClaimTOTPStep(ctx, id, 100); err != nil || !ok {
			t.Fatalf("first claim: ok=%v err=%v", ok, err)
		}
		if ok, err := s.Users().ClaimTOTPStep(ctx, id, 100); err != nil || ok {
			t.Fatalf("replay of step 100 granted: ok=%v err=%v", ok, err)
		}
		if ok, err := s.Users().ClaimTOTPStep(ctx, id, 99); err != nil || ok {
			t.Fatalf("older step 99 granted: ok=%v err=%v", ok, err)
		}
		if ok, err := s.Users().ClaimTOTPStep(ctx, id, 101); err != nil || !ok {
			t.Fatalf("next step 101 refused: ok=%v err=%v", ok, err)
		}
		u, _, err := s.Users().ByID(ctx, id)
		if err != nil || u.TOTPLastStep != 101 {
			t.Fatalf("stored step = %d, err = %v", u.TOTPLastStep, err)
		}

		// Re-enrolment starts a fresh sequence.
		if err := s.Users().SetTOTP(ctx, id, "NEWSECRET"); err != nil {
			t.Fatal(err)
		}
		if u, _, _ := s.Users().ByID(ctx, id); u.TOTPLastStep != 0 {
			t.Fatalf("SetTOTP left step %d behind", u.TOTPLastStep)
		}

		// One winner only, however many callers race for the same step.
		const n = 20
		granted := make([]bool, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				granted[i], errs[i] = s.Users().ClaimTOTPStep(ctx, id, 500)
			}(i)
		}
		wg.Wait()
		wins := 0
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("ClaimTOTPStep[%d]: %v", i, errs[i])
			}
			if granted[i] {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("%d concurrent claims of step 500 were granted, want exactly 1", wins)
		}
	})
}

// TestDeleteSessionsKeepsCallerAndAPITokens pins what a second-factor change
// revokes: every other browser session, never the caller's own and never an
// API token, which is a credential the operator minted deliberately.
func TestDeleteSessionsKeepsCallerAndAPITokens(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		uid, err := s.Users().Create(ctx, User{Username: "sessions", PasswordHash: "h", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		mine, err := s.Tokens().Create(ctx, AuthToken{UserID: uid, Kind: "session", TokenHash: "sess-mine", Scope: "write", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens().Create(ctx, AuthToken{UserID: uid, Kind: "session", TokenHash: "sess-other", Scope: "write", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens().Create(ctx, AuthToken{UserID: uid, Kind: "api", Name: "ha", TokenHash: "api-keep", Scope: "read", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		other, err := s.Users().Create(ctx, User{Username: "sessions-other", PasswordHash: "h", CreatedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens().Create(ctx, AuthToken{UserID: other, Kind: "session", TokenHash: "sess-stranger", Scope: "write", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}

		if err := s.Tokens().DeleteSessions(ctx, uid, mine); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			hash string
			want bool
		}{
			{"sess-mine", true},
			{"sess-other", false},
			{"api-keep", true},
			{"sess-stranger", true},
		} {
			_, ok, err := s.Tokens().ByHash(ctx, tc.hash)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want {
				t.Fatalf("%s present = %v, want %v", tc.hash, ok, tc.want)
			}
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
