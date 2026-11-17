//go:build !ui

package web

import "net/http"

// Handler serves a placeholder when the binary was built without the control
// plane's web UI. Build the UI and compile with -tags ui to embed it.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("drydock API is running.\n\nThe web UI is not built into this binary.\nBuild it with:  cd web && npm ci && npm run build\nthen compile with:  go build -tags ui ./cmd/drydock\n"))
	})
}
