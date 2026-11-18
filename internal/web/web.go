// Package web serves the console: static files embedded in the binary, so the
// engine ships as one artifact with no separate frontend build.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// The console renders model and tool output, so scripts are limited to its own
// files and the page cannot be framed.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the console.
func Handler() http.Handler {
	root, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	server := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		server.ServeHTTP(w, r)
	})
}
