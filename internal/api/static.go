package api

import (
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// contentSecurityPolicy for the dashboard. Vite emits external <script
// type="module"> / <link rel="stylesheet"> (no inline script), so script-src
// stays strict. style-src needs 'unsafe-inline' because runtime style
// injection is unavoidable in the bundle (sonner and the rnui components
// insert <style> elements / set element style attributes). data: images cover
// the TOTP QR code, which qrcode renders to a data URL.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// StaticHandler serves an embedded SPA: real files by path, otherwise the
// index.html fallback so client-side routes reload correctly. An empty or
// index-less FS yields 404 (backend-only build) rather than panicking.
//
// Responses are gzip-compressed when the client asks for it (the production
// bundle is ~2 MB of JS+CSS uncompressed) and carry conservative browser
// hardening headers. Both apply to the static mount ONLY — the API mux is
// untouched, which matters for GET /api/v1/queries/tail (SSE, must stay
// unbuffered).
func StaticHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return gzipStatic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}
		f, err := fsys.Open(clean)
		if err == nil {
			st, serr := f.Stat()
			_ = f.Close()
			// A directory satisfies Open but isn't a real file: letting it
			// through makes FileServer emit an unauthenticated <pre> index
			// listing (e.g. GET /assets/). Treat it as a miss.
			if serr == nil && !st.IsDir() {
				setCacheHeaders(w, clean)
				fileServer.ServeHTTP(w, r)
				return
			}
			err = fs.ErrNotExist
		}
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "static error", http.StatusInternalServerError)
			return
		}
		// Only client-side routes get the index.html fallback. A missing
		// asset must 404 rather than return HTML: once Vite code-splits, a
		// missing chunk served as text/html is a module MIME error instead
		// of an honest 404, and missing fonts/images would silently 200.
		if !isSPARoute(clean) {
			http.NotFound(w, r)
			return
		}
		index, ierr := fsys.Open("index.html")
		if ierr != nil {
			http.NotFound(w, r)
			return
		}
		_ = index.Close()
		w.Header().Set("Cache-Control", "no-cache")
		// net/http's FileServer unconditionally 301-redirects any request
		// whose URL.Path ends in "/index.html" (its own directory
		// canonicalization). Point at "/" instead: FileServer resolves that
		// to the root directory and serves its index.html internally,
		// without re-triggering that redirect.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	}))
}

// isSPARoute reports whether a miss should fall back to index.html. Anything
// under assets/ is build output, and anything with a non-.html extension is
// asking for a file, not a route. Safe for this app: every dashboard route
// (/, /queries, /filtering, /dns, /settings, /account — see web/src/app.tsx)
// is extensionless, and none take a dotted path param.
func isSPARoute(clean string) bool {
	if clean == "assets" || strings.HasPrefix(clean, "assets/") {
		return false
	}
	ext := path.Ext(clean)
	return ext == "" || ext == ".html"
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
}

func setCacheHeaders(w http.ResponseWriter, name string) {
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

var gzipPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

// gzipStatic compresses next's responses when the client advertises gzip.
// Deliberately scoped to the static mount: never wrap the API mux, whose
// /api/v1/queries/tail endpoint streams SSE and must not be buffered.
func gzipStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Unconditional: the response body depends on Accept-Encoding either
		// way, so shared caches must key on it even for identity responses.
		w.Header().Add("Vary", "Accept-Encoding")
		// Range requests are byte offsets into the *identity* encoding;
		// compressing them would return the wrong bytes.
		if !acceptsGzip(r) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" is an explicit refusal.
		return strings.ReplaceAll(params, " ", "") != "q=0"
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	zw          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipResponseWriter) WriteHeader(code int) {
	if code < 200 {
		// 1xx informational — not the final header, don't latch on it.
		g.ResponseWriter.WriteHeader(code)
		return
	}
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	h := g.Header()
	if compressibleStatus(code) && h.Get("Content-Encoding") == "" &&
		compressibleType(h.Get("Content-Type")) {
		// Content-Length describes the identity body; ServeContent already
		// set it from the file size, so it must go.
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		if etag := h.Get("Etag"); etag != "" {
			h.Set("Etag", strings.TrimSuffix(etag, `"`)+`-gzip"`)
		}
		zw, _ := gzipPool.Get().(*gzip.Writer)
		zw.Reset(g.ResponseWriter)
		g.zw = zw
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		// net/http sniffs Content-Type from the first Write; do the same so
		// compressibleType sees the type FileServer would have set.
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.zw != nil {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) close() {
	if g.zw == nil {
		return
	}
	_ = g.zw.Close()
	g.zw.Reset(io.Discard) // drop the reference to the real ResponseWriter
	gzipPool.Put(g.zw)
	g.zw = nil
}

func compressibleStatus(code int) bool {
	// 204 and 304 carry no body.
	return code != http.StatusNoContent && code != http.StatusNotModified
}

func compressibleType(ct string) bool {
	mediaType, _, _ := strings.Cut(ct, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/javascript", "text/javascript", "application/json",
		"application/wasm", "application/xml", "image/svg+xml",
		"application/manifest+json", "application/x-javascript":
		return true
	}
	return false
}
