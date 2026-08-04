package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

var editableSettings = map[string]func(string) bool{
	"upstreams":             func(v string) bool { return strings.TrimSpace(v) != "" },
	"upstream.strategy":     oneOf("failover", "fastest", "race"),
	"blocking.mode":         oneOf("null-ip", "nxdomain"),
	"blocking.ttl":          nonNegInt,
	"cache.min_ttl":         nonNegInt,
	"cache.max_ttl":         nonNegInt,
	"cache.max_entries":     nonNegInt,
	"cache.serve_stale_for": nonNegInt,
	"lists.refresh_hours":   nonNegInt,
	"qlog.retention_days":   nonNegInt,
	"qlog.privacy":          oneOf("full", "anon", "none"),
}

func oneOf(vals ...string) func(string) bool {
	return func(v string) bool {
		for _, x := range vals {
			if v == x {
				return true
			}
		}
		return false
	}
}

func nonNegInt(v string) bool {
	n, err := strconv.ParseInt(v, 10, 64)
	return err == nil && n >= 0
}

func (s *Server) settingsRoutes() {
	s.mux.HandleFunc("GET /api/v1/settings", s.requireAuth(s.handleSettingsGet))
	s.mux.HandleFunc("PUT /api/v1/settings", s.requireAuth(s.handleSettingsPut))
	s.mux.HandleFunc("GET /api/v1/blocking", s.requireAuth(s.handleBlockingGet))
	s.mux.HandleFunc("POST /api/v1/blocking/pause", s.requireAuth(s.handlePause))
	s.mux.HandleFunc("DELETE /api/v1/blocking/pause", s.requireAuth(s.handleResume))
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.deps.Store.Settings().All(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	for k := range all {
		if strings.HasPrefix(k, "instance.") || strings.HasPrefix(k, "stats.") {
			delete(all, k)
		}
	}
	writeJSON(w, http.StatusOK, all)
}

type settingPut struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	body, err := decode[settingPut](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	validate, ok := editableSettings[body.Key]
	if !ok {
		errJSON(w, http.StatusBadRequest, "setting not editable: "+body.Key)
		return
	}
	if !validate(body.Value) {
		errJSON(w, http.StatusBadRequest, "invalid value for "+body.Key)
		return
	}
	if err := s.deps.Store.Settings().Set(r.Context(), body.Key, body.Value); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pauseReq struct {
	GroupID int64 `json:"group_id"`
	Minutes int   `json:"minutes"`
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	body, err := decode[pauseReq](r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Minutes < 1 || body.Minutes > 1440 {
		errJSON(w, http.StatusBadRequest, "minutes must be 1-1440")
		return
	}
	s.deps.Engine.Pause(body.GroupID, time.Duration(body.Minutes)*time.Minute)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	gid, _ := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64)
	s.deps.Engine.Pause(gid, 0)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBlockingGet(w http.ResponseWriter, r *http.Request) {
	gid, _ := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64)
	until := s.deps.Engine.PausedUntil(gid)
	var ms int64
	if !until.IsZero() {
		ms = until.UnixMilli()
	}
	writeJSON(w, http.StatusOK, map[string]int64{"paused_until": ms})
}
