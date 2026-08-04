package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
	"github.com/pquerna/otp/totp"
)

var (
	ErrBadCredentials = errors.New("bad credentials")
	ErrTOTPRequired   = errors.New("totp code required")
	ErrSetupDone      = errors.New("setup already completed")
	// ErrInvalidInput wraps CreateAdmin validation failures so callers can
	// distinguish "bad request" from "storage unavailable" via errors.Is,
	// without the handler layer inspecting store-shaped errors.
	ErrInvalidInput = errors.New("invalid input")
)

const SessionTTL = 30 * 24 * time.Hour

// dummyHash burns the same argon2 cost on unknown-user logins so response
// timing doesn't reveal whether a username exists.
var dummyHash = func() string {
	h, err := HashPassword("dnsaur-timing-equalization")
	if err != nil {
		panic(err)
	}
	return h
}()

type Service struct {
	users  store.UserStore
	tokens store.TokenStore
	Now    func() time.Time
	// verifyTOTP is swapped in by the TOTP feature (Task 12); default rejects
	// any login for users that have a TOTP secret but supplied no valid code.
	verifyTOTP func(secret, code string) bool
}

func New(users store.UserStore, tokens store.TokenStore) *Service {
	return &Service{users: users, tokens: tokens, Now: time.Now,
		verifyTOTP: func(secret, code string) bool { return totp.Validate(code, secret) }}
}

func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.users.Count(ctx)
	return n == 0, err
}

// CreateAdmin creates the single admin account during first-run setup.
// Validation runs first (cheap, no I/O), then an atomic conditional insert
// (UserStore.CreateIfNone) decides the race: Count-then-Create would let two
// concurrent setup requests both pass the emptiness check and both insert.
func (s *Service) CreateAdmin(ctx context.Context, username, password string) error {
	if len(username) == 0 {
		return fmt.Errorf("%w: username required", ErrInvalidInput)
	}
	if len(password) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrInvalidInput)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	created, err := s.users.CreateIfNone(ctx, store.User{Username: username, PasswordHash: hash, CreatedAt: s.Now().UnixMilli()})
	if err != nil {
		return err
	}
	if !created {
		return ErrSetupDone
	}
	return nil
}

func (s *Service) Login(ctx context.Context, username, password, totpCode string) (string, error) {
	u, ok, err := s.users.ByUsername(ctx, username)
	if err != nil {
		return "", err
	}
	if !ok {
		// Burn the same argon2 cost as a real verification so response
		// timing doesn't leak whether the username exists.
		_, _ = VerifyPassword(dummyHash, password)
		return "", ErrBadCredentials
	}
	match, err := VerifyPassword(u.PasswordHash, password)
	if err != nil || !match {
		return "", ErrBadCredentials
	}
	if u.TOTPSecret != "" {
		if totpCode == "" {
			return "", ErrTOTPRequired
		}
		if !s.verifyTOTP(u.TOTPSecret, totpCode) {
			return "", ErrBadCredentials
		}
	}
	plain, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	now := s.Now()
	_, err = s.tokens.Create(ctx, store.AuthToken{
		UserID: u.ID, Kind: "session", TokenHash: hash, Scope: "write",
		CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(SessionTTL).UnixMilli(),
	})
	return plain, err
}

func (s *Service) Authenticate(ctx context.Context, plain string) (store.User, store.AuthToken, error) {
	tok, ok, err := s.tokens.ByHash(ctx, HashToken(plain))
	if err != nil {
		return store.User{}, store.AuthToken{}, err
	}
	now := s.Now()
	if !ok {
		return store.User{}, store.AuthToken{}, ErrBadCredentials
	}
	if tok.ExpiresAt > 0 && tok.ExpiresAt < now.UnixMilli() {
		_ = s.tokens.Delete(ctx, tok.ID)
		return store.User{}, store.AuthToken{}, ErrBadCredentials
	}
	_ = s.tokens.Touch(ctx, tok.ID, now.UnixMilli())
	if tok.ExpiresAt > 0 && time.UnixMilli(tok.ExpiresAt).Sub(now) < SessionTTL/2 {
		_ = s.tokens.SetExpiry(ctx, tok.ID, now.Add(SessionTTL).UnixMilli()) // sliding renewal
	}
	u, ok, err := s.users.ByID(ctx, tok.UserID)
	if err != nil || !ok {
		return store.User{}, store.AuthToken{}, ErrBadCredentials
	}
	return u, tok, nil
}

func (s *Service) Logout(ctx context.Context, plain string) error {
	tok, ok, err := s.tokens.ByHash(ctx, HashToken(plain))
	if err != nil || !ok {
		return err
	}
	return s.tokens.Delete(ctx, tok.ID)
}

func (s *Service) CreateAPIToken(ctx context.Context, userID int64, name, scope string) (string, error) {
	if scope != "read" && scope != "write" {
		return "", fmt.Errorf("scope must be read or write")
	}
	plain, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.tokens.Create(ctx, store.AuthToken{
		UserID: userID, Kind: "api", Name: name, TokenHash: hash, Scope: scope, CreatedAt: s.Now().UnixMilli(),
	})
	return plain, err
}

func (s *Service) RevokeToken(ctx context.Context, userID, tokenID int64) error {
	list, err := s.tokens.ListAPI(ctx, userID)
	if err != nil {
		return err
	}
	for _, t := range list {
		if t.ID == tokenID {
			return s.tokens.Delete(ctx, tokenID)
		}
	}
	return fmt.Errorf("token %d not owned by user %d", tokenID, userID)
}

func (s *Service) ListAPITokens(ctx context.Context, userID int64) ([]store.AuthToken, error) {
	return s.tokens.ListAPI(ctx, userID)
}
