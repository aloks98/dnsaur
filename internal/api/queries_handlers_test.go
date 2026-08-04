package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/store"
)

func seedAPIQlog(t *testing.T, s store.Store) {
	t.Helper()
	err := s.QueryLog().InsertBatch(t.Context(), []store.QueryLogEntry{
		{At: 1000, InstanceID: "i", ClientIP: "10.0.0.5", QName: "a.example", QType: "A", Decision: "forwarded", RCode: "NOERROR"},
		{At: 2000, InstanceID: "i", ClientIP: "10.0.0.6", QName: "ads.example", QType: "A", Decision: "blocked", RCode: "NOERROR"},
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
	// The seeded entries carry near-epoch At values (1000/2000ms), so the
	// window must reach back past Unix zero regardless of the real wall
	// clock the test happens to run under; a fixed literal like 87600h
	// (~10yr) would only cover them before ~1980 and is not date-stable.
	farHours := time.Now().Unix()/3600 + 24
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

	req := httptest.NewRequest("GET", "/api/v1/queries/tail", nil)
	req.AddCookie(cookie)
	ctx, cancelReq := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(w, req)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // let the handler subscribe
	logger.Publish(store.QueryLogEntry{QName: "live.example", Decision: "blocked"})
	time.Sleep(100 * time.Millisecond)
	cancelReq()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tail handler did not flush/close")
	}
	line, _ := bufio.NewReader(w.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "live.example") {
		t.Fatalf("sse line: %q", line)
	}
}
