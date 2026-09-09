package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpPeriod and totpSkew are the defaults totp.Validate applies: 30-second
// time steps, and a code accepted one step either side of now so a clock a
// little out of true, or a code typed slowly, still works.
const (
	totpPeriod = 30
	totpSkew   = 1
)

// verifyTOTPStep is totp.Validate with the matched time step reported back.
//
// The library's Validate answers yes or no across the whole skew window,
// which is not enough to refuse a replay (RFC 6238 §5.2): a code minted for
// step N stays valid through step N+1, so "has anyone logged in during this
// step" would still let the same code through a second time. Trying each
// step in the window on its own, with the library's skew turned off, names
// the step the code actually belongs to — which is the thing that can be
// recorded and refused.
//
// Highest step first, so a code that somehow matched two steps claims the
// later one and cannot be replayed against the earlier.
func verifyTOTPStep(secret, code string, now time.Time) (int64, bool) {
	for delta := totpSkew; delta >= -totpSkew; delta-- {
		at := now.Add(time.Duration(delta*totpPeriod) * time.Second)
		ok, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
			Period: totpPeriod, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err == nil && ok {
			return at.Unix() / totpPeriod, true
		}
	}
	return 0, false
}

func (s *Service) EnableTOTPStart(ctx context.Context, userID int64, username string) (string, string, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "dnsaur", AccountName: username})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// EnableTOTPConfirm turns the second factor on and logs every other session
// out. keepSessionID is the token the caller is holding, which survives:
// enabling TOTP is what someone does when they think their account may be
// compromised, and it means nothing while the sessions minted before it
// stay valid — but logging the user out of the tab they are working in
// would be a bug rather than a safeguard.
//
// Deliberately no replay check on this code, unlike Login. The step counter
// exists to protect authentication, and this endpoint already sits behind
// an authenticated session: a replayed code here conveys no authority the
// caller does not already hold.
func (s *Service) EnableTOTPConfirm(ctx context.Context, userID int64, secret, code string, keepSessionID int64) error {
	if _, ok := verifyTOTPStep(secret, code, s.Now()); !ok {
		return fmt.Errorf("invalid totp code")
	}
	if err := s.users.SetTOTP(ctx, userID, secret); err != nil {
		return err
	}
	return s.tokens.DeleteSessions(ctx, userID, keepSessionID)
}

// DisableTOTP turns the second factor off, and revokes the other sessions
// for the same reason EnableTOTPConfirm does — more so, since this is the
// direction an attacker would want.
func (s *Service) DisableTOTP(ctx context.Context, userID int64, code string, keepSessionID int64) error {
	u, ok, err := s.users.ByID(ctx, userID)
	if err != nil || !ok {
		return ErrBadCredentials
	}
	if u.TOTPSecret == "" {
		return fmt.Errorf("invalid totp code")
	}
	if _, ok := verifyTOTPStep(u.TOTPSecret, code, s.Now()); !ok {
		return fmt.Errorf("invalid totp code")
	}
	if err := s.users.SetTOTP(ctx, userID, ""); err != nil {
		return err
	}
	return s.tokens.DeleteSessions(ctx, userID, keepSessionID)
}
