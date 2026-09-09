package api

import (
	"net/http"

	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) tokensRoutes() {
	s.route("GET /api/v1/tokens", s.requireAuth(s.handleTokensList))
	s.route("POST /api/v1/tokens", s.requireAuth(s.handleTokenCreate))
	s.route("DELETE /api/v1/tokens/{id}", s.requireAuth(s.handleTokenRevoke))
	s.route("POST /api/v1/auth/totp/start", s.requireAuth(s.handleTOTPStart))
	s.route("POST /api/v1/auth/totp/confirm", s.requireAuth(s.handleTOTPConfirm))
	s.route("POST /api/v1/auth/totp/disable", s.requireAuth(s.handleTOTPDisable))
}

func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Auth.ListAPITokens(r.Context(), userFrom(r).ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	if list == nil {
		list = []store.AuthToken{} // encode as [] rather than null
	}
	writeJSON(w, http.StatusOK, list)
}

type tokenCreate struct {
	Name  string `json:"name"`
	Scope string `json:"scope"` // read | write; empty defaults to write
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[tokenCreate](w, r)
	if !ok {
		return
	}
	if body.Name == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	if body.Scope == "" {
		body.Scope = "write"
	}
	// The insert id, not a guess. This used to re-list the user's tokens and
	// take the highest id with a matching name, which two tokens called the
	// same thing made ambiguous and a failed listing made 0.
	id, plain, err := s.deps.Auth.CreateAPIToken(r.Context(), userFrom(r).ID, body.Name, body.Scope)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "token": plain})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	// storeErr, so only "no such token of yours" is a 404 — RevokeToken
	// wraps store.ErrNotFound for exactly that case. Mapping every error
	// here to 404 told a caller the token was gone when the database was
	// merely unreachable, which is the one answer that makes them stop
	// looking.
	if err := s.deps.Auth.RevokeToken(r.Context(), userFrom(r).ID, id); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTOTPStart(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	secret, otpURL, err := s.deps.Auth.EnableTOTPStart(r.Context(), u.ID, u.Username)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "totp generation failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth_url": otpURL})
}

type totpConfirm struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[totpConfirm](w, r)
	if !ok {
		return
	}
	// The caller's own token is kept: enabling TOTP logs every other
	// session out, and logging the user out of the tab they are enrolling
	// from would be a bug rather than a safeguard.
	if err := s.deps.Auth.EnableTOTPConfirm(r.Context(), userFrom(r).ID, body.Secret, body.Code, tokenFrom(r).ID); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type totpDisable struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeOr400[totpDisable](w, r)
	if !ok {
		return
	}
	if err := s.deps.Auth.DisableTOTP(r.Context(), userFrom(r).ID, body.Code, tokenFrom(r).ID); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
