// Package controlplane exposes the assets bundled with the control-plane
// binary. Runtime behavior lives in internal packages and cmd entrypoints.
package controlplane

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/index.html web/app.css web/app.js
var embeddedUI embed.FS

// UIHandler returns a handler for the embedded product UI. Assets are immutable
// within a binary; index.html is deliberately not cached so a newly deployed
// binary is visible immediately.
func UIHandler() http.Handler {
	root, err := fs.Sub(embeddedUI, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Cache-Control", "no-cache")
		case "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
			return
		case "/app.css", "/app.js":
			w.Header().Set("Cache-Control", "public, max-age=3600")
		default:
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
