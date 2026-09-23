// Package web serves the embedded single-file admin interface. The same
// index.html is committed at internal/web/index.html so it can also be opened
// directly in a browser and pointed at a running gateway.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

// FaviconHandler serves the embedded SVG icon for both /favicon.svg and
// /favicon.ico (browsers accept SVG bytes on the .ico path), so tab icons
// work and automatic /favicon.ico probes don't 404.
func FaviconHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconSVG)
	}
}

// Handler serves the UI under /webui. Only exact index paths return HTML so
// stray API typos keep their JSON 404s.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/webui", "/webui/", "/webui/index.html":
			// ok
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	}
}
