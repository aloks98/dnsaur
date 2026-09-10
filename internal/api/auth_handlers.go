package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
)

// The throttle on POST /setup and POST /auth/login. Both are
// unauthenticated and both pay for an argon2id hash, so without a ceiling a
// single client can spend the server's memory and CPU indefinitely — and
// login with no throttle is also an unmetered password oracle, which is
// what makes the 428 "totp code required" answer (given once the password
// verified) tolerable rather than a free confirmation service.
//
// Ten attempts a minute is far above what a person typing a password needs
// and far below what guessing needs. Exceeding it locks the source out for
// a minute rather than merely refusing the extra attempts, so a script that
// keeps hammering keeps extending its own lockout.
const (
	attemptLimit   = 10
	attemptWindow  = time.Minute
	attemptLockout = time.Minute
)

// attemptLimiter counts recent attempts per source and locks a source out
// once it exceeds attemptLimit within attemptWindow.
//
// Per Server, in memory, and deliberately not persisted: it is a brake on a
// burst, not an account-lockout policy, and a restart clearing it is the
// correct behaviour rather than a hole (the operator restarting the server
// is not the attacker).
type attemptLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*attemptEntry
	swept   time.Time
}

type attemptEntry struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{now: time.Now, entries: map[string]*attemptEntry{}}
}

// allow records one attempt from key and reports whether it may proceed.
// When it may not, the duration is how long the caller has to wait, for
// Retry-After.
func (l *attemptLimiter) allow(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)

	e := l.entries[key]
	if e == nil {
		e = &attemptEntry{windowStart: now}
		l.entries[key] = e
	}
	if now.Before(e.lockedUntil) {
		return e.lockedUntil.Sub(now), false
	}
	if now.Sub(e.windowStart) >= attemptWindow {
		e.count, e.windowStart = 0, now
	}
	e.count++
	if e.count > attemptLimit {
		e.lockedUntil = now.Add(attemptLockout)
		return attemptLockout, false
	}
	return 0, true
}

// sweep drops entries that can no longer refuse anything, so a scan across
// many source addresses cannot grow this map without bound. Once a window
// per sweep: the map is small, and the work is proportional to it.
func (l *attemptLimiter) sweep(now time.Time) {
	if now.Sub(l.swept) < attemptWindow {
		return
	}
	l.swept = now
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.windowStart) >= attemptWindow {
			delete(l.entries, k)
		}
	}
}

