package auth

import (
	"context"
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
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, "000000"); err == nil {
		t.Fatal("bad code accepted")
	}
	code, _ := totp.GenerateCode(secret, time.Now())
	if err := svc.EnableTOTPConfirm(ctx, u.ID, secret, code); err != nil {
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
	if err := svc.DisableTOTP(ctx, u.ID, code); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "admin", "password123", ""); err != nil {
		t.Fatalf("login after disable: %v", err)
	}
}
