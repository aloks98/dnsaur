package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

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
	mu                                              sync.Mutex
	clients, records, filters, recompiles, notifies int
	// refreshed is the ids passed to RefreshList, in order, so a test can
	// tell a per-list download from the all-lists one. nextRefreshAt is
	// what NextFilterRefresh reports; set it before the request, since
	// nothing here runs a ticker.
	refreshed     []int64
	nextRefreshAt int64
}

func (f *fakeReloader) ReloadClients(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients++
	return nil
}
func (f *fakeReloader) ReloadZones(ctx context.Context) error {
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

func (f *fakeReloader) RefreshList(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, id)
	return nil
}

func (f *fakeReloader) refreshedLists() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.refreshed...)
}

func (f *fakeReloader) NextFilterRefresh() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextRefreshAt
}

func (f *fakeReloader) RecompileFilters(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recompiles++
	return nil
}

func (f *fakeReloader) recompileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recompiles
}

func (f *fakeReloader) NotifyZones() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifies++
}

func (f *fakeReloader) counts() (clients, records, filters int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients, f.records, f.filters
}

func (f *fakeReloader) notifyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifies
}

// testServer builds a Server on a real sqlite store; helpers reused by all
// later handler-task tests in this package.
// opts adjust Deps before the Server is built, for the few tests that need a
// dependency the default harness deliberately leaves nil (see
// Deps.ZoneRefresher). Variadic so every existing testServer(t) call site
// keeps meaning "the default server".
func testServer(t *testing.T, opts ...func(*Deps)) (*Server, store.Store, *fakeReloader) {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc := auth.New(s.Users(), s.Tokens())
	rl := &fakeReloader{}
	d := Deps{Store: s, Auth: svc, Engine: filter.NewEngine(), Reloader: rl, Version: "test"}
	for _, opt := range opts {
		opt(&d)
	}
	srv := New(d)
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

// tailStream opens a live SSE tail, calls publish, and returns the response
// together with the first line the handler wrote to it.
//
// A real server rather than a recorder, and two waits go away with it.
// handleQueriesTail subscribes *before* it writes the response head, so a
// client holding the headers is a client the next Publish reaches — no
// sleeping to "let the handler subscribe" — and reading the body blocks
// until the event lands rather than guessing at how long that takes. The
// client timeout is what bounds that read, so a handler that never writes
// fails here instead of hanging the package.
func tailStream(t *testing.T, h http.Handler, cookie *http.Cookie, reqHeader http.Header, publish func()) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(h)
	// Registered first, so LIFO closes the body before this: ts.Close waits
	// for the handler, and the handler returns when the client hangs up.
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/queries/tail", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	for k, vs := range reqHeader {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tail: %d", resp.StatusCode)
	}

	publish()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first SSE line: %v", err)
	}
	return resp, line
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
// Server. That matters because Server.buildMux() registers the SPA's "/"
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
	// behind requireAuth for this test. route(), not srv.mux.HandleFunc: mux
	// doesn't exist until Handler() is first called (see server.go), and
	// route() is the only supported way to add one before then.
	srv.route("GET /api/v1/_probe", srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
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
	_, tok, _ := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "t", "write")
	req := httptest.NewRequest("GET", "/api/v1/_probe", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("bearer auth: %d", w.Code)
	}
}

// RFC 9110 §11.1: an authentication scheme name is case-insensitive. A
// client sending "bearer <token>" was answered 401 "authentication
// required", which reads as a bad token rather than as a rejected spelling.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	srv, s, _ := testServer(t)
	srv.route("GET /api/v1/_probe", srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"user": userFrom(r).Username})
	}))
	_ = login(t, srv, s)
	_, tok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "case", "write")
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		req := httptest.NewRequest("GET", "/api/v1/_probe", nil)
		req.Header.Set("Authorization", scheme+" "+tok)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("scheme %q: status = %d %s", scheme, w.Code, w.Body)
		}
	}
}

