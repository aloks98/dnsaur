package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
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

// SessionMaxLifetime is how long a session may live counting from the login
// that created it, no matter how often it is used. Sliding renewal alone
// has no ceiling: a browser that checks in every fortnight pushes the
// expiry out forever, so a cookie lifted once and quietly kept warm never
// expires on its own. Past this the session is deleted and the user logs
// in again, second factor and all.
const SessionMaxLifetime = 90 * 24 * time.Hour

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
	// verifyTOTP reports the 30-second time step a code matched, so the
	// caller can refuse a step that has already been used. It answers
	// ok=false for a user that has a TOTP secret but supplied no valid code.
	verifyTOTP func(secret, code string, now time.Time) (step int64, ok bool)
	// hashPassword is HashPassword, indirected so a test can count how often
	// a request pays the argon2id cost. Which requests hash and which do not
	// is a property of this service, not of wall-clock timing, so it has to
	// be observable as a fact rather than measured.
	hashPassword func(pw string) (string, error)
}

func New(users store.UserStore, tokens store.TokenStore) *Service {
	return &Service{users: users, tokens: tokens, Now: time.Now,
		verifyTOTP:   verifyTOTPStep,
		hashPassword: HashPassword}
}

func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.users.Count(ctx)
	return n == 0, err
}

// CreateAdmin creates the single admin account during first-run setup.
// Validation runs first (cheap, no I/O), then an emptiness check, then the
// argon2id hash, and only then an atomic conditional insert
// (UserStore.CreateIfNone) that decides the race: Count-then-Create is not
// atomic on its own, so it cannot be the last word — two concurrent setup
// requests can both see an empty table, and CreateIfNone is what keeps them
// from both inserting.
//
// The Count is there for cost, not for correctness. /setup is
// unauthenticated and stays reachable forever, so hashing before the check
// let anyone spend a 64 MiB, t=3 argon2id pass per request on an install
// that was configured months ago — and 50 of those at once is 3.2 GiB of
// transient allocation. Checking first makes the request that cannot
// possibly succeed cost one SELECT.
func (s *Service) CreateAdmin(ctx context.Context, username, password string) error {
	if len(username) == 0 {
		return fmt.Errorf("%w: username required", ErrInvalidInput)
	}
	if len(password) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrInvalidInput)
	}
	n, err := s.users.Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrSetupDone
	}
	hash, err := s.hashPassword(password)
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
		step, ok := s.verifyTOTP(u.TOTPSecret, totpCode, s.Now())
		if !ok {
			return "", ErrBadCredentials
		}
		// RFC 6238 §5.2: a code that has authenticated once must not
		// authenticate again. Claiming the step is what refuses the second
		// use, and a refused claim answers exactly as a wrong code does, so
		// the difference is not something an attacker can read off.
		claimed, cerr := s.users.ClaimTOTPStep(ctx, u.ID, step)
		if cerr != nil {
			return "", cerr
		}
		if !claimed {
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
	ceiling, capped := sessionCeiling(tok)
	if capped && !now.Before(ceiling) {
		_ = s.tokens.Delete(ctx, tok.ID)
		return store.User{}, store.AuthToken{}, ErrBadCredentials
	}
	_ = s.tokens.Touch(ctx, tok.ID, now.UnixMilli())
	if tok.ExpiresAt > 0 && time.UnixMilli(tok.ExpiresAt).Sub(now) < SessionTTL/2 {
		exp := now.Add(SessionTTL) // sliding renewal
		if capped && exp.After(ceiling) {
			exp = ceiling
		}
		_ = s.tokens.SetExpiry(ctx, tok.ID, exp.UnixMilli())
	}
	u, ok, err := s.users.ByID(ctx, tok.UserID)
	if err != nil || !ok {
		return store.User{}, store.AuthToken{}, ErrBadCredentials
	}
	return u, tok, nil
}

// sessionCeiling is when tok stops being valid however often it is used.
// Only session tokens have one: an API token is a credential the operator
// minted deliberately and revokes by name, and expiring it out from under a
// script on a schedule nobody set would be a surprise, not a safeguard.
func sessionCeiling(tok store.AuthToken) (time.Time, bool) {
	if tok.Kind != "session" || tok.CreatedAt <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(tok.CreatedAt).Add(SessionMaxLifetime), true
}

func (s *Service) Logout(ctx context.Context, plain string) error {
	tok, ok, err := s.tokens.ByHash(ctx, HashToken(plain))
	if err != nil || !ok {
		return err
	}
	return s.tokens.Delete(ctx, tok.ID)
}

// CreateAPIToken mints an API token and returns its row id alongside the
// plaintext. The id is returned rather than discarded because the caller has
// no other way to learn it: the token's hash is one-way, so a handler that
// needed the id used to re-list the user's tokens and pick the highest one
// with a matching name — ambiguous the moment two tokens share a name, and
// 0 when the listing itself failed.
func (s *Service) CreateAPIToken(ctx context.Context, userID int64, name, scope string) (int64, string, error) {
	if scope != "read" && scope != "write" {
		return 0, "", fmt.Errorf("scope must be read or write")
	}
	plain, hash, err := NewToken()
	if err != nil {
		return 0, "", err
	}
	id, err := s.tokens.Create(ctx, store.AuthToken{
		UserID: userID, Kind: "api", Name: name, TokenHash: hash, Scope: scope, CreatedAt: s.Now().UnixMilli(),
	})
	if err != nil {
		return 0, "", err
	}
	return id, plain, nil
}

// RevokeToken deletes one of this user's API tokens.
//
// A token that is not theirs (or does not exist) comes back wrapping
// store.ErrNotFound, so the API can answer 404 for that case *and only that
// case*. Every error here used to be indistinguishable, and the handler
// mapped all of them to 404 — so a database that was merely unreachable told
// a script the token was gone.
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
	return fmt.Errorf("token %d not owned by user %d: %w", tokenID, userID, store.ErrNotFound)
}

func (s *Service) ListAPITokens(ctx context.Context, userID int64) ([]store.AuthToken, error) {
	return s.tokens.ListAPI(ctx, userID)
}
