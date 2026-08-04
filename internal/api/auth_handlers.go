package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
)

func (s *Server) authRoutes() {
	s.mux.HandleFunc("GET /api/v1/setup", s.handleSetupState)
	s.mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("GET /api/v1/auth/me", s.requireAuth(s.handleMe))
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
	body, err := decode[credsReq](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
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
	body, err := decode[credsReq](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
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
	http.SetCookie(w, sessionCookie(tok, int(auth.SessionTTL/time.Second), r))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("dnsaur_session"); err == nil {
		_ = s.deps.Auth.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, sessionCookie("", -1, r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "totp_enabled": u.TOTPSecret != "",
	})
}

func sessionCookie(value string, maxAge int, r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name: "dnsaur_session", Value: value, Path: "/",
		MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil,
	}
}
