package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist is the built SPA, rooted at the dist directory. Empty when the app
// hasn't been built (backend-only dev) — callers must tolerate that.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return distFS
	}
	return sub
}
