package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) recordsRoutes() {
	s.mux.HandleFunc("GET /api/v1/records", s.requireAuth(s.handleRecordsList))
	s.mux.HandleFunc("POST /api/v1/records", s.requireAuth(s.handleRecordCreate))
	s.mux.HandleFunc("PUT /api/v1/records/{id}", s.requireAuth(s.handleRecordPut))
	s.mux.HandleFunc("DELETE /api/v1/records/{id}", s.requireAuth(s.handleRecordDelete))
}

// reloadRecords runs with a context stripped of cancellation: the write
// already committed, so a client disconnecting mid-request must not abort
// the reload.
func (s *Server) reloadRecords(r *http.Request) {
	if err := s.deps.Reloader.ReloadRecords(context.WithoutCancel(r.Context())); err != nil {
		slog.Error("record reload after api write failed", "err", err)
	}
}

// normalizeRecord validates and canonicalizes; returns ("", false) message on failure.
func normalizeRecord(rec *store.LocalRecord) (string, bool) {
	rec.Name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rec.Name), "."))
	name := strings.TrimPrefix(rec.Name, "*.")
	if name == "" || !strings.Contains(name, ".") || strings.ContainsAny(name, " /\\") {
		return "name must be a domain (wildcard *.parent allowed)", false
	}
	if rec.TTL < 1 || rec.TTL > 86400 {
		return "ttl must be 1-86400", false
	}
	rec.Type = strings.ToUpper(rec.Type)
	switch rec.Type {
	case "A":
		ip := net.ParseIP(rec.Value)
		if ip == nil || ip.To4() == nil {
			return "value must be an IPv4 address", false
		}
	case "AAAA":
		ip := net.ParseIP(rec.Value)
		if ip == nil || ip.To4() != nil {
			return "value must be an IPv6 address", false
		}
	case "CNAME":
		t := strings.ToLower(strings.TrimSuffix(rec.Value, "."))
		if t == "" || !strings.Contains(t, ".") {
			return "cname target must be a domain", false
		}
		rec.Value = t
	case "TXT":
		if rec.Value == "" {
			return "txt value required", false
		}
	default:
		return "type must be A, AAAA, CNAME or TXT", false
	}
	return "", true
}

func (s *Server) handleRecordsList(w http.ResponseWriter, r *http.Request) {
	all, err := s.deps.Store.Records().All(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, all)
}

func (s *Server) handleRecordCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decode[store.LocalRecord](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if msg, ok := normalizeRecord(&body); !ok {
		errJSON(w, http.StatusBadRequest, msg)
		return
	}
	id, err := s.deps.Store.Records().Add(r.Context(), body)
	if err != nil {
		storeErr(w, err)
		return
	}
	s.reloadRecords(r)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleRecordPut(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	body, err := decode[store.LocalRecord](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if msg, ok := normalizeRecord(&body); !ok {
		errJSON(w, http.StatusBadRequest, msg)
		return
	}
	body.ID = id
	if err := s.deps.Store.Records().Update(r.Context(), body); err != nil {
		storeErr(w, err)
		return
	}
	s.reloadRecords(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRecordDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		errJSON(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.deps.Store.Records().Delete(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	s.reloadRecords(r)
	w.WriteHeader(http.StatusNoContent)
}
