package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/aloks98/dnsaur/internal/store"
)

func (s *Server) queriesRoutes() {
	s.route("GET /api/v1/queries", s.requireAuth(s.handleQueriesSearch))
	s.route("GET /api/v1/queries/tail", s.requireAuth(s.handleQueriesTail))
	s.route("GET /api/v1/stats/overview", s.requireAuth(s.handleStatsOverview))
	s.route("GET /api/v1/stats/timeline", s.requireAuth(s.handleStatsTimeline))
	s.route("GET /api/v1/stats/top", s.requireAuth(s.handleStatsTop))
}

func qInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

func (s *Server) handleQueriesSearch(w http.ResponseWriter, r *http.Request) {
	f := store.QueryLogFilter{
		FromMs: qInt(r, "from"), ToMs: qInt(r, "to"),
		ClientIP: r.URL.Query().Get("client"), QNameContains: r.URL.Query().Get("q"),
		Decision: r.URL.Query().Get("decision"), QType: r.URL.Query().Get("type"),
		Limit: int(qInt(r, "limit")), Offset: int(qInt(r, "offset")),
	}
	if f.Offset < 0 {
		// Postgres errors on a negative OFFSET (a spurious 503 for what's
		// really a bad-but-harmless query param); clamp instead of failing.
		f.Offset = 0
	}
	entries, err := s.deps.Store.QueryLog().Search(r.Context(), f)
	if err != nil {
		storeErr(w, err)
		return
	}
	if entries == nil {
		entries = []store.QueryLogEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleQueriesTail(w http.ResponseWriter, r *http.Request) {
	if s.deps.Logger == nil {
		errJSON(w, http.StatusServiceUnavailable, "query log disabled")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		errJSON(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch, cancel := s.deps.Logger.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-ch:
			if !open {
				return
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func hoursFromSec(r *http.Request) int64 {
	hours := qInt(r, "hours")
	if hours <= 0 {
		hours = 24
	}
	return time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
}

func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	from := hoursFromSec(r)
	decisions, err := s.deps.Store.Stats().Counter(r.Context(), from, "decision")
	if err != nil {
		storeErr(w, err)
		return
	}
	clients, err := s.deps.Store.Stats().Counter(r.Context(), from, "client")
	if err != nil {
		storeErr(w, err)
		return
	}
	var total int64
	for _, n := range decisions {
		total += n
	}
	// Entries the query-log buffer discarded because it was full, since
	// this process started. It belongs beside the counts rather than in a
	// corner of the log: every figure here is derived from the query log,
	// and a non-zero value is what says they are undercounts. Deps.Logger
	// is nil on a server with no query logging at all, which has dropped
	// nothing.
	var dropped int64
	if s.deps.Logger != nil {
		dropped = s.deps.Logger.Dropped()
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"total":     total,
		"blocked":   decisions["blocked"],
		"cached":    decisions["cached"] + decisions["stale"],
		"forwarded": decisions["forwarded"],
		"clients":   int64(len(clients)),
		"dropped":   dropped,
	})
}

func (s *Server) handleStatsTimeline(w http.ResponseWriter, r *http.Request) {
	tl, err := s.deps.Store.Stats().Timeline(r.Context(), hoursFromSec(r))
	if err != nil {
		storeErr(w, err)
		return
	}
	type bucket struct {
		Bucket    int64            `json:"bucket"`
		Decisions map[string]int64 `json:"decisions"`
	}
	out := make([]bucket, 0, len(tl))
	for b, d := range tl {
		out = append(out, bucket{Bucket: b, Decisions: d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	writeJSON(w, http.StatusOK, out)
}

var topMetrics = map[string]bool{"domain": true, "blocked_domain": true, "client": true}

func (s *Server) handleStatsTop(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	if !topMetrics[metric] {
		errJSON(w, http.StatusBadRequest, "metric must be domain, blocked_domain or client")
		return
	}
	n := qInt(r, "n")
	if n <= 0 {
		n = 10
	}
	if n > 100 {
		n = 100
	}
	counts, err := s.deps.Store.Stats().Counter(r.Context(), hoursFromSec(r), metric)
	if err != nil {
		storeErr(w, err)
		return
	}
	type kv struct {
		Key   string `json:"key"`
		Count int64  `json:"count"`
	}
	out := make([]kv, 0, len(counts))
	for k, v := range counts {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if int64(len(out)) > n {
		out = out[:n]
	}
	writeJSON(w, http.StatusOK, out)
}
