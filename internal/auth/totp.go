package auth

import (
	"context"
	"fmt"

	"github.com/pquerna/otp/totp"
)

func (s *Service) EnableTOTPStart(ctx context.Context, userID int64, username string) (string, string, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "dnsaur", AccountName: username})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

func (s *Service) EnableTOTPConfirm(ctx context.Context, userID int64, secret, code string) error {
	if !totp.Validate(code, secret) {
		return fmt.Errorf("invalid totp code")
	}
	return s.users.SetTOTP(ctx, userID, secret)
}

func (s *Service) DisableTOTP(ctx context.Context, userID int64, code string) error {
	u, ok, err := s.users.ByID(ctx, userID)
	if err != nil || !ok {
		return ErrBadCredentials
	}
	if u.TOTPSecret == "" || !totp.Validate(code, u.TOTPSecret) {
		return fmt.Errorf("invalid totp code")
	}
	return s.users.SetTOTP(ctx, userID, "")
}
