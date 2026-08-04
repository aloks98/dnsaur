package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// StaticHandler serves an embedded SPA: real files by path, otherwise the
// index.html fallback so client-side routes reload correctly. An empty or
// index-less FS yields 404 (backend-only build) rather than panicking.
func StaticHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}
		f, err := fsys.Open(clean)
		if err == nil {
			_ = f.Close()
			setCacheHeaders(w, clean)
			fileServer.ServeHTTP(w, r)
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "static error", http.StatusInternalServerError)
			return
		}
		// fallback to index.html
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
	})
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
