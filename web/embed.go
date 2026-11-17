//go:build ui

// Package web holds the control plane's web UI, compiled into the binary so
// the whole system ships as one file.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the built UI. Paths that are not files fall back to
// index.html, so a link to a run can be opened or reloaded directly.
func Handler() http.Handler {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if _, err := fs.Stat(root, name); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			} else if strings.HasPrefix(name, "assets/") {
				// Asset file names contain a content hash, so they can be cached forever.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
		}
		files.ServeHTTP(w, r)
	})
}
