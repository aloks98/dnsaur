package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/store"
)

// fakeReloader counts reload calls. filters needs mutex protection: since
// Task/Fix 4 handleListCreate fires its refresh in a background goroutine,
// its RefreshFilters call can run concurrently with a later, synchronous
// handler in the same test (e.g. PATCH/DELETE) touching the same field.
// clients/records stay purely synchronous, but they're guarded too for
// uniformity and because a future async reload there would silently start
// racing otherwise.
type fakeReloader struct {
	mu                         sync.Mutex
	clients, records, filters int
}

func (f *fakeReloader) ReloadClients(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients++
	return nil
}
func (f *fakeReloader) ReloadRecords(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records++
	return nil
}
func (f *fakeReloader) RefreshFilters(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters++
	return nil
}

func (f *fakeReloader) counts() (clients, records, filters int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients, f.records, f.filters
}

// testServer builds a Server on a real sqlite store; helpers reused by all
// later handler-task tests in this package.
func testServer(t *testing.T) (*Server, store.Store, *fakeReloader) {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc := auth.New(s.Users(), s.Tokens())
	rl := &fakeReloader{}
	srv := New(Deps{Store: s, Auth: svc, Engine: filter.NewEngine(), Reloader: rl, Version: "test"})
	return srv, s, rl
}

// login creates the admin (if needed) and returns a session cookie.
func login(t *testing.T, srv *Server, s store.Store) *http.Cookie {
	t.Helper()
	_ = srv.deps.Auth.CreateAdmin(context.Background(), "admin", "password123")
	tok, err := srv.deps.Auth.Login(context.Background(), "admin", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "dnsaur_session", Value: tok}
}

func doReq(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, stringsReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHealthUnauthenticated(t *testing.T) {
	srv, _, _ := testServer(t)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/health", "", nil)
	if w.Code != 200 {
		t.Fatalf("health: %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ok" || body["version"] != "test" {
		t.Fatalf("body: %v", body)
	}
}

func TestUnknownRouteIs404JSON(t *testing.T) {
	srv, _, _ := testServer(t)
	w := doReq(t, srv.Handler(), "GET", "/api/v1/nope", "", nil)
	if w.Code != 404 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

// TestStaticMountDoesNotShadowAPI closes a gap left by Task 1 (flagged in
// Task 14's brief): every other test in this package builds its Server via
// testServer, which always leaves Deps.Static nil — so no committed test
// ever exercised the SPA mount and the API routes together on the same
// Server. That matters because Server.routes() registers the SPA's "/"
// catch-all last, after the "/api/" JSON-404 catch-all; Go's ServeMux picks
// the most specific pattern regardless of registration order, but a future
// refactor that changed pattern specificity (e.g. widening an API pattern,
// or narrowing the root mount) could silently let the SPA shadow /api
// without any test noticing. This builds a Server with a real (fake)
// embedded index and asserts both halves still work side by side: the API
// still answers JSON, and a client-side route still falls back to the SPA's
// index.html instead of hitting the SPA's own 404 or the API's JSON 404.
func TestStaticMountDoesNotShadowAPI(t *testing.T) {
	s, err := store.Open(context.Background(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	static := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>dnsaur</title>")},
	}
	srv := New(Deps{
		Store: s, Auth: auth.New(s.Users(), s.Tokens()), Engine: filter.NewEngine(),
		Reloader: &fakeReloader{}, Version: "test", Static: static,
	})

	// The API must still answer JSON, not be shadowed by the SPA mount.
	w := doReq(t, srv.Handler(), "GET", "/api/v1/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("health content-type: %s", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("health body: %s (err %v)", w.Body.String(), err)
	}

	// An unknown /api/ path must still 404 as JSON, not fall through to the
	// SPA's index.html.
	w = doReq(t, srv.Handler(), "GET", "/api/v1/nope", "", nil)
	if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("unknown api route: %d %s", w.Code, w.Header().Get("Content-Type"))
	}

	// A deep client-side route (e.g. /settings) must fall back to the SPA's
	// index.html with a 200, not a 404.
	w = doReq(t, srv.Handler(), "GET", "/settings", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("deep route: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "dnsaur") {
		t.Fatalf("deep route body isn't the SPA index: %s", w.Body.String())
	}
}

func TestAuthRequired(t *testing.T) {
	srv, s, _ := testServer(t)
	// /api/v1/auth/me is registered in Task 6; use a probe route registered
	// behind requireAuth for this test:
	srv.mux.HandleFunc("GET /api/v1/_probe", srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"user": userFrom(r).Username})
	}))
	if w := doReq(t, srv.Handler(), "GET", "/api/v1/_probe", "", nil); w.Code != 401 {
		t.Fatalf("no auth: %d", w.Code)
	}
	cookie := login(t, srv, s)
	if w := doReq(t, srv.Handler(), "GET", "/api/v1/_probe", "", cookie); w.Code != 200 {
		t.Fatalf("cookie auth: %d %s", w.Code, w.Body.String())
	}
	// bearer path
	tok, _ := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "t", "write")
	req := httptest.NewRequest("GET", "/api/v1/_probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("bearer auth: %d", w.Code)
	}
}

func TestReadScopeToken(t *testing.T) {
	srv, s, _ := testServer(t)
	probe := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"user": userFrom(r).Username})
	})
	srv.mux.HandleFunc("GET /api/v1/_probe", probe)
	srv.mux.HandleFunc("POST /api/v1/_probe", probe)

	// login first so the admin user (id 1) exists before minting an API
	// token for it, mirroring TestAuthRequired's ordering.
	_ = login(t, srv, s)
	tok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "ro", "read")
	if err != nil {
		t.Fatal(err)
	}

	getReq := httptest.NewRequest("GET", "/api/v1/_probe", nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getW, getReq)
	if getW.Code != 200 {
		t.Fatalf("read-scope GET: %d %s", getW.Code, getW.Body.String())
	}

	postReq := httptest.NewRequest("POST", "/api/v1/_probe", nil)
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(postW, postReq)
	if postW.Code != 403 || !strings.Contains(postW.Body.String(), "read-only token") {
		t.Fatalf("read-scope POST: %d %s", postW.Code, postW.Body.String())
	}
}

func TestStoreErr(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"not found", store.ErrNotFound, http.StatusNotFound, "not found"},
		{"in use", store.ErrInUse, http.StatusConflict, "resource in use"},
		{"other", errors.New("boom"), http.StatusServiceUnavailable, "storage unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			storeErr(w, tc.err)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tc.wantCode)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body["error"] != tc.wantMsg {
				t.Fatalf("body = %v, want error=%q", body, tc.wantMsg)
			}
		})
	}
}

type decodeProbe struct {
	Name string `json:"name"`
}

func TestDecode(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"a":1}`))
		v, err := decode[map[string]any](req)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v["a"] != float64(1) {
			t.Fatalf("v = %v", v)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"a","extra":"b"}`))
		if _, err := decode[decodeProbe](req); err == nil {
			t.Fatal("expected error for unknown field")
		}
	})

	t.Run("body too large", func(t *testing.T) {
		big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
		req := httptest.NewRequest("POST", "/x", strings.NewReader(big))
		if _, err := decode[decodeProbe](req); err == nil {
			t.Fatal("expected error for oversized body")
		}
	})
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