// http.ErrAbortHandler is the one panic value net/http defines as *not* an
// error: it means "stop this response silently", and the server itself
// recovers it. Swallowing it here logged a bogus "api panic" and then tried
// to write a 500 onto a connection the handler had deliberately abandoned,
// so it has to be re-panicked for net/http to see.
func TestRecoverPanicRepanicsAbortHandler(t *testing.T) {
	srv, _, _ := testServer(t)
	srv.route("GET /api/v1/_abort", func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	srv.route("GET /api/v1/_boom", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := srv.Handler()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/_abort", nil))
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("recovered = %v; want ErrAbortHandler to travel on to net/http", recovered)
	}

	// The other half: an ordinary panic is still turned into a 500 rather
	// than taking the connection down.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/_boom", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("ordinary panic: status = %d, want 500", w.Code)
	}
}

func TestReadScopeToken(t *testing.T) {
	srv, s, _ := testServer(t)
	probe := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"user": userFrom(r).Username})
	})
	srv.route("GET /api/v1/_probe", probe)
	srv.route("POST /api/v1/_probe", probe)

	// login first so the admin user (id 1) exists before minting an API
	// token for it, mirroring TestAuthRequired's ordering.
	_ = login(t, srv, s)
	_, tok, err := srv.deps.Auth.CreateAPIToken(context.Background(), 1, "ro", "read")
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
		{"duplicate", store.ErrDuplicate, http.StatusConflict, "already exists"},
		// A uniqueness violation arrives joined with the driver error, so
		// the mapping has to survive wrapping — matching on the sentinel,
		// not on the error string.
		{"duplicate wrapped", errors.Join(store.ErrDuplicate, errors.New("UNIQUE constraint failed: groups.name")), http.StatusConflict, "already exists"},
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

// TestCrossSiteCookieWriteRefused is CSRF defence in depth. SameSite=Strict
// is the only thing standing between the session cookie and a form posted
// from another origin, and it is one browser-configuration or
// cookie-policy quirk away from not being there. Sec-Fetch-Site is sent by
// every browser that implements fetch metadata and cannot be set by page
// script, so a cookie-authenticated write that announces itself as
// cross-site is refused outright.
//
// Bearer requests are deliberately untouched: a token is not attached
// automatically by the browser, so a cross-site script that has one has
// already won, and scripts and CLI clients legitimately send no
// Sec-Fetch-Site at all.
func TestCrossSiteCookieWriteRefused(t *testing.T) {
	srv, s, _ := testServer(t)
	cookie := login(t, srv, s)
	_, tok, err := srv.deps.Auth.CreateAPIToken(t.Context(), 1, "csrf", "write")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	req := func(method, site, bearer string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/auth/logout", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	if w := req("POST", "cross-site", "", cookie); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site cookie POST = %d %s, want 403", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var body map[string]string
	_ = json.Unmarshal(req("POST", "cross-site", "", cookie).Body.Bytes(), &body)
	if body["error"] != "cross-site request" {
		t.Fatalf("error string = %q", body["error"])
	}
	// A bearer credential is not ambient, so the same header changes nothing.
	if w := req("POST", "cross-site", tok, nil); w.Code == http.StatusForbidden {
		t.Fatalf("cross-site bearer POST refused: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	// Everything a browser or a script legitimately sends still works.
	for _, site := range []string{"same-origin", "same-site", "none", ""} {
		if w := req("POST", site, "", cookie); w.Code == http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site: %q cookie POST refused: %d", site, w.Code)
		}
		// A fresh session, since a successful logout revoked the last one.
		cookie = login(t, srv, s)
	}
	// Reads are not state-changing, so they are not the target.
	if w := req("GET", "cross-site", "", cookie); w.Code == http.StatusForbidden {
		t.Fatalf("cross-site cookie GET refused: %d", w.Code)
	}
}
