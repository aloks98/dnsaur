package api

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
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
