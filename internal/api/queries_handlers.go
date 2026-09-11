package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// qBool reads a list filter's boolean query parameter. Absent or empty is
// "no filter" rather than false — ?enabled= is the shape a form sends for a
// choice nobody made, and reading it as false would empty the listing.
//
// Exactly "true" or "false", the same grammar the serve.*.enabled settings
// validator holds values to, rather than strconv.ParseBool's wider set: a
// filter that silently accepts "1" and "T" is one more spelling for every
// client to disagree about.
func qBool(r *http.Request, key string) (value, present bool, err error) {
	switch raw := r.URL.Query().Get(key); raw {
	case "":
		return false, false, nil
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%s must be true or false", key)
	}
}

// qID reads a list filter's row-id query parameter, under the same rule
// pathID applies to an id in the path: it has to parse and be positive.
// Absent or empty is "no filter"; anything else that is not an id is an
// error rather than a 0 that quietly matches nothing.
func qID(r *http.Request, key string) (id int64, present bool, err error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return 0, false, nil
	}
	id, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil || id <= 0 {
		return 0, false, fmt.Errorf("%s must be a positive id", key)
	}
	return id, true, nil
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

// sseHeartbeat is how often an idle tail writes a comment frame. Well
// under the 60 seconds nginx, Caddy and every other reverse proxy time an
// idle upstream read out at, and far too rare to be a cost: a stream that
// is actually carrying queries writes this only in the gaps.
const sseHeartbeat = 20 * time.Second

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
	// A comment frame straight after the headers. Node's http-proxy — which
	// Vite's dev server puts in front of this — buffers response headers
	// until the first body byte, so without one an idle stream never opens
	// in the browser: it sits in CONNECTING until the first heartbeat. The
	// spec (WHATWG §9.2.4) has clients ignore comments, so it costs nothing.
	_, _ = io.WriteString(w, ": connected\n\n")
	fl.Flush()
	// The SSE keepalive: a comment frame, which the spec (WHATWG §9.2.4)
	// says a client ignores, so it costs an EventSource nothing to read.
	// Without it a quiet stream is indistinguishable from an abandoned
	// connection, and whatever sits in front of dnsaur closes it on its own
	// idle-read timeout — leaving the dashboard reconnecting on a loop it
	// never asked for.
	ping := time.NewTicker(s.tailHeartbeat)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
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

// maxStatsHours bounds the ?hours= window on the stats endpoints: a leap
// year, which is the longest span any of them can usefully answer for.
//
// Unbounded, time.Duration(hours)*time.Hour overflows int64 at about 2.5
// million hours and wraps negative, so a large enough value asked the store
// for a window in the *future* and got an empty answer back — a 200 that
// reports nothing happened. Clamping keeps the largest window anyone can
// mean and makes every larger one mean the same thing.
const maxStatsHours = 24 * 366

func hoursFromSec(r *http.Request) int64 {
	hours := qInt(r, "hours")
	if hours <= 0 {
		hours = 24
	}
	if hours > maxStatsHours {
		hours = maxStatsHours
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
	slices.SortFunc(out, func(a, b bucket) int { return cmp.Compare(a.Bucket, b.Bucket) })
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
	slices.SortFunc(out, func(a, b kv) int { return cmp.Compare(b.Count, a.Count) })
	if int64(len(out)) > n {
		out = out[:n]
	}
	writeJSON(w, http.StatusOK, out)
}
