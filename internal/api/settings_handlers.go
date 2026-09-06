package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aloks98/dnsaur/internal/upstream"
)

// A validator returns why a value is refused, so the 400 can say it. It was
// a bool: every rejection read "invalid value for upstreams", which names
// the field and not the problem — unhelpful for a free-text setting with a
// grammar, and the reason the upstreams entry was never given a real check
// at all.
var editableSettings = map[string]func(string) error{
	"upstreams":             validUpstreams,
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

// validUpstreams runs the same parser applySettings runs, so a value that
// saves is a value that will build a forwarder.
func validUpstreams(v string) error {
	_, err := upstream.ParseUpstreams(v)
	return err
}

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		if slices.Contains(vals, v) {
			return nil
		}
		return fmt.Errorf("must be one of: %s", strings.Join(vals, ", "))
	}
}

func nonNegInt(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return errors.New("must be a whole number, zero or more")
	}
	return nil
}

func (s *Server) settingsRoutes() {
	s.route("GET /api/v1/settings", s.requireAuth(s.handleSettingsGet))
	s.route("PUT /api/v1/settings", s.requireAuth(s.handleSettingsPut))
	s.route("GET /api/v1/resolver/status", s.requireAuth(s.handleResolverStatus))
	s.route("GET /api/v1/blocking", s.requireAuth(s.handleBlockingGet))
	s.route("POST /api/v1/blocking/pause", s.requireAuth(s.handlePause))
	s.route("DELETE /api/v1/blocking/pause", s.requireAuth(s.handleResume))
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

// resolverStatus is what the settings screen needs to know about the running
// resolver that is not a setting. One field today, plus its reason; a flat
// object rather than a bare boolean so the next such fact does not need a
// second endpoint.
type resolverStatus struct {
	// EncryptionDowngraded: the stored `upstreams` asked for tls:// or
	// https://, would not parse, and the server is resolving through the
	// hardcoded plaintext defaults instead. Queries are going out in the
	// clear while the settings page still shows the operator's encrypted
	// value, which is why this needs saying somewhere other than the log.
	EncryptionDowngraded bool `json:"encryption_downgraded"`
	// Reason is the parse failure, verbatim, or "" when nothing is wrong.
	Reason string `json:"reason"`
}

// handleResolverStatus answers the one round trip the settings page makes
// for server state. Deps.ResolverStatus is nil in test servers with no App
// behind them; a server with no forwarder has downgraded nothing, so that
// answers false.
func (s *Server) handleResolverStatus(w http.ResponseWriter, r *http.Request) {
	var out resolverStatus
	if s.deps.ResolverStatus != nil {
		out.EncryptionDowngraded, out.Reason = s.deps.ResolverStatus.UpstreamDowngrade()
	}
	writeJSON(w, http.StatusOK, out)
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
	if err := validate(body.Value); err != nil {
		// The prefix stays: it is what the existing suite and the web form
		// both key off. The reason is appended, not substituted.
		errJSON(w, http.StatusBadRequest, "invalid value for "+body.Key+": "+err.Error())
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