// throttle counts this request against its source and, when the source is
// over its budget, answers 429 and reports false. Called before anything
// else in the two handlers it guards, so a malformed body, a wrong
// password and the 428 that says the password was right all cost the same
// one attempt.
func (s *Server) throttle(w http.ResponseWriter, r *http.Request) bool {
	retry, ok := s.attempts.allow(s.clientIP(r))
	if ok {
		return true
	}
	secs := int(retry / time.Second)
	if retry%time.Second != 0 {
		secs++
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	errJSON(w, http.StatusTooManyRequests, "too many attempts")
	return false
}

func (s *Server) authRoutes() {
	s.route("GET /api/v1/setup", s.handleSetupState)
	s.route("POST /api/v1/setup", s.handleSetup)
	s.route("POST /api/v1/auth/login", s.handleLogin)
	s.route("POST /api/v1/auth/logout", s.requireAuth(s.handleLogout))
	s.route("GET /api/v1/auth/me", s.requireAuth(s.handleMe))
	s.route("POST /api/v1/auth/password", s.requireAuth(s.handlePasswordChange))
	s.route("DELETE /api/v1/auth/sessions", s.requireAuth(s.handleSessionsRevoke))
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	req, err := s.deps.Auth.SetupRequired(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setup_required": req})
}

type credsReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code,omitempty"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.throttle(w, r) {
		return
	}
	body, ok := decodeOr400[credsReq](w, r)
	if !ok {
		return
	}
	switch err := s.deps.Auth.CreateAdmin(r.Context(), body.Username, body.Password); {
	case err == nil:
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
	case errors.Is(err, auth.ErrSetupDone):
		errJSON(w, http.StatusConflict, "setup already completed")
	case errors.Is(err, auth.ErrInvalidInput):
		errJSON(w, http.StatusBadRequest, err.Error())
	default:
		// Anything else (store failures) must not leak raw storage errors
		// to an unauthenticated caller.
		storeErr(w, err)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.throttle(w, r) {
		return
	}
	body, ok := decodeOr400[credsReq](w, r)
	if !ok {
		return
	}
	// Global constraint: while first-run setup hasn't happened, login must
	// refuse with 409 rather than fall through to bad-credentials/other
	// errors against a still-empty users table.
	if req, serr := s.deps.Auth.SetupRequired(r.Context()); serr != nil {
		storeErr(w, serr)
		return
	} else if req {
		errJSON(w, http.StatusConflict, "setup required")
		return
	}
	tok, err := s.deps.Auth.Login(r.Context(), body.Username, body.Password, body.TOTPCode)
	switch {
	case errors.Is(err, auth.ErrTOTPRequired):
		errJSON(w, http.StatusPreconditionRequired, "totp code required")
		return
	case errors.Is(err, auth.ErrBadCredentials):
		errJSON(w, http.StatusUnauthorized, "bad credentials")
		return
	case err != nil:
		storeErr(w, err)
		return
	}
	http.SetCookie(w, s.sessionCookie(tok, int(auth.SessionTTL/time.Second), r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dnsaur_session"); err == nil {
		_ = s.deps.Auth.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, s.sessionCookie("", -1, r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "totp_enabled": u.TOTPSecret != "",
	})
}

type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handlePasswordChange is throttled on the same budget as login, and for
// the same reason: it runs an argon2id verification and then says whether
// the password was right. Being behind requireAuth narrows who can spend
// that budget but does not make either fact cheaper — a stolen session
// cookie would otherwise be an unmetered oracle for the password it is not
// enough to change.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if !s.throttle(w, r) {
		return
	}
	body, ok := decodeOr400[passwordChange](w, r)
	if !ok {
		return
	}
	// The caller's own credential is kept: the change logs every other
	// session out, and logging them out of the tab they made it from would
	// be a bug rather than a safeguard — the same rule the TOTP handlers
	// follow.
	err := s.deps.Auth.ChangePassword(r.Context(), userFrom(r).ID,
		body.CurrentPassword, body.NewPassword, tokenFrom(r).ID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrBadCredentials):
		// 400, not 401: the request authenticated fine, and answering 401
		// would tell a dashboard its session had expired and send the admin
		// back to the login screen over a typo. There is no enumeration to
		// protect here either — the account is the one the caller is
		// already signed in to.
		errJSON(w, http.StatusBadRequest, "current password is wrong")
	case errors.Is(err, auth.ErrInvalidInput):
		errJSON(w, http.StatusBadRequest, err.Error())
	default:
		storeErr(w, err)
	}
}

// handleSessionsRevoke is "log out everywhere": every session on the
// account but this one. Not throttled — it verifies nothing and costs a
// DELETE — and deliberately not a way to revoke API tokens, which are
// named credentials with their own list and their own revoke.
func (s *Server) handleSessionsRevoke(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Auth.RevokeOtherSessions(r.Context(), userFrom(r).ID, tokenFrom(r).ID); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionCookie(value string, maxAge int, r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name: "dnsaur_session", Value: value, Path: "/",
		MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.overHTTPS(r),
	}
}

// overHTTPS reports whether the request reached the user over TLS, which
// decides the session cookie's Secure attribute.
//
// r.TLS covers the case where Go terminated TLS itself. It is not the usual
// deployment: dnsaur behind nginx or Caddy speaks plain HTTP on the inside,
// and judging by r.TLS alone shipped a 30-day session cookie with no Secure
// attribute to every such install. X-Forwarded-Proto is what the proxy says
// about the outside connection, and it is believed only when the request
// came from an address the operator named in trusted_proxies — it is a
// header, and a client that could set it at will would be choosing its own
// cookie attributes.
func (s *Server) overHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !s.fromTrustedProxy(r) {
		return false
	}
	// A chain of proxies appends, so the client-facing hop is the first
	// entry (RFC 7239 §7.1 says the same of Forwarded).
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
