package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/dnssrv"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/store"
	"github.com/miekg/dns"
)

// seedAPIQlog writes two entries dated within the last hour. They used to
// sit at the Unix epoch, which made every stats test ask for a window of
// half a million hours to reach them — a window `hours` now refuses (see
// maxStatsHours), and one no operator ever means. Dating them from now
// instead lets the stats tests use ordinary windows.
func seedAPIQlog(t *testing.T, s store.Store) {
	t.Helper()
	now := time.Now().UnixMilli()
	err := s.QueryLog().InsertBatch(t.Context(), []store.QueryLogEntry{
		{At: now - 2000, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
		{At: now - 1000, InstanceID: "i", ClientIP: "10.0.0.6", QName: "ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQueriesSearch(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	seedAPIQlog(t, s)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/queries?decision=blocked", "", cookie)
	var entries []store.QueryLogEntry
	_ = json.Unmarshal(w.Body.Bytes(), &entries)
	if w.Code != 200 || len(entries) != 1 || entries[0].QName != "ads.example" {
		t.Fatalf("search: %d %v", w.Code, entries)
	}
}

// TestQueriesSearchNegativeOffset guards against a spurious 503: postgres
// errors on a negative SQL OFFSET, so a client-supplied ?offset=-5 must be
// clamped to 0 rather than passed straight through to the store.
func TestQueriesSearchNegativeOffset(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	seedAPIQlog(t, s)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/queries?offset=-5", "", cookie)
	if w.Code != 200 {
		t.Fatalf("negative offset: %d %s", w.Code, w.Body.String())
	}
	var entries []store.QueryLogEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected both seeded entries, got %v", entries)
	}
}

func TestStatsEndpoints(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	seedAPIQlog(t, s)
	if _, err := s.Stats().Rollup(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	// The seeded entries are minutes old (seedAPIQlog), so an ordinary
	// window covers them — but the hour bucket they land in starts before
	// "now minus 24h" only if the window reaches the bucket's own start, so
	// 48 hours rather than 24.
	const farHours = 48
	w := doReq(t, srv.Handler(), "GET", fmt.Sprintf("/api/v1/stats/overview?hours=%d", farHours), "", cookie)
	var ov map[string]int64
	_ = json.Unmarshal(w.Body.Bytes(), &ov)
	if ov["total"] != 2 || ov["blocked"] != 1 {
		t.Fatalf("overview: %v", ov)
	}
	w = doReq(t, srv.Handler(), "GET", fmt.Sprintf("/api/v1/stats/top?metric=blocked_domain&n=5&hours=%d", farHours), "", cookie)
	var top []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &top)
	if len(top) != 1 || top[0]["key"] != "ads.example" {
		t.Fatalf("top: %v", top)
	}
	if w := doReq(t, srv.Handler(), "GET", "/api/v1/stats/top?metric=passwords", "", cookie); w.Code != 400 {
		t.Fatalf("bad metric accepted: %d", w.Code)
	}
}

// TestStatsOverviewReportsDropped: entries the query-log buffer discarded
// were counted and never shown anywhere, so a dashboard built on a log with
// holes in it looked complete. The overview carries the count — 0 when
// nothing was lost, and 0 as well on a server with no query logger at all,
// which has lost nothing.
func TestStatsOverviewReportsDropped(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)

	w := doReq(t, srv.Handler(), "GET", "/api/v1/stats/overview", "", cookie)
	var ov map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if _, ok := ov["dropped"]; !ok {
		t.Fatalf("no dropped field with no logger installed: %v", ov)
	}
	if ov["dropped"] != 0 {
		t.Fatalf("dropped = %d with no logger installed", ov["dropped"])
	}

	// A logger whose buffer overflowed: four entries emitted into room for
	// two, so two were dropped.
	logger := qlog.New(&nullQLStore{}, qlog.Options{InstanceID: "i", Buffer: 2, BatchSize: 100, FlushEvery: time.Hour})
	h := logger.Middleware()(dnssrv.HandlerFunc(func(ctx context.Context, req *dnssrv.Request) (*dnssrv.Response, error) {
		m := new(dns.Msg)
		m.SetReply(req.Msg)
		return &dnssrv.Response{Msg: m, Decision: dnssrv.DecisionForwarded}, nil
	}))
	for i := 0; i < 4; i++ {
		m := new(dns.Msg)
		m.SetQuestion(fmt.Sprintf("q%d.example.", i), dns.TypeA)
		if _, err := h.ServeDNS(t.Context(), &dnssrv.Request{Msg: m}); err != nil {
			t.Fatal(err)
		}
	}
	srv.deps.Logger = logger

	w = doReq(t, srv.Handler(), "GET", "/api/v1/stats/overview", "", cookie)
	ov = map[string]int64{}
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov["dropped"] != 2 {
		t.Fatalf("dropped = %d, want 2", ov["dropped"])
	}
}

type nullQLStore struct{}

func (nullQLStore) InsertBatch(ctx context.Context, entries []store.QueryLogEntry) error {
	return nil
}

func (nullQLStore) DeleteBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	return 0, nil
}

func (nullQLStore) Search(ctx context.Context, f store.QueryLogFilter) ([]store.QueryLogEntry, error) {
	return nil, nil
}

func TestSSETail(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	logger := qlog.New(&nullQLStore{}, qlog.Options{InstanceID: "i", FlushEvery: time.Hour, BatchSize: 100})
	srv.deps.Logger = logger

	_, line := tailStream(t, srv.Handler(), cookie, nil, func() {
		logger.Publish(store.QueryLogEntry{QName: "live.example", Decision: "blocked"})
	})
	if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "live.example") {
		t.Fatalf("sse line: %q", line)
	}
}

// `hours` had no upper bound, and time.Duration(hours)*time.Hour overflows
// int64 at about 2.5 million hours — so a large enough value wrapped
// negative and asked the store for a window in the future, which answers
// nothing. Clamped to a year, the answer stays the same as the largest
// window anyone can actually mean.
func TestStatsHoursIsClamped(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	h := srv.Handler()
	seedAPIQlog(t, s)
	if _, err := s.Stats().Rollup(t.Context(), 0); err != nil {
		t.Fatal(err)
	}

	want := hoursFromSec(httptest.NewRequest("GET", "/api/v1/stats/overview?hours="+strconv.Itoa(maxStatsHours), nil))
	got := hoursFromSec(httptest.NewRequest("GET", "/api/v1/stats/overview?hours=999999999999", nil))
	if got != want {
		t.Errorf("hours=999999999999 resolved to %d, want the %d-hour clamp at %d", got, maxStatsHours, want)
	}

	for _, path := range []string{
		"/api/v1/stats/overview?hours=999999999999",
		"/api/v1/stats/timeline?hours=999999999999",
		"/api/v1/stats/top?metric=domain&hours=999999999999",
	} {
		w := doReq(t, h, "GET", path, "", cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"total":0`) {
			t.Errorf("GET %s answered an empty window: %s", path, w.Body.String())
		}
	}
}
