package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aloks98/dnsaur/internal/auth"
	"github.com/aloks98/dnsaur/internal/filter"
	"github.com/aloks98/dnsaur/internal/qlog"
	"github.com/aloks98/dnsaur/internal/store"
)

func TestStaticHandlerServesAssetsAndFallsBack(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	h := StaticHandler(fsys)

	// existing asset served
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Code != 200 || w.Body.String() != "console.log(1)" {
		t.Fatalf("asset: %d %q", w.Code, w.Body.String())
	}
	// unknown client route → index.html fallback
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") == "" {
		t.Fatalf("fallback: %d", w.Code)
	}
	if got := w.Body.String(); got == "" {
		t.Fatal("fallback body empty")
	}
}

func TestStaticHandlerEmptyFSIs404NotPanic(t *testing.T) {
	h := StaticHandler(fstest.MapFS{}) // empty build
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty fs: %d", w.Code)
	}
	_ = fs.ValidPath("index.html")
}

// The production bundle is ~2 MB of JS+CSS; without this the cold load ships
// all of it uncompressed.
func TestStaticHandlerGzipsAssets(t *testing.T) {
	// Compressible only if it's big enough to actually shrink; repeat so the
	// gzip framing overhead can't mask a no-op.
	original := []byte(strings.Repeat("console.log('dnsaur');\n", 500))
	h := StaticHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js": {Data: original},
	})

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length %q must not describe the identity body", got)
	}
	if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", w.Header().Get("Vary"))
	}
	if w.Body.Len() >= len(original) {
		t.Fatalf("compressed %d bytes >= original %d", w.Body.Len(), len(original))
	}

	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("decompressed body differs from original (%d vs %d bytes)", len(got), len(original))
	}

	// No Accept-Encoding → identity, unchanged.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/assets/app.js", nil))
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("identity request got Content-Encoding %q", w.Header().Get("Content-Encoding"))
	}
	if !bytes.Equal(w.Body.Bytes(), original) {
		t.Fatal("identity body differs from original")
	}
}

// Compression must be scoped to the static mount only: /api/v1/queries/tail
// is SSE and buffering it would break live tailing. Runs against a server
// that HAS the static mount enabled, so a mis-scoped wrapper would show up.
func TestGzipAndCSPDoNotTouchAPIOrSSE(t *testing.T) {
	s, err := store.Open(context.Background(), "sqlite", t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc := auth.New(s.Users(), s.Tokens())
	srv := New(Deps{
		Store: s, Auth: svc, Engine: filter.NewEngine(), Reloader: &fakeReloader{}, Version: "test",
		Static: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
	})
	cookie := login(t, srv, s)
	logger := qlog.New(&nullQLStore{}, qlog.Options{InstanceID: "i", FlushEvery: time.Hour, BatchSize: 100})
	srv.deps.Logger = logger

	req := httptest.NewRequest("GET", "/api/v1/queries/tail", nil)
	req.AddCookie(cookie)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
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

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE Content-Encoding = %q, want none (must stay unbuffered)", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q", ct)
	}
	// The static hardening headers must not leak onto API responses.
	if got := w.Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("API got CSP %q", got)
	}
	// Body is plain, readable SSE — not gzip framing.
	line, _ := bufio.NewReader(w.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "live.example") {
		t.Fatalf("sse line: %q", line)
	}

	// Same server, static mount: compression and CSP are present there.
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Header().Get("Content-Encoding") != "gzip" || w2.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("static mount not wrapped: enc=%q csp=%q",
			w2.Header().Get("Content-Encoding"), w2.Header().Get("Content-Security-Policy"))
	}
	// A plain JSON API response is likewise left alone.
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/api/v1/health", nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	srv.Handler().ServeHTTP(w3, req3)
	if got := w3.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("API health Content-Encoding = %q", got)
	}
}

func TestStaticHandlerDirectoryIsNotListed(t *testing.T) {
	h := StaticHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	})
	for _, p := range []string{"/assets/", "/assets"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404 (body %q)", p, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "app.js") {
			t.Fatalf("%s leaked a directory listing", p)
		}
	}
}

func TestStaticHandlerMissingAssetIs404NotIndex(t *testing.T) {
	h := StaticHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	})
	// Missing build output / files with an extension → honest 404.
	for _, p := range []string{"/assets/nope.js", "/assets/chunk-abc.js", "/favicon.svg", "/fonts/x.woff2"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", p, w.Code)
		}
	}
	// Client-side routes still fall back to the SPA shell.
	for _, p := range []string{"/settings", "/filtering/lists", "/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", p, w.Code)
		}
	}
}

func TestStaticHandlerSecurityHeaders(t *testing.T) {
	h := StaticHandler(fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>dnsaur</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	})
	for _, p := range []string{"/", "/settings", "/assets/app.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s X-Content-Type-Options = %q", p, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Fatalf("%s Referrer-Policy = %q", p, got)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s CSP = %q", p, csp)
		}
		if !strings.Contains(csp, "script-src 'self'") {
			t.Fatalf("%s CSP missing script-src: %q", p, csp)
		}
	}
}
