package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestTOTPEnableConfirmLogin(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	_ = svc.CreateAdmin(ctx, "admin", "password123")
	u, _, _ := svc.users.ByUsername(ctx, "admin")

	secret, url, err := svc.EnableTOTPStart(ctx, u.ID, u.Username)
	if err != nil || secret == "" || url == "" {
		t.Fatalf("start: %q %q %v", secret, url, err)
	}
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, "000000", 0); err == nil {
		t.Fatal("bad code accepted")
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, code, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "admin", "password123", ""); err != ErrTOTPRequired {
		t.Fatalf("login without code: %v", err)
	}
	code, _ = totp.GenerateCode(secret, time.Now())
	if _, err := svc.Login(ctx, "admin", "password123", code); err != nil {
		t.Fatalf("login with code: %v", err)
	}
	code, _ = totp.GenerateCode(secret, time.Now())
	if err := svc.DisableTOTP(ctx, u.ID, code, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "admin", "password123", ""); err != nil {
		t.Fatalf("login after disable: %v", err)
	}
}

// TestTOTPCodeCannotBeReplayed is RFC 6238 §5.2: a code that has logged in
// once must never log in again. The library's Validate is stateless and its
// window spans the neighbouring steps, so a code read over someone's
// shoulder (or lifted from a proxy log, or replayed from a captured request
// body) stayed usable for up to 90 seconds.
//
// The clock is fixed, so the second attempt is the *same* code within the
// same step — the replay itself, not a different code that happens to look
// like one.
func TestTOTPCodeCannotBeReplayed(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	if err := svc.CreateAdmin(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	u, _, _ := svc.users.ByUsername(ctx, "admin")

	secret, _, err := svc.EnableTOTPStart(ctx, u.ID, u.Username)
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, code, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Login(ctx, "admin", "password123", code); err != nil {
		t.Fatalf("first login with the code: %v", err)
	}
	if _, err := svc.Login(ctx, "admin", "password123", code); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("the same code logged in twice: %v", err)
	}

	// A code from an earlier step is a replay too — the window reaches
	// backwards, so "not the current step" is not the same as "not used".
	older, err := totp.GenerateCode(secret, now.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if older != code {
		if _, err := svc.Login(ctx, "admin", "password123", older); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("code from the previous step accepted after a later one: %v", err)
		}
	}

	// The next step's code is not a replay and must still work.
	now = now.Add(30 * time.Second)
	next, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "admin", "password123", next); err != nil {
		t.Fatalf("fresh code from the next step refused: %v", err)
	}
}

// TestTOTPChangeRevokesOtherSessions covers the other half of a
// second-factor change: turning TOTP on (or off) is what someone does after
// deciding their account may be compromised, and it meant nothing at all
// while every session already minted stayed valid. The session that made
// the change survives — being logged out of the tab you just used is a
// bug, not security.
func TestTOTPChangeRevokesOtherSessions(t *testing.T) {
	ctx := context.Background()
	svc := New(&memUsers{}, newMemTokens())
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	if err := svc.CreateAdmin(ctx, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	u, _, _ := svc.users.ByUsername(ctx, "admin")

	mine, err := svc.Login(ctx, "admin", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Login(ctx, "admin", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	_, apiTok, err := svc.CreateAPIToken(ctx, u.ID, "homeassistant", "write", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, mineTok, err := svc.Authenticate(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}

	secret, _, err := svc.EnableTOTPStart(ctx, u.ID, u.Username)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.GenerateCode(secret, now)
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, code, mineTok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authenticate(ctx, other); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("session from before TOTP was enabled still works: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, mine); err != nil {
		t.Fatalf("the session that enabled TOTP was logged out: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, apiTok); err != nil {
		t.Fatalf("API token revoked by a TOTP change: %v", err)
	}

	// Disabling does the same: it is the more suspicious direction of the two.
	now = now.Add(60 * time.Second)
	third, err := svc.Login(ctx, "admin", "password123", mustCode(t, secret, now))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(60 * time.Second)
	if err := svc.DisableTOTP(ctx, u.ID, mustCode(t, secret, now), mineTok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authenticate(ctx, third); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("session survived TOTP being disabled: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, mine); err != nil {
		t.Fatalf("the session that disabled TOTP was logged out: %v", err)
	}
}

func mustCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return code
}
