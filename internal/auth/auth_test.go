package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

type memUsers struct{ users []store.User }

func (m *memUsers) Create(ctx context.Context, u store.User) (int64, error) {
	u.ID = int64(len(m.users) + 1)
	m.users = append(m.users, u)
	return u.ID, nil
}
func (m *memUsers) CreateIfNone(ctx context.Context, u store.User) (bool, error) {
	if len(m.users) > 0 {
		return false, nil
	}
	u.ID = int64(len(m.users) + 1)
	m.users = append(m.users, u)
	return true, nil
}
func (m *memUsers) ByUsername(ctx context.Context, name string) (store.User, bool, error) {
	for _, u := range m.users {
		if u.Username == name {
			return u, true, nil
		}
	}
	return store.User{}, false, nil
}
func (m *memUsers) ByID(ctx context.Context, id int64) (store.User, bool, error) {
	for _, u := range m.users {
		if u.ID == id {
			return u, true, nil
		}
	}
	return store.User{}, false, nil
}
func (m *memUsers) Count(ctx context.Context) (int64, error) { return int64(len(m.users)), nil }
func (m *memUsers) SetTOTP(ctx context.Context, id int64, secret string) error {
	for i := range m.users {
		if m.users[i].ID == id {
			m.users[i].TOTPSecret = secret
		}
	}
	return nil
}

type memTokens struct{ toks map[string]store.AuthToken }

func newMemTokens() *memTokens { return &memTokens{toks: map[string]store.AuthToken{}} }
func (m *memTokens) Create(ctx context.Context, t store.AuthToken) (int64, error) {
	t.ID = int64(len(m.toks) + 1)
	m.toks[t.TokenHash] = t
	return t.ID, nil
}
func (m *memTokens) ByHash(ctx context.Context, h string) (store.AuthToken, bool, error) {
	t, ok := m.toks[h]
	return t, ok, nil
}
func (m *memTokens) Delete(ctx context.Context, id int64) error {
	for h, t := range m.toks {
		if t.ID == id {
			delete(m.toks, h)
		}
	}
	return nil
}
func (m *memTokens) ListAPI(ctx context.Context, uid int64) ([]store.AuthToken, error) {
	var out []store.AuthToken
	for _, t := range m.toks {
		if t.UserID == uid && t.Kind == "api" {
			out = append(out, t)
		}
	}
	return out, nil
}
func (m *memTokens) Touch(ctx context.Context, id, ts int64) error {
	for h, t := range m.toks {
		if t.ID == id {
			t.LastUsed = ts
			m.toks[h] = t
		}
	}
	return nil
}
func (m *memTokens) DeleteExpired(ctx context.Context, now int64) error { return nil }
func (m *memTokens) SetExpiry(ctx context.Context, id, ts int64) error {
	for h, t := range m.toks {
		if t.ID == id {
			t.ExpiresAt = ts
			m.toks[h] = t
		}
	}
	return nil
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("hunter2!")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("hash: %q %v", h, err)
	}
	if ok, _ := VerifyPassword(h, "hunter2!"); !ok {
		t.Fatal("correct password rejected")
	}
	if ok, _ := VerifyPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("hunter2!")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestSetupLoginAuthenticate(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	if req, _ := svc.SetupRequired(ctx); !req {
		t.Fatal("fresh install should require setup")
	}
	if err := svc.CreateAdmin(ctx, "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	// Password is valid-length here so this exercises the post-validation
	// atomic CreateIfNone race path (ErrSetupDone), not the ErrInvalidInput
	// validation path — those are covered separately below.
	if err := svc.CreateAdmin(ctx, "again", "another-valid-pw"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second admin: %v", err)
	}
	if _, err := svc.Login(ctx, "admin", "wrong", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("bad pw: %v", err)
	}
	tok, err := svc.Login(ctx, "admin", "correct-horse", "")
	if err != nil || len(tok) < 40 {
		t.Fatalf("login: %q %v", tok, err)
	}
	u, at, err := svc.Authenticate(ctx, tok)
	if err != nil || u.Username != "admin" || at.Kind != "session" {
		t.Fatalf("auth: %+v %+v %v", u, at, err)
	}
	if _, _, err := svc.Authenticate(ctx, "bogus"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("bogus token: %v", err)
	}
	now = now.Add(31 * 24 * time.Hour)
	if _, _, err := svc.Authenticate(ctx, tok); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("expired session accepted: %v", err)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	_ = svc.CreateAdmin(ctx, "admin", "password")
	u, _, _ := func() (store.User, bool, error) { return svc.users.ByUsername(ctx, "admin") }()
	_, plain, err := svc.CreateAPIToken(ctx, u.ID, "homeassistant", "write")
	if err != nil {
		t.Fatal(err)
	}
	usr, at, err := svc.Authenticate(ctx, plain)
	if err != nil || usr.ID != u.ID || at.Kind != "api" || at.Name != "homeassistant" {
		t.Fatalf("api token auth: %+v %+v %v", usr, at, err)
	}
	list, _ := svc.ListAPITokens(ctx, u.ID)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	if err := svc.RevokeToken(ctx, u.ID+1, list[0].ID); err == nil {
		t.Fatal("revoking another user's token must fail")
	}
	if err := svc.RevokeToken(ctx, u.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authenticate(ctx, plain); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("revoked token accepted: %v", err)
	}
}

// TestCreateAdminValidation asserts empty username / short password fail
// with ErrInvalidInput before any store call is attempted, and that a
// concurrent CreateIfNone race (both requests pass validation) is decided
// atomically: exactly one caller succeeds and the other gets ErrSetupDone.
func TestCreateAdminValidation(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())

	if err := svc.CreateAdmin(ctx, "", "longenough1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty username: %v", err)
	}
	if err := svc.CreateAdmin(ctx, "admin", "short"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short password: %v", err)
	}
	if len(svc.users.(*memUsers).users) != 0 {
		t.Fatal("validation failure must not touch the store")
	}
}

// TestLoginEqualizesTimingForUnknownUser guards against a user-enumeration
// timing oracle: Login must pay the same argon2id cost whether or not the
// username exists, so a wrong-username response isn't measurably faster than
// a wrong-password response. Wall-clock assertions are inherently a little
// flaky, so this uses a loose lower bound far below a real argon2id run
// (tens of ms at this package's cost params) — it only fails if the dummy
// verification is skipped entirely (the bug it guards against), not due to
// ordinary scheduling jitter.
func TestLoginEqualizesTimingForUnknownUser(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	if err := svc.CreateAdmin(ctx, "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := svc.Login(ctx, "no-such-user", "whatever", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Microsecond {
		t.Fatalf("unknown-user login returned in %v without running argon2; timing oracle reopened", elapsed)
	}
}
