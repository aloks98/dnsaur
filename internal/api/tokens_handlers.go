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
	body, err := decode[tokenCreate](r)
	if err != nil || body.Name == "" {
		errJSON(w, http.StatusBadRequest, "name required")
		return
	}
	if body.Scope == "" {
		body.Scope = "write"
	}
	plain, err := s.deps.Auth.CreateAPIToken(r.Context(), userFrom(r).ID, body.Name, body.Scope)
	if err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	list, _ := s.deps.Auth.ListAPITokens(r.Context(), userFrom(r).ID)
	var id int64
	for _, t := range list {
		if t.Name == body.Name && t.ID > id {
			id = t.ID
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "token": plain})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Auth.RevokeToken(r.Context(), userFrom(r).ID, id); err != nil {
		errJSON(w, http.StatusNotFound, "not found")
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
	body, err := decode[totpConfirm](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.deps.Auth.EnableTOTPConfirm(r.Context(), userFrom(r).ID, body.Secret, body.Code); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type totpDisable struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	body, err := decode[totpDisable](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.deps.Auth.DisableTOTP(r.Context(), userFrom(r).ID, body.Code); err != nil {
		errJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
